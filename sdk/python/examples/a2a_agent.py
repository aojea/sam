"""An agent written with the A2A SDK, on the mesh. The A2A SDK's server runs
as it always does, a Starlette app under uvicorn on a loopback port; the mesh
SDK accepts requests for a2a://agent and forwards them to it. Other members
reach the agent by peer ID through a router; the mesh policy decides which
ones, and the SDK turns the rest away before a request reaches the app.

    python a2a_agent.py

SAM_CONTROL_PLANE_URL names the mesh. The first run enrolls with the file
SAM_BOOTSTRAP_TOKEN_PATH (a token the mesh operator gave you) or SAM_JWT_PATH
(a workload identity token your platform issues, such as a Kubernetes
projected service account token), and keeps the identity and credential in
SAM_STATE_DIR; later runs resume from there, as the same peer, without it.
"""

import os
import socket
import threading

import trio
import uvicorn
from a2a.helpers import get_message_text, new_text_message
from a2a.server.agent_execution import AgentExecutor, RequestContext
from a2a.server.events import EventQueue
from a2a.server.request_handlers import DefaultRequestHandler
from a2a.server.routes import create_agent_card_routes, create_jsonrpc_routes
from a2a.server.tasks import InMemoryTaskStore
from a2a.types import AgentCapabilities, AgentCard, AgentInterface, AgentSkill, Role
from agent_mesh import AgentMesh, MeshSession
from starlette.applications import Starlette

mesh = AgentMesh.enroll(
    os.environ.get("SAM_CONTROL_PLANE_URL", "https://mesh.example.com"),
    bootstrap_token_path=os.environ.get("SAM_BOOTSTRAP_TOKEN_PATH"),
    jwt_path=os.environ.get("SAM_JWT_PATH"),
    state_dir=os.environ.get("SAM_STATE_DIR", "~/.config/sam-mesh/a2a-agent"),
    # A plaintext http:// control plane is otherwise accepted only on loopback.
    allow_insecure=os.environ.get("SAM_INSECURE_CONTROL_PLANE") == "true",
)


class EchoExecutor(AgentExecutor):
    """One message in, one message out. X-Peer-Id is the caller the mesh
    verified; the SDK sets it after authorizing the request."""

    async def execute(self, context: RequestContext, event_queue: EventQueue) -> None:
        caller = context.call_context.state["headers"].get("x-peer-id", "someone") if context.call_context else "someone"
        said = get_message_text(context.message) if context.message else ""
        await event_queue.enqueue_event(new_text_message(f"{caller} said: {said}", role=Role.ROLE_AGENT, context_id=context.context_id))

    async def cancel(self, context: RequestContext, event_queue: EventQueue) -> None:
        pass


def a2a_app(agent_url: str) -> Starlette:
    """The A2A SDK's server, as its samples build it. The card names the agent
    as the mesh reaches it: http://mesh/sam/<peer-id>/a2a/agent."""
    card = AgentCard(
        name="Echo agent",
        description="Answers every message with what it said and who sent it.",
        version="1.0.0",
        default_input_modes=["text/plain"],
        default_output_modes=["text/plain"],
        capabilities=AgentCapabilities(streaming=True),
        supported_interfaces=[AgentInterface(protocol_binding="JSONRPC", url=agent_url, protocol_version="1.0")],
        skills=[AgentSkill(id="echo", name="Echo", description="Repeats the message.", tags=["echo"], examples=["hello"])],
    )
    handler = DefaultRequestHandler(agent_executor=EchoExecutor(), task_store=InMemoryTaskStore(), agent_card=card)
    return Starlette(routes=[*create_agent_card_routes(card), *create_jsonrpc_routes(handler, "/")])


def serve_locally(app: Starlette) -> str:
    """Runs uvicorn on a free loopback port in a thread and returns its URL.
    The A2A SDK runs on asyncio, the mesh SDK on trio; a thread keeps them apart."""
    with socket.socket() as probe:
        probe.bind(("127.0.0.1", 0))
        port = probe.getsockname()[1]
    server = uvicorn.Server(uvicorn.Config(app, host="127.0.0.1", port=port, log_level="warning"))
    threading.Thread(target=server.run, daemon=True).start()
    return f"http://127.0.0.1:{port}"


async def main() -> None:
    backend_url = serve_locally(a2a_app(MeshSession.mesh_url(mesh.identity.peer_id, "a2a://agent")))
    async with mesh.join() as session:
        target = await session.accept_a2a(backend_url)
        print(f"accepting {target} as {session.peer_id}", flush=True)
        print(f"agent card at {session.agent_url}/.well-known/agent-card.json", flush=True)
        await trio.sleep_forever()


trio.run(main)
