---
title: "Getting started"
linkTitle: "Getting started"
weight: 1
---

Every SAM mesh has a **control plane** (which enrolls members, distributes
policy, and runs the router) and the **members** (`sam-node` or native SDK
programs) that connect to it.

To connect an agent or machine to a mesh, you need **a running control plane
and its URL** (`https://...`).

## How to get a control plane and its URL

Pick the option that matches what you want to do:

| Control plane option | How to run it and obtain its URL | Architecture & Ingress | Best for |
|---|---|---|---|
| **1. Shared Public Testnet** | Already running — use **`https://bananas.sam-mesh.dev`** | Hosted shared control plane and router | Trying `sam-node` and calling your first remote tool or model in 60 seconds ([Quick start](quickstart/)). |
| **2. Local Control Plane (`sam-one`)** | Run `sam-one --data-dir ~/sam-one --tunnel cloudflare --tunnel-install` and copy `API URL:` from the startup banner | Single binary (control plane + router + console) with SQLite and an HTTPS tunnel | Running your own private mesh from a workstation or VM in seconds ([Your own mesh](your-own-mesh/)). |
| **3. Codespace Control Plane (`sam-one`)** | Open the repository in a [GitHub codespace](../guides/codespaces/), run `make testnet`, make port 8080 public | `sam-one` in a container on your GitHub account, published on `https://<codespace>-8080.app.github.dev` | A control plane of your own with nothing installed and no cloud account, for as long as the codespace runs ([Codespaces](../guides/codespaces/)). |
| **4. Cloud Control Plane (`sam-one`)** | **[Cloud Run](../guides/cloud-run/)**: `gcloud run deploy sam-one --image ghcr.io/google/sam-one:latest ...`<br>**[SkyPilot](../guides/skypilot/)**: `sky launch -c sam-hub deploy/skypilot/sam-one.yaml` | Always-on `sam-one` with managed TLS/WSS ingress + PostgreSQL or persistent disk *(can also run on cloud free tiers for testing)* | Operating a dedicated production control plane in your own cloud account ([Cloud Run](../guides/cloud-run/) · [SkyPilot](../guides/skypilot/)). |

---

## Walkthroughs

1. **[Quick start](quickstart/)**: Join the public `bananas.sam-mesh.dev`
   testnet, discover remote MCP tools and inference models, and give your AI
   agent the SAM skill.
2. **[Your own mesh](your-own-mesh/)**: Start `sam-one` in one command, grab
   the Control Plane URL from the startup banner, publish a local Ollama model,
   and call it from another node.
