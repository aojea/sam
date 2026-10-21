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

"""Self-Hosted Agent Harness (ReAct Loop) talking ONLY to its local agentmesh-node sidecar.

Notice what is NOT in this file:
  - No database URLs, SQLite paths, or SQL credentials
  - No corporate IP addresses or VPN routes
  - No upstream API keys for compliance.internal
  - No LLM server addresses

The harness talks strictly to its local sidecar (http://127.0.0.1:19101):
  - /mcp  for discovering and calling tools across the mesh
  - /v1   for calling the self-hosted DeepSeek/Gemma reasoning model
  - /mesh/<peer>/egress/compliance.internal/... for reaching internal REST APIs with brokered secrets
"""

import json
import os
import sys
import urllib.request

SIDECAR_URL = os.environ.get("AGENTMESH_SIDECAR_URL", "http://127.0.0.1:19101").rstrip("/")
API_TOKEN = os.environ.get("AGENTMESH_API_TOKEN", "agent-sidecar-secret")
LLM_MODEL = os.environ.get("LLM_MODEL", "deepseek-r1:1.5b")


class LocalSidecarClient:
    """Minimal stdlib client for the local agentmesh-node sidecar (/mcp, /v1, /mesh)."""

    def __init__(self, base_url: str, token: str) -> None:
        self.base_url = base_url
        self.token = token
        self.session_id: str | None = None
        self._req_id = 0
        self._opener = urllib.request.build_opener(urllib.request.ProxyHandler({}))

    def _next_id(self) -> int:
        self._req_id += 1
        return self._req_id

    def mcp_rpc(self, method: str, params: dict | None = None) -> dict:
        headers = {
            "Content-Type": "application/json",
            "Accept": "application/json, text/event-stream",
            "X-Mesh-Authentication": f"Bearer {self.token}",
        }
        if self.session_id:
            headers["Mcp-Session-Id"] = self.session_id

        payload: dict = {"jsonrpc": "2.0", "method": method}
        if not method.startswith("notifications/"):
            payload["id"] = self._next_id()
        if params is not None:
            payload["params"] = params

        req = urllib.request.Request(
            f"{self.base_url}/mcp",
            data=json.dumps(payload).encode("utf-8"),
            headers=headers,
            method="POST",
        )
        with self._opener.open(req, timeout=15) as resp:
            sid = resp.headers.get("Mcp-Session-Id")
            if sid:
                self.session_id = sid
            raw = resp.read().decode("utf-8").strip()
            if not raw:
                return {}
            # Streamable HTTP may return plain JSON or SSE "data: {...}" lines
            for line in reversed(raw.splitlines()):
                line = line.strip()
                if line.startswith("data:"):
                    line = line[len("data:") :].strip()
                if line.startswith("{"):
                    return json.loads(line)
            return json.loads(raw)

    def init_mcp(self) -> None:
        self.mcp_rpc(
            "initialize",
            {
                "protocolVersion": "2024-11-05",
                "capabilities": {},
                "clientInfo": {"name": "private-diy-harness", "version": "1.0.0"},
            },
        )
        self.mcp_rpc("notifications/initialized")

    def call_sidecar_tool(self, name: str, arguments: dict) -> str:
        res = self.mcp_rpc("tools/call", {"name": name, "arguments": arguments})
        if "error" in res:
            raise RuntimeError(json.dumps(res["error"]))
        result = res.get("result") or {}
        texts = [c.get("text", "") for c in (result.get("content") or []) if c.get("type") == "text"]
        text_out = "\n".join(texts).strip()
        if result.get("isError"):
            raise RuntimeError(text_out)
        return text_out

    def chat_completion(self, messages: list[dict]) -> str:
        req = urllib.request.Request(
            f"{self.base_url}/v1/chat/completions",
            data=json.dumps({"model": LLM_MODEL, "messages": messages, "stream": False}).encode("utf-8"),
            headers={
                "Content-Type": "application/json",
                "Authorization": f"Bearer {self.token}",
            },
            method="POST",
        )
        with self._opener.open(req, timeout=30) as resp:
            body = json.loads(resp.read().decode("utf-8"))
        return body["choices"][0]["message"]["content"]

    def call_egress_get(self, peer_id: str, destination: str, path: str) -> dict:
        req = urllib.request.Request(
            f"{self.base_url}/mesh/{peer_id}/egress/{destination}{path}",
            headers={"X-Mesh-Authentication": f"Bearer {self.token}"},
            method="GET",
        )
        with self._opener.open(req, timeout=15) as resp:
            return json.loads(resp.read().decode("utf-8"))


