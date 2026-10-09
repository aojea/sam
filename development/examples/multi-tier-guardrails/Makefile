# Targets for the Multi-Tier Guardrails & Quality Gates example.
#
#   make node-v1-staging   start Alice's support node in env=staging (node-v1.yaml)
#   make node-v1-prod      start Alice's support node promoted to env=prod

WORK_DIR ?= /tmp/sam-record-guardrails
CP_URL ?= http://127.0.0.1:18190
REPO_BIN := $(abspath ../../../bin)
AGENTMESH_ONE ?= $(if $(wildcard $(REPO_BIN)/agentmesh-one),$(REPO_BIN)/agentmesh-one,agentmesh-one)
AGENTMESH_NODE ?= $(if $(wildcard $(REPO_BIN)/agentmesh-node),$(REPO_BIN)/agentmesh-node,agentmesh-node)

.PHONY: node-v1-staging node-v1-prod

node-v1-staging:
	$(AGENTMESH_NODE) run --control-plane "$(CP_URL)" --insecure-control-plane \
	  --bootstrap-token-path "$(WORK_DIR)/dev-v1.token" \
	  --config node-v1.yaml --secrets-dir "$(WORK_DIR)/secrets" \
	  --data-dir "$(WORK_DIR)/node-v1" --bind-addr 127.0.0.1:19101 --socket-path="" \
	  --listen "/ip4/127.0.0.1/udp/0/quic-v1,/ip4/127.0.0.1/tcp/0" \
	  --api-token-path "$(WORK_DIR)/dev-api.token" \
	  --allow-loopback --control-plane-sync-interval 5s

node-v1-prod:
	$(AGENTMESH_NODE) run --control-plane "$(CP_URL)" --insecure-control-plane \
	  --bootstrap-token-path "$(WORK_DIR)/dev-v1-prod.token" \
	  --config "$(WORK_DIR)/node-v1-prod.yaml" --secrets-dir "$(WORK_DIR)/secrets" \
	  --data-dir "$(WORK_DIR)/node-v1" --bind-addr 127.0.0.1:19101 --socket-path="" \
	  --listen "/ip4/127.0.0.1/udp/0/quic-v1,/ip4/127.0.0.1/tcp/0" \
	  --api-token-path "$(WORK_DIR)/dev-api.token" \
	  --allow-loopback --control-plane-sync-interval 5s
