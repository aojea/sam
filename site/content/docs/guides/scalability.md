---
title: "Scalability"
linkTitle: "Scalability"
weight: 7
---

This guide is for the operator of a mesh that grows past a few hundred
members. It explains what each component spends per member, which setting
bounds what, how to size a deployment for the fleet you expect, how to
place members on routers with labels, and which metrics tell you that a
bound is near. The numbers come from the public testnet, where routers on
2 vCPU held 3200 to 3500 connections each and a fleet of 3200 members
stayed through a rollout of every router and both control plane replicas.

## What a member costs

A member is one `agentmesh-node` process. It holds a session with two
routers (`--routers`), chosen from those the control plane lists, and a
relay reservation on each. Through those sessions it keeps a GossipSub
subscription for control plane events and a DHT client. It opens short
connections to other routers when a DHT query or a call takes it there,
and to the router of a peer it calls, where it runs the credential
handshake before asking for the circuit. At rest it uses about 55 MiB of
memory, 300 goroutines and 20 file descriptors, and almost no CPU.

Joining is the expensive moment. The member enrolls at the control plane
(one signed request, one database write, one credential minted), reads the
router list with each router's labels and load, picks its routers and
opens a connection to each, and on each one runs a TLS or Noise handshake,
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
| Routers per member | `agentmesh-node --routers` | 2 | A member holds this many routers; one is enough to be on the mesh, the second covers a router restart. |
| Connections per router | `agentmesh-router --high-watermark`, `--low-watermark` | 4000, 15% below (3400) | At the high mark the router sends members away, at random, down to the low mark; they attach to another router and stay off this one for five minutes. A mesh of N routers holds about N × 4000 / 2 members. |
| Connections per source address | `agentmesh-router --conns-per-source-ip` | a quarter of the high mark (1000) | The router refuses the next connection from that address. Members behind one NAT or one cluster's egress share an address. |
| Relay reservations per router | `agentmesh-router --relay-max-reservations` | the high mark (4000) | A member that cannot reserve is not reachable through that router. |
| Relayed connections per peer | `agentmesh-router --relay-max-circuits` | 1024 | A service called through the relay holds one circuit per caller; the next caller is refused. |
| Enrollments per second | control plane, fixed | 10 a second, burst 20 | The next enrollment is answered `429` with `Retry-After`; the node retries for up to three minutes. |
| Connections per node | `agentmesh-node`, fixed | 400 high, 100 low | A node called by more peers than this at once closes connections down to 100. |
| Router lease | `agentmesh-control-plane --lease-duration`, `agentmesh-router --shutdown-lease-ttl` | 15m, 30s | A router that stops without the last lease stays listed for the lease duration. |

The first two rows are the model. A fleet of F members on N routers, each
member holding K of them, puts about F × K / N sessions on each router, so
adding a router adds members: the fleet a mesh holds is N × H / K for a
high watermark H. A member picks the routers with the most room when it
joins, so a new router fills from the members that join after it and from
those the full routers send away; the members already placed do not move
on their own. When a router stops, its members spread over the N − 1
others, which is where the headroom in the next section goes.

## Sizing the routers

Start from the fleet you expect, F members on N routers with `--routers`
at its default of 2, and size for the day one router is down:

- `--high-watermark` at least 1.2 × F × 2 / (N − 1). Each router then
  holds F × 2 / N sessions in normal running, with room for the members of
  a stopped router and for the connections members open in passing (a DHT
  query, a call to a peer on this router, two connections each while they
  enroll). 5000 members on four routers need 4000; 10,000 members need
  six routers at 4800 or seven at 4000.
- `--low-watermark` at its default, 15% below the high mark. The gap is
  what one shedding moves to the other routers; a smaller gap sheds more
  often, a larger one moves more members at once. A router that sheds is
  one the fleet has outgrown: the members it sends away land on routers
  that are nearly as full, and the fix is a router, not a watermark.
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

