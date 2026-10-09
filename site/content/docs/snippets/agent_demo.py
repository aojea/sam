import asyncio
import os
import sys

import httpx
from mcp import ClientSession
from mcp.client.streamable_http import streamable_http_client


async def main():
    # Connect to the local SAM node's MCP endpoint.
    # By default, sam-node listens at 127.0.0.1:8080.
    url = os.environ.get("SAM_MCP_URL", "http://127.0.0.1:8080/mcp")
    token = os.environ.get("SAM_API_TOKEN", "")
    print(f"Connecting to SAM Node at {url}")

    headers = {"X-Mesh-Authentication": f"Bearer {token}"} if token else {}
    try:
        async with httpx.AsyncClient(headers=headers) as http:
            async with streamable_http_client(url, http_client=http) as (read, write):
                async with ClientSession(read, write) as session:
                    await session.initialize()

                    # Discover available tools provided by the SAM node
                    tools = (await session.list_tools()).tools
                    print(f"Discovered {len(tools)} tools:")
                    for tool in tools:
                        print(f" - {tool.name}: {tool.description}")

                    # Call the get_mesh_info tool to get information about the mesh
                    print("\nCalling get_mesh_info tool...")
                    result = await session.call_tool("get_mesh_info", {})
                    print("Result:")
                    for block in result.content:
                        print(getattr(block, "text", block))

    except Exception as e:
        print(f"Error connecting to SAM Node: {e}")
        sys.exit(1)


if __name__ == "__main__":
    asyncio.run(main())
