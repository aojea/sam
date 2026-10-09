---
title: "Gemini Buddy"
linkTitle: "Gemini Buddy"
weight: 20
---

Hold a real multi-turn conversation with a second LLM exposed as an ordinary
mesh service. Your agent never resends the conversation, because the service
remembers its own side.

Source: [`development/examples/gemini-buddy-mcp/`](https://github.com/google/sam/tree/main/development/examples/gemini-buddy-mcp).

## The idea

Sometimes you want your agent to talk to another model: for a second
opinion, to discuss a design, or to use a specialist that builds up its own
context over many turns. The simple approach is to carry both conversations
in the orchestrator. Your agent would have to store the other model's
transcript and replay all of it on every turn.

This use case shows that you do not have to. A **buddy** is a plain MCP
service backed by the Gemini CLI that owns its own conversation. Your agent
sends one message per turn and gets one reply. The buddy keeps the running
transcript on the server side, keyed by a `session_id`. It is built from an
ordinary `sam-node` MCP service, and it is the conversational version of the
one-shot
[`code-reviewer`](https://github.com/google/sam/tree/main/development/examples/code-reviewer-pool/reviewer)
example.

## Two conversations, two places

This example clears up one common confusion: talking to another LLM does not
mean maintaining two conversations from your side.

- **Your conversation** (you and your agent) lives in your agent's context.
- **The buddy's conversation** lives inside the buddy service process.

When your agent calls the buddy's `chat` tool, it sends only the next
message, never the history. The buddy appends the message to that session's
transcript, replays the whole transcript locally to a one-shot `gemini -p`
call, stores the reply, and returns only that reply. The reply becomes part
of your agent's context like any other tool result, so neither side
maintains the other's transcript.

The buddy service therefore owns the conversation, and the model is a
stateless backend that can be swapped. The same service works for any CLI
(`gemini`, `claude`, `codex`) by changing one `spawn` line.

## The pieces

- **`gemini-buddy` service**: a normal MCP service that exposes two tools.
  `chat` takes `{ message, session_id? }` (the session defaults to
  `"default"`), appends the message to that session's in-memory transcript,
  runs one `gemini -p` turn with the transcript on stdin, and returns the
  buddy's reply. `reset` takes `{ session_id? }` and clears that session, so
  the next `chat` starts fresh.
- **Orchestrator**: any mesh MCP client (an agent harness or a custom
  program). It discovers the buddy with `find_remote_tools` and drives it
  with `call_remote_tool`, one message at a time.

## What you can do with it

- **Second opinion.** Discuss a design or a difficult bug with a different
  model across several turns, without managing its context.
- **A specialist that accumulates context.** Let the buddy build up domain
  knowledge over many messages (walk it through a subsystem, then keep
  asking questions) that your own agent never has to reload.
- **A reusable pattern.** Swap the Gemini CLI for any other model CLI. The
  service structure is the same: own the transcript, and run a stateless
  one-shot call per turn. `reset` gives you clean, isolated sessions on
  demand.

## Try it on kind

The repository ships a [kind](https://kind.sigs.k8s.io/)-based local mesh that
brings the buddy up with one command.

### 1. Set an LLM key for the buddy image

The buddy runs the Gemini CLI, so set your API key on the API-key `ENV` line
in `development/examples/gemini-buddy-mcp/Dockerfile` before building. A
free Google AI Studio key is enough for the demo.

### 2. Bring the mesh up and deploy the buddy

```bash
make build            # builds ./bin/sam-node (once)
make kind-up          # control plane + router (no sam-nodes yet)
docker build -t gemini-buddy-mcp:local development/examples/gemini-buddy-mcp
kind load docker-image --name sam-kind gemini-buddy-mcp:local
helm --kube-context kind-sam-kind -n sam-kind install gemini-buddy charts/sam-node \
  -f development/kind/sam-node.values.yaml \
  -f development/examples/gemini-buddy-mcp/values.yaml
```

### 3. Start a local orchestrator node

```bash
make kind-local-node  # a local sam-node enrolled in the mesh; leave it running
```

`kind-local-node` runs in the foreground in its own shell and exposes the
mesh MCP tools at `http://127.0.0.1:9099/mcp` (token `devtoken`). No
`kubectl port-forward` is needed. This local node is the entry point into
the mesh for your orchestrator.

### 4. Point your harness at the local node

Add the local node as an MCP server in the harness you use to drive the
mesh. The details differ per harness (some use a JSON or TOML config file,
others a UI), but the settings are always the same:

- **Transport:** HTTP (Streamable HTTP / `http`)
- **URL:** `http://127.0.0.1:9099/mcp`
- **Header:** `X-Mesh-Authentication: Bearer devtoken`

For example, harnesses that use the common `mcpServers` JSON config (Claude
Code, Cursor, and others) would add:

```json
{
  "mcpServers": {
    "sam-mesh": {
      "type": "http",
      "url": "http://127.0.0.1:9099/mcp",
      "headers": { "X-Mesh-Authentication": "Bearer devtoken" }
    }
  }
}
```

Check the MCP documentation of your harness for its exact config format.
Once connected, the mesh exposes `find_remote_tools` and `call_remote_tool`
as tools that your agent (or program) can call.

### 5. Have a conversation

Tell your agent what you want. The mesh tool descriptions and the buddy's
own instructions guide it to discover the service and route each turn, so
you do not have to name any tools. A natural first prompt:

> Find the `gemini-buddy` service in the mesh, then introduce yourself and tell it
> I'm planning a trip to Kyoto in spring and that I'm slightly obsessed with ramen.

Then, in a separate message, check that the buddy remembers without you
resending anything:

> Ask the buddy where I'm headed and what food I'm into. Don't remind it, just ask.

The buddy answers correctly even though your agent never sent the first turn
again. The transcript lives in the service and not in your context. Finally,
clear it:

> Reset the conversation with the buddy, then ask it where I'm going again.

After the reset it no longer knows, because the service dropped that
session's transcript.

Once you are comfortable with it, hand the whole loop to your agent and
watch two models converse:

> Find the `gemini-buddy` service on the mesh, reset its session, then have a
> 10-turn conversation with it, one question per turn, each building on its last
> answer, and show me every question-and-answer pair as you go.

## Configuration

| var | default | used by |
|-----|---------|---------|
| `GEMINI_API_KEY` | *(placeholder, set in the Dockerfile)* | buddy |
| `GEMINI_CLI_TRUST_WORKSPACE` | `true` | buddy |

The listen port (`7780`) and model (`gemini-3.1-flash-lite`) are constants at
the top of `buddy_server.mjs`. Change them there if needed.
