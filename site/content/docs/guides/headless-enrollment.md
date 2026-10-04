---
title: "Headless enrollment"
linkTitle: "Headless enrollment"
weight: 3
---

Servers, containers and routers cannot complete an interactive login: there
is no browser, and no person to log in. Such a machine enrolls in one of two
ways: with an OIDC token that it already holds, or with a bootstrap token
that an operator mints for it. This guide covers both, and then explains
what happens when the credential of such a node expires.

## With an OIDC token that the workload already has

If the platform gives the workload a token from an issuer that the control
plane trusts, no operator step is needed. On Kubernetes this is a projected
service account token with the right audience:

```yaml
volumes:
  - name: sam-token
    projected:
      sources:
        - serviceAccountToken:
            path: sam-token
            expirationSeconds: 3600
            audience: sam-mesh-audience
```

```bash
sam-node run --control-plane https://mesh.example.com --jwt-path /var/run/secrets/tokens/sam-token
```

The control plane must list the cluster's issuer in `--issuer` and the
audience in `--allowed-audiences`. The policy must bind the service account
(`user:system:serviceaccount:<namespace>:<name>`) to `sam:role:node`.
Routers enroll the same way with `sam-router --jwt-path`. The
[Kubernetes guide](../kubernetes/) shows the complete setup.

A workload with an OAuth client ID and secret can use the client-credentials
grant instead, with `--oidc-issuer`, `--client-id` and
`--client-secret-path` (or `SAM_CLIENT_SECRET`).

## With a bootstrap token

For a machine that has no identity of its own, an operator mints a token.

### 1. Mint

Call the control plane API with the admin token. `sam-one` prints the admin
token at start and writes it to its data directory. A separate
`sam-control-plane` reads it from the file given by `--admin-token-path` or
from the `SAM_ADMIN_TOKEN` environment variable.

```bash
curl -fsS -X POST https://mesh.example.com/admin/bootstrap-tokens \
  -H "Authorization: Bearer $SAM_ADMIN_TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"role":"sam:role:node","ttl_hours":24,"max_usages":1,"description":"build-runner-3"}'
```

```json
{"id":"62e92ffca…","token":"sam-bt-72fb0175788dee0…","role":"sam:role:node","expires_at":"2026-09-20T15:00:00Z"}
```

The plaintext `token` is shown once. The control plane keeps only its hash.
`role` is the role that the token enrolls into (`sam:role:router` for a
router). `ttl_hours` defaults to 24 and `max_usages` to 1. The Bootstrap
Tokens view in the console and `sam-one token create` do the same thing. An
OIDC user who is already enrolled can mint tokens for their own machines
through `POST /user/bootstrap-tokens` with their ID token. Such tokens
record the user as owner, and banning the user disables them.

Write the token to a file on the target machine. The node reads it from a
file and not from a flag, so it does not appear in process listings or in
the shell history.

### 2. Enroll

```bash
sam-node join https://mesh.example.com --bootstrap-token-path /etc/sam/bootstrap-token
```

or, to enroll and start in one step:

```bash
sam-node run --control-plane https://mesh.example.com --bootstrap-token-path /etc/sam/bootstrap-token
```

The node submits its public key, the token, and a signature over a fresh
challenge to `POST /enroll`. If the control plane runs with
`--auto-approve-enrollment` (the default in the Helm chart and in `sam-one`),
the credential comes back immediately. Otherwise the request is queued and
the node polls until an administrator decides.

### 3. Approve

```bash
curl -fsS https://mesh.example.com/admin/enrollments -H "Authorization: Bearer $SAM_ADMIN_TOKEN"
curl -fsS -X POST https://mesh.example.com/admin/enrollments/<request-id>/approve \
  -H "Authorization: Bearer $SAM_ADMIN_TOKEN"
```

`.../reject` refuses the request. The console lists pending requests with
the same two actions. Approval spends one usage of the token and checks again
that the token is still valid. If the token was revoked, expired or used up
between the request and the approval, the approval fails. It also fails if
the node declared a label that the token's role does not permit.

### Labels

A node enrolled with a bootstrap token declares labels in its configuration
file, like any other node, and the token's role must allow them through
`allowed_labels`. Approval by an administrator does not bypass this check.
The grant decides which labels a node may carry.

### Revoking a token

```bash
curl -fsS -X DELETE https://mesh.example.com/admin/bootstrap-tokens/<id> \
  -H "Authorization: Bearer $SAM_ADMIN_TOKEN"
```

The token stays in the list, marked as revoked, so the record of what it
enrolled is kept. Nodes that already enrolled with it are not affected. To
remove one of those, ban the node.

## When the credential expires

A node enrolled with a bootstrap token has no login to fall back on. Its
credential is refreshed automatically while it runs. But if the node is off
for longer than the control plane's key grace period (`--key-grace-period`,
one hour by default), it comes back with a credential signed by a retired
key, and `/refresh` refuses it. An SDK member in the same position enrolls
again on start when its `enroll` call is given a token, and otherwise
fails with `CredentialRetiredError` naming the state directory. There are
three ways out, in order of preference.

**Enroll again.** Mint a new token and run `sam-node join` with it. The
control plane already knows the peer ID, so it mints a new credential
directly instead of queueing a new approval, as long as the token's role
matches and the node is not banned. Each re-enrollment spends one token
usage, so `max_usages` limits how often a machine can be brought back this
way.

**Widen the grace period.** A longer `--key-grace-period` on the control
plane gives a quiet node more time to refresh. The trade-off is that a
machine that has been lost or stolen can keep renewing for the same time.

**Allow autonomous recovery.** A node whose record has
`autonomous_recovery` set may refresh on proof of its own key alone, even
after the signing key that issued its credential is gone. Set it on the token
so that every node enrolled with it inherits the flag, or set it on one node
afterwards:

```bash
# on the token, at mint time
-d '{"role":"sam:role:node","max_usages":10,"autonomous_recovery":true}'

# on an enrolled node
curl -fsS -X POST https://mesh.example.com/admin/nodes/<peer-id>/autonomous-recovery \
  -H "Authorization: Bearer $SAM_ADMIN_TOKEN" -H "Content-Type: application/json" \
  -d '{"enabled":true}'
```

This is off by default, because a node that can always recover holds a
credential that never expires in practice. Only a ban stops it. It suits
fleets where an operator round trip per stale node is impractical and the
machines are controlled in other ways.

## Banning

```bash
curl -fsS -X POST https://mesh.example.com/admin/revoke \
  -H "Authorization: Bearer $SAM_ADMIN_TOKEN" -H "Content-Type: application/json" \
  -d '{"peer_id":"12D3KooW…"}'
```

The node's next refresh fails and its daemon exits. Connected peers drop it
as soon as the ban event reaches them. `POST /admin/nodes/<peer-id>/unban`
reverses the ban. `sam-control-plane admin ban --peer <id>` and `unban` do
the same directly in the database, for cases where the API is not reachable.
