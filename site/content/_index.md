---
title: Agent Mesh
description: A private, zero-trust network for AI agents to publish, discover and call tools and models across machines.
---
Agent Mesh (Agent Mesh — OSS Agent Mesh) is a private network for AI agents. A node runs beside an agent, publishes
the tools and models it offers, finds what other nodes offer, and calls them
over authenticated peer-to-peer connections, through relays when the machines
cannot reach each other directly.

Nothing is reachable by default: a node exposes no services until told to, and
no node may call a service the mesh policy has not granted. Identity comes from
your identity provider, and the control plane that turns it into mesh
credentials is one you can run yourself.
