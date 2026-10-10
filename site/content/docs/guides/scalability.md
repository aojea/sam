---
title: "Scalability"
linkTitle: "Scalability"
weight: 7
---

This guide is for the operator of a mesh that grows past a few hundred
members. It explains what each component spends per member, which setting
bounds what, how to size a deployment for the fleet you expect, and which
metrics tell you that a bound is near. The numbers come from the public
testnet, where a fleet of 3000 members was resident on a mesh of three
routers on 2 vCPU each twenty minutes after the first one started, and
stayed through a rollout of every router and both control plane replicas.

## What a member costs

A member is one `agentmesh-node` process. It holds one connection to every
router in the mesh, two while it enrolls, and a relay reservation on two of
them. Through those connections it keeps a GossipSub subscription for
control plane events and a DHT client. At rest it uses about 55 MiB of
memory, 300 goroutines and 20 file descriptors, and almost no CPU.

Joining is the expensive moment. The member enrolls at the control plane
(one signed request, one database write, one credential minted), then opens
a connection to every router, and on each one runs a TLS or Noise handshake,
an identify exchange and the credential handshake, in which the router
verifies the member's credential and the member verifies the router's. A
router pays a few milliseconds of CPU per member that joins, and a fleet
that joins at once pays them all at once.

## What bounds the mesh

The table lists each bound, the component that enforces it, its default and
what happens when it is reached. The sections after it explain how to size
them.

| Bound | Where | Default | At the bound |
|---|---|---|---|
| Connections per router | `agentmesh-router --high-watermark`, `--low-watermark` | 4000, 1000 | Above the high mark, the router closes connections down to the low mark. Every member holds one, so this is the size of the mesh. |
| Connections per source address | `agentmesh-router --conns-per-source-ip` | a quarter of the high mark (1000) | The router refuses the next connection from that address. Members behind one NAT or one cluster's egress share an address. |
| Relay reservations per router | `agentmesh-router --relay-max-reservations` | the high mark (4000) | A member that cannot reserve is not reachable through that router. |
| Relayed connections per peer | `agentmesh-router --relay-max-circuits` | 1024 | A service called through the relay holds one circuit per caller; the next caller is refused. |
| Enrollments per second | control plane, fixed | 10 a second, burst 20 | The next enrollment is answered `429` with `Retry-After`; the node retries for up to three minutes. |
| Connections per node | `agentmesh-node`, fixed | 400 high, 100 low | A node called by more peers than this at once closes connections down to 100. |
| Router lease | `agentmesh-control-plane --lease-duration`, `agentmesh-router --shutdown-lease-ttl` | 15m, 30s | A router that stops without the last lease stays listed for the lease duration. |

Two of these need a word. Adding routers does not add members: every
member connects to every router, so a mesh of N members puts N connections
on each router whatever their number. More routers add relay capacity,
redundancy and places to join from; the high watermark of one router is
what bounds N. The low watermark is a floor you must keep above the fleet:
when a router crosses the high mark it trims to the low one, and a low
mark below the number of members disconnects most of the mesh at once.

## Sizing the routers

Start from the fleet you expect, with headroom for members that enroll
(two connections each while they do) and for the next step of growth. For
a fleet of F members:

- `--high-watermark` at least 1.5 F. The testnet runs 4000 for 3000
  members; three of its routers held 3200 to 3500 connections each at the
  end of the run, which is as close to the mark as you want to be.
- `--low-watermark` above F. Trimming is for a router that is over its
  budget, and the members it trims lose nothing for long (the node redials
  a router that drops it after 2 s, then 4 s, and so on), but trimming
  below the fleet is an outage you configured.
- `--conns-per-source-ip` at the number of members that share one address,
  times two if they may all enroll at once. One cluster behind one egress
  address with 500 nodes needs 1000. The default follows the high mark.
- `--relay-max-reservations` stays at the high mark. `--relay-max-circuits`
  is the number of callers one service may have at once through one router;
  raise it for a service the whole mesh calls.

Memory follows connections: about 150 KiB each, so a router at 3500
connections uses about 550 MiB, with 20,000 goroutines. CPU follows joins
and relayed traffic, and a router needs a core it can use. Under a CPU
limit a router answers handshakes late, members time out and retry, and
the retries cost more than the handshakes did. On Kubernetes set a request
and no limit, as `charts/agentmesh` and the testnet manifests do; the
testnet runs its routers on 2 vCPU without a limit. The router sizes its
libp2p stream limits from the watermarks, so a small VM admits as many
members as its watermarks say; what a small VM cannot do is handshake them
quickly.

When you change a router's flags, keep `minReadySeconds` at 90 s or more:
the StatefulSet waits that long between pods so that the members of one
router are back before the next one stops. Each router announces
`--shutdown-lease-ttl` (30 s) in its last lease, so it stays listed for
the time a restart takes and is dropped from `/info` if it does not come
back.

## Sizing the join

