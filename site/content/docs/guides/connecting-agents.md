---
title: "Connecting agents"
linkTitle: "Connecting agents"
weight: 2
aliases:
  - /docs/user/agent-usage/
  - /docs/integrations/claude-code/
  - /docs/integrations/claude-desktop/
  - /docs/integrations/antigravity/
  - /docs/integrations/gemini/
  - /docs/integrations/openclaw/
  - /docs/integrations/vscode-copilot/
---

An agent uses the mesh through the `sam-node` that runs on its own machine.
The node is an MCP server, so you can point any MCP client at it. This guide
gives the configuration for the common clients and explains what the agent
gets.

## What every client needs

Three values, all printed by `sam-node run --daemonize`:

- **The endpoint**: `http://127.0.0.1:8080/mcp` (Streamable HTTP).
- **The token**: the contents of `~/.config/sam-mesh/api-token`, or the
  `SAM_API_TOKEN` you started the node with. It goes in the header
  `X-Sam-Authentication: Bearer <token>`.
- **The socket**: `~/.config/sam-mesh/sam.sock`. Anything the agent runs in a
  shell can use the socket without a token. This is the easier way to reach
  the node's `/v1` inference endpoint from scripts.

The node must be running before the client starts. MCP clients start and
manage stdio servers themselves, but `sam-node` is an HTTP server that they
connect to. You can run `sam-node run --daemonize` as often as you like, so it
is safe to put it in a shell profile or to let the agent run it.

## What the agent gets

Six tools:

| Tool | Purpose |
|---|---|
| `get_mesh_info` | Router, connected peers, DHT size, the local socket path. A quick health check. |
| `list_local_services` | The services this node publishes. |
| `discover_remote_services` | The services other nodes publish, by type (`mcp`, `inference`, `a2a`) and optionally by name. Returns the hosting peer ID for each. |
| `find_remote_tools` | The tools behind MCP services, across the mesh or limited to a peer, a service, or a tool name. |
| `describe_remote_tool` | The input schema of one tool on one peer. Call this before `call_remote_tool`, because the provider chooses the argument names. |
| `call_remote_tool` | Run a tool on a peer. Takes `peer_id`, the namespaced `tool_name` (`mcp://service/tool`) and `arguments`. |

Inference is not an MCP tool. `discover_remote_services` with
`{"type":"inference"}` lists providers, but to use a model you send an
ordinary OpenAI request to the node's `/v1` endpoint, with `base_url`
`http://127.0.0.1:8080/v1` and the token as the API key. `/v1/models` lists
the available models.

