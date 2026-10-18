---
title: "Cloud Run"
linkTitle: "Cloud Run"
weight: 5
aliases:
  - /docs/user/cloud-run-deployment/
---

`agentmesh-one` serves its HTTP API, the web console, and the router's WebSocket
transport on a single port (`8080`), and ships as a pre-built container image
(`ghcr.io/google/agentmesh-one:latest`). It derives a deterministic router `PeerID`
from `AGENTMESH_ADMIN_TOKEN`, and a Cloud Run service URL is known before the
service exists (`https://<service>-<project number>.<region>.run.app`), so you
can deploy a standalone control plane and router to Google Cloud Run in a
single command. The URL goes in `--external-url`: it is the router address
members dial and the OIDC issuer the control plane signs with, and
`agentmesh-one` never infers either from request headers.

*(Note: For multi-cloud VM deployments on GCP, AWS, Azure, OCI, or Kubernetes,
see the [SkyPilot guide](../skypilot/).)*

## 1. Deploy `agentmesh-one` to Cloud Run

```bash
PROJECT=my-gcp-project
REGION=us-central1
JOIN_TOKEN="mesh_tok_$(openssl rand -hex 16)"
ADMIN_TOKEN="mesh_adm_$(openssl rand -hex 16)"
PROJECT_NUMBER=$(gcloud projects describe "$PROJECT" --format='value(projectNumber)')
EXTERNAL_URL="https://agentmesh-one-${PROJECT_NUMBER}.${REGION}.run.app"

gcloud run deploy agentmesh-one \
  --project "$PROJECT" --region "$REGION" \
  --image ghcr.io/google/agentmesh-one:latest \
  --allow-unauthenticated \
  --min-instances 1 --max-instances 1 \
  --port 8080 \
  --no-cpu-throttling \
  --timeout 3600 \
  --set-env-vars "AGENTMESH_TOKEN=${JOIN_TOKEN},AGENTMESH_ADMIN_TOKEN=${ADMIN_TOKEN},AGENTMESH_EXTERNAL_URL=${EXTERNAL_URL}"
```

Without `--external-url` (or `AGENTMESH_EXTERNAL_URL`) the instance advertises
its own listener, which nothing outside Cloud Run can reach. The first request
that arrives through the front door then logs
`Requests arrive through a reverse proxy but --external-url is not set`, and
`agentmesh_one_proxied_requests_without_external_url_total` counts them.

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

### Operator-owned signing keys and policy without a database

Without PostgreSQL every revision starts from an empty store, which means
new signing keys: every credential the mesh issued stops verifying until each
node re-enrolls. If you want the root of trust in your hands anyway, keep the
keys in Secret Manager (wrap the secret with your own Cloud KMS key if you
want: `--kms-key-name` on `gcloud secrets create`) and mount them as a file;
do the same with the policy:

```bash
openssl genpkey -algorithm ed25519 -out ed25519-v1.pem
openssl genpkey -algorithm ec -pkeyopt ec_paramgen_curve:P-256 -out p256-v1.pem
cat ed25519-v1.pem p256-v1.pem | gcloud secrets create mesh-signing-keys --data-file=-
gcloud secrets create mesh-policy --data-file=policy.json
SA=$(gcloud projects describe "$PROJECT" --format='value(projectNumber)')-compute@developer.gserviceaccount.com
for s in mesh-signing-keys mesh-policy; do
  gcloud secrets add-iam-policy-binding "$s" --member="serviceAccount:$SA" --role=roles/secretmanager.secretAccessor
done
gcloud run services update agentmesh-one --project "$PROJECT" --region "$REGION" \
  --set-secrets=/etc/agentmesh/keys/signing-keys.pem=mesh-signing-keys:latest,/etc/agentmesh/policy.json=mesh-policy:latest \
  --args="--data-dir=/data,--port=8080,--signing-key-path=/etc/agentmesh/keys/signing-keys.pem,--policy-file=/etc/agentmesh/policy.json"
```

(Cloud Run mounts one secret per directory, hence the two paths.) The
control plane installs the keys and seeds the policy on every start, so a new
revision changes neither: credentials issued before it keep verifying, and
`/keys` and `/jwks` publish the same keys. While the file is present the
control plane never generates or rotates a key on its own; it re-reads the
file every 30 seconds and installs what changed, and the log says
`Signing keys are operator-managed; automatic key rotation is off`.

Nodes started with `--bootstrap-token-path` complete the picture: after a
revision the control plane does not know them (enrollment records live in
the store), they notice at their next sync (`401 node credential required`)
and enroll again with the token, without a restart. PostgreSQL keeps the
records too, and is what you want for durable revocations.

#### Rotating the keys

Rotation is yours, and it is an edit to the file; nothing is restarted or
redeployed. For each kind of key (Ed25519 for Biscuits, P-256 for the ES256
JWTs) the first key in the file signs and every later one only verifies.

