---
title: "SkyPilot"
linkTitle: "SkyPilot"
weight: 6
---

[SkyPilot](https://docs.skypilot.co/) provisions and manages compute across
Google Cloud, AWS, Azure, OCI, and Kubernetes using your existing cloud
credentials (`sky check`). The repository includes a ready-to-run recipe,
[`deploy/skypilot/agentmesh-one.yaml`](https://github.com/google/agentmesh/blob/main/deploy/skypilot/agentmesh-one.yaml),
that deploys a standalone `agentmesh-one` control plane and router with a persistent
disk and an outbound Cloudflare HTTPS tunnel (requiring no inbound firewall
rules).

*(Note: While `deploy/skypilot/agentmesh-one.yaml` defaults to production VM sizing
(`cpus: 2+`, `memory: 8+`, matching `e2-standard-2` on GCP or `t3.large` on AWS),
you can also set `cpus: 0.25+` and `memory: 1+` to run on cloud provider free
tiers such as GCP `e2-micro` or AWS `t4g.micro` for testing.)*

## 1. Deploy `agentmesh-one` with SkyPilot

### Production deployment (custom domain)

If you have a tunnel authentication token bound to a custom hostname (such as
`https://mesh.example.com`), launch the cluster with `AGENTMESH_EXTERNAL_URL` and
`AGENTMESH_TUNNEL_TOKEN`:

```bash
export AGENTMESH_TUNNEL_TOKEN="eyJhIjoi..."

sky launch -y -c agentmesh-hub deploy/skypilot/agentmesh-one.yaml --detach-run \
  --env AGENTMESH_EXTERNAL_URL=https://mesh.example.com \
  --secret AGENTMESH_TUNNEL_TOKEN
```

### Quick deployment (automatic HTTPS tunnel)

To launch immediately with an automatic `https://<name>.trycloudflare.com`
address and zero DNS setup:

```bash
sky launch -y -c agentmesh-hub deploy/skypilot/agentmesh-one.yaml --detach-run
```

To target a specific cloud provider (for example Google Cloud), pass `--infra gcp`:

```bash
sky launch -y -c agentmesh-hub deploy/skypilot/agentmesh-one.yaml --infra gcp --detach-run
```

## 2. Obtain your Control Plane URL and tokens

Read the `agentmesh-one` startup banner from job `1`:

```bash
sky logs agentmesh-hub 1 --no-follow
```

```text
══════════════════════════════════════════════════════════════════
Agent Mesh standalone mesh is ready!

API URL:      https://distinct-kent-bradford-elderly.trycloudflare.com
Tunnel:       https://distinct-kent-bradford-elderly.trycloudflare.com -> http://0.0.0.0:8080
Web Console:  https://distinct-kent-bradford-elderly.trycloudflare.com/console
Router Peer:  12D3KooWScRWXVx2zSaPWC7NectYnuyhaGxpLYkNgCkp2g2tkfkY
Admin Token:  mesh_adm_e4075529d72bb22d78446799f683dd89
Join Token:   mesh_tok_05a5d01cf091187c980e68e1bd7d7d7a

To enroll a node:
  agentmesh-node join https://distinct-kent-bradford-elderly.trycloudflare.com --bootstrap-token-path /home/gcpuser/agentmesh-one/join-token
══════════════════════════════════════════════════════════════════
```

Copy the `API URL` and `Join Token` from the banner:

```bash
URL="https://distinct-kent-bradford-elderly.trycloudflare.com"
echo -n "mesh_tok_05a5d01cf091187c980e68e1bd7d7d7a" > join-token
```

## 3. Verify the dataplane with two nodes

Once you have `$URL` and `join-token`, verify end-to-end service discovery,
Biscuit authorization, and WebSocket relay forwarding between two nodes:

1. **Start Node A (exposing an inference service)**:
   ```bash
   cat > node-a.yaml <<'EOF'
   version: "v1alpha1"
   services:
     - type: inference
       name: prod-llm
       target_url: "http://127.0.0.1:11434"
   EOF

   agentmesh-node run --control-plane "$URL" \
     --bootstrap-token-path join-token \
     --config node-a.yaml --data-dir ~/node-a --bind-addr=
   ```
2. **Start Node B and call Node A's service through the mesh**:
   ```bash
   agentmesh-node run --control-plane "$URL" \
     --bootstrap-token-path join-token \
     --data-dir ~/node-b --bind-addr=

   curl -s --unix-socket ~/node-b/agentmesh.sock http://localhost/v1/models
   curl -s --unix-socket ~/node-b/agentmesh.sock http://localhost/v1/chat/completions \
     -H 'Content-Type: application/json' \
     -d '{"model":"gemma3:1b","messages":[{"role":"user","content":"Ping across the mesh"}]}'
   ```

## 4. Manage the cluster

```bash
# Check status of the SkyPilot cluster
sky status agentmesh-hub

# Administer tokens and policies remotely using agentmesh-one
export AGENTMESH_ADMIN_TOKEN="mesh_adm_..."
agentmesh-one token create --server "$URL" --role mesh:role:node --max-usages 1
agentmesh-one token list   --server "$URL"

# Tear down the cluster when no longer needed
sky down -y agentmesh-hub
```