Routers connect to one another, every pair, to run the DHT and GossipSub
across the mesh; a router never sheds those connections. That is N × (N − 1)
/ 2 connections for N routers, which is nothing at ten routers and a
question at a hundred; a mesh that large is several meshes.

When you change a router's flags, keep `minReadySeconds` at 90 s or more:
the StatefulSet waits that long between pods so that the members of one
router have settled on the others before the next one stops. A router that
stops announces `--shutdown-lease-ttl` (30 s) in its last lease, so it
stays listed for the time a restart takes and is dropped from `/info` if
it does not come back, and it tells each of its members it is draining
before it closes, so they attach to another router within the second and
leave this one alone for that time.

## Placing members with labels

A router may carry labels, `--label region=eu --label zone=eu-a`, which
the control plane checks against the router role's `allowed_labels` at
enrollment and lists on `/info`. A member picks its routers by them:

- `--router-selector region=eu` holds only routers that carry every pair
  named. A member whose selector no router carries waits, with a warning
  that names the selector and the labels it saw, and is not on the mesh
  until a router that matches exists.
- `--router-prefer zone=eu-a` orders the routers that pass the selector:
  those carrying more of the preferred pairs first, and among equals the
  one with the most room. A member stays on its zone's routers while they
  have capacity and falls to the region's others when they do not.

A selector places a member; it does not isolate it. Discovery and calls
cross the whole mesh: a member on the routers of one region finds a service
on the routers of another and calls it through the service's router, where
it authenticates on the way. What a selector buys is locality for the
relayed traffic, and a sizing you can do per group: the F in the formula
above is the members whose selector lands on a set of routers, and the N
is that set.

One value per key: a router carries `zone=eu-a` or `zone=eu-b`, not both.
A hierarchy is several keys on one router, `region=eu zone=eu-a`, with
members selecting on the broad key and preferring on the narrow one.

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
| `agentmesh_router_connections{direction="inbound"}` against `agentmesh_router_connection_watermark{level="high"}` | How full a router is. The gap, summed over the routers that are not draining, is the free capacity of the mesh. |
| `agentmesh_router_goaway_sent_total{reason="OVERLOADED"}` | A router at its high mark sent members to the others. Rising on a router that is not draining, the fleet has outgrown its routers. |
| `agentmesh_router_inbound_connections_refused_total` | Members behind one address have hit `--conns-per-source-ip`. |
| `libp2p_relaysvc_reservations_total{type="opened"}` minus `{type="closed"}`, against `agentmesh_router_relay_limit{limit="reservations"}` | Reservations in use against the budget. |
| `libp2p_relaysvc_connection_rejections_total{reason="resource limit exceeded"}` | A service has more callers than `--relay-max-circuits`. |
| `libp2p_rcmgr_blocked_resources` | libp2p refused a connection or stream on a scope. On a router this should stay flat. |
| `agentmesh_router_auth_handshakes_total{result="read_failed"}` | Members that timed out mid-handshake: the router is slow, usually CPU. |
| `agentmesh_router_draining` | The router is stopping. Exclude it from capacity sums. |
| `agentmesh_control_plane_http_requests_total{code="429"}` | Members turned away by the enrollment limiter; they retry. |
| `agentmesh_control_plane_http_request_duration_seconds` | The control plane's own latency; p99 above a second under a join burst means the database is the bound. |
| `agentmesh_node_mesh_connected` | On every node: `0` for five minutes is a member off the mesh. |
| `agentmesh_node_routers{state="attached"}` against `{state="wanted"}` | On every node: fewer attached than wanted for long is a member that cannot find a router with room, or one whose selector too few routers carry. `agentmesh_node_router_candidates{match="selector"}` is how many it could choose from. |

An autoscaler for the routers has its inputs in the first two rows: scale
out when the free capacity of the mesh is below what one router holds, so
that the stop of any one router still fits, or when a router that is not
draining sends members away. A new router fills from the members that
join after it and from those the full routers shed; it does not pull
members off routers that have room.

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
