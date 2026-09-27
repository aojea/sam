# A mesh without a toolchain: the released sam-one and sam-node binaries are
# copied out of the published images. SAM_CHANNEL selects `stable` (the last
# release tag, what hub.sam-mesh.dev runs) or `latest` (main, what
# bananas.sam-mesh.dev runs).
ARG SAM_CHANNEL=stable
FROM ghcr.io/google/sam-one:${SAM_CHANNEL} AS sam-one
FROM ghcr.io/google/sam-node:${SAM_CHANNEL} AS sam-node

FROM mcr.microsoft.com/devcontainers/base:bookworm@sha256:3aacff4130e6cf04709f9cab1d7a6d3e1cc4bff6202bc61611831a18d3755673
COPY --from=sam-one /sam-one /usr/local/bin/sam-one
COPY --from=sam-node /sam-node /usr/local/bin/sam-node
