#!/usr/bin/env python3
# Copyright 2026 Google LLC
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

"""Private On-Premises & Cloud Enterprise Backends (stdlib only, zero pip dependencies).

Starts three loopback services that sit behind their respective agentmesh-node
sidecars:
  1. 127.0.0.1:18890 — Private SQLite ERP (/mcp) & Payroll (/payroll-mcp) MCP servers.
  2. 127.0.0.1:18891 — Private Vendor Compliance REST API (verifies brokered secret).
  3. 127.0.0.1:18892 — Private OpenAI-compatible reasoning endpoint (proxies to a
                       live Ollama/vLLM server at OLLAMA_UPSTREAM if running, or
                       serves deterministic DeepSeek-R1 <think> reasoning locally).
"""

import json
import os
import sqlite3
import threading
import urllib.error
import urllib.request
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

DB_PATH = os.environ.get("ERP_DB_PATH", "/tmp/agentmesh-private-erp.db")
MCP_PORT = int(os.environ.get("ERP_MCP_PORT", "18890"))
COMPLIANCE_PORT = int(os.environ.get("COMPLIANCE_API_PORT", "18891"))
LLM_PORT = int(os.environ.get("PRIVATE_LLM_PORT", "18892"))
LLM_MODEL = os.environ.get("LLM_MODEL", "deepseek-r1:1.5b")
OLLAMA_UPSTREAM = os.environ.get("OLLAMA_UPSTREAM", "http://127.0.0.1:11434")
EXPECTED_COMPLIANCE_SECRET = os.environ.get("COMPLIANCE_API_SECRET", "private-vault-secret-2026")


def init_db() -> None:
    with sqlite3.connect(DB_PATH) as conn:
        conn.execute(
            """
            CREATE TABLE IF NOT EXISTS invoices (
                invoice_id TEXT PRIMARY KEY,
                vendor_id TEXT NOT NULL,
                vendor_name TEXT NOT NULL,
                sku TEXT NOT NULL,
                quantity INTEGER NOT NULL,
                amount_usd REAL NOT NULL,
                status TEXT NOT NULL
            )
            """
        )
        conn.execute(
            """
            CREATE TABLE IF NOT EXISTS inventory (
                sku TEXT PRIMARY KEY,
                description TEXT NOT NULL,
                on_hand INTEGER NOT NULL,
                warehouse TEXT NOT NULL
            )
            """
        )
        conn.execute(
            """
            CREATE TABLE IF NOT EXISTS wire_transfers (
                id INTEGER PRIMARY KEY AUTOINCREMENT,
                invoice_id TEXT NOT NULL,
                amount_usd REAL NOT NULL,
                iban TEXT NOT NULL
            )
            """
        )
        conn.execute(
            """
            INSERT OR REPLACE INTO invoices
                (invoice_id, vendor_id, vendor_name, sku, quantity, amount_usd, status)
            VALUES
                ('INV-2026-089', 'ACME-01', 'Acme Robotics GmbH', 'SKU-GPU-H200', 4, 48500.00, 'PENDING_APPROVAL'),
                ('INV-2026-090', 'GLOBEX-02', 'Globex Logistics', 'SKU-RACK-42U', 2, 6400.00, 'PAID')
            """
        )
        conn.execute(
            """
            INSERT OR REPLACE INTO inventory
                (sku, description, on_hand, warehouse)
            VALUES
                ('SKU-GPU-H200', 'NVIDIA H200 Bare-Metal Server Blade', 14, 'Munich-Vault-B'),
                ('SKU-RACK-42U', '42U Private Server Rack', 5, 'Frankfurt-DC-1')
            """
        )
        conn.commit()


