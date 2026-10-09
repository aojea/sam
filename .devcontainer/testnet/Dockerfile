# A mesh without a toolchain: the released agentmesh-one and agentmesh-node binaries are
# copied out of the published images. AGENTMESH_CHANNEL selects `stable` (the last
# release tag, what hub.__PROTECT_Agent Mesh_MESH_DEV__ runs) or `latest` (main, what
# bananas.__PROTECT_Agent Mesh_MESH_DEV__ runs).
ARG AGENTMESH_CHANNEL=stable
FROM ghcr.io/google/agentmesh-one:${AGENTMESH_CHANNEL} AS agentmesh-one
FROM ghcr.io/google/agentmesh-node:${AGENTMESH_CHANNEL} AS agentmesh-node

FROM mcr.microsoft.com/devcontainers/base:bookworm@sha256:3aacff4130e6cf04709f9cab1d7a6d3e1cc4bff6202bc61611831a18d3755673
COPY --from=agentmesh-one /agentmesh-one /usr/local/bin/agentmesh-one
COPY --from=agentmesh-node /agentmesh-node /usr/local/bin/agentmesh-node