A2A agents are not MCP tools either. `discover_remote_services` with
`{"type":"a2a"}` lists them, and a standard A2A client, or the
`sam-a2a-bridge` MCP server, talks to them. See
[Calling A2A agents](#calling-a2a-agents) below.

Two things an agent should know (the skill below tells it): service names
are not unique across the mesh, so the peer ID identifies a provider, and
discovery is best-effort per peer, so on a partly reachable mesh some entries
come back with an `error` field next to the good ones.

## The skill

Tools tell an agent what it can do. A skill tells it when and how to use
them. `sam-node` ships one:

```bash
sam-node skill install            # ~/.claude/skills/sam-mesh/ and ~/.gemini/config/skills/sam-mesh/
sam-node skill install --project  # ./.claude/skills/ and ./.agents/skills/ in the current repository
sam-node skill install --dir DIR  # anywhere else
sam-node skill list               # where it is installed and whether it is current
sam-node skill show               # print it, for a harness with its own layout
```

Reinstall the skill after you upgrade `sam-node`. `list` says `outdated` when
the installed copy differs from the one in the binary. Restart the client
afterwards so that it loads both the skill and the tools.

## Claude Code

```bash
claude mcp add --transport http sam-mesh http://127.0.0.1:8080/mcp \
  --header "X-Sam-Authentication: Bearer <token>"
```

The default scope is the current project. `--scope user` makes the server
available everywhere, and `--scope project` writes a shareable `.mcp.json`.
The equivalent file entry, where `type` is required:

```json
{
  "mcpServers": {
    "sam-mesh": {
      "type": "http",
      "url": "http://127.0.0.1:8080/mcp",
      "headers": { "X-Sam-Authentication": "Bearer <token>" }
    }
  }
}
```

`claude mcp list` should report the server as connected. Tools load at
session start. `/mcp` inside a session shows them, and `/skills` shows
`sam-mesh` once the skill is installed.

## Google Antigravity

Add to `~/.gemini/config/mcp_config.json`:

```json
{
  "mcpServers": {
    "sam-mesh": {
      "serverUrl": "http://127.0.0.1:8080/mcp",
      "headers": { "X-Sam-Authentication": "Bearer <token>" }
    }
  }
}
```

Antigravity picks up the change without a restart. `sam-node skill install`
writes the skill to Antigravity's global skills directory. `--project` writes
it to `.agents/skills/` in the workspace instead.

## VS Code and GitHub Copilot

Create `.vscode/mcp.json` in the workspace. VS Code asks for the token once
and keeps it in its secret store, so the file is safe to commit:

```json
{
  "inputs": [
    { "type": "promptString", "id": "sam-api-token",
      "description": "SAM node API token", "password": true }
  ],
  "servers": {
    "sam-mesh": {
      "type": "http",
      "url": "http://127.0.0.1:8080/mcp",
      "headers": { "X-Sam-Authentication": "Bearer ${input:sam-api-token}" }
    }
  }
}
```

VS Code does not start a newly declared server on its own. Open the file and
use the **Start** action shown above the entry, or run **MCP: List Servers**
from the command palette. A server listed as stopped was read correctly and
is waiting to be started. If you entered a wrong token, clear it with
**MCP: Reset Cached Tokens**. New tools may not appear in a chat that is
already in progress. Start a new chat and check that `sam-mesh` is ticked in
the tools picker. To use the server in all workspaces, put the same JSON in
the user-level configuration (**MCP: Open User Configuration**). Copilot
reads `~/.claude/skills/`, so the default skill install applies.

## Claude Desktop

Claude Desktop launches stdio servers only, so it needs a bridge.
[`mcp-remote`](https://www.npmjs.com/package/mcp-remote) is a small
stdio-to-HTTP proxy that Claude Desktop can launch. Add it to
`claude_desktop_config.json` (`~/Library/Application Support/Claude/` on
macOS, `%APPDATA%\Claude\` on Windows):

```json
{
  "mcpServers": {
    "sam-mesh": {
      "command": "npx",
      "args": [
        "mcp-remote", "http://127.0.0.1:8080/mcp", "--allow-http",
        "--header", "X-Sam-Authentication: Bearer <token>"
      ]
    }
  }
}
```

`--allow-http` is needed because the node serves plain HTTP on loopback. Quit
and reopen Claude Desktop. The tools appear under the connectors icon. On
Windows the command may need to be `npx.cmd`. Claude's cloud-hosted "custom
connectors" cannot reach a node on your machine, so use the bridge.

## OpenClaw

```bash
openclaw mcp set sam-mesh '{
  "url": "http://127.0.0.1:8080/mcp",
  "headers": { "X-Sam-Authentication": "Bearer <token>" }
}'
openclaw mcp list
```

Restart the OpenClaw gateway after adding it.

## Your own code

Any MCP SDK works. In Python, with the official `mcp` package (2.x):

```python
import httpx
from mcp import ClientSession
from mcp.client.streamable_http import streamable_http_client

http = httpx.AsyncClient(headers={"X-Sam-Authentication": "Bearer <token>"})
async with streamable_http_client("http://127.0.0.1:8080/mcp", http_client=http) as (read, write):
    async with ClientSession(read, write) as session:
        await session.initialize()
        tools = await session.list_tools()
```

The repository's `sam-mcp-python` package wraps this in a `SamClient`.
`sam-mcp-python/examples/gemini_agent.py` is a complete agent that maps the
node's tools to Gemini function calls with the `google-genai` SDK. For
inference, point any OpenAI SDK at `http://127.0.0.1:8080/v1` with the token
as `api_key`, or at the socket with no key.

## Calling A2A agents

An agent published as `type: a2a` on another node is reachable at
`http://127.0.0.1:8080/sam/<peer-id>/a2a/<name>/` on your node. Get the
peer ID and the name from `discover_remote_services` with
`{"type":"a2a"}`. Your node rewrites the agent card so that a standard A2A
client keeps talking through the node ([Networking](../../concepts/networking/#a2a-agents)
explains why). There are two ways to call one.

### With an A2A SDK

Point any A2A client at that URL and send the node's token in
`X-Sam-Authentication`. With the Python `a2a-sdk`:

```python
import httpx
from a2a.client import A2ACardResolver, ClientConfig, create_client

url = "http://127.0.0.1:8080/sam/<peer-id>/a2a/<name>"
async with httpx.AsyncClient(headers={"X-Sam-Authentication": "Bearer <token>"}) as http:
    card = await A2ACardResolver(http, url).get_agent_card()
    client = await create_client(card, client_config=ClientConfig(httpx_client=http))
    # client.send_message(...) as against any A2A server
```

The card that comes back points at the same mesh URL, so every message goes
through your node. `contextId` and `taskId` work as they do against a local
agent. The [A2A chat use case](../../use-cases/chat-a2a/) is a complete
example.

### With the `sam-a2a-bridge` MCP server

A harness that only speaks MCP cannot use an A2A SDK. `sam-a2a-bridge` is a
small stdio MCP server that talks to your node and exposes A2A as three
tools:

| Tool | Purpose |
|---|---|
| `get_agent_card(peer, service)` | The agent's skills, accepted input and output MIME types, and whether it streams. Call it before sending structured data or files. |
| `send_agent_task(peer, service, message?, data?, file_path?, ...)` | Send text, a JSON object, or a file (up to 5 MB) to the agent. Returns at once with `task_id`, `context_id`, `state` and the agent's reply so far. Pass `context_id` back to continue a conversation, and `required_labels` (`key=value,...`) to require attested labels on the provider. |
| `get_agent_task(peer, service, task_id)` | Poll a task that has not reached a terminal state. Files that the agent returns are saved under the download directory. |

`make build` builds it to `bin/sam-a2a-bridge` (it is its own Go module, so
the A2A SDK stays out of the root dependency graph). Register it like any
stdio MCP server, with the node's URL and token:

```bash
claude mcp add sam-a2a-bridge -- ./bin/sam-a2a-bridge -url http://127.0.0.1:8080 -token <token>
```

| Flag | Meaning |
|---|---|
| `-url` | The node's API URL. Default `http://localhost:8080`. |
| `-token` | The node's API token. |
| `-download-dir` | Where files returned by agents are written. Default `~/.sam/a2a-downloads`, created if missing. A directory you pass yourself must already exist. |

The repository ships a skill for it in `agents/skills/sam-a2a-bridge/`, in
the same format as the mesh skill above. A refusal with
`403: Required labels not attested by provider` means the label gate in your
node stopped the call before any data left it. That is the intended result
when the provider does not match, and the agent should report it instead of
retrying with weaker labels.

## A note on trust

A client configured this way is a client of your node. It holds the node's
token and gets everything that the node's credential grants. That is the
right arrangement for an assistant on your own machine. Code that you do not
trust should not be able to reach the node's API at all. The
[sandboxed agents preview](../../preview/sandboxed-agents/) covers that case.

## When it does not work

- **401**: the header value does not match the node's token. Over the
  socket no token is needed. Over TCP it is required.
- **Connection refused**: the node is not running, or it is bound to another
  address. If
  `curl -s -o /dev/null -w '%{http_code}\n' http://127.0.0.1:8080/mcp`
  answers `401`, the endpoint is up.
- **Node in WSL or in a container**: the client runs on the host, so bind
  the node to an address the host can reach (`--bind-addr 0.0.0.0:8080`) or
  forward the port.
- **No remote tools**: check `get_mesh_info` first. An empty
  `connected_peers` means the node has not reached a router. A `dht_size` of
  zero means it has not found the mesh yet.
- **A tool is listed with an `error`**: the provider's backend is not
  answering. Another peer that offers the same service name may work.
