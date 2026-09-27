# The admin's side of the demo: a mesh on a public URL, one node in the
# office fronting a model, an MCP server and api.github.com, and the two
# tokens that admit the node and the agent. Each server target runs in the
# foreground; give it its own terminal.
#
#   make ollama                     the office model, in Docker
#   make mcp                        the MCP reference server on :3001
#   make mesh                       sam-one on a public https URL (banner prints it)
#   make tokens URL=https://...     one token for the node, one for the agent
#   make pep URL=https://...        the node, with the GitHub credential on its host
#   make revoke URL=https://...     take api.github.com off the mesh
#   make ban URL=... PEER=12D3...   cut one member off
#   make clean                      forget everything

DEMO_DIR ?= $(HOME)/sam-demo
REPO_BIN := $(abspath ../../../bin)
SAM_ONE ?= $(if $(wildcard $(REPO_BIN)/sam-one),$(REPO_BIN)/sam-one,sam-one)
SAM_NODE ?= $(if $(wildcard $(REPO_BIN)/sam-node),$(REPO_BIN)/sam-node,sam-node)
# The read-only GitHub token the node presents. Never a flag value: a file.
GITHUB_TOKEN_FILE ?= $(HOME)/.config/sam-demo/github-ro
MODEL ?= gemma3:1b
PORT ?= 8090

.PHONY: ollama mcp mesh tokens secrets pep policy revoke ban clean

ollama:
	docker start sam-demo-ollama 2>/dev/null || \
	  docker run -d --name sam-demo-ollama -p 127.0.0.1:11434:11434 -v sam-demo-ollama:/root/.ollama ollama/ollama >/dev/null
	@for _ in $$(seq 1 30); do curl -sf http://127.0.0.1:11434/ >/dev/null && break; sleep 1; done
	docker exec sam-demo-ollama ollama pull $(MODEL)
	curl -s http://127.0.0.1:11434/v1/models

mcp:
	npx -y @modelcontextprotocol/server-everything streamableHttp

mesh:
	$(SAM_ONE) --data-dir "$(DEMO_DIR)/one" --port $(PORT) \
	  --tunnel cloudflare --tunnel-install \
	  --policy-file policy.json --no-join-token --enroll-qr=false --log-level warn

tokens:
	@test -n "$(URL)" || { echo "make tokens URL=https://... (the API URL from the sam-one banner)"; exit 1; }
	@mkdir -p "$(DEMO_DIR)"
	@$(SAM_ONE) token create --server "$(URL)" --data-dir "$(DEMO_DIR)/one" \
	  --max-usages 1 --description "office node" 2>/dev/null \
	  | awk '/^Token:/ {print $$2}' > "$(DEMO_DIR)/pep-token"
	@$(SAM_ONE) token create --server "$(URL)" --data-dir "$(DEMO_DIR)/one" \
	  --role agent --max-usages 1 --description "agent in the dev sandbox" 2>/dev/null \
	  | awk '/^Token:/ {print $$2}' > "$(DEMO_DIR)/agent-token"
	@test -s "$(DEMO_DIR)/pep-token" -a -s "$(DEMO_DIR)/agent-token" || { echo "token create failed"; exit 1; }
	@echo "tokens in $(DEMO_DIR)/pep-token (role sam:role:node) and $(DEMO_DIR)/agent-token (role agent)"

secrets:
	@test -s "$(GITHUB_TOKEN_FILE)" || { echo "GITHUB_TOKEN_FILE=$(GITHUB_TOKEN_FILE) is missing: a read-only fine-grained token for google/sam"; exit 1; }
	@mkdir -p "$(DEMO_DIR)/secrets"
	@install -m 600 "$(GITHUB_TOKEN_FILE)" "$(DEMO_DIR)/secrets/github-ro"
	@echo "credential github-ro in $(DEMO_DIR)/secrets"

pep: secrets
	@test -n "$(URL)" || { echo "make pep URL=https://..."; exit 1; }
	$(SAM_NODE) run --control-plane "$(URL)" \
	  --bootstrap-token-path "$(DEMO_DIR)/pep-token" \
	  --config pep.yaml --secrets-dir "$(DEMO_DIR)/secrets" \
	  --data-dir "$(DEMO_DIR)/pep" --bind-addr= \
	  --control-plane-sync-interval 30s

# The admin token: the one sam-one generated into the data dir, unless the
# environment supplies it (then the banner does not show it either).
ADMIN_TOKEN = $${SAM_ADMIN_TOKEN:-$$(cat "$(DEMO_DIR)/one/admin-token")}

policy:
	@test -n "$(URL)" || { echo "make policy URL=https://..."; exit 1; }
	@echo "POST policy.json to $(URL)/policies"
	@curl -sS -X POST "$(URL)/policies" \
	  -H "Authorization: Bearer $(ADMIN_TOKEN)" \
	  -H 'Content-Type: application/json' --data @policy.json; echo

revoke:
	@test -n "$(URL)" || { echo "make revoke URL=https://..."; exit 1; }
	@echo "POST policy.json without its egress section to $(URL)/policies"
	@jq 'del(.egress)' policy.json | curl -sS -X POST "$(URL)/policies" \
	  -H "Authorization: Bearer $(ADMIN_TOKEN)" \
	  -H 'Content-Type: application/json' --data @-; echo

ban:
	@test -n "$(URL)" -a -n "$(PEER)" || { echo "make ban URL=https://... PEER=12D3KooW..."; exit 1; }
	@$(SAM_ONE) admin ban "$(PEER)" --server "$(URL)" --data-dir "$(DEMO_DIR)/one"

clean:
	rm -rf "$(DEMO_DIR)"
	docker rm -f sam-demo-ollama >/dev/null 2>&1 || true