ERP_TOOLS = [
    {
        "name": "lookup_invoice",
        "description": "Look up a corporate invoice by invoice_id in the SQLite ERP database (read-only).",
        "inputSchema": {
            "type": "object",
            "properties": {"invoice_id": {"type": "string", "description": "Invoice ID, e.g. INV-2026-089"}},
            "required": ["invoice_id"],
        },
    },
    {
        "name": "check_inventory",
        "description": "Check warehouse stock levels for a hardware SKU in SQLite (read-only).",
        "inputSchema": {
            "type": "object",
            "properties": {"sku": {"type": "string", "description": "Hardware SKU, e.g. SKU-GPU-H200"}},
            "required": ["sku"],
        },
    },
    {
        "name": "execute_wire_transfer",
        "description": "Execute an outbound bank wire transfer and mark an invoice PAID (mutating financial tool).",
        "inputSchema": {
            "type": "object",
            "properties": {
                "invoice_id": {"type": "string"},
                "amount_usd": {"type": "number"},
                "iban": {"type": "string"},
            },
            "required": ["invoice_id", "amount_usd", "iban"],
        },
    },
]

PAYROLL_TOOLS = [
    {
        "name": "dump_executive_salaries",
        "description": "Export executive compensation records from the restricted HR payroll vault.",
        "inputSchema": {"type": "object", "properties": {}},
    }
]


def call_erp_tool(name: str, args: dict) -> str:
    if name == "lookup_invoice":
        inv_id = str(args.get("invoice_id", ""))
        with sqlite3.connect(DB_PATH) as conn:
            row = conn.execute(
                "SELECT invoice_id, vendor_id, vendor_name, sku, quantity, amount_usd, status "
                "FROM invoices WHERE invoice_id = ?",
                (inv_id,),
            ).fetchone()
        if not row:
            return f"Invoice {inv_id} not found"
        iid, vid, vname, sku, qty, amt, status = row
        return (
            f"Invoice {iid}: vendor={vname} ({vid}), sku={sku}, qty={qty}, "
            f"amount=${amt:,.2f}, status={status}"
        )

    if name == "check_inventory":
        sku = str(args.get("sku", ""))
        with sqlite3.connect(DB_PATH) as conn:
            row = conn.execute(
                "SELECT sku, description, on_hand, warehouse FROM inventory WHERE sku = ?",
                (sku,),
            ).fetchone()
        if not row:
            return f"SKU {sku} not found in inventory"
        s_code, desc, on_hand, wh = row
        return f"SKU {s_code} ({desc}): on_hand={on_hand} units, warehouse={wh}"

    if name == "execute_wire_transfer":
        inv_id = str(args.get("invoice_id", ""))
        amount = float(args.get("amount_usd", 0.0))
        iban = str(args.get("iban", ""))
        with sqlite3.connect(DB_PATH) as conn:
            conn.execute(
                "INSERT INTO wire_transfers (invoice_id, amount_usd, iban) VALUES (?, ?, ?)",
                (inv_id, amount, iban),
            )
            conn.execute("UPDATE invoices SET status = 'PAID' WHERE invoice_id = ?", (inv_id,))
            conn.commit()
        return f"WIRE_EXECUTED: invoice={inv_id}, amount=${amount:,.2f}, destination_iban={iban}"

    return f"Unknown ERP tool: {name}"


