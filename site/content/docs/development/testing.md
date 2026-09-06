---
title: "Testing"
linkTitle: "Testing"
---
Current testing is intentionally minimal and aligned with the current binaries.

## Test Layers

1. Go tests: `make test`
2. BATS CLI tests: `make test-e2e`
3. Containerized BATS mesh tests: `make test-e2e-container`
4. Console UI tests: `make ui-test`

## Commands

```bash
make build
make test
make test-e2e
make test-e2e-container
make ui-test
```

## Go Tests

Run all Go tests with race detection:

```bash
make test
```

Run only integration package:

```bash
go test ./tests/integration/...
```

## BATS CLI Tests

These tests validate current command behavior for:

- `sam-node`
- `sam-control-plane`
- `sam-router`

Run:

```bash
make test-e2e
```

## Containerized Mesh BATS

The container framework is implemented in:

- `tests/e2e/lib/container_mesh.bash`

It starts:

1. mock OIDC container
2. `sam-control-plane` container
3. `sam-router` container
4. multiple `sam-node` containers

Run:

```bash
make test-e2e-container
```

Optional image override:

```bash
MESH_RUNTIME_IMAGE=sam-e2e-runtime:dev make test-e2e-container
```

## Console UI

Playwright drives the console in `tests/ui/console.spec.js`. The stack is built
in `tests/ui/lib/stack.sh` and runs entirely as local processes against a
throwaway sqlite database, so no docker, kind or postgres is involved.

```bash
make ui-test
make ui-test WHAT="policy editor"   # --grep a subset
```

To click around that same stack instead of asserting against it:

```bash
make ui-dev
```

That seeds a mesh policy, enrolls a router and a node with bootstrap tokens, then
leaves the console running with its URL and admin token printed. Only the OIDC
issuer is a stand-in: it serves a discovery document but cannot sign tokens,
which is why enrollment uses bootstrap tokens rather than a JWT.

The console serves its static assets from `internal/console/public`, so edits to
the HTML, CSS or JS need only a browser refresh; only Go changes need a rebuild.

## Troubleshooting

- Ensure Docker daemon is running before container tests.
- Ensure `bats` is installed and available in `PATH`.
- If a test fails, inspect containers:

```bash
docker ps -a
docker logs <container-name>
```
