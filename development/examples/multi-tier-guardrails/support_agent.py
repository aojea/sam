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

"""LLM-backed A2A Support Agent using the official a2a-sdk and Ollama/OpenAI API.

Maintains conversation history per A2A contextId in SQLite so that when traffic
shifts across replicas (e.g. from laptop v1 to Cloud Run v2), the conversation
context survives seamlessly across the mesh hop.
"""

import json
import os
import sqlite3
import sys

import httpx
import uvicorn
from a2a.helpers.proto_helpers import new_task
from a2a.server.agent_execution.agent_executor import AgentExecutor
from a2a.server.agent_execution.context import RequestContext
from a2a.server.events.event_queue import EventQueue
from a2a.server.request_handlers import DefaultRequestHandler
from a2a.server.routes import create_agent_card_routes, create_jsonrpc_routes
from a2a.server.tasks.inmemory_task_store import InMemoryTaskStore
from a2a.server.tasks.task_updater import TaskUpdater
from a2a.types import (
    AgentCapabilities,
    AgentCard,
    AgentInterface,
    AgentSkill,
    Part,
    TaskState,
)
from starlette.applications import Starlette

REPLICA_ID = sys.argv[1] if len(sys.argv) > 1 else os.environ.get("REPLICA_ID", "v1-laptop")
PORT = int(sys.argv[2]) if len(sys.argv) > 2 else int(os.environ.get("PORT", "17771"))
DB_PATH = os.environ.get("ORDERS_DB_PATH", "/tmp/agentmesh-multi-tier-orders.db")
LLM_BASE_URL = os.environ.get("LLM_BASE_URL", "http://127.0.0.1:11434/v1")
LLM_API_KEY = os.environ.get("LLM_API_KEY", "ollama")
LLM_MODEL = os.environ.get("LLM_MODEL", "gemma3:1b")

SYSTEM_PROMPT = (
    "You are the Acme Support & Refund Triage Agent. "
    "Answer the customer's question concisely in one or two short sentences using the "
    "Order Database Snapshot provided. Keep previous turns in mind."
)


def init_history_db() -> None:
    with sqlite3.connect(DB_PATH) as conn:
        conn.execute(
            """
            CREATE TABLE IF NOT EXISTS chat_history (
                id INTEGER PRIMARY KEY AUTOINCREMENT,
                context_id TEXT NOT NULL,
                role TEXT NOT NULL,
                content TEXT NOT NULL
            )
            """
        )
        conn.commit()


def load_orders_snapshot() -> str:
    if not os.path.exists(DB_PATH):
        return "Order #1042: customer=alice@acme.com, item='Mechanical Keyboard Pro', total=$129.00, status=SHIPPED"
    with sqlite3.connect(DB_PATH) as conn:
        rows = conn.execute(
            "SELECT order_id, customer, item, amount_dollars, status FROM orders ORDER BY order_id"
        ).fetchall()
    return "; ".join(
        f"Order #{r[0]} (customer={r[1]}, item={r[2]}, total=${r[3]:.2f}, status={r[4]})"
        for r in rows
    )


def get_history(context_id: str) -> list[dict[str, str]]:
    with sqlite3.connect(DB_PATH) as conn:
        rows = conn.execute(
            "SELECT role, content FROM chat_history WHERE context_id = ? ORDER BY id",
            (context_id,),
        ).fetchall()
    return [{"role": r[0], "content": r[1]} for r in rows]


def append_history(context_id: str, role: str, content: str) -> None:
    with sqlite3.connect(DB_PATH) as conn:
        conn.execute(
            "INSERT INTO chat_history (context_id, role, content) VALUES (?, ?, ?)",
            (context_id, role, content),
        )
        conn.commit()


class SupportAgentExecutor(AgentExecutor):
    """Executes A2A tasks using a real LLM (Ollama / Gemini) and shared SQLite state."""

    async def execute(self, context: RequestContext, event_queue: EventQueue) -> None:
        user_text = context.get_user_input()
        ctx_id = context.context_id
        history = get_history(ctx_id)
        orders_info = load_orders_snapshot()

        messages = [
            {"role": "system", "content": f"{SYSTEM_PROMPT}\nOrder Database Snapshot: {orders_info}"},
            *history,
            {"role": "user", "content": user_text},
        ]

        async with httpx.AsyncClient(timeout=60.0) as client:
            resp = await client.post(
                f"{LLM_BASE_URL}/chat/completions",
                headers={"Authorization": f"Bearer {LLM_API_KEY}"},
                json={
                    "model": LLM_MODEL,
                    "messages": messages,
                    "temperature": 0.2,
                },
            )
            resp.raise_for_status()
            llm_reply = resp.json()["choices"][0]["message"]["content"].strip()

        append_history(ctx_id, "user", user_text)
        append_history(ctx_id, "assistant", llm_reply)

        updater = TaskUpdater(event_queue, context.task_id, ctx_id)
        if context.current_task is None:
            await event_queue.enqueue_event(
                new_task(context.task_id, ctx_id, TaskState.TASK_STATE_SUBMITTED)
            )
        formatted = f"[{REPLICA_ID} · {LLM_MODEL}] {llm_reply}"
        await updater.complete(updater.new_agent_message([Part(text=formatted)]))

    async def cancel(self, context: RequestContext, event_queue: EventQueue) -> None:
        pass


agent_card = AgentCard(
    name="Acme Support & Refund Triage Agent",
    description=f"LLM-backed customer support agent ({REPLICA_ID}, model={LLM_MODEL})",
    version="1.0.0",
    capabilities=AgentCapabilities(streaming=False),
    default_input_modes=["text/plain"],
    default_output_modes=["text/plain"],
    skills=[
        AgentSkill(
            id="order_lookup",
            name="Order Status Lookup",
            description="Check shipping status and details of customer orders",
            tags=["orders", "support"],
            examples=["Check order #1042 for alice@acme.com"],
        ),
        AgentSkill(
            id="refund_triage",
            name="Refund Triage",
            description="Triage customer refund eligibility",
            tags=["refunds", "billing"],
            examples=["Is order #1042 eligible for a refund?"],
        ),
    ],
    supported_interfaces=[
        AgentInterface(
            protocol_binding="JSONRPC",
            protocol_version="1.0",
            url=f"http://127.0.0.1:{PORT}/",
        )
    ],
)

handler = DefaultRequestHandler(
    agent_executor=SupportAgentExecutor(),
    task_store=InMemoryTaskStore(),
    agent_card=agent_card,
)

app = Starlette(
    routes=[
        *create_jsonrpc_routes(request_handler=handler, rpc_url="/"),
        *create_agent_card_routes(agent_card=agent_card),
    ]
)

if __name__ == "__main__":
    init_history_db()
    uvicorn.run(app, host="127.0.0.1", port=PORT, log_level="warning")