1. **Generate** the new key where you keep keys (never on the control plane):
   `openssl genpkey -algorithm ed25519 -out ed25519-v2.pem`.
2. **Publish the overlap**: new key first, the previous one kept after it.
   ```bash
   cat ed25519-v2.pem ed25519-v1.pem p256-v1.pem | gcloud secrets versions add mesh-signing-keys --data-file=-
   ```
   Cloud Run updates the mounted `latest` in the running instance; within
   about a minute the control plane logs `Installed operator signing key
   {"algorithm": "ed25519", "fingerprint": "…"}` and `Operator signing keys
   reloaded … "signing_key_changed": true`, `/keys` publishes both keys, new
   credentials are signed by the new one, and a `KEY_ROTATION` event tells
   routers and nodes to refresh. A key stays valid for verification for as
   long as you list it. Routers log `Control plane signing keys changed`
   with the fingerprints they now trust.
3. **Wait out the overlap**: at least as long as the longest-lived
   credential the previous key signed (the Biscuit TTL for Ed25519, the STS
   JWT lifetime for P-256).
4. **Drop the previous key** the same way:
   ```bash
   cat ed25519-v2.pem p256-v1.pem | gcloud secrets versions add mesh-signing-keys --data-file=-
   gcloud secrets versions destroy 1 --secret=mesh-signing-keys   # when nothing may ever need it again
   ```
   A running instance gives the dropped key `--control-plane-key-grace-period`
   (default 1h) before it stops verifying; a new instance never installs it.
   Moving a previous key back to the front makes it the signing key again.

The file must carry at least one key of each kind: owning the keys means
owning both, and a file with only one kind is refused rather than leaving
the other generated and unrotated.

What a verifier accepts is always `/keys`, signed by every valid key, so a
node still holding the previous key accepts the response that announces the
new one. The two kinds of key rotate independently.

The whole procedure, including a control plane that restarts with an empty
store between steps, is exercised by
`TestOperatorSigningKeyRotationProcess` in `tests/integration`.

Several issuers on Cloud Run: `--args` splits on commas, so either repeat
the flag (`--workload-issuer=A,--workload-issuer=B` inside `--args`, each a
separate argument) or switch the delimiter: `--args="^;^--port=8080;--workload-issuer=A,B;..."`.

Why each Cloud Run flag matters:

- `--min-instances 1 --max-instances 1`: the router's DHT and relay state
  live in the single process. Two instances would form two separate meshes
  behind one URL. See [A revision rollout runs two instances](#a-revision-rollout-runs-two-instances)
  for the one time Cloud Run runs two anyway.
- `--no-cpu-throttling`: the router runs background loops (lease renewal,
  key sync, DHT maintenance) between HTTP requests.
- `--timeout 3600`: Cloud Run bounds the lifetime of a streaming request,
  and each node's WebSocket connection is one. Nodes reconnect automatically
  when the limit closes a connection. The same value bounds how long a
  rollout keeps members on the previous instance (below).
- `--allow-unauthenticated`: the mesh authenticates its own callers using
  Biscuit credentials and tokens.

### A revision rollout runs two instances

`--max-instances 1` holds within a revision. When you deploy a new one
(`gcloud run deploy`, `gcloud run services update`, a new secret version
with `--update-secrets`), Cloud Run starts the new instance, moves all new
requests to it, and keeps the old instance alive until every request it is
serving has ended. A member's WebSocket connection to the router is such a
request, so the old instance stays up with its members attached until each
connection reaches `--timeout`, counted from when that connection was
opened, not from the rollout. Cloud Run sends SIGTERM only then; the
router's shutdown drain (`GoAway DRAINING`) finds no member left, as each
one has already reconnected on its own.

Until that happens the mesh has two directories: members attached to the
old instance announce and look up there, members that attach after the
rollout (new ones, restarted ones, and each existing one as its connection
is cut) do so on the new instance, and neither side sees the other's
services. HTTP requests are not affected: they go to the new instance,
which without PostgreSQL answers `401 node credential required` until the
node re-enrolls (see
[Operator-owned signing keys and policy without a database](#operator-owned-signing-keys-and-policy-without-a-database));
the control plane counts those in
`agentmesh_control_plane_member_auth_refusals_total{reason="unknown_node"}`.

The bound is yours to choose. `--timeout 3600` means a rollout can take up
to an hour to converge, with each member reconnecting once an hour in
steady state; `--timeout 600` converges within ten minutes, with each
member reconnecting every ten. A reconnect is a redial, a handshake and a
re-announcement of the member's services, so pick the longest timeout
whose convergence window you can live with. To converge at once, restart
the members (`kubectl rollout restart deployment/<node>`): a restarted
member always attaches to the live instance.

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
