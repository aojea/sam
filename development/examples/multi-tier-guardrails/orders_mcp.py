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

"""Orders & Refunds MCP server backed by SQLite, exposed via agentmesh-node."""

import os
import sqlite3
from mcp.server.mcpserver import MCPServer

DB_PATH = os.environ.get("ORDERS_DB_PATH", "/tmp/agentmesh-multi-tier-orders.db")
PORT = int(os.environ.get("ORDERS_MCP_PORT", "18888"))


def init_db() -> None:
    with sqlite3.connect(DB_PATH) as conn:
        conn.execute(
            """
            CREATE TABLE IF NOT EXISTS orders (
                order_id TEXT PRIMARY KEY,
                customer TEXT NOT NULL,
                item TEXT NOT NULL,
                amount_dollars REAL NOT NULL,
                status TEXT NOT NULL
            )
            """
        )
        conn.execute(
            """
            CREATE TABLE IF NOT EXISTS refunds (
                id INTEGER PRIMARY KEY AUTOINCREMENT,
                order_id TEXT NOT NULL,
                amount_dollars REAL NOT NULL,
                reason TEXT NOT NULL
            )
            """
        )
        conn.execute(
            """
            INSERT OR REPLACE INTO orders (order_id, customer, item, amount_dollars, status)
            VALUES
                ('1042', 'alice@acme.com', 'Mechanical Keyboard Pro', 129.00, 'SHIPPED'),
                ('1043', 'bob@acme.com', 'USB-C Dock', 89.00, 'DELIVERED')
            """
        )
        conn.commit()


mcp = MCPServer("orders-db")


@mcp.tool()
def get_order_status(order_id: str) -> str:
    """Look up an order by order_id in the SQLite orders database (read-only)."""
    with sqlite3.connect(DB_PATH) as conn:
        row = conn.execute(
            "SELECT order_id, customer, item, amount_dollars, status FROM orders WHERE order_id = ?",
            (order_id,),
        ).fetchone()
    if not row:
        return f"Order #{order_id} not found"
    oid, customer, item, amount, status = row
    return f"Order #{oid}: status={status}, item='{item}', total=${amount:.2f}, customer={customer}"


@mcp.tool()
def issue_refund(order_id: str, amount_dollars: float, reason: str = "customer request") -> str:
    """Issue a refund for an order and update its status in SQLite (mutating financial operation)."""
    with sqlite3.connect(DB_PATH) as conn:
        conn.execute(
            "INSERT INTO refunds (order_id, amount_dollars, reason) VALUES (?, ?, ?)",
            (order_id, amount_dollars, reason),
        )
        conn.execute(
            "UPDATE orders SET status = 'REFUNDED' WHERE order_id = ?",
            (order_id,),
        )
        conn.commit()
    return f"REFUND_EXECUTED: order=#{order_id}, amount=${amount_dollars:.2f}, reason='{reason}'"


if __name__ == "__main__":
    init_db()
    mcp.run(transport="streamable-http", host="127.0.0.1", port=PORT)