The control plane admits ten enrollments a second for the whole mesh. A
member it turns away retries after `Retry-After` with jitter for up to
three minutes, so a fleet of up to about 1500 members may start at once and
joins over 150 s. A larger fleet starts in waves of that size, or at a rate
of ten a second. The limiter protects the control plane and is a constant
of the build.

The routers set the other half of the join rate. A fleet of 500 joining at
20 a second was admitted in under a minute by routers on 2 vCPU. Against
the same routers under a 500m CPU limit, more than a third of the members
on one host exited at the handshake timeout. The node now retries a router
that did not admit it in time (after 2 s, 4 s and 8 s), so a slow router
costs the fleet time first; it costs members when the retries run out.

A member that enrolls needs its control plane and its routers reachable
from where it runs. Behind a NAT it is reached through a relay, which costs
a reservation on the router and nothing else; a member with a public
address may run `--reachability auto` and be dialled directly once AutoNAT
has confirmed the address, which takes it off the relay.

## Sizing a service

A service is reachable through the node that publishes it, and callers that
cannot dial that node directly reach it through a relay circuit on a router
where it holds a reservation. Three things bound how many callers one node
serves at once:

- `--relay-max-circuits` on the router, 1024 by default.
- The node's connection manager, 400 connections. A node that many callers
  reach at once closes connections above that.
- The backend behind the node. A `command` backend is one process that
  every caller shares; a URL backend is whatever the server behind it can
  take.

A service that the whole mesh calls is published by several nodes. Callers
discover every provider of a name and pick one, so capacity grows with
providers. A provider that does not answer costs its caller one attempt:
the caller is answered `502 Bad Gateway` and may try another provider. On
the testnet, 500 members calling a service published by two nodes in the
same second completed 55 first calls within two minutes; the rest were
answered 502 and retried. Size the number of providers to the number of
callers you expect in one moment.

## Running many nodes on one host

A load generator, or a host that runs one node per agent, needs the kernel
limits a few hundred libp2p hosts need. For 500 nodes on one machine:

```text
fs.file-max = 4194304
fs.nr_open = 4194304
kernel.pid_max = 4194304
kernel.threads-max = 2097152
net.core.somaxconn = 4096
net.core.rmem_max = 7500000
net.core.wmem_max = 7500000
vm.max_map_count = 1048576
```

with `nofile` at 1048576 and `nproc` unlimited in `limits.conf`. Leave the
ephemeral port range alone; if the nodes serve on fixed ports (metrics, the
API), reserve those with `net.ipv4.ip_local_reserved_ports` so an outgoing
connection is never given one. Every node on the host shares its source
address, so the router's `--conns-per-source-ip` must cover them, twice
while they enroll. Memory is 55 MiB per node; 500 nodes fit in 32 GiB with
room for their backends.

## What to watch

The router and the control plane serve Prometheus metrics on
`--metrics-addr`. These are the ones that say a bound is near:

| Metric | Reads as |
|---|---|
| `agentmesh_router_connections{direction="inbound"}` against `agentmesh_router_connection_watermark{level="high"}` | How full a router is. The gap is the free capacity of the mesh. |
| `agentmesh_router_inbound_connections_refused_total` | Members behind one address have hit `--conns-per-source-ip`. |
| `libp2p_relaysvc_reservations_total{type="opened"}` minus `{type="closed"}`, against `agentmesh_router_relay_limit{limit="reservations"}` | Reservations in use against the budget. |
| `libp2p_relaysvc_connection_rejections_total{reason="resource limit exceeded"}` | A service has more callers than `--relay-max-circuits`. |
| `libp2p_rcmgr_blocked_resources` | libp2p refused a connection or stream on a scope. On a router this should stay flat. |
| `agentmesh_router_auth_handshakes_total{result="read_failed"}` | Members that timed out mid-handshake: the router is slow, usually CPU. |
| `agentmesh_router_draining` | The router is stopping. Exclude it from capacity sums. |
| `agentmesh_control_plane_http_requests_total{code="429"}` | Members turned away by the enrollment limiter; they retry. |
| `agentmesh_control_plane_http_request_duration_seconds` | The control plane's own latency; p99 above a second under a join burst means the database is the bound. |
| `agentmesh_node_mesh_connected` | On every node: `0` for five minutes is a member off the mesh. |

An autoscaler for the routers has its inputs in the first two rows, with
one caveat from the model above: a new router adds relay capacity and
redundancy, and the connection count on each existing router stays what it
was. To admit more members, raise the watermarks and give the routers the
memory the connections need.

## Measuring your own mesh

`agentmesh-bench join` starts N nodes on one host from one bootstrap token
and reports every member's journey from process start to its first call
through the mesh, then holds the fleet resident and samples its readiness.
`tests/scale/member-journey.sh` runs it against a deployment with the
control plane and router metrics read before and after, and
`tests/scale/member-ladder.sh` runs it from several hosts in steps, so each
step lands on a mesh already carrying the ones before. The report says how
many members became ready and how long the p50 and p95 took, which is the
number to compare against the bounds above before a fleet does.
