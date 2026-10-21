# Private DIY Enterprise Stack (On-Prem & Cloud) — 3D Live-Execution Video Script

[`record.sh`](./record.sh) and [`render_3d_video.py`](./render_3d_video.py) produce a 56-second, 1920x1080 (30fps) **3D perspective architectural walkthrough** (`site/static/demo-private-diy-stack.mp4`) driven by a **real live execution** of `agentmesh-one`, four `agentmesh-node` sidecars, `agent_harness.py`, `mcp-client`, and `backends.py`.

## How It Works

1. **Step 1 — Boot Real Mesh Binaries (`MeshEnvironment`):**
   - Starts `backends.py` (SQLite ERP database `mcp://erp-db`, Payroll Vault `mcp://payroll-vault`, Vendor Compliance REST API `egress://compliance.internal`, and OpenAI-compatible reasoning engine `inference://private-llm`).
   - Starts `agentmesh-one` with [`policy.json`](./policy.json) and enrolls four real `agentmesh-node` sidecars:
     - [`node-agent.yaml`](./node-agent.yaml) (`127.0.0.1:19101` — Cloud Run / Workstation zone)
     - [`node-inference.yaml`](./node-inference.yaml) (`127.0.0.1:19103` — Cloud / On-Prem GPU zone)
     - [`node-tools.yaml`](./node-tools.yaml) (`127.0.0.1:19102` — On-Premises Corporate Vault zone)
     - [`node-external.yaml`](./node-external.yaml) (`127.0.0.1:19104` — Untrusted External zone, `dept=external`)

2. **Step 2 — Execute All 4 Acts Against Live Binaries:**
   - **Scene 1 (Act 1 — Sidecar Discovery):** Queries `/api/act1/discover` to verify real libp2p Peer IDs (`12D3KooW...`) and Kademlia DHT service discovery.
   - **Scenes 2 & 3 (Act 2 — Hybrid ReAct Loop & Vault Secret Brokering):** Executes `agent_harness.py` via `/api/act2/run-agent` to audit `INV-2026-089`, check `SKU-GPU-H200` stock, and verify vendor `ACME-01` with node-side `secrets/compliance-api-key` injection.
   - **Scene 4 (Act 3a & 3b — Default-Deny on Service Names & HTTP Methods):** Calls `mcp://payroll-vault/dump_executive_salaries` and `POST /v1/vendors/ACME-01`, capturing the live Biscuit Datalog denial and `HTTP 403` response.
   - **Scene 5 (Act 3c — Residency & Department Label Check):** Calls `mcp://erp-db/lookup_invoice` from `node-external` (`dept=external`), capturing the live Datalog label rejection on `node-tools`.
   - **Scene 6 (Act 3d — Task-Scoped Sealed `tar_block` Biscuit):** Mints a task-scoped sealed Biscuit via RFC 8693 `POST /oauth/token`, verifies `lookup_invoice` (`ALLOW`), and blocks prompt-injected `execute_wire_transfer` (`DENY`).
   - **Scene 7 (Act 4 — Correlated Audit Trail & Mesh-Wide Ban):** Revokes `node-external` via `agentmesh-one admin ban` and summarizes all live `ALLOW` / `DENY` audit events.

3. **Step 3 — 3D Perspective Rendering (`pycairo` + `numpy` + `ffmpeg`):**
   - Renders the 3D zones, extruded server racks, rotating Biscuit holocubes, curved 3D `libp2p` packet beams, 3D hexagonal force-field shields on `DENY`, and the live binary telemetry HUD to `site/static/demo-private-diy-stack.mp4`.

## Re-Recording

```bash
make -C development/examples/private-diy-stack record
```
