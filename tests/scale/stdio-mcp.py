#!/usr/bin/env python3
#
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
"""An MCP server over stdio, for a provider node in a local mesh.

Built on the official Python SDK (the `mcp` package, as in
development/examples), so what a member reaches through the mesh is a real
server and not an imitation of one. A node declares it as a command backend
and advertises it; the member-journey harness calls it as "everything".

    pip install "mcp>=2,<3"

    services:
      - type: mcp
        name: everything
        command: ["python3", "tests/scale/stdio-mcp.py"]
"""
from mcp.server.mcpserver import MCPServer

mcp = MCPServer("everything")


@mcp.tool()
def echo(text: str) -> str:
    """Return the text given."""
    return text


if __name__ == "__main__":
    mcp.run(transport="stdio")
