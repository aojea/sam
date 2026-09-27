"""An agent in a sandbox, on the mesh.

The sandbox has no route into the office. This program has one identity on
the mesh and reaches, by name, what the mesh policy grants that identity: a
model, an MCP server and an external API whose credential it never holds.

    python agent.py               every step below, in order
    python agent.py models        list the office model
    python agent.py ask "..."     one chat completion
    python agent.py tool          call an MCP tool
    python agent.py github GET /repos/google/sam/pulls?state=open&per_page=1
    python agent.py github POST /repos/google/sam/pulls
    python agent.py github GET /user

SAM_CONTROL_PLANE_URL names the mesh. The first run enrolls with the token in
SAM_BOOTSTRAP_TOKEN_PATH and keeps identity and credential in SAM_STATE_DIR.
"""

import json
import logging
import os
import sys

import trio
from agent_mesh import AgentMesh

LLM = "inference://office-llm"
TOOLS = "mcp://tools"
GITHUB = "egress://api.github.com"

# py-libp2p narrates every failed dial over several lines; the verdicts this
# program prints are the story. SAM_DEBUG=1 brings the narration back.
if not os.environ.get("SAM_DEBUG"):
    logging.getLogger("libp2p").setLevel(logging.CRITICAL)

mesh = AgentMesh.enroll(
    os.environ.get("SAM_CONTROL_PLANE_URL", "https://mesh.example.com"),
    bootstrap_token_path=os.environ.get("SAM_BOOTSTRAP_TOKEN_PATH"),
    # The role the token was minted for; the policy's grants hang off it.
    role=os.environ.get("SAM_ROLE", "agent"),
    state_dir=os.environ.get("SAM_STATE_DIR", "~/.config/sam-mesh/sandboxed-agent"),
    allow_insecure=os.environ.get("SAM_INSECURE_CONTROL_PLANE") == "true",
)


def say(line: str) -> None:
    print(line, flush=True)


_providers = {}


async def reach(session, service):
    """The first provider of service that answers, remembered for this run."""
    if service in _providers:
        return _providers[service]
    providers = await session.discover(service)
    if not providers:
        raise SystemExit(f"nobody on the mesh serves {service}")
    for provider in providers:
        try:
            await session.connect(provider)
            _providers[service] = provider
            return provider
        except (ConnectionError, PermissionError) as err:
            # A provider record can outlive its member; say so in one line.
            print(f"{provider.peer_id[:16]}… not reachable: {str(err).splitlines()[0][:80]}", file=sys.stderr)
    raise SystemExit(f"no provider of {service} is reachable")


async def models(session):
    pep = await reach(session, LLM)
    say(f"→ GET {LLM} /v1/models")
    resp = await session.request(pep, LLM, "/v1/models")
    ids = [m["id"] for m in resp.json().get("data", [])]
    say(f"← {resp.status}  {', '.join(ids)}  (served by {pep.peer_id[:16]}…, in the office)")
    return ids


async def ask(session, question):
    pep = await reach(session, LLM)
    ids = (await session.request(pep, LLM, "/v1/models")).json().get("data", [])
    model = os.environ.get("MODEL") or (ids[0]["id"] if ids else "gemma3:1b")
    say(f"→ POST {LLM} /v1/chat/completions  model={model}")
    say(f"   {question}")
    resp = await session.request(
        pep, LLM, "/v1/chat/completions",
        method="POST",
        headers={"Content-Type": "application/json"},
        body=json.dumps({"model": model, "messages": [{"role": "user", "content": question}]}),
    )
    if resp.status != 200:
        say(f"← {resp.status} {resp.text[:200]}")
        return
    say(f"← {resp.status}  {resp.json()['choices'][0]['message']['content'].strip()}")


async def tool(session):
    pep = await reach(session, TOOLS)
    tools = await session.list_tools(pep, TOOLS)
    say(f"→ {TOOLS}  {len(tools)} tools: {', '.join(t.name for t in tools[:4])}, …")
    say("→ call get-sum(a=2, b=3)")
    result = await session.call_tool(pep, TOOLS, "get-sum", {"a": 2, "b": 3})
    say(f"← {' '.join(result.text)}")


async def github(session, method, path):
    pep = await reach(session, GITHUB)
    say(f"→ {method} {GITHUB} {path}")
    resp = await session.request(
        pep, GITHUB, path,
        method=method,
        # GitHub refuses requests without a User-Agent; the node forwards it.
        headers={"Accept": "application/vnd.github+json", "User-Agent": "sam-sandboxed-agent"},
        body=b"{}" if method in ("POST", "PUT", "PATCH") else None,
    )
    if resp.status == 200:
        data = resp.json()
        if isinstance(data, list) and data and "number" in data[0]:
            say(f"← {resp.status}  #{data[0]['number']} {data[0]['title']}")
        else:
            say(f"← {resp.status}  {resp.text[:120]}")
        return
    verdict = resp.headers.get("proxy-status") or resp.headers.get("Proxy-Status") or ""
    say(f"← {resp.status}  {verdict or resp.text[:120]}")


async def main(argv):
    try:
        session_cm = mesh.join()
        session = await session_cm.__aenter__()
    except RuntimeError as err:
        # Every router refused this identity: the admin cut it off.
        raise SystemExit(f"cut off from the mesh: {str(err).splitlines()[0].rstrip(':')}")
    try:
        say(f"on the mesh as {session.peer_id}")
        step = argv[0] if argv else "all"
        if step == "models":
            await models(session)
        elif step == "ask":
            await ask(session, " ".join(argv[1:]) or "In one sentence, in English: why should an agent never hold API credentials?")
        elif step == "tool":
            await tool(session)
        elif step == "github":
            await github(session, argv[1].upper(), argv[2])
        elif step == "all":
            await models(session)
            await ask(session, "In one sentence, in English: why should an agent never hold API credentials?")
            await tool(session)
            await github(session, "GET", "/repos/google/sam/pulls?state=open&per_page=1")
            await github(session, "POST", "/repos/google/sam/pulls")
            await github(session, "GET", "/user")
        else:
            raise SystemExit(__doc__)
    finally:
        await session_cm.__aexit__(None, None, None)


trio.run(main, sys.argv[1:])
