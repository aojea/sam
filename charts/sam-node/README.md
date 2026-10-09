# sam-node

Deploys one SAM node into a Kubernetes cluster, optionally hosting a service
as a sidecar container. The node authenticates to the control plane with a
projected ServiceAccount token (Workload Identity Federation); the token's
`audience` must be listed in the control plane's `allowedAudiences`.

## Usage

A bare node (a caller / mesh participant with no local service):

```bash
helm install my-node charts/sam-node \
  --set controlPlaneUrl=http://sam-mesh-control-plane:8080
```

A node hosting an MCP service (see `development/examples/*/values.yaml` for
complete, working examples):

```yaml
controlPlaneUrl: http://sam-mesh-control-plane:8080
config:
  version: v1alpha1
  services:
    - type: mcp
      name: calculator
      description: Simple math operations
      target_url: http://127.0.0.1:7777/mcp
service:
  name: calc-mcp
  image: calc-mcp:local
```

## Values

| Key | Default | Meaning |
|-----|---------|---------|
| `controlPlaneUrl` | — (required) | Control plane URL the node enrolls with |
| `audience` | `agentmesh-audience` | Projected token audience |
| `apiToken` | `""` (generated) | Bearer token for the node's local REST API, stored in the Secret `<release>-api-token` and mounted as a file. Empty generates a random one on first install; set to pin |
| `podSecurityContext` / `securityContext` | nonroot 65532, seccomp RuntimeDefault, no capabilities | Pod and container security contexts |
| `bindAddr` | `127.0.0.1:8080` | Node API bind address (loopback = pod-private) |
| `extraArgs` | `[]` | Extra sam-node args |
| `config` | empty services | Merged over the chart's defaults and rendered as `sam-node.yaml`; pods roll on config changes |
| `service.image` | `""` | Service container image; empty = bare node |
| `service.name/command/env/ports/resources` | — | Service container spec |
| `serviceAccount.create/name/annotations` | `true` / fullname / `{}` | Skip creation, reuse an existing SA, or annotate it (e.g. Workload Identity) |
| `image.repository/tag/pullPolicy` | `sam-node:local` | Node image |
| `replicaCount` | `1` | Each replica enrolls as its own mesh node |
