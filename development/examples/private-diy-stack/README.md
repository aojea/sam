# Private DIY Enterprise Stack — Safely On-Premises & in Cloud (`Gemma / DeepSeek` + `Agent Harness` + `Agent Mesh`)

Run a zero-trust AI workforce **safely across on-premises infrastructure and Google Cloud** (Cloud Run, GKE, or local servers) without site-to-site VPNs, inbound firewall holes, or raw credentials inside agent containers:

1. **The "Sidecar" Node Architecture Across On-Prem & Cloud:**
   - [`node-agent.yaml`](./node-agent.yaml) runs alongside your agent ([`agent_harness.py`](./agent_harness.py), whether on a workstation, Cloud Run, or GKE) and exposes the mesh as a local **Model Context Protocol (MCP)** server (`127.0.0.1:19101/mcp`) and a local **OpenAI-compatible endpoint** (`127.0.0.1:19101/v1`). The agent has zero database credentials or network routing code.
   - [`node-inference.yaml`](./node-inference.yaml) sits next to your GPU reasoning server (`Ollama` / `vLLM` serving `deepseek-r1:1.5b` or `gemma3` in cloud or on-prem) and publishes `inference://private-llm` to the mesh.
   - [`node-tools.yaml`](./node-tools.yaml) sits inside the on-premises vault zone next to your private SQLite ERP database (`mcp://erp-db`), Payroll vault (`mcp://payroll-vault`), and internal Vendor Compliance REST API (`egress://compliance.internal`), injecting the Vault-brokered API secret (`secrets/compliance-api-key`) at the egress boundary so sensitive credentials never leave the vault node.

2. **Zero-Trust Identity & Default-Deny Policy ([`policy.json`](./policy.json)):**
   - **Default-Deny on Service Names:** Even though `mcp://payroll-vault` runs on the same vault node as `mcp://erp-db`, the control plane policy only grants `mcp://erp-db`—calls to `mcp://payroll-vault` are cryptographically rejected.
   - **HTTP Method/Path Guardrails:** `egress://compliance.internal` permits `GET /v1/vendors/*` and blocks `POST` mutations with `HTTP 403`.
   - **Residency & Department Labels:** `node-tools.yaml` enforces `check if label("residency", "on-prem");` and `check if label("dept", "finance");`.
   - **Task-Scoped Sealed Biscuits (`tar_block`):** Minting a task token via RFC 8693 `/oauth/token` narrows the agent's Biscuit to `lookup_invoice` and `check_inventory`, blocking prompt-injected calls to `execute_wire_transfer`.

## Quick Start

```bash
# 1. Run the 4-act CLI verification script (~12s, zero external dependencies)
make -C development/examples/private-diy-stack run

# 2. Launch the Interactive Browser Workbench on http://127.0.0.1:18899
make -C development/examples/private-diy-stack ui

# 3. Or connect inference://private-llm to a live Ollama / vLLM server:
USE_LIVE_OLLAMA=1 OLLAMA_MODEL=deepseek-r1:1.5b \
  make -C development/examples/private-diy-stack ui

# 4. Re-record the 3-pane terminal MP4 walkthrough (see SCRIPT.md):
make -C development/examples/private-diy-stack record
```
