---
title: "Mesh policy"
linkTitle: "Mesh policy"
weight: 6
aliases:
  - /docs/development/policy/
---

The mesh policy is one document held by the control plane: a list of roles,
a list of bindings and a list of egress destinations. It is posted as JSON
(protojson of `PolicyConfig` in `api/sam.proto`) to `POST /policies`, read
back from `GET /admin/policy`, edited in the console, or given to
`sam-one --policy-file` for first boot.

```json
{
  "roles": [
    {
      "name": "sam:role:node",
      "allowed_services": ["system://sam.catalog"],
      "allowed_labels": ["region=*", "team=platform"]
    },
    {
      "name": "developer",
      "allowed_services": ["mcp://code-reviewer", "mcp://build-runner.*", "inference://*"],
      "allowed_targets": ["group:dev-nodes", "node:12D3KooWSpecialNode"],
      "allowed_agents": ["*.dev.acme.example"],
      "custom_datalog": ["tier(\"standard\");"]
    },
    {
      "name": "contractor",
      "allowed_services": ["egress://api.github.com"],
      "allowed_targets": ["*"],
      "http": [
        { "service": "egress://api.github.com", "methods": ["GET"], "paths": ["/repos/acme/*"] }
      ]
    }
  ],
  "bindings": [
    { "role": "sam:role:node", "members": ["group:engineering", "user:system:serviceaccount:sam-nodes:calc-mcp-sam-node"] },
    { "role": "developer",     "members": ["group:engineering"] },
    { "role": "contractor",    "members": ["group:external"] }
  ],
  "egress": [
    { "name": "api.github.com", "credential": "github-eu", "served_by": ["site=eu"] }
  ]
}
```

The whole document is replaced on every post. Unknown fields are rejected,
so a misspelt key fails the request instead of dropping a grant without
notice.

## Roles