def main() -> None:
    task_prompt = (
        sys.argv[1]
        if len(sys.argv) > 1
        else "Audit Invoice INV-2026-089 for Acme Robotics, verify SKU-GPU-H200 warehouse stock, and check vendor ACME-01 compliance."
    )

    sidecar = LocalSidecarClient(SIDECAR_URL, API_TOKEN)
    sidecar.init_mcp()

    print(f'    [Harness] Task: "{task_prompt}"')
    print(f"    [Harness] Step 1 -> Asking local MCP server ({SIDECAR_URL}/mcp) for remote tools on the mesh...")
    raw_catalog = sidecar.call_sidecar_tool("find_remote_tools", {"service_name": "erp-db"})
    catalog = json.loads(raw_catalog.splitlines()[-1])
    tools_found = [entry["tool_name"] for entry in catalog if "tool_name" in entry]
    target_peer = catalog[0]["peer_id"]
    print(f"              Discovered {len(tools_found)} tools on Vault Peer {target_peer[:16]}…: {', '.join(tools_found)}")

    schema_raw = sidecar.call_sidecar_tool(
        "describe_remote_tool",
        {"peer_id": target_peer, "tool_name": "mcp://erp-db/lookup_invoice"},
    )
    schema_obj = json.loads(schema_raw.splitlines()[-1])
    req_fields = schema_obj.get("input_schema", {}).get("required", [])
    print(f"              Inspected schema for mcp://erp-db/lookup_invoice (required={req_fields})")

    print(f"    [Harness] Step 2 -> Consulting self-hosted reasoning model ({LLM_MODEL}) via {SIDECAR_URL}/v1/chat/completions...")
    plan_response = sidecar.chat_completion(
        [
            {"role": "system", "content": f"Available mesh tools: {', '.join(tools_found)}"},
            {"role": "user", "content": task_prompt},
        ]
    )
    for line in plan_response.splitlines():
        if line.strip():
            print(f"              {line}")

    print("    [Harness] Step 3 -> Executing tool calls & internal API checks through local sidecar...")
    inv_result = sidecar.call_sidecar_tool(
        "call_remote_tool",
        {
            "peer_id": target_peer,
            "tool_name": "mcp://erp-db/lookup_invoice",
            "arguments": {"invoice_id": "INV-2026-089"},
        },
    )
    print(f"              [mcp://erp-db/lookup_invoice]  -> {inv_result}")

    stock_result = sidecar.call_sidecar_tool(
        "call_remote_tool",
        {
            "peer_id": target_peer,
            "tool_name": "mcp://erp-db/check_inventory",
            "arguments": {"sku": "SKU-GPU-H200"},
        },
    )
    print(f"              [mcp://erp-db/check_inventory] -> {stock_result}")

    comp_result = sidecar.call_egress_get(target_peer, "compliance.internal", "/v1/vendors/ACME-01")
    print(
        f"              [egress://compliance.internal] -> vendor={comp_result['name']}, "
        f"sanctions={comp_result['sanctions_check']}, iso27001={comp_result['iso27001_certified']} "
        f"(brokered secret verified={comp_result['brokered_auth_verified']})"
    )

    print(f"    [Harness] Step 4 -> Sending tool observations back to {LLM_MODEL} for final synthesis...")
    final_answer = sidecar.chat_completion(
        [
            {"role": "user", "content": task_prompt},
            {"role": "assistant", "content": plan_response},
            {
                "role": "user",
                "content": (
                    f"TOOL_OBSERVATIONS:\n"
                    f"- invoice: {inv_result}\n"
                    f"- inventory: {stock_result}\n"
                    f"- compliance: {json.dumps(comp_result)}"
                ),
            },
        ]
    )
    for line in final_answer.splitlines():
        if line.strip():
            print(f"              {line}")


if __name__ == "__main__":
    main()