class MCPHandler(BaseHTTPRequestHandler):
    def _json(self, status: int, body: dict) -> None:
        data = json.dumps(body).encode("utf-8")
        self.send_response(status)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(data)))
        self.end_headers()
        self.wfile.write(data)

    def do_GET(self) -> None:
        if self.path == "/healthz":
            self._json(200, {"status": "ok"})
            return
        self._json(404, {"error": "not found"})

    def do_POST(self) -> None:
        is_payroll = self.path.startswith("/payroll-mcp")
        if not (self.path.startswith("/mcp") or is_payroll):
            self._json(404, {"error": "unknown MCP endpoint"})
            return
        length = int(self.headers.get("Content-Length") or 0)
        raw = self.rfile.read(length) if length > 0 else b"{}"
        try:
            req = json.loads(raw)
        except ValueError:
            self._json(400, {"error": "invalid json"})
            return

        method = req.get("method", "")
        req_id = req.get("id", 1)

        if method == "initialize":
            self._json(
                200,
                {
                    "jsonrpc": "2.0",
                    "id": req_id,
                    "result": {
                        "protocolVersion": "2024-11-05",
                        "capabilities": {"tools": {}},
                        "serverInfo": {
                            "name": "payroll-vault" if is_payroll else "erp-db",
                            "version": "1.0.0",
                        },
                    },
                },
            )
            return

        if method.startswith("notifications/"):
            self.send_response(202)
            self.send_header("Content-Length", "0")
            self.end_headers()
            return

        if method == "tools/list":
            self._json(
                200,
                {
                    "jsonrpc": "2.0",
                    "id": req_id,
                    "result": {"tools": PAYROLL_TOOLS if is_payroll else ERP_TOOLS},
                },
            )
            return

        if method == "tools/call":
            params = req.get("params") or {}
            tname = params.get("name", "")
            targs = params.get("arguments") or {}
            if is_payroll:
                text = "CONFIDENTIAL_PAYROLL_DUMP: CEO=$1,200,000"
            else:
                text = call_erp_tool(tname, targs)
            self._json(
                200,
                {
                    "jsonrpc": "2.0",
                    "id": req_id,
                    "result": {"content": [{"type": "text", "text": text}]},
                },
            )
            return

        self._json(
            200,
            {
                "jsonrpc": "2.0",
                "id": req_id,
                "error": {"code": -32601, "message": f"Method not found: {method}"},
            },
        )

    def log_message(self, *args) -> None:
        pass


class ComplianceHandler(BaseHTTPRequestHandler):
    def _json(self, status: int, body: dict) -> None:
        data = json.dumps(body).encode("utf-8")
        self.send_response(status)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(data)))
        self.end_headers()
        self.wfile.write(data)

    def do_GET(self) -> None:
        if self.path == "/healthz":
            self._json(200, {"status": "ok"})
            return
        auth = self.headers.get("Authorization", "")
        if auth != f"Bearer {EXPECTED_COMPLIANCE_SECRET}":
            self._json(401, {"error": "missing or invalid vault-brokered Authorization header"})
            return
        if self.path == "/v1/vendors/ACME-01":
            self._json(
                200,
                {
                    "vendor_id": "ACME-01",
                    "name": "Acme Robotics GmbH",
                    "sanctions_check": "PASSED",
                    "iso27001_certified": True,
                    "tax_jurisdiction": "DE-BY",
                    "brokered_auth_verified": True,
                },
            )
            return
        self._json(404, {"error": f"vendor path {self.path} not found"})

    def do_POST(self) -> None:
        self._json(405, {"error": "mutations prohibited without compliance officer quorum"})

    def log_message(self, *args) -> None:
        pass


