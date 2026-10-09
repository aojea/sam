---
title: "Cloud Run"
linkTitle: "Cloud Run"
weight: 5
aliases:
  - /docs/user/cloud-run-deployment/
---

`agentmesh-one` serves its HTTP API, the web console, and the router's WebSocket
transport on a single port (`8080`), and ships as a pre-built container image
(`ghcr.io/google/agentmesh-one:latest`). Because `agentmesh-one` automatically infers its
public `wss://` router address from Cloud Run's `Host` and `X-Forwarded-Proto`
headers on `/info` and `/enroll` and derives a deterministic router `PeerID`
from `AGENTMESH_ADMIN_TOKEN`, you can deploy a standalone control plane and router to
Google Cloud Run in a single command.

*(Note: For multi-cloud VM deployments on GCP, AWS, Azure, OCI, or Kubernetes,
see the [SkyPilot guide](../skypilot/).)*

## 1. Deploy `agentmesh-one` to Cloud Run

```bash
PROJECT=my-gcp-project
REGION=us-central1
JOIN_TOKEN="mesh_tok_$(openssl rand -hex 16)"
ADMIN_TOKEN="mesh_adm_$(openssl rand -hex 16)"

gcloud run deploy agentmesh-one \
  --project "$PROJECT" --region "$REGION" \
  --image ghcr.io/google/agentmesh-one:latest \
  --allow-unauthenticated \
  --min-instances 1 --max-instances 1 \
  --port 8080 \
  --no-cpu-throttling \
  --timeout 3600 \
  --set-env-vars "AGENTMESH_TOKEN=${JOIN_TOKEN},AGENTMESH_ADMIN_TOKEN=${ADMIN_TOKEN}"
```

### Production persistence with PostgreSQL

By default, Cloud Run's container filesystem is in-memory. When an instance
restarts, `agentmesh-one` preserves its router `PeerID` (derived from
`AGENTMESH_ADMIN_TOKEN`) and pinned tokens (`AGENTMESH_TOKEN`, `AGENTMESH_ADMIN_TOKEN`), and
active nodes re-enroll automatically.

For full state durability across container revisions and restarts (preserving
enrolled peers, minted tokens, and console policy edits), connect `agentmesh-one` to
a PostgreSQL database (such as Cloud SQL, AlloyDB, or a serverless Postgres
instance for testing):

```bash
  --args="--data-dir=/data,--port=8080,--db-driver=postgres,--db-dsn=${POSTGRES_DSN}"
```

Why each Cloud Run flag matters:

- `--min-instances 1 --max-instances 1`: the router's DHT and relay state
  live in the single process. Two instances would form two separate meshes
  behind one URL.
- `--no-cpu-throttling`: the router runs background loops (lease renewal,
  key sync, DHT maintenance) between HTTP requests.
- `--timeout 3600`: Cloud Run bounds the lifetime of a streaming request,
  and each node's WebSocket connection is one. Nodes reconnect automatically
  when the limit closes a connection.
- `--allow-unauthenticated`: the mesh authenticates its own callers using
  Biscuit credentials and tokens.

## 2. Obtain your Control Plane URL

`gcloud run deploy` prints your `Service URL` when deployment completes:

```text
Service [agentmesh-one] revision [agentmesh-one-00001-xxx] has been deployed and is serving 100 percent of traffic.
Service URL: https://agentmesh-one-628944397724.us-central1.run.app
```

You can also query it at any time:

```bash
URL=$(gcloud run services describe agentmesh-one --project "$PROJECT" --region "$REGION" \
  --format='value(status.url)')
curl -s "$URL/readyz"   # {"status":"ready"}
```

## 3. Verify the dataplane with two nodes

Once your control plane URL (`$URL`) is ready, verify end-to-end service
discovery, Biscuit authorization, and WebSocket relay routing across two nodes:

1. **Save the bootstrap token** (or mint one with `agentmesh-one token create`):
   ```bash
   echo -n "$JOIN_TOKEN" > join-token
   ```
2. **Start Node A (exposing an inference service)**:
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
3. **Start Node B and call Node A's service through the mesh**:
   ```bash
   agentmesh-node run --control-plane "$URL" \
     --bootstrap-token-path join-token \
     --data-dir ~/node-b --bind-addr=

   curl -s --unix-socket ~/node-b/agentmesh.sock http://localhost/v1/models
   curl -s --unix-socket ~/node-b/agentmesh.sock http://localhost/v1/chat/completions \
     -H 'Content-Type: application/json' \
     -d '{"model":"gemma3:1b","messages":[{"role":"user","content":"Ping across the mesh"}]}'
   ```

## 4. Administer from your workstation

`agentmesh-one` is also the admin client. With `AGENTMESH_ADMIN_TOKEN` exported:

```bash
export AGENTMESH_ADMIN_TOKEN="$ADMIN_TOKEN"
agentmesh-one token create --server "$URL" --role mesh:role:node --max-usages 1
agentmesh-one token qr     --server "$URL"
agentmesh-one token list   --server "$URL"
agentmesh-one token revoke <token-id> --server "$URL"
agentmesh-one admin ban <peer-id>     --server "$URL"
```
