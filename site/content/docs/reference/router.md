---
title: "agentmesh-router"
linkTitle: "agentmesh-router"
weight: 3
---

`agentmesh-router` is a libp2p peer with a stable identity that new nodes connect
to first. It hosts the DHT, relays traffic between nodes that cannot reach
each other, and forwards the control plane's signed events. It has no policy
of its own.

```text
agentmesh-router [flags]
```

## Enrollment

A router enrolls like a node, requesting `mesh:role:router`, with one of:

| Flag | Meaning |
|---|---|
| `--jwt-path` | File containing an OIDC token (a projected service account token, for example). |
| `--bootstrap-token-path` | File containing a bootstrap token minted with `"role": "mesh:role:router"`. |
| `--oidc-token`, `--bootstrap-token` | The same tokens as values. Visible in process listings. The file forms are preferred. |
| `--label` | A `key=value` label declared at enrollment, repeatable (`--label region=eu --label zone=eu-west1-b`). The control plane signs it only if `mesh:role:router` allows it in `allowed_labels`, lists it on `/info`, and nodes choose routers by it with `--router-selector` and `--router-prefer`. The same labels, with the same meaning, as a node's; see [labels](../../concepts/authorization/#labels). |

The mesh policy must bind the router's identity to `mesh:role:router`. The
`agentmesh-p2p` Helm chart handles this: its bootstrap job binds the router's
service account and mints a bootstrap token with `max_usages` equal to the
replica count.

## Flags

| Flag | Default | Meaning |
|---|---|---|
| `--control-plane` | `http://127.0.0.1:8080` | Control plane URL. `https://` is required unless the host is loopback. |
| `--insecure-control-plane` | `false` | Accept plaintext `http://` to a non-loopback host, such as an in-cluster Service. |
| `--listen` | `/ip4/0.0.0.0/tcp/5001`, `/ip6/::/tcp/5001` | libp2p listen addresses. Repeatable. Add `/ip4/0.0.0.0/udp/5001/quic-v1` for QUIC. |
| `--external-addr` | none | Addresses to announce instead of the detected ones, such as `/dnsaddr/bootstrap.example.com` or `/ip4/<public-ip>/tcp/5001`. Repeatable. |
| `--keys-path` | `router.key` | The router's private key. It fixes the peer ID across restarts and belongs on persistent storage. |
| `--keys-sync-interval` | `5m` | How often `/keys` is polled for signing-key rotations. |
| `--lease-renew-interval` | `300s` | How often the lease is renewed. Must be well below the control plane's `--lease-duration`. |
| `--allow-loopback` | `false` | Announce and accept loopback and link-local addresses. For a router and nodes on one host. |
| `--conns-per-source-ip` | a quarter of `--high-watermark` (`1000`) | Inbound connections accepted per source address. One address can hold at most this share of the router's connection budget, so filling a router takes at least four addresses. Members behind a NAT or a cluster's SNAT share one address and each holds one connection per router, two while enrolling. |
| `--low-watermark`, `--high-watermark` | 15% below the high mark (`3400`), `4000` | How many members the router holds: peers that passed the handshake, other routers aside. At the high mark the router sends members away (see below) until it is back at the low mark, so the gap between the two is what one shedding moves; the default moves 15%. The same numbers bound connections: above the high mark the connection manager closes connections, oldest first among those that carry no session (a member attached elsewhere whose DHT client is connected here, a control plane publisher, a stranger), then members; connections to the other routers are never closed. |
| `--dht-provider-addr-ttl` | `15m` | How long a service announcement lives after a node last made it. Nodes re-announce every 5 minutes, so a node that is gone drops out of discovery within this time. `0` keeps the default. |
| `--dht-max-record-age` | library default | DHT value record lifetime. |
| `--relay-limit-duration`, `--relay-limit-data` | `1h`, `0` | Caps on each relayed connection: lifetime, and bytes per direction (`512MiB`, `1GB`). The relay cuts the connection when either is reached. `0` means no limit. |
| `--relay-max-reservations` | `--high-watermark` (`4000`) | Members that may hold a relay reservation on this router at once, which is how many members it can reach on behalf of callers. Per source address the cap is `--conns-per-source-ip`. |
| `--relay-max-circuits` | `1024` | Relayed connections one peer may hold at once through this router, as caller or as destination. A service that many members call at once needs this many open circuits. |
| `--shutdown-lease-ttl` | `30s` | Sent in the router's last lease when it stops: how long it expects to be away, which is how long the control plane keeps listing it. The default covers a pod that restarts in place. A router that is being removed for good can send `1s`. `0` sends nothing and the lease expires on the control plane's schedule. |
| `--metrics-addr` | off | Serve `/metrics`, `/healthz` and `/readyz` without authentication on this address. `/readyz` returns `200` once the router is enrolled and the libp2p host is up. Keep this address separate from the libp2p ports and inside the cluster. |
| `--log-level` | `info` | `debug`, `info`, `warn`, `error`. `LOG_FORMAT=json` selects JSON output. |

