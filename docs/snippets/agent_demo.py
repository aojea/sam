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
