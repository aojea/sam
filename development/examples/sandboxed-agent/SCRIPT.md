# An agent in a sandbox, a network the admin controls

A three to four minute recording. Two machines: the admin's workstation
("the office", left) and a developer sandbox with no route into the office
(a GitHub codespace, right). A caption line at the bottom carries the
narration below, so the recording explains itself and every command is on
screen. `record.sh` produces it; the sections below are what it types and
what it says.

## Before recording

On the workstation:

```bash
make build                                   # ./bin/sam-one and ./bin/sam-node
cd development/examples/sandboxed-agent
make ollama                                  # the office model, in Docker
make mcp &                                   # the MCP reference server on :3001
export GITHUB_TOKEN_FILE=~/.config/sam-demo/github-ro   # read-only, google/sam pull requests
```

The sandbox: a codespace on `google/sam` (any configuration with Python),
with the SDK installed and `agent.py` in `~/sandbox`:

```bash
python3 -m venv ~/venv && ~/venv/bin/pip install ./sdk/python
mkdir ~/sandbox && cp development/examples/sandboxed-agent/agent.py ~/sandbox/
```

The developer receives the single-use token the admin mints in scene 2 out
of band, as `~/sandbox/agent-token`. The recording does not show a token.

## Scenes

Narration is what the caption pane shows while the commands run.

### 0. The problem

> An agent decides at run time which API it calls and with what. You cannot
> review that in a pull request. Security wants it sandboxed. The developer
> wants the model, the tools and the internal API it needs. A VPN into the
> office is the usual bridge: slow to develop against, and it turns the
> sandbox into a door with credentials inside.

### 1. Two machines

Left, the office. Right, a sandbox somewhere else: a codespace on GitHub.

```bash
# sandbox
curl -m 3 http://office-llm.corp.internal:11434/v1/models
```

> The sandbox has no route into the office. It has a route to one URL, and
> the mesh decides what is behind it.

### 2. The admin starts a mesh and writes one policy

```bash
# office
make mesh
```

> One command: a control plane, a router and a console, on a public https
> URL, with the policy in `policy.json`. No standing join token: every
> member gets a token minted for its role.

```bash
# office
export URL=https://<from the banner>
make tokens URL=$URL
make pep URL=$URL 2>&1 | python3 audit.py
```

> The office node fronts three things: a model on loopback, an MCP server
> on loopback, and `api.github.com` with a read-only token in a file on
> this machine. The policy assigns the destination to the node by its
> label, `site=office`. The role `agent` may call the three by name, and
> `api.github.com` only with `GET` under `/repos/google/sam/`.

`policy.json` is on screen here, shortened to the `agent` role and the
`egress` entry.

### 3. The developer's program joins

```bash
# sandbox
export SAM_CONTROL_PLANE_URL=$URL SAM_BOOTSTRAP_TOKEN_PATH=~/sandbox/agent-token
python agent.py models
```

> The agent is an ordinary Python program with the SDK. It enrolls once,
> with a single-use token for the role `agent`, and gets an identity.
> Nothing else runs in the sandbox: no sidecar, no proxy variables, no VPN.
> The model in the office answers by name.

### 4. A model, a tool

```bash
# sandbox
python agent.py ask
python agent.py tool
```

> A chat completion runs on the office workstation. A tool call reaches
> the MCP server there. Every decision is one line in the node's log.

### 5. An external API, with a credential the sandbox never held

```bash
# sandbox
python agent.py github GET '/repos/google/sam/pulls?state=open&per_page=1'
```

> GitHub answers 200. The node presented the office's token; the request
> the sandbox sent had none.

```bash
# sandbox
python agent.py github POST /repos/google/sam/pulls
python agent.py github GET /user
```

> The agent may try anything. `POST` is outside the grant. `/user` is
> outside the grant. The network answers 403 before GitHub hears of it.

### 6. The admin takes the destination off the mesh

```bash
# office
make revoke URL=$URL
# sandbox
python agent.py github GET '/repos/google/sam/pulls?state=open&per_page=1'
```

> One policy change. The node withdraws `api.github.com` within seconds,
> and the same request finds no service. Nothing to revoke in the sandbox:
> it never had anything.

### 7. The admin cuts the agent off

```bash
# office
make ban URL=$URL PEER=<the agent's peer ID>
# sandbox
python agent.py github GET /user
```

> The identity is banned. No router admits it again.

### 8. Close

> The developer used their own sandbox and wrote a plain program. The
> admin wrote one policy document and read one log. The credential never
> left the office.

## Notes for the narrator

- Grants live in the member's credential for its lifetime (24 hours by
  default, `--control-plane-biscuit-ttl`). Removing a service from a role
  takes effect at the next refresh; removing a destination or banning a
  member takes effect within the node's sync jitter, which is why scenes 6
  and 7 use those two.
- The node polls the control plane every 30 seconds in this demo
  (`--control-plane-sync-interval`) and reacts to policy events within a
  tenth of that. The default is 15 minutes.
- The model's answer is different every take. That is the point of scene 0.

## Recording

```bash
CODESPACE=<name> GITHUB_TOKEN_FILE=~/github-ro ./record.sh   # tmux, asciinema, then agg and ffmpeg
```

The captions are typed into their pane at reading speed (`CAPTION_CPS`,
24 characters per second), and the driver moves on when a caption is fully
shown, so the narration paces the take. Afterwards every silence in the
cast longer than `IDLE_MAX` (1.5 s) is cut to that length: a join, a
tunnel coming up, a timeout. Typing is untouched. The video is rendered at
`SPEED` (1.25) and the subtitles are remapped through both, so they stay
in sync.

Outputs `demo.cast`, `demo.gif`, `demo.mp4` and `demo.srt` (the narration
with timestamps, for a voice-over or subtitles) in `out/`, which git
ignores. The published copy is `site/static/demo-sandboxed-agent.mp4`.
