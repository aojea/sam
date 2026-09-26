---
title: "Networking and the node API"
linkTitle: "Networking"
weight: 4
---

This page follows a request from an agent to a service on another node:
how the calling node finds the provider, how the two connect, and what the
agent sees at each end.

## The node's local API

An agent does not speak to the mesh directly. It speaks to the `sam-node` on
its own machine, through one of two listeners:

- **TCP**, `127.0.0.1:8080` by default (`--bind-addr`). Every request must
  carry the node's API token as `X-Sam-Authentication: Bearer <token>`.
- **A Unix socket**, `<data-dir>/sam.sock` by default (`--socket-path`),
  created with mode `0600`. No token is needed, because only the user who
  owns the socket can open it. `docker.sock` works the same way.

You can turn either listener off by passing an empty value. With
`--bind-addr=`, the node has no listening port and no secret to manage. This
is a good setup when everything that uses the node runs as the same user.

The token travels in `X-Sam-Authentication` and not in `Authorization`, so
that `Authorization` always means the same thing: the credential for the
service that is being called through the node. On the proxy path that header
is forwarded unchanged. The endpoints that never forward anything (`/mcp`,
`/v1/*`, `/sam/service/*`) also accept the token in `Authorization`, for
clients that cannot set a custom header.

The API has four parts:

| Path | What it is |
|---|---|
| `/mcp` | An MCP server (Streamable HTTP). Its tools are the mesh operations: list and discover services, find and describe tools, call a tool on a peer. |
| `/v1/models`, `/v1/chat/completions`, `/v1/completions` | An OpenAI-compatible endpoint. `/v1/models` lists every model that a reachable `inference://` provider serves. A completion request is routed to a provider that serves the requested model. |
| `/sam/<peer-id>/<type>/<name>/<path>` | A reverse proxy to one specific service on one specific node, for every service type. `<type>` is `mcp`, `inference` or `a2a`, and `<path>` is passed through to the backend. This is how a caller reaches an A2A agent: `/sam/<peer-id>/a2a/<name>/` serves the agent's card, rewritten for the mesh, and its JSON-RPC endpoint. |
| `/sam/service/discover` | Service discovery over plain HTTP, with the same results as the `discover_remote_services` tool. |

The [node API reference](../../reference/node-api/) lists every route.

## Discovery

A node that publishes a service does two things. It records itself in the
Kademlia DHT, hosted by the routers, as a provider of that service name. It
also announces the service over GossipSub to nodes that have asked about that
name. A node that looks for a service asks the DHT for providers, and for
names it asked about before it already has the gossip announcements.

Before a node advertises a backend, it probes the backend
(`--backend-probe-timeout`, 2 seconds by default). A backend that does not
answer is not advertised, so `discover_remote_services` does not list
services that would fail on the first call.

Discovery results are hints. They tell the caller who claims to provide a
service and, if the provider declared labels, what those labels are. Nothing
is authorized on that basis. The provider's credential is verified when the
connection is made.

Every node also runs the built-in `system://sam.catalog` service, which
answers the question "what do you host?" for callers whose credential grants
it. `find_remote_tools` uses it to list a peer's tools.

## Connecting

Nodes are libp2p peers. By default a node listens on QUIC (`udp/5001`) and
TCP (`tcp/5002`) and advertises the addresses it has, including private
ones unless you pass `--announce-private=false`. Two nodes that can reach
each other connect directly. When neither can accept an inbound connection,
which is the common case, they use a **relay circuit** through a router.
Hole punching is enabled, so a relayed connection is upgraded to a direct
one when the NATs allow it.

Routers are the fixed points. A node learns their addresses from the control
plane's `/info` at enrollment and fetches them again when its connections
drop. A router accepts a connection only from a peer that presents a valid
credential, so the DHT and the relay are closed to anyone who is not
enrolled.

When a node opens a stream to another node for a service request, the first
message is an authentication frame with the caller's credential and the
service it wants. The receiver verifies the credential, evaluates policy for
that service (see [Authorization](../authorization/)), and answers with its
own credential. Both sides now know who the other is. The caller can check
what it needs to know about the provider, for example its labels, before it
sends any request data.

## What travels where

Every hop between two nodes is encrypted by a libp2p secure channel that
binds the connection to the peer's identity: TLS 1.3, which Go peers and the
Node and Python SDKs use, or Noise, which a browser can speak. Routers and
nodes offer TLS first and accept Noise; two peers that both have TLS land on
it. A relay forwards ciphertext and learns
only that the two peers are talking. The control plane sees enrollments,
refreshes, router leases, policy fetches and catalog reports. It never sees a
request.

Traffic to the control plane is protobuf over HTTPS. Traffic between nodes is
HTTP over libp2p streams for service requests, and protobuf frames for the
authentication handshake, the DHT and gossip.

## Inference routing

`inference://` services are OpenAI-compatible HTTP backends: vLLM, Ollama,
LiteLLM, or a hosted API behind a local proxy. The node's `/v1` endpoint
hides which node serves a model. It keeps a short-lived view of the model
list of every reachable provider. It prefers a local provider when one
serves the model, and otherwise picks among remote providers, ranked by the
labels it knows about and by load. Backends are registered by their root URL
and the node adds the `/v1` prefix. The same backend is therefore reachable
through the `/v1` endpoint and, with `/v1/...` appended, through the proxy
path.

A caller can restrict which providers are acceptable with
`X-Sam-Required-Labels: key=value[,key=value]`. The node then verifies the
provider's credential and confirms that the labels are attested in it before
forwarding. The header is removed before the request leaves the node. An
operator can set a floor that every provider must meet with
`egress.require_labels` in the node configuration.

## Mesh events

The control plane publishes bans and signing-key rotations as signed events.
Routers inject them into a GossipSub topic that every node subscribes to. A
node that receives a ban event drops the banned peer immediately. A node
that receives a rotation event fetches the new key set. Each event is
verified against the control plane's key before a node acts on it or
forwards it.

## Resource limits

Nodes rate-limit authentication attempts per peer and cap the size of every
frame and body they read. Routers cap inbound connections per source IP
(`--conns-per-source-ip`) and total connections (`--low-watermark`,
`--high-watermark`). A router behind a TLS-terminating proxy, or behind a NAT
that puts many peers on one address, needs a higher per-source limit.
`sam-one` raises it automatically for its embedded router.
