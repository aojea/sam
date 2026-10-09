"""Minimal A2A chat REPL: resolves the agent card through the mesh and talks to it."""
import asyncio
import os
import sys
import uuid

import httpx
from a2a.client import A2ACardResolver, ClientConfig, create_client
from a2a.helpers import get_message_text
from a2a.types import Message, Part, Role, SendMessageRequest


async def main(url: str) -> None:
    token = os.environ.get("SAM_API_TOKEN", "devtoken")
    async with httpx.AsyncClient(timeout=120, headers={"X-Mesh-Authentication": f"Bearer {token}"}) as http:
        card = await A2ACardResolver(http, url).get_agent_card()
        print(f"{card.name}: {card.description}")
        for iface in card.supported_interfaces:
            print(f"  {iface.protocol_binding} -> {iface.url}")
        client = await create_client(card, client_config=ClientConfig(httpx_client=http))
        context_id = None
        while True:
            try:
                text = input("you> ")
            except EOFError:
                return
            if not text.strip():
                continue
            message = Message(
                role=Role.ROLE_USER,
                message_id=str(uuid.uuid4()),
                parts=[Part(text=text)],
                context_id=context_id,
            )
            async for event in client.send_message(SendMessageRequest(message=message)):
                if event.HasField("message"):
                    context_id = event.message.context_id or context_id
                    print(f"agent> {get_message_text(event.message)}")


if __name__ == "__main__":
    if len(sys.argv) != 2:
        sys.exit("usage: chat.py http://127.0.0.1:9099/mesh/<peer-id>/a2a/chat")
    asyncio.run(main(sys.argv[1]))
