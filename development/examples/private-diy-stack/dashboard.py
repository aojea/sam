#!/usr/bin/env python3
"""Interactive Web Workbench for the Private DIY Enterprise Stack.

Starts the real Agent Mesh control plane (agentmesh-one), four agentmesh-node
sidecars (node-tools, node-inference, node-agent, node-external), and the
SQLite ERP / Compliance / DeepSeek backends across on-prem and cloud zones,
then serves a live browser dashboard on http://127.0.0.1:18899.
"""

import http.server
import json
import os
import re
import shutil
import signal
import sqlite3
import subprocess
import sys
import tempfile
import time
import urllib.error
import urllib.parse
import urllib.request

for _proxy_var in ("HTTP_PROXY", "HTTPS_PROXY", "http_proxy", "https_proxy", "ALL_PROXY", "all_proxy"):
    os.environ.pop(_proxy_var, None)
os.environ["NO_PROXY"] = "*"
os.environ["no_proxy"] = "*"

HERE = os.path.dirname(os.path.abspath(__file__))
sys.path.insert(0, HERE)
from agent_harness import LocalSidecarClient  # noqa: E402

REPO_ROOT = os.path.abspath(os.path.join(HERE, "..", "..", ".."))
STATIC_DIR = os.path.join(HERE, "static")
DASHBOARD_PORT = int(os.environ.get("DASHBOARD_PORT", "18899"))
CP_PORT = int(os.environ.get("CP_PORT", "18095"))
CP_URL = f"http://127.0.0.1:{CP_PORT}"
AGENT_URL = "http://127.0.0.1:19101"
AGENT_TOKEN = "agent-sidecar-secret"
EXTERNAL_TOKEN = "external-sidecar-secret"

_OPENER = urllib.request.build_opener(urllib.request.ProxyHandler({}))


def _find_binaries():
    candidates = [
        os.environ.get("BIN_DIR", ""),
        os.path.join(REPO_ROOT, "bin"),
        "/tmp/sam-bin",
    ]
    for d in candidates:
        if d and os.path.isfile(os.path.join(d, "agentmesh-one")) and os.path.isfile(os.path.join(d, "agentmesh-node")):
            return d
    raise RuntimeError("agentmesh-one and agentmesh-node binaries not found; run `make build` first.")


