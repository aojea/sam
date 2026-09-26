---
title: "Authorization"
linkTitle: "Authorization"
weight: 3
---

Authorization in SAM answers one question: may this caller use this service
on this node? Three sources contribute to the answer, and the node that hosts
the service combines them: the caller's credential, the mesh policy, and the
node's own configuration. Each source can only narrow what the others allow.
If none of them grants access, the answer is no.

## Services are the unit of authorization

A node publishes services, each with a type and a name: `mcp://calculator`,
`inference://vllm-eu`, `a2a://triage`, and the built-in
`system://sam.catalog` that answers discovery queries. Policy grants access
to services by these names. It does not look inside a service: a grant on
`mcp://db` offers every tool that the MCP server exposes. To offer different
privilege levels, publish different services (`mcp://db-reader`,
`mcp://db-writer`) and grant them separately. Choosing which tools a backend
exposes is the job of the backend, or of a small MCP server placed in front
of it.

## Mesh policy: roles and bindings

The mesh policy is a document held by the control plane. You edit it through
`POST /policies` or the console. `sam-one` can also seed it on first boot
from a file (`--policy-file`). The document has two lists.

**Roles** name a set of permissions:

```json
{
  "name": "developer",
  "allowed_services": ["mcp://code-reviewer", "mcp://build-runner.*", "inference://*"],
  "allowed_targets": ["group:dev-nodes"],
  "allowed_labels": ["region=*"],
  "allowed_agents": [],
  "custom_datalog": []
}
```

- `allowed_services`: the services holders may call, as `type://name`. `*`
  can stand for the whole name, for a leading component (`mcp://*.internal`)
  or for a trailing one (`mcp://build-runner.*`). A bare `*` means every
  service.
- `allowed_targets`: the nodes holders may call, as facts about the
  destination node's identity: `node:<peer-id>`, `user:<sub>`,
  `email:<addr>`, `group:<name>`, `idp_role:<name>`, or `*`. If absent, any
  node may be called.
- `allowed_labels`: the labels a node with this role may declare at
  enrollment: `key=value`, `key=*` or `*`. If absent, the node may declare
  no labels.
- `allowed_agents`: the agent identifiers a node with this role may claim to
  act for. Only used by the sandboxed-agent
  [preview](../../preview/sandboxed-agents/).
- `custom_datalog`: extra Datalog facts or rules for holders of the role.

**Bindings** attach roles to identities:

```json
{ "role": "developer", "members": ["group:eng", "email:alice@example.com"] }
```

A member is one of: `user:`, `email:`, `group:` or `idp_role:` followed by a
value from the identity's OIDC claims; `node:` followed by a peer ID; or the
special value `sam:system:authenticated`, which matches every identity that
the identity provider authenticates. Be careful with the last one. On a
public identity provider it means everyone, so bind it only to roles with
few grants.

Roles are never members and never claims. `role:x` is not a valid member,
and an identity provider cannot give out a mesh role by putting it in a
`roles` claim. Such a claim becomes an `idp_role` fact, and a binding can
choose to honour it. The three built-in roles, `sam:role:node`,
`sam:role:router` and `sam:role:sambox`, follow the same rule: a binary can
only enroll if a binding gives its identity the role it needs.

The control plane validates a policy when it is posted. It rejects a policy
that references an undefined role, uses an unknown member prefix, or would
give a single identity more grants than the Biscuit authorizer can
evaluate.

## From policy to facts

At enrollment and at every refresh, the control plane resolves the identity's
roles from the bindings and writes the result into the credential as Datalog
facts: one `role(...)` fact per role, plus the grants compiled from the
lists of every role. For example, `allowed_services: ["mcp://calculator"]`
becomes `granted_service_exact("mcp", "calculator")`, `mcp://*` becomes
`granted_service_all("mcp")`, and `mcp://*.internal` becomes
`granted_service_suffix("mcp", ".internal")`. Wildcards keep their dot, so
`*.acme.example` matches `svc.acme.example` but not `evil-acme.example`.

The control plane also renders the policy as Datalog rules such as
`role("developer") <- group("eng")` and
`granted_service_exact("mcp", "calculator") <- role("developer")`. Nodes
fetch this text every `--control-plane-sync-interval` (15 minutes by default,
sooner when a policy update event reaches them) and add it to their
authorizer as it arrives. When a node verifies a credential, these rules run
against the identity facts in it. A grant added to the policy therefore
reaches every node within the sync interval, with no need to reissue
credentials. Removing a grant takes effect through the credential instead:
the facts already in a token stay valid until the token is refreshed, which
happens within its TTL (24 hours by default).

## What the hosting node checks

When a request for service `S` arrives from peer `P`, the node builds a
Biscuit authorizer and adds the following, in this order:

1. **The request**: `service("mcp", "calculator")` for the requested
   service, and `connection_peer_id(P)` from the authenticated connection.
   If the caller named an agent, the agent claim is added together with the
   check that the caller's own token grants that agent namespace. When the
   node handles the request as HTTP it adds `method("GET")` and
   `path("/v1/models")`, the path as the backend will see it; for a
   destination outside the mesh it adds `host(...)` and `port(...)`.
2. **The baseline checks**: `client_peer_id($id), connection_peer_id($id)`
   (the token belongs to the peer that presents it), and the expiration
   check against the current time.
3. **The node's own identity facts**, taken from its own credential, as
   `target_fact("group", "dev-nodes")` and similar. The caller's
   `allowed_targets` are matched against these facts. The destination node
   proves that it is an intended target; the origin node does not check its
   own traffic.
