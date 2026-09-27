---
title: "Sandboxed Agent"
linkTitle: "Sandboxed Agent"
weight: 5
---

An agent runs in a sandbox with no route into the office: here, a GitHub
codespace. Through the mesh it reaches a model and an MCP server that listen
on loopback in the office, and `api.github.com` with a credential it never
holds. The admin decides what it may call, down to the HTTP method and path,
watches every decision, and cuts it off when done.

<video autoplay loop muted playsinline controls style="width: 100%; border-radius: 8px;">
  <source src="../../../demo-sandboxed-agent.mp4" type="video/mp4">
</video>

Source: [`development/examples/sandboxed-agent/`](https://github.com/google/sam/tree/main/development/examples/sandboxed-agent).
The scenes, the narration and the recording recipe are in its
[`SCRIPT.md`](https://github.com/google/sam/blob/main/development/examples/sandboxed-agent/SCRIPT.md).

## The idea

An agentic application is not deterministic: the code decides at run time
which API it calls and with what, so its network behaviour cannot be
reviewed in a pull request. The usual way to give it the model, the tools
and the internal API it needs is a VPN into the corporate network, which is
slow to develop against and turns the sandbox into a door with credentials
inside.

The mesh replaces the VPN with one identity and one policy. The agent is an
ordinary program written with the [Python SDK](../../guides/native-sdks/);
the sandbox needs outbound HTTPS to one URL and nothing else, so any sandbox
works: a container, a microVM, a codespace. What the agent can reach is
decided by name on the admin's side, and the credential for the external
API stays on a node in the office.

## The pieces

**One policy** ([`policy.json`](https://github.com/google/sam/blob/main/development/examples/sandboxed-agent/policy.json)).
The role `agent` may call `inference://office-llm`, `mcp://tools` and
`egress://api.github.com`, the last one only with `GET` under
`/repos/google/sam/`. The destination `api.github.com` is served by nodes
labelled `site=office` with the credential named `github-ro`.

**One node in the office** ([`pep.yaml`](https://github.com/google/sam/blob/main/development/examples/sandboxed-agent/pep.yaml)).
A `sam-node` with the label `site=office` fronts Ollama and an MCP server on
loopback. The control plane assigns `api.github.com` to it because of the
label; the node reads the token from `<--secrets-dir>/github-ro` on every
request and presents it to GitHub. See
[egress destinations](../../guides/egress-destinations/).

**One program in the sandbox** ([`agent.py`](https://github.com/google/sam/blob/main/development/examples/sandboxed-agent/agent.py)).
It enrolls once with a single-use token minted for the role `agent`, then
discovers each service by name and calls it: `/v1/models` and a chat
completion, an MCP tool, and three requests to GitHub of which the policy
allows one.

## Run it yourself

On the office machine, from `development/examples/sandboxed-agent/`:

```bash
make ollama                      # the office model, in Docker
make mcp                         # the MCP reference server on :3001, in its own terminal
make mesh                        # sam-one on a public https URL; copy the API URL from the banner
make tokens URL=$URL             # one token for the node, one for the agent
GITHUB_TOKEN_FILE=~/github-ro make pep URL=$URL 2>&1 | python3 audit.py
```

`GITHUB_TOKEN_FILE` is a fine-grained token with read access to pull
requests on `google/sam`, and nothing else; the node is the only process
that reads it.

In the sandbox, with the SDK installed (`pip install ./sdk/python`) and the
agent token the admin handed you:

```bash
export SAM_CONTROL_PLANE_URL=$URL SAM_BOOTSTRAP_TOKEN_PATH=./agent-token
python agent.py                  # every step, or one of: models, ask, tool, github GET /user
```

Then, back in the office, `make revoke URL=$URL` takes the destination off
the mesh and `make ban URL=$URL PEER=<peer id>` cuts the agent off.

## What to notice

- The request GitHub receives carries the office's token and none of the
  agent's headers. The sandbox never had a credential to leak.
- `POST /repos/google/sam/pulls` and `GET /user` are answered `403` with
  `Proxy-Status: sam-node; error=http_request_denied` by the node, before
  GitHub hears of them. Each is one `DENY` line in the node's log with the
  peer, the role, the method and the path.
- Removing the `egress` entry from the policy withdraws the destination on
  the node within its sync jitter (the example runs with
  `--control-plane-sync-interval 30s`); the same request then finds no
  service. Banning the peer disconnects it and no router admits it again.
- Grants live in a member's credential until it is refreshed (24 hours by
  default, `--control-plane-biscuit-ttl`). Removing a service from a role
  takes effect at that refresh; withdrawing a destination or banning a
  member takes effect at once, which is why the demo uses those two.