class MeshEnvironment:
    def __init__(self):
        self.bin_dir = _find_binaries()
        self.agentmesh_one = os.path.join(self.bin_dir, "agentmesh-one")
        self.agentmesh_node = os.path.join(self.bin_dir, "agentmesh-node")
        self.mcp_client = os.path.join(self.bin_dir, "mcp-client")
        self.work_dir = tempfile.mkdtemp(prefix="sam-private-diy-ui.")
        self.db_path = os.path.join(self.work_dir, "erp.db")
        self.procs = []
        self.tools_peer = ""
        self.gpu_peer = ""
        self.agent_peer = ""
        self.external_peer = ""
        self.external_banned = False

    def start(self):
        os.makedirs(os.path.join(self.work_dir, "secrets"), exist_ok=True)
        for name in ("cp", "node-tools", "node-inference", "node-agent", "node-external"):
            os.makedirs(os.path.join(self.work_dir, name), exist_ok=True)

        self._write_secret("compliance-api-key", "vault-brokered-compliance-key-99481")
        self._write_file("tools-api.token", "vault-sidecar-secret")
        self._write_file("gpu-api.token", "gpu-sidecar-secret")
        self._write_file("agent-api.token", AGENT_TOKEN)
        self._write_file("external-api.token", EXTERNAL_TOKEN)

        env = os.environ.copy()
        env["ERP_DB_PATH"] = self.db_path
        env["COMPLIANCE_API_SECRET"] = "vault-brokered-compliance-key-99481"

        backends_log = open(os.path.join(self.work_dir, "backends.log"), "w")
        self.procs.append(
            subprocess.Popen([sys.executable, os.path.join(HERE, "backends.py")], env=env, stdout=backends_log, stderr=subprocess.STDOUT)
        )
        self._wait_http("http://127.0.0.1:18890/healthz")
        self._wait_http("http://127.0.0.1:18891/healthz")
        self._wait_http("http://127.0.0.1:18892/healthz")

        cp_log_path = os.path.join(self.work_dir, "cp.log")
        cp_log = open(cp_log_path, "w")
        self.procs.append(
            subprocess.Popen(
                [
                    self.agentmesh_one,
                    "--port",
                    str(CP_PORT),
                    "--data-dir",
                    os.path.join(self.work_dir, "cp"),
                    "--policy-file",
                    os.path.join(HERE, "policy.json"),
                    "--no-join-token",
                    "--enroll-qr=false",
                    "--log-level",
                    "info",
                ],
                env=env,
                stdout=cp_log,
                stderr=subprocess.STDOUT,
            )
        )
        for _ in range(80):
            if os.path.exists(os.path.join(self.work_dir, "cp", "admin-token")) and os.path.exists(cp_log_path):
                if "Agent Mesh standalone mesh is ready" in open(cp_log_path, errors="replace").read():
                    break
            time.sleep(0.25)
        else:
            raise RuntimeError("Timed out waiting for agentmesh-one to become ready")

        for tok_name in ("tools", "gpu", "agent", "external"):
            raw_tok = subprocess.check_output(
                [
                    self.agentmesh_one,
                    "token",
                    "create",
                    "--server",
                    CP_URL,
                    "--data-dir",
                    os.path.join(self.work_dir, "cp"),
                    "--max-usages",
                    "1",
                    "--description",
                    f"{tok_name}-sidecar",
                ],
                env=env,
                stderr=subprocess.DEVNULL,
                text=True,
            )
            m = re.search(r"^Token:\s+(\S+)", raw_tok, re.MULTILINE)
            tok = m.group(1) if m else raw_tok.strip()
            self._write_file(f"{tok_name}.token", tok)

        self._start_node(
            "node-tools",
            "tools.token",
            "node-tools.yaml",
            "127.0.0.1:19102",
            "tools-api.token",
            extra_args=["--secrets-dir", os.path.join(self.work_dir, "secrets")],
        )
        self._start_node("node-inference", "gpu.token", "node-inference.yaml", "127.0.0.1:19103", "gpu-api.token")
        self._start_node("node-agent", "agent.token", "node-agent.yaml", "127.0.0.1:19101", "agent-api.token")
        self._start_node("node-external", "external.token", "node-external.yaml", "127.0.0.1:19104", "external-api.token")

        self.tools_peer = self._wait_peer("node-tools.log")
        self.gpu_peer = self._wait_peer("node-inference.log")
        self.agent_peer = self._wait_peer("node-agent.log")
        self.external_peer = self._wait_peer("node-external.log")

        for _ in range(50):
            try:
                req1 = urllib.request.Request(
                    f"{AGENT_URL}/mesh/service/discover?type=mcp&name=erp-db",
                    headers={"X-Mesh-Authentication": f"Bearer {AGENT_TOKEN}"},
                )
                req2 = urllib.request.Request(
                    f"{AGENT_URL}/mesh/service/discover?type=inference&name=private-llm",
                    headers={"X-Mesh-Authentication": f"Bearer {AGENT_TOKEN}"},
                )
                d1 = json.loads(_OPENER.open(req1, timeout=2).read().decode("utf-8"))
                d2 = json.loads(_OPENER.open(req2, timeout=2).read().decode("utf-8"))
                if d1 and d2:
                    break
            except Exception:
                pass
            time.sleep(0.3)

    def _write_secret(self, name, val):
        path = os.path.join(self.work_dir, "secrets", name)
        with open(path, "w") as f:
            f.write(val)
        os.chmod(path, 0o600)

    def _write_file(self, name, val):
        path = os.path.join(self.work_dir, name)
        with open(path, "w") as f:
            f.write(val)
        os.chmod(path, 0o600)

    def _start_node(self, name, token_file, yaml_file, bind_addr, api_token_file, extra_args=None):
        log_f = open(os.path.join(self.work_dir, f"{name}.log"), "w")
        cmd = [
            self.agentmesh_node,
            "run",
            "--control-plane",
            CP_URL,
            "--insecure-control-plane",
            "--bootstrap-token-path",
            os.path.join(self.work_dir, token_file),
            "--config",
            os.path.join(HERE, yaml_file),
            "--data-dir",
            os.path.join(self.work_dir, name),
            "--bind-addr",
            bind_addr,
            "--socket-path=",
            "--listen",
            "/ip4/127.0.0.1/udp/0/quic-v1,/ip4/127.0.0.1/tcp/0",
            "--api-token-path",
            os.path.join(self.work_dir, api_token_file),
            "--allow-loopback",
            "--control-plane-sync-interval",
            "5s",
        ]
        if extra_args:
            cmd.extend(extra_args)
        self.procs.append(subprocess.Popen(cmd, stdout=log_f, stderr=subprocess.STDOUT))

    def _wait_http(self, url, timeout=20.0):
        deadline = time.time() + timeout
        while time.time() < deadline:
            try:
                with _OPENER.open(url, timeout=1.5) as resp:
                    if resp.status < 500:
                        return
            except Exception:
                pass
            time.sleep(0.2)
        raise RuntimeError(f"Timed out waiting for {url}")

    def _wait_peer(self, log_name, timeout=25.0):
        log_path = os.path.join(self.work_dir, log_name)
        deadline = time.time() + timeout
        while time.time() < deadline:
            if os.path.exists(log_path):
                text = open(log_path, errors="replace").read()
                if "Agent Mesh Node Online" in text:
                    m = re.search(r"PeerID:\s+(\S+)", text)
                    if m:
                        return m.group(1)
            time.sleep(0.2)
        raise RuntimeError(f"Timed out waiting for PeerID in {log_name}")

    def parse_audit_events(self):
        audit_re = re.compile(r"Audit Traceability\s+(\{.*\})\s*$")
        tar_re = re.compile(r'task authorization denied tool "([^"]+)".*tar_block\[1\] \("([^"]+)"\)')
        events = []
        for log_name in ("node-inference.log", "node-tools.log", "node-agent.log"):
            p = os.path.join(self.work_dir, log_name)
            if not os.path.exists(p):
                continue
            for raw_line in open(p, errors="replace"):
                line = raw_line.rstrip("\n")
                m = audit_re.search(line)
                if m:
                    try:
                        d = json.loads(m.group(1))
                    except json.JSONDecodeError:
                        continue
                    who = f"{d.get('role') or '-'} ({d.get('peer_id', '')[:12]}…)"
                    what = " ".join(x for x in (d.get("method"), d.get("path")) if x) or d.get("protocol", "-")
                    events.append(
                        {
                            "decision": d.get("decision", "?").upper(),
                            "caller": who,
                            "method": what,
                            "service": d.get("target", "-"),
                        }
                    )
                    continue
                tm = tar_re.search(line)
                if tm:
                    events.append(
                        {
                            "decision": "DENY",
                            "caller": f"tar_block ({tm.group(2)})",
                            "method": f"tools/call {tm.group(1)}",
                            "service": "mcp://erp-db",
                        }
                    )
        return events

    def sqlite_counts(self):
        if not os.path.exists(self.db_path):
            return {"invoices_count": 0, "wire_transfers_count": 0}
        conn = sqlite3.connect(self.db_path)
        inv = conn.execute("SELECT COUNT(*) FROM invoices").fetchone()[0]
        wires = conn.execute("SELECT COUNT(*) FROM wire_transfers").fetchone()[0]
        conn.close()
        return {"invoices_count": inv, "wire_transfers_count": wires}

    def stop(self):
        for p in self.procs:
            try:
                p.terminate()
            except Exception:
                pass
        for p in self.procs:
            try:
                p.wait(timeout=3)
            except Exception:
                pass
        shutil.rmtree(self.work_dir, ignore_errors=True)