## What it does at run time

1. Fetches `/keys` and enrolls. The credential carries
   `role("mesh:role:router")` and the relay right.
2. Starts the libp2p host, the DHT in server mode, the relay service and
   GossipSub.
3. Registers a lease at `POST /routers/lease` with its announced addresses
   and renews it every `--lease-renew-interval`. The control plane lists the
   router on `/info` while the lease is live.
4. Runs the mutual credential handshake on every inbound connection and
   refuses peers whose credential does not verify and peers on the ban list.
5. Refreshes its own credential before it expires, like a node.
6. Every 5 seconds, compares the members it holds, the peers that passed
   the handshake, with `--high-watermark`. At the mark it picks members at
   random, as many as take it back to `--low-watermark`, and sends each a
   go-away on `/mesh/goaway/1.0.0` with the reason `OVERLOADED` and a
   retry time of 5 minutes. A member that takes the message attaches to
   another router at once and stays off this one for that time; one that
   does not is closed after a 5 second grace, like any other. Connections
   to the other routers are never shed: the DHT runs over them.
7. On `SIGTERM` or `SIGINT`, sends a last lease with `--shutdown-lease-ttl`,
   sends every member a go-away with the reason `DRAINING` and that ttl as
   the retry time, and after the same grace closes its connections. The
   control plane lists the router for that long and no longer, so a router
   that restarts in place is still there when its members redial it, and
   joiners are sent to the routers that remain once the time has passed.
   The operator states the time the router will be down, the way BGP
   graceful shutdown announces a maintenance window.

A router stores nothing except its key. Restarting a router loses no
important state. Nodes reconnect and publish their services again.

## Sizing

The router sizes its libp2p resource limits from `--high-watermark` and
`--conns-per-source-ip`, not from the memory of the machine: the stream
budgets of the system, of identify, of the credential handshake and of the
relay follow the connection budget, so a router on a small VM admits as
many members as its watermarks say. What the machine needs is CPU: every
member that joins costs the router a TLS handshake, an identify exchange and
a credential verification, and a router at its CPU limit answers them late
enough that members time out and retry, which costs more. Give a router at
least one full core and no CPU limit; on Kubernetes, set a request and
leave the limit off. The testnet runs its routers on 2 vCPU without a
limit, and a fleet of 500 members joining at 20 a second is admitted in
under a minute. The [scalability guide](../../guides/scalability/) sizes
the watermarks and the relay budget for a fleet.

## Metrics

The metrics address exposes, next to the Go runtime metrics:

| Metric | Meaning |
|---|---|
| `agentmesh_router_ready` | `1` once the router is enrolled and the host is up. |
| `agentmesh_router_draining` | `1` from the moment the router starts stopping until the process exits. |
| `agentmesh_router_connections{direction}` | Open connections, `inbound` and `outbound`. |
| `agentmesh_router_connection_watermark{level}` | The `low` and `high` watermarks in force. The gap between `inbound` connections and the `high` watermark is the free capacity. |
| `agentmesh_router_conns_per_source_ip_limit` | The per-address cap in force. |
| `agentmesh_router_relay_limit{limit}` | The relay budget in force: `reservations`, `reservations_per_ip` and `circuits_per_peer`. |
| `agentmesh_router_connected_peers`, `agentmesh_router_authenticated_peers`, `agentmesh_router_banned_peers` | Peers in each state. |
| `agentmesh_router_dht_routing_table_size` | DHT routing table size. |
| `agentmesh_router_auth_handshakes_total{result}`, `agentmesh_router_lease_renewals_total{result}` | Handshakes and lease renewals by outcome. |
| `agentmesh_router_goaway_sent_total{reason,result}` | Members sent away, by reason (`DRAINING`, `OVERLOADED`) and outcome: `ok` when the member took the message, `unsupported` when it does not speak the protocol and was closed instead, `failed` when the stream broke. A rising `OVERLOADED` count on a router that is not draining is a fleet that has outgrown its routers. |
| `agentmesh_router_inbound_connections_refused_total` | Inbound connections refused by the per-address cap. |
| `libp2p_relaysvc_*` | The relay service's own counters: `reservations_total{type}` and `connections_total{type}` (`opened`, `closed`, `renewed`), requests by response status, rejections and bytes relayed. The difference between `opened` and `closed` is the number in use; read it against `agentmesh_router_relay_limit` to see how close the relay is to its budget. |

A fleet is at capacity when `agentmesh_router_connections{direction="inbound"}`
approaches the `high` watermark or open relay reservations approach
`agentmesh_router_relay_limit{limit="reservations"}` on every router that is not
draining; both are the inputs for an autoscaler. `/healthz` and `/readyz`
on the same address are the probes to use in a pod spec.
