"""Talks to an agent on the mesh with the A2A SDK's client. The mesh SDK is an
httpx transport; the A2A client fetches the agent card and sends messages as
it would to any A2A server, and every request travels to the peer through a
router with this member's credential.

    python a2a_call.py 12D3KooW... "hello"

SAM_CONTROL_PLANE_URL names the mesh. The first run enrolls with the file
SAM_BOOTSTRAP_TOKEN_PATH (a token the mesh operator gave you) or SAM_JWT_PATH
(a workload identity token your platform issues, such as a Kubernetes
projected service account token), and keeps the identity and credential in
SAM_STATE_DIR; later runs resume from there without it.
"""

import os
import sys

import httpx
import trio
from a2a.client import A2ACardResolver, ClientConfig, create_client
from a2a.helpers import get_message_text, new_text_message
from a2a.types import Role, SendMessageRequest
from agent_mesh import AgentMesh, MeshSession, MeshTransport

if len(sys.argv) < 2:
    raise SystemExit("usage: a2a_call.py <peer-id> [text]")
peer_id = sys.argv[1]
text = sys.argv[2] if len(sys.argv) > 2 else "hello"

mesh = AgentMesh.enroll(
    os.environ.get("SAM_CONTROL_PLANE_URL", "https://mesh.example.com"),
    bootstrap_token_path=os.environ.get("SAM_BOOTSTRAP_TOKEN_PATH"),
    jwt_path=os.environ.get("SAM_JWT_PATH"),
    state_dir=os.environ.get("SAM_STATE_DIR", "~/.config/sam-mesh/a2a-caller"),
    # A plaintext http:// control plane is otherwise accepted only on loopback.
    allow_insecure=os.environ.get("SAM_INSECURE_CONTROL_PLANE") == "true",
)


async def main() -> None:
    async with mesh.join() as session:
        print(f"on the mesh as {session.peer_id}")

        # The agent's URL on the mesh, and an httpx client that carries requests to it.
        agent_url = MeshSession.mesh_url(peer_id, "a2a://agent")
        async with httpx.AsyncClient(transport=MeshTransport(session), timeout=60) as http:
            card = await A2ACardResolver(http, agent_url).get_agent_card()
            print(f"agent: {card.name}, {card.description}")

            client = await create_client(card, ClientConfig(httpx_client=http, streaming=False))
            request = SendMessageRequest(message=new_text_message(text, role=Role.ROLE_USER))
            async for response in client.send_message(request):
                if response.HasField("message"):
                    print(get_message_text(response.message))
                elif response.HasField("task") and response.task.status.HasField("message"):
                    print(get_message_text(response.task.status.message))
            await client.close()


trio.run(main)