MESH: MeshEnvironment = None  # type: ignore


class WorkbenchHandler(http.server.BaseHTTPRequestHandler):
    def log_message(self, fmt, *args):
        pass

    def _send_json(self, obj, status=200):
        body = json.dumps(obj).encode("utf-8")
        self.send_response(status)
        self.send_header("Content-Type", "application/json; charset=utf-8")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def _send_file(self, path, content_type):
        if not os.path.isfile(path):
            self.send_error(404)
            return
        data = open(path, "rb").read()
        self.send_response(200)
        self.send_header("Content-Type", content_type)
        self.send_header("Content-Length", str(len(data)))
        self.end_headers()
        self.wfile.write(data)

    def do_GET(self):
        if self.path in ("/", "/index.html"):
            return self._send_file(os.path.join(STATIC_DIR, "index.html"), "text/html; charset=utf-8")
        if self.path in ("/styles.css", "/static/styles.css"):
            return self._send_file(os.path.join(STATIC_DIR, "styles.css"), "text/css; charset=utf-8")
        if self.path in ("/app.js", "/static/app.js"):
            return self._send_file(os.path.join(STATIC_DIR, "app.js"), "application/javascript; charset=utf-8")
        if self.path == "/api/state":
            return self._send_json(
                {
                    "nodes": [
                        {
                            "title": "1. Workstation (node-agent)",
                            "peer_id": MESH.agent_peer,
                            "short_peer": MESH.agent_peer[:16] + "…",
                            "labels": ["residency=on-prem", "zone=workstation", "dept=finance"],
                            "banned": False,
                        },
                        {
                            "title": "2. GPU Node (node-inference)",
                            "peer_id": MESH.gpu_peer,
                            "short_peer": MESH.gpu_peer[:16] + "…",
                            "labels": ["residency=on-prem", "zone=gpu-pool", "inference://private-llm"],
                            "banned": False,
                        },
                        {
                            "title": "3. Vault Node (node-tools)",
                            "peer_id": MESH.tools_peer,
                            "short_peer": MESH.tools_peer[:16] + "…",
                            "labels": ["residency=on-prem", "zone=vault", "dept=finance"],
                            "banned": False,
                        },
                        {
                            "title": "4. External Node (node-external)",
                            "peer_id": MESH.external_peer,
                            "short_peer": MESH.external_peer[:16] + "…",
                            "labels": ["residency=on-prem", "dept=external"],
                            "banned": MESH.external_banned,
                        },
                    ],
                    "sqlite": MESH.sqlite_counts(),
                    "external_banned": MESH.external_banned,
                    "audit": MESH.parse_audit_events(),
                }
            )
        self.send_error(404)

    def do_POST(self):
        length = int(self.headers.get("Content-Length", "0"))
        raw_body = self.rfile.read(length).decode("utf-8") if length > 0 else "{}"
        try:
            body = json.loads(raw_body) if raw_body else {}
        except json.JSONDecodeError:
            body = {}

        if self.path == "/api/act1/discover":
            sidecar = LocalSidecarClient(AGENT_URL, AGENT_TOKEN)
            sidecar.init_mcp()
            cat_raw = sidecar.call_sidecar_tool("find_remote_tools", {"service_name": "erp-db"})
            req = urllib.request.Request(
                f"{AGENT_URL}/v1/models",
                headers={"Authorization": f"Bearer {AGENT_TOKEN}"},
            )
            with _OPENER.open(req, timeout=10) as resp:
                models_data = json.loads(resp.read().decode("utf-8"))
            summary = (
                f"• Vault Tool PeerID      : {MESH.tools_peer} (mcp://erp-db, egress://compliance.internal)\n"
                f"• Bare-Metal GPU PeerID  : {MESH.gpu_peer} (inference://private-llm)\n"
                f"• Discovered Models (/v1): {[m.get('id') for m in models_data.get('data', [])]}\n\n"
                f"• Discovered MCP Tools (127.0.0.1:19101/mcp -> find_remote_tools):\n{cat_raw}"
            )
            return self._send_json({"summary": summary})

        if self.path == "/api/act2/run-agent":
            task = body.get(
                "task",
                "Audit Invoice INV-2026-089 for Acme Robotics, verify SKU-GPU-H200 warehouse stock, and check vendor ACME-01 compliance.",
            )
            sidecar = LocalSidecarClient(AGENT_URL, AGENT_TOKEN)
            sidecar.init_mcp()
            raw_catalog = sidecar.call_sidecar_tool("find_remote_tools", {"service_name": "erp-db"})
            catalog = json.loads(raw_catalog.splitlines()[-1])
            tools_found = [entry["tool_name"] for entry in catalog if "tool_name" in entry]
            vault_peer = catalog[0]["peer_id"]
            step1 = f"Discovered {len(tools_found)} tools on Vault Peer {vault_peer[:16]}…: {', '.join(tools_found)}"

            plan_msg = sidecar.chat_completion(
                [
                    {"role": "system", "content": f"Available mesh tools: {', '.join(tools_found)}"},
                    {"role": "user", "content": task},
                ]
            )
            step2_think, step2_plan = "", plan_msg
            if "<think>" in plan_msg and "</think>" in plan_msg:
                step2_think = plan_msg.split("<think>", 1)[1].split("</think>", 1)[0].strip()
                step2_plan = plan_msg.split("</think>", 1)[1].strip()

            inv_res = sidecar.call_sidecar_tool(
                "call_remote_tool",
                {"peer_id": vault_peer, "tool_name": "mcp://erp-db/lookup_invoice", "arguments": {"invoice_id": "INV-2026-089"}},
            )
            stock_res = sidecar.call_sidecar_tool(
                "call_remote_tool",
                {"peer_id": vault_peer, "tool_name": "mcp://erp-db/check_inventory", "arguments": {"sku": "SKU-GPU-H200"}},
            )
            comp_data = sidecar.call_egress_get(vault_peer, "compliance.internal", "/v1/vendors/ACME-01")

            step3 = (
                f"[mcp://erp-db/lookup_invoice]  -> {inv_res}\n"
                f"[mcp://erp-db/check_inventory] -> {stock_res}\n"
                f"[egress://compliance.internal] -> vendor={comp_data['name']}, sanctions={comp_data['sanctions_check']}, "
                f"iso27001={comp_data['iso27001_certified']} (brokered secret verified={comp_data['brokered_auth_verified']})"
            )

            synth_msg = sidecar.chat_completion(
                [
                    {"role": "user", "content": task},
                    {"role": "assistant", "content": plan_msg},
                    {
                        "role": "user",
                        "content": (
                            f"TOOL_OBSERVATIONS:\n"
                            f"- invoice: {inv_res}\n"
                            f"- inventory: {stock_res}\n"
                            f"- compliance: {json.dumps(comp_data)}"
                        ),
                    },
                ]
            )
            step4_think, step4_decision = "", synth_msg
            if "<think>" in synth_msg and "</think>" in synth_msg:
                step4_think = synth_msg.split("<think>", 1)[1].split("</think>", 1)[0].strip()
                step4_decision = synth_msg.split("</think>", 1)[1].strip()

            return self._send_json(
                {
                    "step1_discovery": step1,
                    "step2_think": step2_think,
                    "step2_plan": step2_plan,
                    "step3_results": step3,
                    "step4_think": step4_think,
                    "step4_decision": step4_decision,
                }
            )

        if self.path == "/api/act3/test":
            check = body.get("check", "3a")
            if check == "3a":
                out = subprocess.run(
                    [
                        MESH.mcp_client,
                        "-url",
                        f"{AGENT_URL}/mcp",
                        "-token-path",
                        os.path.join(MESH.work_dir, "agent-api.token"),
                        "-tool",
                        "call_remote_tool",
                        "-args",
                        json.dumps({"peer_id": MESH.tools_peer, "tool_name": "mcp://payroll-vault/dump_executive_salaries", "arguments": {}}),
                    ],
                    capture_output=True,
                    text=True,
                )
                msg = (out.stdout + out.stderr).strip().splitlines()[-1]
                return self._send_json({"output": f"DENIED -> {msg}"})

            if check == "3b":
                req = urllib.request.Request(
                    f"{AGENT_URL}/mesh/{MESH.tools_peer}/egress/compliance.internal/v1/vendors/ACME-01",
                    data=b'{"sanctions_check":"OVERRIDE"}',
                    headers={"X-Mesh-Authentication": f"Bearer {AGENT_TOKEN}", "Content-Type": "application/json"},
                    method="POST",
                )
                try:
                    with _OPENER.open(req, timeout=10) as resp:
                        msg = f"HTTP {resp.status}"
                except urllib.error.HTTPError as e:
                    msg = f"DENIED -> HTTP {e.code} ({e.read().decode('utf-8', errors='replace').strip()})"
                return self._send_json({"output": msg})

            if check == "3c":
                out = subprocess.run(
                    [
                        MESH.mcp_client,
                        "-url",
                        "http://127.0.0.1:19104/mcp",
                        "-token-path",
                        os.path.join(MESH.work_dir, "external-api.token"),
                        "-tool",
                        "call_remote_tool",
                        "-args",
                        json.dumps({"peer_id": MESH.tools_peer, "tool_name": "mcp://erp-db/lookup_invoice", "arguments": {"invoice_id": "INV-2026-089"}}),
                    ],
                    capture_output=True,
                    text=True,
                )
                msg = (out.stdout + out.stderr).strip().splitlines()[-1]
                return self._send_json({"output": f"DENIED -> {msg}"})

            if check == "3d":
                token_req = urllib.request.Request(
                    f"{AGENT_URL}/oauth/token",
                    data=urllib.parse.urlencode(
                        {
                            "grant_type": "urn:ietf:params:oauth:grant-type:token-exchange",
                            "resource": "mcp://erp-db",
                            "scope": "tool:lookup_invoice tool:check_inventory",
                            "seal": "true",
                        }
                    ).encode("utf-8"),
                    headers={"Authorization": f"Bearer {AGENT_TOKEN}"},
                    method="POST",
                )
                with _OPENER.open(token_req, timeout=10) as resp:
                    task_biscuit = json.loads(resp.read().decode("utf-8"))["access_token"]
                task_token_path = os.path.join(MESH.work_dir, "task-biscuit.token")
                MESH._write_file("task-biscuit.token", task_biscuit)

                allowed_proc = subprocess.run(
                    [
                        MESH.mcp_client,
                        "-url",
                        f"{AGENT_URL}/mcp",
                        "-token-path",
                        task_token_path,
                        "-tool",
                        "call_remote_tool",
                        "-args",
                        json.dumps({"peer_id": MESH.tools_peer, "tool_name": "mcp://erp-db/lookup_invoice", "arguments": {"invoice_id": "INV-2026-089"}}),
                    ],
                    capture_output=True,
                    text=True,
                )
                allowed_out = (allowed_proc.stdout + allowed_proc.stderr).strip().splitlines()[-1]

                denied_proc = subprocess.run(
                    [
                        MESH.mcp_client,
                        "-url",
                        f"{AGENT_URL}/mcp",
                        "-token-path",
                        task_token_path,
                        "-tool",
                        "call_remote_tool",
                        "-args",
                        json.dumps(
                            {
                                "peer_id": MESH.tools_peer,
                                "tool_name": "mcp://erp-db/execute_wire_transfer",
                                "arguments": {"invoice_id": "INV-2026-089", "amount_usd": 250000, "iban": "DE89-ATTACKER"},
                            }
                        ),
                    ],
                    capture_output=True,
                    text=True,
                )
                denied_msg = (denied_proc.stdout + denied_proc.stderr).strip().splitlines()[-1]
                wires = MESH.sqlite_counts()["wire_transfers_count"]
                return self._send_json(
                    {
                        "output": (
                            f"1. Read (lookup_invoice): ALLOW ({allowed_out[:48]}…)\n"
                            f"2. Write (execute_wire_transfer): DENIED -> {denied_msg}\n"
                            f"3. SQLite wire_transfers count: {wires} (database untouched!)"
                        )
                    }
                )

        if self.path == "/api/act4/ban":
            out = subprocess.run(
                [
                    MESH.agentmesh_one,
                    "admin",
                    "ban",
                    MESH.external_peer,
                    "--server",
                    CP_URL,
                    "--data-dir",
                    os.path.join(MESH.work_dir, "cp"),
                ],
                capture_output=True,
                text=True,
            )
            MESH.external_banned = True
            return self._send_json({"output": (out.stdout + out.stderr).strip()})

        self.send_error(404)


def main():
    global MESH
    print("Starting Private DIY Enterprise Stack mesh & live browser workbench...", flush=True)
    MESH = MeshEnvironment()
    try:
        MESH.start()
    except Exception:
        MESH.stop()
        raise

    def _shutdown(signum, frame):
        MESH.stop()
        sys.exit(0)

    signal.signal(signal.SIGINT, _shutdown)
    signal.signal(signal.SIGTERM, _shutdown)

    server = http.server.ThreadingHTTPServer(("127.0.0.1", DASHBOARD_PORT), WorkbenchHandler)
    print(f"READY: Private DIY Enterprise Stack Workbench listening on http://127.0.0.1:{DASHBOARD_PORT}", flush=True)
    try:
        server.serve_forever()
    finally:
        MESH.stop()


if __name__ == "__main__":
    main()
