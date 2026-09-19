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
| `/sam/<peer-id>/<type>/<name>/<path>` | A reverse proxy to one specific service on one specific node, for every service type. `<type>` is `mcp`, `inference` or `a2a`, and `<path>` is passed through to the backend. For an A2A agent this path is the agent's URL as seen from the mesh: it serves the agent card, rewritten for the mesh, and carries the agent's JSON-RPC requests. See [A2A agents](#a2a-agents) below. |
| `/sam/service/discover` | Service discovery over plain HTTP, with the same results as the `discover_remote_services` tool. |

The [node API reference](../../reference/node-api/) lists every route.

A node publishes three kinds of service, and each one enters the mesh
through the same discovery, connection and policy steps described on this
page:

| Type | Backend | How a caller uses it |
|---|---|---|
| `mcp` | An MCP server: a subprocess over stdio, or a Streamable HTTP URL. | Through the node's `/mcp` tools, or directly through the proxy path. |
| `inference` | An OpenAI-compatible HTTP API. | Through the node's `/v1` endpoint, which chooses a provider, or directly through the proxy path. |
| `a2a` | An agent that speaks the [A2A protocol](https://a2a-protocol.org/) over HTTP. | Through the proxy path, with a standard A2A client. |

## Discovery

A node that publishes a service does two things. It records itself in the
Kademlia DHT, hosted by the routers, as a provider of that service name. It
also announces the service over GossipSub to nodes that have asked about that
name. A node that looks for a service asks the DHT for providers, and for
names it asked about before it already has the gossip announcements.

Before a node advertises a backend, it probes the backend
(`--backend-probe-timeout`, 2 seconds by default). An MCP backend must
complete an MCP `initialize`, and an A2A agent must serve its agent card. A
backend that does not answer is not advertised, so `discover_remote_services`
does not list services that would fail on the first call. Inference backends
are not probed. They are advertised as declared, and their model list is
fetched when a caller asks for it.

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

Every hop between two nodes is encrypted by libp2p's TLS 1.3 secure channel.
TLS is the only security transport enabled, so that FIPS-validated
cryptography can be used end to end. A relay forwards ciphertext and learns
only that the two peers are talking. The control plane sees enrollments,
refreshes, router leases, policy fetches and catalog reports. It never sees a
request.

Traffic to the control plane is protobuf over HTTPS. Traffic between nodes is
HTTP over libp2p streams for service requests of every type (MCP, inference
and A2A), and protobuf frames for the authentication handshake, the DHT and
gossip.

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

## A2A agents

`a2a://` services are agents that speak the
[A2A protocol](https://a2a-protocol.org/) (Agent2Agent) over HTTP. This is
how one agent on the mesh calls another agent as a peer, instead of calling
it as a tool. The agent is declared on its node with `type: a2a` and a
`target_url`, and remote callers reach it at
`/sam/<peer-id>/a2a/<name>/` on their own node. The request goes through
the same authenticated stream and the same policy check as any other
service request. The policy grant is `a2a://<name>`.

A2A clients start from the agent card at `/.well-known/agent-card.json`.
The card contains the agent's own interface URLs, which are addresses on
the provider's machine and are not reachable from anywhere else. Forwarding
the card unchanged would make a standard client fetch it through the mesh
and then try to connect to `127.0.0.1` on the wrong host. The caller's node
therefore handles the card itself:

1. On a `GET` of `/sam/<peer-id>/a2a/<name>/.well-known/agent-card.json`
   (or of the bare service path, for SDKs that treat the base URL as the
   card location), the node holds the request and fetches the card from the
   agent over the mesh.
2. It rewrites every interface URL to the mesh path that the client used,
   so the client keeps talking through the node.
3. It removes bindings that the mesh cannot carry. gRPC needs its own
   end-to-end connection, so only the JSON-RPC and HTTP+JSON bindings
   remain. A card with neither, which includes cards in a pre-1.0 format,
   is refused with `502`.
4. It advertises streaming as off and removes the card's signatures, because
   the content has changed.

The client then follows the rewritten card. Every following request, which
is a JSON-RPC `POST` to the same path, is forwarded to the agent unchanged.
The A2A `contextId` and `taskId` travel inside the messages and are not
touched, so multi-turn conversations work as they do against a local agent.

`X-Sam-Required-Labels` works on A2A requests in the same way as on
inference requests. The calling node verifies the provider's credential,
refuses with `403` if the labels are not attested, and removes the header
before forwarding. A caller that needs an agent to run in a given region can
require it on every message.

On the provider side, the node advertises the agent only while the agent
serves its card. An agent that is still starting, or that has stopped, is
not listed in discovery. This is what lets a node declare an agent before
the agent process exists, which the
[sandboxed agents preview](../../preview/sandboxed-agents/) relies on.

[Exposing services](../../guides/exposing-services/#a2a-agents) shows the
declaration, [Connecting agents](../../guides/connecting-agents/#calling-a2a-agents)
shows the client side, and the [A2A chat use case](../../use-cases/chat-a2a/)
runs one end to end.

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
