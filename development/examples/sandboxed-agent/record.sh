#!/bin/bash
# Copyright 2026 Google LLC
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

# Records SCRIPT.md. A tmux window holds the office (left: mesh, admin,
# node), the sandbox (right: a shell in a codespace) and a caption line
# (bottom). The scenes are typed into the panes at a human pace and paced
# on what appears in them, so every take is the same. asciinema captures
# the window; agg and ffmpeg render it.
#
#   CODESPACE=<name> ./record.sh
#
# Needs: make ollama and make mcp already running, ./bin built, the
# GitHub token at $GITHUB_TOKEN_FILE, and the codespace prepared as
# SCRIPT.md describes (SDK in ~/venv, agent.py and agent-token in ~/sandbox).

set -o errexit
set -o nounset
set -o pipefail

CODESPACE=${CODESPACE:?set CODESPACE to the codespace name (gh codespace list)}
HERE=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
OUT=$HERE/out
SESSION=samdemo
COLS=${COLS:-190}
ROWS=${ROWS:-54}
CAPTION_FILE=/tmp/samdemo-caption.txt
SRT=$OUT/demo.srt
GITHUB_TOKEN_FILE=${GITHUB_TOKEN_FILE:-$HOME/.config/sam-demo/github-ro}
DEMO_DIR=${DEMO_DIR:-$HOME/sam-demo}

mkdir -p "$OUT"
rm -f "$SRT"
: > "$CAPTION_FILE"

# --- helpers ---------------------------------------------------------------