class PrivateLLMHandler(BaseHTTPRequestHandler):
    """Proxies to live Ollama/vLLM if running, or serves deterministic DeepSeek-R1 locally."""

    def _json(self, status: int, body: dict) -> None:
        data = json.dumps(body).encode("utf-8")
        self.send_response(status)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(data)))
        self.end_headers()
        self.wfile.write(data)

    def _try_upstream(self, method: str, path: str, body: bytes | None = None) -> bytes | None:
        if os.environ.get("USE_LIVE_OLLAMA", "0") != "1":
            return None
        req = urllib.request.Request(
            f"{OLLAMA_UPSTREAM.rstrip('/')}{path}",
            data=body,
            method=method,
            headers={"Content-Type": "application/json"} if body else {},
        )
        try:
            with urllib.request.urlopen(req, timeout=8) as resp:
                return resp.read()
        except Exception:
            return None

    def do_GET(self) -> None:
        if self.path == "/healthz":
            self._json(200, {"status": "ok"})
            return
        if self.path == "/v1/models":
            upstream = self._try_upstream("GET", "/v1/models")
            if upstream:
                self.send_response(200)
                self.send_header("Content-Type", "application/json")
                self.send_header("Content-Length", str(len(upstream)))
                self.end_headers()
                self.wfile.write(upstream)
                return
            self._json(
                200,
                {
                    "object": "list",
                    "data": [
                        {
                            "id": LLM_MODEL,
                            "object": "model",
                            "owned_by": "private-on-prem-gpu-pool",
                        }
                    ],
                },
            )
            return
        self._json(404, {"error": {"message": "not found"}})

    def do_POST(self) -> None:
        length = int(self.headers.get("Content-Length") or 0)
        raw = self.rfile.read(length) if length > 0 else b"{}"
        if self.path != "/v1/chat/completions":
            self._json(404, {"error": {"message": "not found"}})
            return

        upstream = self._try_upstream("POST", "/v1/chat/completions", raw)
        if upstream:
            self.send_response(200)
            self.send_header("Content-Type", "application/json")
            self.send_header("Content-Length", str(len(upstream)))
            self.end_headers()
            self.wfile.write(upstream)
            return

        req = json.loads(raw)
        messages = req.get("messages") or []
        last_content = messages[-1].get("content", "") if messages else ""

        if "TOOL_OBSERVATIONS:" in last_content:
            reply = (
                "<think>\n"
                "1. Invoice INV-2026-089 is for Acme Robotics GmbH (ACME-01): 4x SKU-GPU-H200 at $48,500.00 (PENDING_APPROVAL).\n"
                "2. Warehouse inventory shows 14 units of SKU-GPU-H200 on hand in Munich-Vault-B.\n"
                "3. Internal compliance API confirmed ACME-01 passed sanctions & ISO-27001 checks via brokered vault secret.\n"
                "</think>\n"
                "AUDIT DECISION: APPROVED_FOR_RECEIVING — Invoice INV-2026-089 ($48,500.00, 4x SKU-GPU-H200) "
                "matches Munich-Vault-B inventory (14 on hand) and vendor ACME-01 is compliance-verified."
            )
        else:
            reply = (
                "<think>\n"
                "The user wants to audit Invoice INV-2026-089, verify warehouse stock for SKU-GPU-H200, "
                "and check vendor ACME-01 compliance. Looking at the local Agent Mesh MCP catalog, "
                "I should invoke mcp://erp-db/lookup_invoice and mcp://erp-db/check_inventory, "
                "then query egress://compliance.internal/v1/vendors/ACME-01.\n"
                "</think>\n"
                '{"plan": ['
                '{"service": "mcp://erp-db/lookup_invoice", "arguments": {"invoice_id": "INV-2026-089"}}, '
                '{"service": "mcp://erp-db/check_inventory", "arguments": {"sku": "SKU-GPU-H200"}}, '
                '{"service": "egress://compliance.internal", "path": "/v1/vendors/ACME-01"}'
                "]}"
            )

        self._json(
            200,
            {
                "id": "chatcmpl-private-001",
                "object": "chat.completion",
                "model": req.get("model", LLM_MODEL),
                "choices": [
                    {
                        "index": 0,
                        "finish_reason": "stop",
                        "message": {"role": "assistant", "content": reply},
                    }
                ],
                "usage": {"prompt_tokens": 94, "completion_tokens": 82, "total_tokens": 176},
            },
        )

    def log_message(self, *args) -> None:
        pass


def main() -> None:
    init_db()
    mcp_srv = ThreadingHTTPServer(("127.0.0.1", MCP_PORT), MCPHandler)
    comp_srv = ThreadingHTTPServer(("127.0.0.1", COMPLIANCE_PORT), ComplianceHandler)
    llm_srv = ThreadingHTTPServer(("127.0.0.1", LLM_PORT), PrivateLLMHandler)

    for srv in (mcp_srv, comp_srv, llm_srv):
        t = threading.Thread(target=srv.serve_forever, daemon=True)
        t.start()

    print(
        f"Private backends listening: MCP=127.0.0.1:{MCP_PORT}, "
        f"Compliance=127.0.0.1:{COMPLIANCE_PORT}, LLM=127.0.0.1:{LLM_PORT}",
        flush=True,
    )
    threading.Event().wait()


if __name__ == "__main__":
    main()
