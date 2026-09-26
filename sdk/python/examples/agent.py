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