4. **The node's local rules**, from the `attenuation` block of its
   configuration file: extra facts, extra checks, and `allow` and `deny`
   policies.
5. **The baseline policies**: `allow if service($t,$n), granted_service_exact($t,$n)`
   and the equivalent policies for sets, prefixes, suffixes, per-type and
   global wildcards, plus the target check
   `allow_network_target(...) or target_unrestricted(true)`, and the rules
   that turn an [HTTP grant](../../reference/policy/#http-grants) into a
   service grant when the request's method and path match it.
6. **The synced mesh policy rules** described above.

Biscuit evaluates every `check` and requires all of them to pass. It then
walks the policies in order and applies the first `allow` or `deny` that
matches. As a result, a failing check denies the request regardless of any
policy. A local `deny` placed before the baseline `allow` overrides a grant.
A local `allow` can admit a caller that the policy did not grant, but it can
never admit a caller that fails a check.

Before any of this, the connection itself is gated. A peer on the ban list is
dropped at the transport layer, and a peer whose credential does not verify
under a trusted signing key cannot name a service at all.

## Why a caller cannot forge a fact

Two kinds of fact meet in the authorizer. The facts in the credential's
authority block were written and signed by the control plane: roles,
grants, labels, the peer the token is bound to. The facts about the request
(`service`, `method`, `path`, `host`, `port`, `agent`, `connection_peer_id`,
`time`) are added by the node that received the request, from what arrived
on the wire. A caller writes neither. It cannot change the authority block
without breaking the signature, and it cannot put a fact into the request
set because the node computes that set itself.

Biscuit does let a holder append a block to a token. That is how a token is
attenuated: a holder can add a check that narrows what the token does. The
facts of an appended block are visible only to that block's own checks and
never to the authorizer's policies, so an appended `role("admin")` grants
nothing. `internal/identity`'s
`TestAttenuationBlockFactsAreInvisibleToTheAuthorizer` pins this, and nodes
refuse an inbound token that carries appended blocks at all.

This is what lets the policy grow without a schema change. A requirement
that needs a new dimension is a new fact or a new rule, written in the same
language that the existing grants compile to. A role's `custom_datalog` can
mint `tier("contractor")` into every holder's credential, a node's
`attenuation` can then say `deny if tier("contractor"), method($m), !($m == "GET")`,
and neither `PolicyRole` nor any wire message changed. The structured fields
(`allowed_services`, `allowed_agents`, `http`, ...) are the common cases,
compiled to Datalog by the control plane; `custom_datalog` and
`attenuation` are the same engine written by hand.

## Local rules

The `attenuation` block in `sam-node.yaml` gives the hosting node the final
say. Use it for constraints that the operator of that node wants regardless
of what the mesh policy grants:

```yaml
attenuation:
  rules:
    - 'maintenance() <- time($t), $t > 2026-12-31T00:00:00Z;'
  checks:
    - 'check if label("jurisdiction", "eu");'        # every caller must carry this label
  policies:
    - 'deny if service("mcp", "db-writer"), group("contractors");'
    - 'deny if maintenance();'
```

`rules` derive new facts, `checks` must all hold, and `policies` are
evaluated before the baseline policies. A syntax error in any of them stops
the node at start, so a broken rule cannot weaken the node without notice.
The mobile app has the same block, with the same syntax, in its settings.

## Labels

Labels are `key=value` pairs. A node declares them in its configuration, and
the control plane writes them into the node's credential as `label(k, v)`
facts, one per label, if a role the node holds allows them. Labels let
policy describe where a node is or what it is for. SAM does not define a
fixed set of keys: `region`, `jurisdiction`, `team`, `compliance`, or
whatever the operator needs.

Labels are used in three places:

- **A provider restricting callers** adds `check if label("region", "eu")`
  to its `attenuation.checks`. Every caller's credential must then carry
  that label.
- **A caller choosing providers** sends `X-Sam-Required-Labels: region=eu`
  on the node's `/v1` inference endpoints or on a proxied A2A request, or
  passes `required_labels` to `call_remote_tool`. Before it sends any request
  data, the calling node fetches the provider's credential through the
  mutual handshake, verifies it, and confirms that the label facts are
  present. A provider that cannot show them is skipped.
- **An operator drawing a boundary** sets `egress.require_labels` in the
  node configuration. Every provider this node talks to must attest all of
  those labels, whether or not the caller asked for any. The caller can add
  further requirements but cannot remove the operator's.

The header and the operator floor have different matching rules, and the
difference follows from their purpose. The header is any-of: the caller is
choosing among acceptable providers. The floor is all-of: the operator is
drawing a line. Labels seen in discovery results are only used to rank
candidates. The only labels that authorize anything are the signed ones in a
credential.

## Agents acting through a node

A node may forward requests on behalf of a sandboxed agent and name that
agent to the destination. The name travels next to the token, not inside it.
The destination accepts the name only if the calling node's credential
grants that agent namespace through `allowed_agents`. A node with no such
grant cannot name any agent. This is attribution, not proof, and that is why
the namespace belongs to the node's role and not to the agent. The
[sandboxed agents preview](../../preview/sandboxed-agents/) has the details.

## See also

- [Policy reference](../../reference/policy/): every field, pattern and fact
  name.
- [Node configuration reference](../../reference/node-config/): the
  `attenuation`, `labels` and `egress` blocks.
- [Reaching services outside the mesh](../../guides/egress-destinations/):
  the node as a policy enforcement point for an application's outbound
  HTTP calls.
