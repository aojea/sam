---
title: "Use cases"
linkTitle: "Use cases"
weight: 5
---

Worked examples of things built on top of the mesh. Each pattern combines
ordinary `agentmesh-node` features (discovery, remote tool calls, leasing) into
something larger, without changes to the node itself.

None of the examples depends on a specific harness. The orchestrator is any
MCP client, whether an agent harness (Claude Code, Codex, Antigravity) or a
custom program, talking to the tools that a local node exposes.

The runnable source for every example is under
[`development/examples/`](https://github.com/google/agentmesh/tree/main/development/examples)
in the repository. Each page links to its directory.
