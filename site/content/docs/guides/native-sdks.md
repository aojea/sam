---
title: "Native SDKs"
linkTitle: "Native SDKs"
weight: 6
---

An agent written in JavaScript or Python can be a member of the mesh itself,
with no `sam-node` beside it. The SDK enrolls with the control plane, joins
through a router, finds services, calls MCP tools and inference or A2A
endpoints, and answers A2A requests for the agent itself. Every caller of
the agent is checked against the mesh policy before anything reaches your
code.

This guide takes you from nothing to two programs on a mesh: an agent that
other members can call, and a caller that reaches a service by name and the
agent by its peer ID. Both programs are in the repository under
[`sdk/js/examples`](https://github.com/google/sam/tree/main/sdk/js/examples)
and
[`sdk/python/examples`](https://github.com/google/sam/tree/main/sdk/python/examples),
and the repository's tests run them against a real mesh, so what you read
here is what runs.

An SDK member calls what the mesh offers and accepts A2A requests for its
own agent. It does not publish services. A tool, a model or an agent that
others should find by name runs behind a `sam-node`, which publishes it and
enforces the policy; see [Your own mesh](../../getting-started/your-own-mesh/).
The reasons are in the
[SDK README](https://github.com/google/sam/blob/main/sdk/README.md#agents-not-services).

## 1. Get a mesh

A program needs the URL of a control plane and a token that lets it enroll.
You get them in one of two ways.

### Run your own

`sam-one` runs a control plane, a router and a web console in one process.
The [install script](../../getting-started/quickstart/#1-install) provides
it. Start it with a directory for its state:

```bash
sam-one --data-dir ~/sam-one
```

The banner it prints has the URL, and the token is written to
`~/sam-one/join-token`:

```text
API URL:      http://0.0.0.0:33775
Join Token:   sam_tok_…
```

On first boot `sam-one` seeds an open development policy, so any enrolled
member may publish and call any service. That is right for trying the
programs below on your laptop. Before you share the mesh, replace it, as
[Your own mesh](../../getting-started/your-own-mesh/#5-before-you-share-it)
explains.

This mesh is reachable from your machine only. To let programs on other
machines join, start `sam-one` with `--tunnel cloudflare`, which publishes
the port on a temporary public `https` hostname and prints that URL in the
banner instead. Use that URL as the control plane URL everywhere below. [Your
own mesh](../../getting-started/your-own-mesh/) covers the tunnel, a real
hostname and the console.

### Join a mesh someone else runs

Ask the operator for the control plane URL and a bootstrap token. Operators
mint tokens in the console or with `sam-one token create`; a mesh with an
identity provider lets you mint your own from the console after logging in.
Save the token in a file that only you can read. Do not put it on a command
line: it would sit in `ps` output and shell history.

### Tell the programs about it

The programs below read the mesh from two environment variables. Set them in
every terminal you use:

```bash
export SAM_CONTROL_PLANE_URL=http://127.0.0.1:33775   # the API URL from the banner
export SAM_BOOTSTRAP_TOKEN_PATH=~/sam-one/join-token
```

A plain `http://` URL is accepted only for a control plane on the same
machine. Anything else needs `https://`, because the control plane is the
trust root of every member and the SDK refuses to fetch it over plaintext
from a remote address. Inside a network you already trust, such as a
Kubernetes cluster where the control plane is a cluster-local service, set
`SAM_INSECURE_CONTROL_PLANE=true` (`allowInsecure` in code), the same choice
as `sam-node --insecure-control-plane`.

On a platform that issues workload identity tokens, a program needs no
bootstrap token. A mesh whose control plane trusts the platform's issuer
enrolls the program from that token instead; on Kubernetes that is a
projected service account token, as [Headless enrollment](../headless-enrollment/)
shows for `sam-node`. Set `SAM_JWT_PATH` to the token file rather than
`SAM_BOOTSTRAP_TOKEN_PATH`. That is how the public testnets run these same
programs as canaries beside the `sam-node` ones.

## 2. Install the SDK

```bash
npm install @sam-mesh/sdk          # Node.js 22 or later
```

```bash
pip install sam-mesh               # Python 3.11 or later
```

The Python package is imported as `agent_mesh`. It runs on trio, because
py-libp2p does; under asyncio, use it through `anyio` with the trio backend.

## 3. Be an agent

This program joins the mesh and answers A2A requests for `a2a://agent`
until you stop it. It publishes nothing: there is no name to look up. A
caller reaches it by the peer ID it prints, through a router. Without an
argument the handler in the program answers; with the URL of an A2A server
running beside it, requests are forwarded there, which is how an agent
written with an A2A SDK joins the mesh.

JavaScript, `agent.js`:

<!-- embed: sdk/js/examples/agent.ts -->
```ts
// An agent on the mesh: joins, then answers A2A requests from other members
// until stopped. It publishes nothing. There is no service name to look up; a
// caller reaches the agent by its peer ID, through a router, as a2a://agent.
// The mesh policy decides which members may call; the SDK turns the others
// away before anything reaches this code.
//
//   node agent.js                        # answered by the handler below
//   node agent.js http://127.0.0.1:9999  # forwarded to an A2A server beside it
//
// SAM_CONTROL_PLANE_URL names the mesh. The first run enrolls with the file
// SAM_BOOTSTRAP_TOKEN_PATH (a token the mesh operator gave you) or
// SAM_JWT_PATH (a workload identity token your platform issues, such as a
// Kubernetes projected service account token), and keeps the identity and
// credential in SAM_STATE_DIR; later runs resume from there, as the same
// peer, without it.
import { homedir } from "node:os";
import { AgentMesh } from "@sam-mesh/sdk";

const [backendURL] = process.argv.slice(2);

const mesh = await AgentMesh.enroll({
  controlPlaneUrl: process.env.SAM_CONTROL_PLANE_URL ?? "https://mesh.example.com",
  bootstrapTokenPath: process.env.SAM_BOOTSTRAP_TOKEN_PATH,
  jwtPath: process.env.SAM_JWT_PATH,
  stateDir: process.env.SAM_STATE_DIR ?? `${homedir()}/.config/sam-mesh/agent`,
  // A plaintext http:// control plane is otherwise accepted only on loopback.
  allowInsecure: process.env.SAM_INSECURE_CONTROL_PLANE === "true",
});
const session = await mesh.join();

// Answers every path with who was asked and who asked; a real agent runs an
// A2A server here, or beside this process at backendURL.
const target = await session.acceptA2A(
  backendURL !== undefined
    ? { url: backendURL }
    : { handler: (request, caller) => Response.json({ name: "agent", path: new URL(request.url).pathname, caller: caller.peerId }) },
);
console.log(`accepting ${target} as ${session.peerId}`);

const stop = () => void session.close().then(() => process.exit(0));
process.on("SIGINT", stop);
process.on("SIGTERM", stop);
```
<!-- /embed -->

Python, `agent.py`:

<!-- embed: sdk/python/examples/agent.py -->
```python
"""An agent on the mesh: joins, then answers A2A requests from other members
until stopped. It publishes nothing. There is no service name to look up; a
caller reaches the agent by its peer ID, through a router, as `a2a://agent`.
The mesh policy decides which members may call; the SDK turns the others away
before anything reaches this code.

    python agent.py                        # answered by the handler below
    python agent.py http://127.0.0.1:9999  # forwarded to an A2A server beside it

SAM_CONTROL_PLANE_URL names the mesh. The first run enrolls with the file
SAM_BOOTSTRAP_TOKEN_PATH (a token the mesh operator gave you) or SAM_JWT_PATH
(a workload identity token your platform issues, such as a Kubernetes
projected service account token), and keeps the identity and credential in
SAM_STATE_DIR; later runs resume from there, as the same peer, without it.
"""

import json
import os
import sys

import trio
from agent_mesh import AgentMesh, HTTPRequest, HTTPResponse, VerifiedBiscuit

backend_url = sys.argv[1] if len(sys.argv) > 1 else None

mesh = AgentMesh.enroll(
    os.environ.get("SAM_CONTROL_PLANE_URL", "https://mesh.example.com"),
    bootstrap_token_path=os.environ.get("SAM_BOOTSTRAP_TOKEN_PATH"),
    jwt_path=os.environ.get("SAM_JWT_PATH"),
    state_dir=os.environ.get("SAM_STATE_DIR", "~/.config/sam-mesh/agent"),
    # A plaintext http:// control plane is otherwise accepted only on loopback.
    allow_insecure=os.environ.get("SAM_INSECURE_CONTROL_PLANE") == "true",
)


async def card(request: HTTPRequest, caller: VerifiedBiscuit) -> HTTPResponse:
    """Answers every path with who was asked and who asked; a real agent runs
    an A2A server here, or beside this process at backend_url."""
    body = json.dumps({"name": "agent", "path": request.path, "caller": caller.peer_id})
    return HTTPResponse(status=200, headers={"content-type": "application/json"}, body=body.encode())


async def main() -> None:
    async with mesh.join() as session:
        target = await session.accept_a2a(backend_url or card)
        print(f"accepting {target} as {session.peer_id}", flush=True)
        await trio.sleep_forever()


trio.run(main)
```
<!-- /embed -->

Run one of them:

```bash
node agent.js
# or
python agent.py
```

```text
accepting a2a://agent as 12D3KooWQmB5…
```

The program spent the token, saved its identity and credential under
`~/.config/sam-mesh/agent`, joined through the router and started answering.
Run it again and it resumes from that directory, as the same peer; the token
is no longer needed. Keep the peer ID: it is how the agent is reached, from
another program written with an SDK, a `sam-node` on any machine, or a
phone.

## 4. Call it

This program calls something on the mesh: a tool or a path of a service
someone published, found by name, or an agent that published nothing,
reached by its peer ID.

JavaScript, `call.js`:

<!-- embed: sdk/js/examples/call.ts -->
```ts
// Calls something on the mesh: a tool of an MCP service or a path of an
// inference or A2A service someone published, found by name, or an agent that
// published nothing, reached by its peer ID.
//
//   node call.js mcp://everything echo '{"message": "hi"}'
//   node call.js inference://ollama /v1/models
//   node call.js 12D3KooW... a2a://agent /card
//
// SAM_CONTROL_PLANE_URL names the mesh. The first run enrolls with the file
// SAM_BOOTSTRAP_TOKEN_PATH (a token the mesh operator gave you) or
// SAM_JWT_PATH (a workload identity token your platform issues, such as a
// Kubernetes projected service account token), and keeps the identity and
// credential in SAM_STATE_DIR; later runs resume from there without it.
import { homedir } from "node:os";
import { AgentMesh, type DiscoveredProvider } from "@sam-mesh/sdk";

let argv = process.argv.slice(2);
// A first argument that is not a service target is the peer ID of an agent.
let peerId: string | undefined;
if (argv[0] !== undefined && !argv[0].includes("://")) {
  [peerId, ...argv] = argv as [string, ...string[]];
}
const [service = peerId !== undefined ? "a2a://agent" : "mcp://everything", toolOrPath = peerId !== undefined ? "/card" : "echo", args = '{"message": "hi"}'] = argv;

const mesh = await AgentMesh.enroll({
  controlPlaneUrl: process.env.SAM_CONTROL_PLANE_URL ?? "https://mesh.example.com",
  bootstrapTokenPath: process.env.SAM_BOOTSTRAP_TOKEN_PATH,
  jwtPath: process.env.SAM_JWT_PATH,
  stateDir: process.env.SAM_STATE_DIR ?? `${homedir()}/.config/sam-mesh/caller`,
  // A plaintext http:// control plane is otherwise accepted only on loopback.
  allowInsecure: process.env.SAM_INSECURE_CONTROL_PLANE === "true",
});
const session = await mesh.join();
console.log(`on the mesh as ${session.peerId}`);

// The peer to call: an agent named by ID, or a provider of a published service.
let provider: DiscoveredProvider = { peerId: peerId ?? "", addrs: [] };
if (peerId !== undefined) {
  // An agent is not in the discovery table; the SDK finds the path to its
  // peer ID through the routers.
  await session.connect(peerId);
} else {
  const providers = await session.discover(service);
  if (providers.length === 0) {
    throw new Error(`no member of the mesh serves ${service}`);
  }
  // A provider record can outlive its member; the first that answers is used.
  let reached = false;
  for (const candidate of providers) {
    try {
      await session.connect(candidate);
      provider = candidate;
      reached = true;
      break;
    } catch (err) {
      console.error(`${candidate.peerId}: ${(err as Error).message}`);
    }
  }
  if (!reached) {
    throw new Error(`no provider of ${service} is reachable`);
  }
}
console.log(`${service} is served by ${provider.peerId}`);

if (service.startsWith("mcp://")) {
  const tools = await session.listTools(provider, service);
  console.log(`tools: ${tools.map((t) => t.name).join(", ")}`);
  const result = await session.callTool(provider, service, toolOrPath, JSON.parse(args));
  console.log(result.text.join("\n"));
} else {
  const response = await session.request(provider, service, toolOrPath);
  console.log(response.status, response.text());
}

await session.close();
```
<!-- /embed -->

Python, `call.py`:

<!-- embed: sdk/python/examples/call.py -->
```python
"""Calls something on the mesh: a tool of an MCP service or a path of an
inference or A2A service someone published, found by name, or an agent that
published nothing, reached by its peer ID.

    python call.py mcp://everything echo '{"message": "hi"}'
    python call.py inference://ollama /v1/models
    python call.py 12D3KooW... a2a://agent /card

SAM_CONTROL_PLANE_URL names the mesh. The first run enrolls with the file
SAM_BOOTSTRAP_TOKEN_PATH (a token the mesh operator gave you) or SAM_JWT_PATH
(a workload identity token your platform issues, such as a Kubernetes
projected service account token), and keeps the identity and credential in
SAM_STATE_DIR; later runs resume from there without it.
"""

import json
import os
import sys

import trio
from agent_mesh import AgentMesh

argv = sys.argv[1:]
# A first argument that is not a service target is the peer ID of an agent.
peer_id = None
if argv and "://" not in argv[0]:
    peer_id, argv = argv[0], argv[1:]
service = argv[0] if len(argv) > 0 else ("a2a://agent" if peer_id else "mcp://everything")
tool_or_path = argv[1] if len(argv) > 1 else ("/card" if peer_id else "echo")
args = json.loads(argv[2]) if len(argv) > 2 else {"message": "hi"}

mesh = AgentMesh.enroll(
    os.environ.get("SAM_CONTROL_PLANE_URL", "https://mesh.example.com"),
    bootstrap_token_path=os.environ.get("SAM_BOOTSTRAP_TOKEN_PATH"),
    jwt_path=os.environ.get("SAM_JWT_PATH"),
    state_dir=os.environ.get("SAM_STATE_DIR", "~/.config/sam-mesh/caller"),
    # A plaintext http:// control plane is otherwise accepted only on loopback.
    allow_insecure=os.environ.get("SAM_INSECURE_CONTROL_PLANE") == "true",
)


async def main() -> None:
    async with mesh.join() as session:
        print(f"on the mesh as {session.peer_id}")

        if peer_id:
            # An agent is not in the discovery table; the SDK finds the path
            # to its peer ID through the routers.
            await session.connect(peer_id)
            provider = peer_id
        else:
            providers = await session.discover(service)
            if not providers:
                raise SystemExit(f"no member of the mesh serves {service}")
            # A provider record can outlive its member; the first that answers is used.
            for provider in providers:
                try:
                    await session.connect(provider)
                    break
                except (ConnectionError, PermissionError) as err:
                    print(f"{provider.peer_id}: {err}", file=sys.stderr)
            else:
                raise SystemExit(f"no provider of {service} is reachable")
        print(f"{service} is served by {peer_id or provider.peer_id}")

        if service.startswith("mcp://"):
            tools = await session.list_tools(provider, service)
            print("tools:", ", ".join(t.name for t in tools))
            result = await session.call_tool(provider, service, tool_or_path, args)
            print("\n".join(result.text))
        else:
            response = await session.request(provider, service, tool_or_path)
            print(response.status, response.text)


trio.run(main)
```
<!-- /embed -->

In a second terminal, with the same two environment variables set, call the
agent by the peer ID it printed. Either language reaches an agent written
with the other:

```bash
python call.py 12D3KooWQmB5… a2a://agent /card
```

```text
on the mesh as 12D3KooWHZ2M…
a2a://agent is served by 12D3KooWQmB5…
200 {"name": "agent", "path": "/card", "caller": "12D3KooWHZ2M…"}
```

A service a `sam-node` publishes is found by name instead. On a mesh with
the `everything` canary of the public testnets, or your own `sam-node` with
an MCP server behind it:

```bash
node call.js mcp://everything echo '{"message": "hi"}'
```

```text
on the mesh as 12D3KooWHZ2M…
mcp://everything is served by 12D3KooWAb3d…
tools: echo, add, …
Echo: hi
```

The caller and the agent are two identities on the mesh with their own
state directories, `caller` and `agent`. Each spent a token on its first
run; the standing join token of `sam-one` admits any number of members.

## What happened

`enroll` sent the token and the program's public key to the control plane
and received a credential: a signed token that names the member, its role
and the routers it may use. The identity key and the credential live in the
state directory, so the next run resumes without a token, and the SDK
renews the credential before it expires. Delete the directory to enroll
afresh, for instance with other labels. The directory has the same layout
in both SDKs and in `sam-node`: a program in one language resumes a
directory written by the other, and `sam-node state import` runs the same
identity as a node (see the [sam-node reference](../../reference/sam-node/#state)).

`join` connected to the routers named in the credential, proved the
program's identity to each and verified the router's own credential, and
reserved a relay slot so that other members reach the program through the
router. The program opens no port of its own. While the session is open,
the SDK follows the control plane: keys, bans and router addresses on
`sam-node`'s schedule, and sooner when the control plane announces a change
over the mesh.

`discover` looked a service up by name in a table the routers host. An
agent is not in that table; given its peer ID, `connect` dials the path
through every router that admitted the caller, and the router opens a
circuit because it admitted the agent too. Either way the SDK verifies the
peer's credential before sending anything, and `requiredLabels`
(`required_labels` in Python) refuses a peer whose control-plane-attested
labels do not carry the values you ask for.

`acceptA2A` (`accept_a2a`) fetched the mesh policy and started answering.
Every caller must present a credential signed by a trusted control plane
key, bound to the connection's peer and unexpired, and its role must be
granted `a2a://agent` by the policy. The check runs on the same Datalog
text a `sam-node` evaluates. A caller the policy does not admit is turned
away before the request reaches your handler or your A2A server. Your
handler sees the verified caller as `caller.peerId` (`caller.peer_id`) and
a forwarded server as the `X-Peer-Id` header; the caller's credential is
never forwarded. The connection is encrypted end to end between the two
peers; the router forwards ciphertext.

Under the open development policy of `sam-one`, every member holds the
`node` role and that role may call any service. On a shared mesh the policy
grants a role the services it may call (`allowed_services: ["a2a://agent"]`
or a pattern) and the members it may reach; see
[Mesh policy](../../reference/policy/). A refused caller receives `403`.

## Beyond the examples

- **Naming a peer.** `callTool`, `listTools`, `request`, `authenticate` and
  `connect` accept a provider from `discover`, a peer id, or a multiaddr.
  With a peer id alone the SDK goes through the routers.
- **Every service of a type.** `discover("mcp")` lists every MCP provider
  on the mesh; `discover("mcp://everything")` those of one service.
- **A member's own catalog.** `listTools(peer, "")` returns a `sam-node`'s
  `list_local_services` tool.
- **An agent written with an A2A SDK.** Run its server on a local port and
  pass the URL: `acceptA2A({ url: "http://127.0.0.1:9999" })`,
  `accept_a2a("http://127.0.0.1:9999")`. In JavaScript an Express app with
  the A2A SDK's handlers can also run in the process:
  `acceptA2A({ listener: app })`. The agent's name defaults to `agent`;
  `name` picks another, and callers use `a2a://<name>`.
- **Calling an agent with an A2A SDK client.** The mesh is a transport for
  HTTP clients. A peer's service has the URL
  `http://mesh/sam/<peer-id>/a2a/agent` (`MeshSession.meshURL`,
  `MeshSession.mesh_url`); the A2A JavaScript client takes
  `fetchImpl: session.fetch()`, the Python one an
  `httpx.AsyncClient(transport=MeshTransport(session))`. Response bodies
  stream, so `message/stream` works.
- **Reading the policy.** `session.policyRules` (`session.policy_rules`)
  holds the Datalog the session enforces, for logging or tests.

## What the SDKs do not do

- They do not publish services. An MCP server, a model or an agent that
  others should find by name runs behind a `sam-node`, which publishes it
  to the discovery table and reports it to the console.
- They do not run a sidecar API or a sandbox. An agent that needs the egress
  policy enforcement of `sam-box` runs beside a `sam-node`.
- They do not serve the discovery table. A member is a client of it; the
  routers hold the records.
- They do not run in a browser. Node.js and CPython only, until the transport
  to routers from a browser is designed.

The wire contract and the interoperability facts the tests pin are in
[`sdk/README.md`](https://github.com/google/sam/blob/main/sdk/README.md).
