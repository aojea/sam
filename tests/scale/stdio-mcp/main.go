// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// Command stdio-mcp is an MCP server over stdio, for a provider node in a
// local mesh. It is the Go twin of tests/scale/stdio-mcp.py, built on the
// official Go SDK, so a member's first call through the mesh lands on a real
// server in either language.
//
//	go build -o bin/stdio-mcp ./tests/scale/stdio-mcp
//
//	services:
//	  - type: mcp
//	    name: everything
//	    command: ["bin/stdio-mcp"]
package main

import (
	"context"
	"fmt"
	"os"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type echoInput struct {
	Text string `json:"text" jsonschema:"the text to return"`
}

func main() {
	server := mcp.NewServer(&mcp.Implementation{Name: "everything", Version: "0"}, nil)
	mcp.AddTool(server, &mcp.Tool{Name: "echo", Description: "Return the text given"},
		func(_ context.Context, _ *mcp.CallToolRequest, in echoInput) (*mcp.CallToolResult, any, error) {
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: in.Text}}}, nil, nil
		})
	if err := server.Run(context.Background(), &mcp.StdioTransport{}); err != nil {
		fmt.Fprintln(os.Stderr, "stdio-mcp:", err)
		os.Exit(1)
	}
}