# type_in <pane> <text>: types text like a person and presses Enter.
type_in() {
  local pane=$1 text=$2 i c
  for ((i = 0; i < ${#text}; i++)); do
    c=${text:i:1}
    tmux send-keys -t "$pane" -l -- "$c"
    sleep "0.0$((RANDOM % 5 + 2))"
  done
  sleep 0.3
  tmux send-keys -t "$pane" Enter
  # Let the shell echo the newline before the next keystroke arrives.
  sleep 0.8
}

# wait_for <pane> <regex> [seconds]: waits until the pane shows regex.
wait_for() {
  local pane=$1 re=$2 timeout=${3:-60} i
  for ((i = 0; i < timeout * 2; i++)); do
    if tmux capture-pane -p -t "$pane" -S -300 | grep -qE -- "$re"; then
      return 0
    fi
    sleep 0.5
  done
  echo "timed out waiting for /$re/ in $pane" >&2
  tmux capture-pane -p -t "$pane" -S -40 >&2
  return 1
}

# grab <pane> <regex>: the first match of regex in the pane.
grab() {
  tmux capture-pane -p -t "$1" -S -300 | grep -oE -- "$2" | head -1
}

# count <pane> <regex>: how many lines of the pane match regex.
count() {
  tmux capture-pane -p -t "$1" -S -300 | grep -cE -- "$2" || true
}

# wait_prompt <pane> [seconds]: waits until the pane's last line is a prompt.
wait_prompt() {
  local pane=$1 timeout=${2:-30} i
  for ((i = 0; i < timeout * 4; i++)); do
    if tmux capture-pane -p -t "$pane" | sed -e :a -e '/^\s*$/{$d;N;ba' -e '}' | tail -1 | grep -qE '\$ ?$'; then
      return 0
    fi
    sleep 0.25
  done
  return 0
}

# expect <pane> <command> <regex> [seconds]: types command, then waits for a
# line matching regex that was not on screen before, and for the prompt.
expect() {
  local pane=$1 cmd=$2 re=$3 timeout=${4:-60} before i
  before=$(count "$pane" "$re")
  type_in "$pane" "$cmd"
  for ((i = 0; i < timeout * 2; i++)); do
    if (( $(count "$pane" "$re") > before )); then
      wait_prompt "$pane"
      return 0
    fi
    sleep 0.5
  done
  echo "timed out waiting for new /$re/ in $pane after: $cmd" >&2
  tmux capture-pane -p -t "$pane" -S -40 >&2
  return 1
}

REC_START=
caption_n=0
caption_prev_at=
caption_prev=
# caption <text>: shows text in the caption pane and records it for the srt.
caption() {
  local now
  now=$(date +%s.%N)
  if [[ -n "$REC_START" && -n "$caption_prev" ]]; then
    srt_entry "$caption_prev_at" "$now" "$caption_prev"
  fi
  printf '%s\n' "$1" > "$CAPTION_FILE"
  caption_prev=$1
  caption_prev_at=$now
}

srt_time() { # seconds since REC_START -> HH:MM:SS,mmm
  python3 -c 'import sys; t=float(sys.argv[1])-float(sys.argv[2]); t=max(t,0); h=int(t//3600); m=int(t%3600//60); s=t%60; print(f"{h:02d}:{m:02d}:{int(s):02d},{int((s-int(s))*1000):03d}")' "$1" "$REC_START"
}

srt_entry() {
  caption_n=$((caption_n + 1))
  {
    echo "$caption_n"
    echo "$(srt_time "$1") --> $(srt_time "$2")"
    echo "$3"
    echo
  } >> "$SRT"
}

pause() { sleep "${1:-2}"; }

cleanup() {
  tmux kill-session -t "$SESSION" 2>/dev/null || true
  tmux kill-session -t "${SESSION}-rec" 2>/dev/null || true
}
trap cleanup EXIT

# --- the window -------------------------------------------------------------

cleanup
tmux new-session -d -s "$SESSION" -x "$COLS" -y "$ROWS" -c "$HERE"
tmux set -t "$SESSION" -g pane-border-status top
tmux set -t "$SESSION" -g pane-border-format ' #{pane_title} '
tmux set -t "$SESSION" -g status off
MESH=$(tmux display -p -t "$SESSION:0.0" '#{pane_id}')
CAPTION=$(tmux split-window -P -F '#{pane_id}' -v -l 4 -t "$MESH" -c "$HERE")
SANDBOX=$(tmux split-window -P -F '#{pane_id}' -h -l 50% -t "$MESH" -c "$HERE")
NODE=$(tmux split-window -P -F '#{pane_id}' -v -l 20 -t "$MESH" -c "$HERE")
ADMIN=$(tmux split-window -P -F '#{pane_id}' -v -l 12 -t "$MESH" -c "$HERE")
tmux select-pane -t "$MESH" -T 'office · mesh (sam-one)'
tmux select-pane -t "$ADMIN" -T 'office · admin'
tmux select-pane -t "$NODE" -T 'office · node (what it serves, every decision)'
tmux select-pane -t "$SANDBOX" -T 'developer sandbox · a codespace, no VPN'
tmux select-pane -t "$CAPTION" -T ''

# The caption pane reprints its file whenever it changes.
cat > /tmp/samdemo-caption.py <<'EOF'
import os, sys, textwrap, time
path, cols = sys.argv[1], int(sys.argv[2])
last = None
while True:
    try:
        text = open(path).read().strip()
    except FileNotFoundError:
        text = ""
    if text != last:
        sys.stdout.write("\033[2J\033[H")
        for line in textwrap.wrap(text, cols - 4)[:3]:
            sys.stdout.write("  \033[1m" + line + "\033[0m\n")
        sys.stdout.flush()
        last = text
    time.sleep(0.2)
EOF
tmux send-keys -t "$CAPTION" "python3 /tmp/samdemo-caption.py $CAPTION_FILE $COLS" Enter

# Quiet prompts. The admin token comes from the environment so the banner
# names its source instead of showing it.
ADMIN_TOKEN=$(openssl rand -hex 16)
for p in "$MESH" "$ADMIN" "$NODE"; do
  tmux send-keys -t "$p" "export SAM_ADMIN_TOKEN=$ADMIN_TOKEN GITHUB_TOKEN_FILE=$GITHUB_TOKEN_FILE DEMO_DIR=$DEMO_DIR AUDIT_RAW=$OUT/node.log PS1='office\$ '; clear" Enter
done
rm -f "$OUT/node.log"
tmux send-keys -t "$SANDBOX" "gh codespace ssh -c $CODESPACE" Enter
wait_for "$SANDBOX" '\$\s*$' 120
tmux send-keys -t "$SANDBOX" "PS1='sandbox\$ '; cd ~/sandbox && source ~/venv/bin/activate && rm -rf ~/.config/sam-mesh/sandboxed-agent ~/sandbox/state && clear" Enter
wait_for "$SANDBOX" 'sandbox\$' 30
# A fresh mesh every take; the downloaded cloudflared is kept.
rm -rf "$DEMO_DIR/pep"
if [[ -d "$DEMO_DIR/one" ]]; then
  find "$DEMO_DIR/one" -mindepth 1 -maxdepth 1 ! -name bin -exec rm -rf {} +
fi

# --- record -----------------------------------------------------------------

# The recorder runs in its own tmux session so its pty has the window's size.
tmux new-session -d -s "${SESSION}-rec" -x "$COLS" -y "$ROWS" \
  "asciinema rec -q --overwrite --cols $COLS --rows $ROWS -c 'tmux attach -t $SESSION' '$OUT/demo.cast'"
sleep 2
REC_START=$(date +%s.%N)

caption "An agent decides at run time which API it calls, and with what. You cannot review that in a pull request. Security wants it sandboxed; the developer wants the model, the tools and the internal API it needs."
pause 7
caption "The usual bridge is a VPN into the office: slow to develop against, and it turns the sandbox into a door with credentials inside."
pause 6

# 1. Two machines
caption "Left: the office. Right: a developer sandbox somewhere else, here a GitHub codespace. It has no route into the office."
expect "$SANDBOX" "curl -m 3 http://office-llm.corp.internal:11434/v1/models" 'Could not resolve host|Connection timed out' 10
pause 3

# 2. The mesh and the policy
caption "The admin starts a mesh: a control plane, a router and a console, on a public https URL, with the policy in policy.json. No standing join token."
expect "$MESH" "make mesh" 'SAM standalone mesh is ready' 120
URL=$(grab "$MESH" 'https://[a-z0-9-]+\.trycloudflare\.com')
# A quick tunnel is routed a few seconds after cloudflared prints it, and
# once in a while it dies at once; either way, do not type into a dead URL.
for i in $(seq 1 30); do
  [[ $(curl -s -o /dev/null -w '%{http_code}' "$URL/healthz") == 200 ]] && break
  sleep 2
  if (( i == 30 )); then echo "the tunnel at $URL never answered; abort this take" >&2; exit 1; fi
done
pause 3

caption "One token per member, minted for its role. The office node gets sam:role:node and the label site=office; the agent gets the role agent."
type_in "$ADMIN" "export URL=$URL"
expect "$ADMIN" "make tokens URL=\$URL" 'tokens in' 30
# The developer receives the token out of band; here, over ssh.
gh codespace ssh -c "$CODESPACE" -- 'cat > ~/sandbox/agent-token; chmod 600 ~/sandbox/agent-token' < "$DEMO_DIR/agent-token" 2>/dev/null
pause 2

caption "The policy: the agent may call the model, the MCP server and api.github.com by name, and api.github.com only with GET under /repos/google/sam/. The destination is served by nodes labelled site=office."
type_in "$ADMIN" "jq -c '.roles[1].allowed_services, .roles[1].http[0], .egress[0]' policy.json"
pause 8

caption "The office node fronts a model and an MCP server on loopback. api.github.com is assigned to it by the policy; its read-only token is a file on this machine, read by the node, never by an agent."
type_in "$NODE" "export URL=$URL"
expect "$NODE" "make pep URL=\$URL 2>&1 | python3 audit.py" 'SAM Node Online' 60
pause 4

# 3. The developer's program joins
caption "The agent is an ordinary Python program with the SDK. The admin handed the developer the single-use token out of band. It enrolls once and gets an identity: no sidecar, no proxy variables, no VPN."
type_in "$SANDBOX" "export SAM_CONTROL_PLANE_URL=$URL SAM_BOOTSTRAP_TOKEN_PATH=~/sandbox/agent-token"
expect "$SANDBOX" "python agent.py models" '← 200  gemma3' 90
PEER=$(grab "$SANDBOX" 'on the mesh as 12D3KooW[A-Za-z0-9]+' | awk '{print $NF}')
pause 3

# 4. A model, a tool
caption "A chat completion runs on the office workstation. The answer is different every take; that is the point."
expect "$SANDBOX" "python agent.py ask" '← 200  ' 90
pause 5
caption "A tool call reaches the MCP server in the office. On the left, every decision is one line in the node's log."
expect "$SANDBOX" "python agent.py tool" 'sum of 2 and 3' 60
pause 4

# 5. External API
caption "An external API. GitHub answers 200: the node presented the office's token. The request the sandbox sent had none."
expect "$SANDBOX" "python agent.py github GET '/repos/google/sam/pulls?state=open&per_page=1'" '← 200  #' 60
pause 4
caption "The agent may try anything. POST is outside the grant; /user is outside the grant. The network answers 403 before GitHub hears of it."
expect "$SANDBOX" "python agent.py github POST /repos/google/sam/pulls" 'http_request_denied' 60
expect "$SANDBOX" "python agent.py github GET /user" 'http_request_denied' 60
wait_for "$NODE" 'DENY.*GET /user' 30
pause 5

# 6. Revoke
caption "The admin takes api.github.com off the mesh: one policy change. The node withdraws it within seconds."
expect "$ADMIN" "make revoke URL=\$URL" 'success' 30
wait_for "$NODE" 'Withdrawn egress://api.github.com' 60
pause 2
caption "The same request finds no service. Nothing to revoke in the sandbox: it never had anything."
expect "$SANDBOX" "python agent.py github GET '/repos/google/sam/pulls?state=open&per_page=1'" '← 404' 60
pause 4

# 7. Ban
caption "And when the admin decides this agent is done: the identity is banned, and no router admits it again."
expect "$ADMIN" "make ban URL=\$URL PEER=$PEER" 'banned' 30
wait_for "$NODE" 'BANNED' 30
expect "$SANDBOX" "python agent.py github GET /user" 'cut off from the mesh' 90
pause 4

# 8. Close
caption "The developer used their own sandbox and wrote a plain program. The admin wrote one policy document and read one log. The credential never left the office."
pause 8
caption "sam-mesh.dev"
pause 4

# Ends the recording: the attached client exits with the session.
END=$(date +%s.%N)
srt_entry "$caption_prev_at" "$END" "$caption_prev"
tmux kill-session -t "$SESSION"
for _ in $(seq 1 30); do tmux has-session -t "${SESSION}-rec" 2>/dev/null || break; sleep 1; done

# --- render -------------------------------------------------------------------

# The teardown clears the screen; the recording ends on the closing caption.
python3 - "$OUT/demo.cast" "$END" <<'EOF'
import json, sys
path, end = sys.argv[1], float(sys.argv[2])
lines = open(path).read().splitlines()
cutoff = end - json.loads(lines[0])["timestamp"] - 0.5
kept = [lines[0]] + [l for l in lines[1:] if json.loads(l)[0] <= cutoff]
open(path, "w").write("\n".join(kept) + "\n")
EOF

agg --cols "$COLS" --rows "$ROWS" --font-size 14 --theme monokai --idle-time-limit 30 --last-frame-duration 3 "$OUT/demo.cast" "$OUT/demo.gif"
ffmpeg -y -loglevel error -i "$OUT/demo.gif" -movflags faststart -pix_fmt yuv420p \
  -vf 'scale=trunc(iw/2)*2:trunc(ih/2)*2' "$OUT/demo.mp4"
echo "recorded: $OUT/demo.cast $OUT/demo.gif $OUT/demo.mp4 $SRT"
