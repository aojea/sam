# Multi-Tier Guardrails & Quality Gates — Recording Script

A ~3.5-minute terminal recording demonstrating how SAM serves all **5 enterprise personas** across **4 acts** with real backends (`gemma3:1b` on Ollama, official `a2a-sdk`, official `mcp` SDK + SQLite, and `api.github.com`). Every config file (`node-v1.yaml`, `node-caller.yaml`, `node-v2.yaml`, `node-contractor.yaml`, `policy.json`) and every command (`sam-node join`, `make node-v1-staging`, `curl`, `python3 a2a_client.py`, `mcp-client`, `sam-one admin ban`) is shown directly on screen.

## Scenes

### Act 1: Developer Onboarding, Quarantine & Label Points 1 & 2a
*(Personas: Agent Developer Alice + Central Security / Platform)*

```bash
# node-v1
cat node-v1.yaml

# admin (Label Point 1 — Mesh Enforcement)
jq -c '.roles[0].allowed_labels' policy.json
sed 's/staging/prod/' node-v1.yaml > $WORK_DIR/node-v1-prod.yaml && \
  sam-node join $CP_URL --insecure-control-plane \
    --bootstrap-token-path $WORK_DIR/dev-spoof.token \
    --config $WORK_DIR/node-v1-prod.yaml --data-dir $WORK_DIR/spoof

# node-v1
make node-v1-staging 2>&1 | python3 audit.py

# caller (Label Point 2a — Input Node Enforcement)
cat node-caller.yaml
curl -i -s -H 'X-Sam-Authentication: Bearer caller-secret' \
  http://127.0.0.1:19102/sam/$V1_PEER/a2a/support.acme/.well-known/agent-card.json | head -n 5

# admin (Quality Gate Promotion)
jq '.roles[0].allowed_labels += ["env=prod"]' policy.json | \
  curl -s -X POST $CP_URL/policies -H "Authorization: Bearer $ADMIN_TOKEN" --data @-

# node-v1
make node-v1-prod 2>&1 | python3 audit.py

# caller
curl -s -H 'X-Sam-Authentication: Bearer caller-secret' \
  http://127.0.0.1:19102/sam/$V1_PEER/a2a/support.acme/.well-known/agent-card.json | \
  jq '{name, url: .supportedInterfaces[0].url}'
```

### Act 2: Zero-Downtime Replica Swap & Label Point 3 (End-User / Agent Intent)
*(Persona: Platform / Networking)*

```bash
# caller
python3 a2a_client.py http://127.0.0.1:19102/sam/$V1_PEER/a2a/support.acme \
  ctx-acme-1042 'Check order #1042 for alice@acme.com' replica=v1-laptop

cat node-v2.yaml

python3 a2a_client.py http://127.0.0.1:19102/sam/$V2_PEER/a2a/support.acme \
  ctx-acme-1042 'What item did Alice order and has it shipped?' replica=v2-cloudrun
```

### Act 3: 3-Tier Guardrails
*(Personas: Central Security, Department Lead, Individual End-User)*

```bash
# Layer 1 — Central Security (Org HTTP Policy + Secret Brokering)
jq -c '.roles[0].http[0], .egress[0]' policy.json
curl -s -H 'X-Sam-Authentication: Bearer caller-secret' \
  "http://127.0.0.1:19102/sam/$V1_PEER/egress/api.github.com/repos/google/sam/pulls?state=open&per_page=1" | \
  jq -c '.[0] | {number, title}'
curl -i -s -X POST -H 'X-Sam-Authentication: Bearer caller-secret' \
  http://127.0.0.1:19102/sam/$V1_PEER/egress/api.github.com/repos/google/sam/pulls -d '{}' | head -n 5

# Layer 2 — Department Lead (Label Point 2b — Egress Node Positive Check)
cat node-contractor.yaml
mcp-client -url http://127.0.0.1:19104/mcp -token contractor-secret -tool call_remote_tool \
  -args '{"peer_id":"'$V1_PEER'","tool_name":"mcp://orders-db/get_order_status","arguments":{"order_id":"1042"}}'

# Layer 3 — Individual End-User (RFC 8693 Task Biscuit tar_block)
TASK_BISCUIT=$(curl -s -X POST http://127.0.0.1:19102/oauth/token \
  -H 'Authorization: Bearer caller-secret' \
  -d 'grant_type=urn:ietf:params:oauth:grant-type:token-exchange&resource=mcp://orders-db&scope=tool:get_order_status&seal=true' | jq -r .access_token)
mcp-client -url http://127.0.0.1:19102/mcp -token $TASK_BISCUIT -tool call_remote_tool \
  -args '{"peer_id":"'$V1_PEER'","tool_name":"mcp://orders-db/get_order_status","arguments":{"order_id":"1042"}}'
mcp-client -url http://127.0.0.1:19102/mcp -token $TASK_BISCUIT -tool call_remote_tool \
  -args '{"peer_id":"'$V1_PEER'","tool_name":"mcp://orders-db/issue_refund","arguments":{"order_id":"1042","amount_dollars":5000}}'
```

### Act 4: Day-2 Operations — Audit & Instant Kill-Switch
*(Persona: SRE / Operations)*

```bash
# admin
sam-one admin ban $CONTRACTOR_PEER --server $CP_URL --data-dir $WORK_DIR/one
```

## Recording

```bash
./development/examples/multi-tier-guardrails/record.sh
```
