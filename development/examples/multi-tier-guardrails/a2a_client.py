#!/usr/bin/env python3
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

"""A2A client using the official a2a-sdk: resolves the agent card and sends a message."""

import asyncio
import os
import sys
import uuid

import httpx
from a2a.client import A2ACardResolver, ClientConfig, create_client
from a2a.helpers import get_message_text
from a2a.types import Message, Part, Role, SendMessageRequest


async def send_once(url: str, prompt: str, context_id: str, required_labels: str = "") -> None:
    token = os.environ.get("AGENTMESH_API_TOKEN", "")
    headers = {"X-Mesh-Authentication": f"Bearer {token}"}
    if required_labels:
        headers["X-Mesh-Required-Labels"] = required_labels

    async with httpx.AsyncClient(timeout=120.0, headers=headers) as http:
        card = await A2ACardResolver(http, url).get_agent_card()
        client = await create_client(card, client_config=ClientConfig(httpx_client=http))
        message = Message(
            role=Role.ROLE_USER,
            message_id=str(uuid.uuid4()),
            parts=[Part(text=prompt)],
            context_id=context_id,
        )
        async for event in client.send_message(SendMessageRequest(message=message)):
            if event.HasField("task") and event.task.HasField("status") and event.task.status.HasField("message"):
                print(get_message_text(event.task.status.message))
                return
            if event.HasField("message"):
                print(get_message_text(event.message))
                return


if __name__ == "__main__":
    if len(sys.argv) < 4:
        sys.exit("usage: a2a_client.py <mesh-a2a-url> <context-id> <prompt> [required-labels]")
    req_labels = sys.argv[4] if len(sys.argv) > 4 else ""
    asyncio.run(send_once(sys.argv[1], sys.argv[3], sys.argv[2], req_labels))
