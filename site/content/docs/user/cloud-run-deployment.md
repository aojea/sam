---
title: "Cloud Run Deployment"
linkTitle: "Cloud Run Deployment"
weight: 6
---

# Deploying a SAM Mesh on Cloud Run

`sam-one` is the all-in-one SAM distribution: control plane, libp2p router,
web console and storage in a single binary serving a single public port.
Because everything — REST API, console and mesh (WebSocket) traffic — is
multiplexed on one HTTP port, it runs on Cloud Run and any platform that
forwards HTTP/WebSockets to a container.

This guide was validated end to end on Cloud Run: node enrollment over
`wss`, a relayed service call between two NAT-hidden nodes through the
Cloud Run router, and the admin CLI against the public URL.

## 1. Build and push the image

```bash
# Pick your project/region and an Artifact Registry docker repository.
PROJECT=my-project
REGION=us-central1
IMG=${REGION}-docker.pkg.dev/${PROJECT}/sam-mesh/sam-one:latest

docker build -f Dockerfile.sam-one -t "$IMG" .
gcloud auth configure-docker ${REGION}-docker.pkg.dev
docker push "$IMG"
```

## 2. Deploy the service

Cloud Run has no persistent disk, so pass fixed join/admin tokens as
environment variables — otherwise fresh ones are generated on every
instance start:

```bash
JOIN_TOKEN="sam_tok_$(openssl rand -hex 16)"
ADMIN_TOKEN="sam_adm_$(openssl rand -hex 16)"

gcloud run deploy sam-one \
  --project "$PROJECT" --region "$REGION" \
  --image "$IMG" \
  --allow-unauthenticated \
  --min-instances 1 --max-instances 1 \
  --port 8080 \
  --no-cpu-throttling \
  --timeout 3600 \
  --set-env-vars "SAM_TOKEN=${JOIN_TOKEN},SAM_ADMIN_TOKEN=${ADMIN_TOKEN}"
```

Flag notes, all load-bearing:

* **`--min-instances 1 --max-instances 1`** — standalone mode is a
  singleton: the router's DHT and relay state live in the one process.
* **`--no-cpu-throttling`** — the router runs background loops (leases,
  key sync, DHT); request-based CPU throttling stalls them between
  requests.
* **`--timeout 3600`** — Cloud Run caps streaming requests; WebSocket mesh
  connections live inside that budget. Nodes reconnect automatically when
  the cap severs a connection, but a low timeout means needless churn.
* **`--port 8080`** matches the image's default `--port 8080` argument
  (see the `CMD` in `Dockerfile.sam-one`).

Then tell the router its public URL so it advertises a dialable `wss`
multiaddr (the URL is only known after the first deploy):

```bash
URL=$(gcloud run services describe sam-one --project "$PROJECT" \
  --region "$REGION" --format='value(status.url)')

gcloud run services update sam-one \
  --project "$PROJECT" --region "$REGION" \
  --update-env-vars "SAM_EXTERNAL_URL=${URL}"
```

## 3. Verify

```bash
curl -s "$URL/readyz"           # 200
curl -s "$URL/info" | head -c 200   # advertises /dns4/<host>/tcp/443/wss/p2p/<peer-id>
```

The web console is served from the same URL at `${URL}/console`.

> [!NOTE]
> `/healthz` returns a Google frontend 404 on `run.app` domains — the
> path is reserved by the platform and never reaches the container. Use
> `/readyz` for probes.

## 4. Join nodes from anywhere

```bash
sam-node run --control-plane "$URL" --bootstrap-token "$JOIN_TOKEN"
```

The node enrolls over HTTPS, discovers the router's `wss` multiaddr from
`/info`, and connects through the same public port. Nodes behind NAT are
reachable by other nodes via relay circuits through the Cloud Run router —
no inbound connectivity required on either side.

## 5. Example: share a service across the mesh

This walkthrough was run verbatim against a Cloud Run deployment: two
nodes on different networks, neither reachable from the other
(`--announce-private=false` withholds their private addresses, forcing all
traffic through the Cloud Run relay).

On the **provider** machine, start any local HTTP backend, declare it in a
node config file, and run a node with that config. Services only exist by
declaration at startup — there is no runtime registration endpoint — so
the backend comes up first and the node probes it before advertising:

```bash
mkdir -p /tmp/www && echo "hello from provider" > /tmp/www/hello.txt
python3 -m http.server 9000 --bind 127.0.0.1 --directory /tmp/www &

cat > ~/provider-services.yaml <<'EOF'
version: "v1alpha1"
services:
  - type: "mcp"
    name: "hello"
    description: "example"
    target_url: "http://127.0.0.1:9000"
EOF

sam-node run --control-plane "$URL" --bootstrap-token "$JOIN_TOKEN" \
  --data-dir ~/provider --announce-private=false \
  --config ~/provider-services.yaml &
```

Note the node's PeerID from its startup output.

On the **consumer** machine, run a node the same way, then call the
service by peer and name through the local egress proxy:

```bash
sam-node run --control-plane "$URL" --bootstrap-token "$JOIN_TOKEN" \
  --data-dir ~/consumer --announce-private=false &

curl --unix-socket ~/consumer/sam.sock \
  "http://localhost/sam/<provider-peer-id>/mcp/hello/hello.txt"
# -> hello from provider
```

The request crosses consumer → Cloud Run router (relay circuit) →
provider → backend, with mutual Biscuit authentication between the nodes
and policy enforced on the service name. Allow a few seconds after the
provider starts for propagation on first call.

## 6. Operate with the CLI

The `sam-one` binary doubles as an admin client for the running service:

```bash
export SAM_ADMIN_TOKEN="$ADMIN_TOKEN"

# Mint a scoped, single-use enrollment token
sam-one token create --server "$URL" --role sam:role:node --max-usages 1

# List tokens and their usage
sam-one token list --server "$URL"

# Ban a peer from the mesh
sam-one admin ban --server "$URL" <peer-id>
```

## Operational notes

* **State is ephemeral.** The SQLite database lives in the container's
  in-memory filesystem: enrolled nodes and minted tokens are lost on
  instance restart, and the router's peer identity rotates (nodes
  re-discover it via `/info` and re-enroll with the join token). Keep
  `SAM_TOKEN`/`SAM_ADMIN_TOKEN` pinned via env vars. For durable state,
  run `sam-one` on a VM with a disk, or point `--db-driver postgres` at a
  managed database.
* **Rollouts briefly overlap revisions.** During a deploy, `/info` may
  advertise the new instance while some WebSocket upgrades still land on
  the draining one; nodes refuse the peer-ID mismatch and retry. Joins
  succeed once the old revision drains.
* **Proxied traffic shares source IPs.** All traffic reaches the
  container from a handful of frontend proxy IPs. `sam-one` already
  raises libp2p's per-source-IP connection and rate budgets for this
  (`ConnsPerSourceIP`); if you front a discrete `sam-router` with a proxy
  yourself, set `--conns-per-source-ip` accordingly.

## Anywhere else

The same single-port binary runs on any host without flags:

```bash
sam-one --data-dir /var/lib/sam-one
```

A free port is picked and published in the startup banner together with
the generated tokens; pass `--port 8080` (and optionally
`--bind-address`) for a fixed one, and `--external-url https://mesh.example.com`
when fronted by a reverse proxy or DNS name.
