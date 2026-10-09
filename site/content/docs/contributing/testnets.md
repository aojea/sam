---
title: "Testnets"
linkTitle: "Testnets"
weight: 1
aliases:
  - /docs/development/release-tracks/
---

The project runs two public meshes for its own testing and for anyone who
wants to try the software without deploying anything.

| | `bananas.__PROTECT_Agent Mesh_MESH_DEV__` | `hub.__PROTECT_Agent Mesh_MESH_DEV__` |
|---|---|---|
| Built from | every push to `main` | every `v*` tag |
| Image tag | the commit SHA | the release version |
| Purpose | continuous integration of unreleased code | the latest release, for interoperability testing |

Both are deployed by `.github/workflows/deploy.yaml` to a GKE cluster from
the templates in `.github/k8s/`. Each testnet has a control plane Deployment
on PostgreSQL, a router StatefulSet that announces
`/dnsaddr/bootstrap.<env>.__PROTECT_Agent Mesh_MESH_DEV__` (a DNS-sync CronJob keeps the record
current), the console, and a few canary nodes that publish demo services:
the MCP `everything` server, an OpenRouter proxy, and a vLLM instance when
one is running. Identity comes from a Dex instance at `auth.__PROTECT_Agent Mesh_MESH_DEV__`
that accepts Google and GitHub logins, and from the cluster's own issuer for
in-cluster workloads.

Deploys run one at a time per environment, replace pods one at a time, and
fail the workflow if the rollout does not complete. A probe CronJob enrolls
a new node every half hour and calls a tool through it. The
[testnet-health](https://github.com/google/agentmesh/actions/workflows/testnet-health.yaml)
badge on the repository turns red when the probe fails, and an issue
labelled `testnet-health` names the failed check.

## What they are not

The testnets run on donated resources with no uptime commitment. Their
policy binds `mesh:system:authenticated` to `mesh:role:node`, so anyone who
can log in with Google or GitHub can enroll a node, and the demo services
are granted to every node. Nothing that you publish from a node enrolled
there is private. The testnets are a place to see the software work and the
project's own canary. If you need a mesh you can rely on, run its control
plane yourself.

Only the enrollment paths are routed to the public. `/healthz`, `/readyz`,
`/metrics` and `/admin` answer `404` from outside the cluster. `/info` is
public, because enrollment needs it, and it lists the current router
addresses.