| Field | Type | Meaning |
|---|---|---|
| `name` | string, required, unique | The role name. `sam:role:node`, `sam:role:router` and `sam:role:sambox` are the roles that the three binaries request at enrollment. Any other name is an ordinary role. |
| `allowed_services` | list of service patterns | Services that holders may call. |
| `allowed_targets` | list of target patterns | Nodes that holders may call. If absent, any node may be called. |
| `allowed_labels` | list of label patterns | Labels that a node holding this role may declare at enrollment. If absent, no labels may be declared. |
| `allowed_agents` | list of agent patterns | Agent identifiers that a node holding this role may claim to act for. If absent, no agent may be named. |
| `custom_datalog` | list of Datalog statements | Facts are minted into the credentials of holders. Rules are distributed to nodes and applied when a holder is verified. |
| `http` | list of HTTP grants | Narrows an `allowed_services` entry to HTTP methods and paths. See [HTTP grants](#http-grants). |

### Service patterns

`type://name`, where `type` is `mcp`, `inference`, `a2a`, `egress` or
`system`, and `name` consists of dot-separated DNS-style labels. For
`egress` the name is the hostname of a destination outside the mesh, written
lowercase, with no scheme, port or path: `egress://api.github.com/v3` is
rejected, because the path belongs in the role's `http` entry. See
[Egress destinations](#egress-destinations).

| Pattern | Compiles to | Matches |
|---|---|---|
| `mcp://calculator` | `granted_service_exact("mcp", "calculator")`. Several exact entries of one type are merged into one `granted_service_set`. | that service |
| `mcp://*.internal` | `granted_service_suffix("mcp", ".internal")` | `a.internal` and `b.c.internal`, but not `xinternal` |
| `mcp://build.*` | `granted_service_prefix("mcp", "build.")` | `build.runner`, but not `builder` |
| `mcp://*` | `granted_service_all("mcp")` | every MCP service |
| `*` | `granted_service_all_types(true)` | every service |

A fact whose only meaning is that a grant exists carries the single term
`true`, because the Biscuit grammar requires at least one term per predicate.
The same holds for `target_unrestricted(true)`, `granted_agent_all(true)` and
`agent_authorized(true)` below.

`system://sam.catalog` is the built-in discovery service that every node
runs. A role that should be able to list the tools of a node needs it.

### Target patterns

`fact:value`, where `fact` is one of the identity facts that a node's
credential can carry: `node` (peer ID), `user`, `email`, `group`,
`idp_role`. The values come from the destination node's own credential, so
`group:dev-nodes` means "nodes whose enrolling identity is in group
`dev-nodes`".

| Pattern | Matches |
|---|---|
| `node:12D3KooW...` | one node |
| `group:dev-nodes` | nodes enrolled by a member of that group |
| `email:*.acme.example` | suffix match on the email of the enrolling identity |
| `group:*` | any node with a `group` fact |
| `*` | any node |

Exact entries of one fact are merged into one `granted_target_set` fact.
`agent:` is not a valid target, because a node's identity does not say which
agents it hosts.

### Label patterns

| Pattern | Permits |
|---|---|
| `key=value` | exactly that pair |
| `key=*` | any value for that key |
| `*` | any label |

### HTTP grants

An entry of `http` narrows one `allowed_services` entry of the same role to
HTTP methods and paths. The service must be written exactly as it appears in
`allowed_services`; at least one of `methods` and `paths` must be set.

| Field | Meaning |
|---|---|
| `service` | The `allowed_services` entry this narrows. |
| `methods` | Methods the holder may use, uppercase, such as `GET`. Empty means any method. |
| `paths` | Paths the holder may request, as the backend sees them. `/user` matches that path only; `/v2/public/*` matches every path under the prefix. Empty means any path. |

```json
{ "service": "egress://api.github.com", "methods": ["GET", "HEAD"], "paths": ["/repos/acme/*", "/user"] }
```

The control plane compiles the entry into facts in the holder's credential
and withholds the plain service grant for that entry:

```datalog
http_granted_service_exact("egress", "api.github.com")
granted_method("egress", "api.github.com", ["GET", "HEAD"])
granted_path_prefix("egress", "api.github.com", "/repos/acme/")
granted_path_exact("egress", "api.github.com", ["/user"])
```

Baseline rules on every node derive `granted_service_exact("egress",
"api.github.com")` from these facts only when the request's `method()` and
`path()` facts satisfy them. The ordinary allow policies then decide as they
do for any grant. The rules are positive, so a request that carries no HTTP
method (a tunnel, a non-HTTP stream) derives nothing and a narrowed grant
denies it. A role that holds the same service plainly, through another
entry or another role, is not narrowed: grants are a union.

A node built before this field existed has no derivation rules, so it denies
a narrowed grant entirely rather than treating it as unrestricted.

## Egress destinations

An entry of `egress` is a destination outside the mesh that selected nodes
serve as `egress://<name>`. The admin writes it once; each selected node
receives it at `GET /egress`, registers the service, announces it on the DHT
and forwards requests to it. Nodes hold no egress configuration of their
own, and `type: egress` in `sam-node.yaml` is refused.

| Field | Meaning |
|---|---|
| `name` | The destination hostname, lowercase, without a port or a path. It is the service name in grants (`egress://<name>`) and in the `service()` fact. One hostname; no wildcard. |
| `target_url` | Where the serving node forwards requests. Optional; `https://<name>` when empty. `http` or `https`, no credential, no query. |
| `credential` | Name of the credential the serving node presents to the destination. The node reads the file `<--secrets-dir>/<credential>` (default `/etc/sam/secrets`): `TOKEN` is sent as `Authorization: Bearer TOKEN`, `user:pass` as HTTP Basic. The file is read on every request, so a rotation by the platform applies at once. The value never travels through the control plane. |
| `served_by` | Role names or `key=value` labels selecting the serving nodes. A node matches when any entry names one of its roles or attested labels. |

The control plane renders one rule per `served_by` entry into the mesh
policy, `granted_service_exact("egress", "api.github.com") <- role("pep")`
or `<- label("site", "eu")`, so a serving node authorizes a local request
with its own credential. Every other caller needs `egress://<name>` on its
own role. A destination that names a credential the platform did not deliver
to a node is refused by that node at registration and logged; the other
destinations are still served.

On an egress request the destination node injects `host()` and `port()`
next to `method()` and `path()`, and the destination sees the node's
credential only: the caller's `Authorization`, `Cookie`, `X-Sam-*` and
`X-Forwarded-*` headers are removed. See [Node API](../node-api/#egress)
for how a local client reaches a destination.

The control plane warns in its log when a posted destination selects no
enrolled node, since a selector with a typo is valid in form and would
otherwise surface only as `404` at the callers.

Keys match `[a-zA-Z0-9_.-]{1,63}`. Values are up to 255 characters with no
`,`, `=` or control characters. Every label that a node declares must be
permitted by a role it holds, or enrollment fails. Manual approval of a
bootstrap enrollment does not bypass this check.

### Agent patterns

An agent identifier is lowercase and dot-separated, with at least two labels
(`reviewer-7.prod.acme.example`). A pattern is an identifier, `*.<suffix>`,
`<prefix>.*`, or `*`. Wildcards are anchored on label boundaries. `*` lets
every holder name any agent, and nodes log a warning when they receive such
a grant. See the [sandboxed agents preview](../../preview/sandboxed-agents/).

### `custom_datalog`

Each entry is one Biscuit Datalog fact or rule. A **fact**
(`tier("standard");`) is minted into the credential of every holder, where
`attenuation` statements on any node can refer to it. A **rule**
(`head($x) <- body($x);`) is compiled into the node-side rule set and
applied when a holder is verified. `allow` and `deny` policies are not
accepted here. They belong in a node's `attenuation.policies`. An entry that
parses as neither a fact nor a rule fails validation.

## Bindings

| Field | Meaning |
|---|---|
| `role` | A role name from `roles`. A binding to an undefined role is rejected. |
| `members` | Identities that receive the role. At least one is required. |

A member is one of:

| Member | Matches |
|---|---|
| `user:<sub>` | the OIDC subject. For a Kubernetes service account: `user:system:serviceaccount:<namespace>:<name>`. |
| `email:<address>` | a verified email claim |
| `group:<name>` | an entry of the `groups` claim |
| `idp_role:<name>` | an entry of the issuer's `roles` claim |
| `node:<peer-id>` | one node, by key |
| `agent:<id>` | an agent identifier that a node names when acting for the agent. Only as trustworthy as the node's `allowed_agents` grant. |
| `sam:system:authenticated` | every identity that the identity provider authenticates |

`role:` is not a member, because a role cannot grant a role. A bootstrap
token enrollment carries no OIDC claims. Such a node receives exactly the
token's role and nothing that a binding on claims would add, so the grants
must be on the role itself.

## Evaluation

At enrollment and at refresh, the control plane resolves the identity's roles
from the bindings and mints into the credential one `role()` fact per role
and the compiled `granted_*` facts of every role. The control plane also
renders the policy as Datalog rules such as
`role("developer") <- group("engineering")` and
`granted_service_exact("mcp","code-reviewer") <- role("developer")`, served
as `datalog_rules` at `GET /policies`. Nodes fetch this text
(`--control-plane-sync-interval`, 15 minutes) and add it to their authorizer
as it arrives; no member derives rules from roles and bindings itself, so a
node written in any language evaluates the same rules. These rules run at
verification time. Additions therefore reach nodes within the sync interval,
and removals within the credential TTL.

At the destination node, in this order:

1. Facts for the request: `service($type, $name)`, `connection_peer_id($id)`,
   `time($now)`, and `agent($id)` if a claim was made. On an HTTP request,
   `method($m)` and `path($p)`; on a request for an egress destination,
   `host($h)` and `port($n)`.
2. Checks that always apply: `client_peer_id($id), connection_peer_id($id)`,
   `time($t), expiration($e), $t <= $e`, and `agent_authorized(true)` when an
   agent was named.
3. The node's own identity as `target_fact($name, $value)` facts.
4. The node's `attenuation` rules, checks and policies.
5. Baseline policies: `allow if service($t,$n), granted_service_exact($t,$n)`
   and the set, prefix, suffix, per-type and global variants; the check
   `allow_network_target($f,$v) or target_unrestricted(true)`; the rules that
   derive a service grant from an [HTTP grant](#http-grants).
6. The synced mesh policy rules.

All checks must hold, and the first matching policy decides. Without a
matching `allow`, the request is denied.

## Limits

The control plane rejects a policy that could give one identity, through
overlapping bindings, more Datalog facts than the Biscuit authorizer can
evaluate. The limit is far above any ordinary policy. A policy that reaches
it should use wildcards or sets instead of listing every entry.

## Datalog vocabulary

Every fact name used by SAM, for writing `custom_datalog` or `attenuation`
statements.

| Fact | Terms | Minted by |
|---|---|---|
| `node` | peer ID | control plane |
| `client_peer_id` | peer ID | control plane |
| `expiration` | date | control plane |
| `role` | name | control plane |
| `user`, `email`, `group`, `idp_role` | string | control plane, from OIDC claims |
| `label` | key, value | control plane |
| `right` | `relay` | control plane, for routers |
| `target_unrestricted` | | control plane, for roles with `allowed_targets: ["*"]` or none |
| `granted_service_exact`, `_set`, `_prefix`, `_suffix`, `_all`, `_all_types` | type, name/set/pattern | control plane and node rules |
| `granted_target_exact`, `_set`, `_prefix`, `_suffix`, `_all`, `_all_facts` | fact, value/set/pattern | control plane and node rules |
| `granted_agent_exact`, `_set`, `_prefix`, `_suffix`, `_all` | pattern | control plane and node rules |
| `http_granted_service_exact`, `_prefix`, `_suffix`, `_all`, `_all_types` | type, name/pattern | control plane and node rules, for an `http` entry |
| `granted_method`, `granted_method_any` | type, key, set | control plane and node rules, for an `http` entry |
| `granted_path_exact`, `granted_path_prefix`, `granted_path_any` | type, key, set/prefix | control plane and node rules, for an `http` entry |
| `service` | type, name | destination node, per request |
| `connection_peer_id` | peer ID | destination node, per request |
| `time` | date | destination node, per request |
| `agent` | identifier | destination node, from the caller's claim |
| `method` | string | destination node, on an HTTP request: the method as received; `CONNECT` on a tunnel |
| `path` | string | destination node, on an HTTP request: the path as the backend sees it, leading slash, no query; empty on a tunnel |
| `host` | hostname | destination node, on an egress request |
| `port` | integer | destination node, on an egress request |
| `target_fact` | fact, value | destination node, from its own credential |
| `allow_network_target` | fact, value | derived by baseline rules |
| `agent_authorized` | | derived by baseline rules |
| `http_method_ok`, `http_path_ok` | type, key | derived by baseline rules |

A node's `attenuation` can refer to the request facts. The Datalog dialect
has no `!=`; a negation is written with `!`:

```yaml
attenuation:
  policies:
    - 'deny if method($m), !($m == "GET");'
    - 'deny if path($p), $p.starts_with("/admin/");'
    - 'deny if port($p), !($p == 443);'
    - 'deny if host("payroll.internal.example.com");'
```
