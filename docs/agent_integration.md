# Agent Integration Guide

SAM is designed to be the networking layer for autonomous AI agents. The easiest way for your agent to interact with the mesh is through the **Model Context Protocol (MCP)** exposed locally by your node.

Every `sam-node` runs a local MCP server that allows agents to:
- Discover tools available on the mesh.
- Connect to remote peers securely.
- Query mesh information (e.g. `get_mesh_info`).
- Call tools remotely (via `call_remote_tool`).

## Connecting via MCP

The `sam-node` exposes the MCP server over HTTP Server-Sent Events (SSE). By default, it listens at `127.0.0.1:8080`.

The repository provides a Python SDK (`sam-mcp-python`) which implements the MCP client.

### Prerequisites

You need the `sam_mcp` package installed. From the repo root, run:

```bash
pip install ./sam-mcp-python
```

### Python SDK Demo

The following snippet demonstrates how to connect to the local node's MCP server, list the available tools, call the local `get_mesh_info` tool, and then optionally call the same tool remotely on another peer via the `call_remote_tool` tool.

```python
import asyncio
import os
import sys
import json
from sam_mcp.client import SamClient

async def main():
    # Connect to the local SAM node's MCP SSE endpoint
    # By default, sam-node listens at 127.0.0.1:8080
    url = os.environ.get("SAM_MCP_URL", "http://127.0.0.1:8080/mcp/events")
    target_peer = os.environ.get("TARGET_PEER_ID")

    print(f"Connecting to SAM Node at {url}")

    try:
        async with SamClient(server_url=url) as client:
            # Discover available tools provided by the local SAM node
            tools = await client.get_tools()
            print(f"Discovered {len(tools)} tools:")
            for tool in tools:
                print(f" - {tool['name']}: {tool['description']}")

            # Call the get_mesh_info tool to get information about the mesh
            print("\nCalling local get_mesh_info tool...")
            result = await client.call_tool("get_mesh_info", {})
            print("Local Result:")
            print(result)

            if target_peer:
                print(f"\nCalling get_mesh_info tool remotely on peer {target_peer}...")
                remote_result = await client.call_tool("call_remote_tool", {
                    "peer_id": target_peer,
                    "tool_name": "get_mesh_info",
                    "arguments": "{}"
                })
                print("Remote Result:")
                print(remote_result)
            else:
                print("\nNo TARGET_PEER_ID provided, skipping remote tool call.")

    except Exception as e:
        print(f"Error connecting to SAM Node: {e}")
        sys.exit(1)

if __name__ == "__main__":
    asyncio.run(main())
```

*(You can find this snippet at `docs/snippets/agent_demo.py`)*

### Example Output

When you run the demo with a target peer ID environment variable, you'll see output similar to this:

```
Connecting to SAM Node at http://127.0.0.1:8080/mcp/events
Discovered tools:
 - get_mesh_info: Retrieve information about the mesh...
 - call_remote_tool: Call an MCP tool on a remote agent...

Calling local get_mesh_info tool...
Local Result:
{'mesh_name': 'e2e-mesh', 'connected_peers': ...}

Calling get_mesh_info tool remotely on peer 12D3Koo...
Remote Result:
[{'type': 'text', 'text': '{"mesh_name":"e2e-mesh","connected_peers":...}'}]
```
