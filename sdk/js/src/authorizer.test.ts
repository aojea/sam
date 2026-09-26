// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// The provider authorizer against tokens minted the way the control plane
// mints them and policy rules rendered the way it renders them. The
// decisions here are the ones internal/node/middleware_test.go pins.

import assert from "node:assert/strict";
import { before, test } from "node:test";
import { AuthorizationError, authorizeCaller, type AuthorizeRequest } from "./authorizer.ts";
import { loadBiscuit } from "./biscuit.ts";
import { BASELINE_DATALOG } from "./gen/datalog.ts";

type Wasm = Awaited<ReturnType<typeof loadBiscuit>>;

let wasm: Wasm;
let cpKeyPair: InstanceType<Wasm["KeyPair"]>;
let otherKeyPair: InstanceType<Wasm["KeyPair"]>;
let cpKey: Uint8Array;

const PROVIDER = "12D3KooWProvider00000000000000000000000000000000000";
const CALLER = "12D3KooWCaller0000000000000000000000000000000000000";

function mint(facts: string[], keyPair = cpKeyPair, expiration = "2035-01-01T00:00:00Z"): Uint8Array {
  const b = wasm.Biscuit.builder();
  b.addFact(wasm.Fact.fromString(`expiration(${expiration})`));
  for (const f of facts) {
    b.addFact(wasm.Fact.fromString(f));
  }
  return b.build(keyPair.getPrivateKey()).toBytes();
}

/** A token as the control plane mints it for a node bound to peerId. */
function nodeToken(peerId: string, extra: string[] = [], keyPair = cpKeyPair): Uint8Array {
  return mint([`node(${JSON.stringify(peerId)})`, `client_peer_id(${JSON.stringify(peerId)})`, `role("sam:role:node")`, ...extra], keyPair);
}

before(async () => {
  wasm = await loadBiscuit();
  cpKeyPair = new wasm.KeyPair(wasm.SignatureAlgorithm.Ed25519);
  otherKeyPair = new wasm.KeyPair(wasm.SignatureAlgorithm.Ed25519);
  cpKey = new Uint8Array(Buffer.from(cpKeyPair.getPublicKey().toString().replace(/^ed25519\//, ""), "hex"));
});

function options(policyRules: string[], ownBiscuit?: Uint8Array) {
  return {
    trustedKeys: () => [cpKey],
    ownBiscuit: () => ownBiscuit ?? nodeToken(PROVIDER, [`granted_service_all_types(true)`, `target_unrestricted(true)`]),
    policyRules: () => policyRules,
  };
}

function request(biscuit: Uint8Array, targetService = "mcp://calc", agent?: string): AuthorizeRequest {
  const req: AuthorizeRequest = { biscuit, peerId: CALLER, targetService, protocol: "/sam/mcp/1.0.0" };
  if (agent !== undefined) {
    req.agent = agent;
  }
  return req;
}

// The role grants the service through the mesh policy rules, exactly as the
// control plane renders them; the token itself carries only the role.
const NODE_ROLE_GRANTS = [
  `granted_service_set("mcp", ["calc"]) <- role("sam:role:node")`,
  `target_unrestricted(true) <- role("sam:role:node")`,
];

test("a role granted the service by the mesh policy is allowed", async () => {
  const verified = await authorizeCaller(request(nodeToken(CALLER)), options(NODE_ROLE_GRANTS));
  assert.equal(verified.peerId, CALLER);
  assert.deepEqual(verified.roles, ["sam:role:node"]);
});

test("grants minted into the token are enough on their own", async () => {
  const token = nodeToken(CALLER, [`granted_service_exact("mcp", "calc")`, `target_unrestricted(true)`]);
  await authorizeCaller(request(token), options([]));
});

test("the empty target is the protocol in the system namespace", async () => {
  const token = nodeToken(CALLER, [`granted_service_all("sam:system")`, `target_unrestricted(true)`]);
  await authorizeCaller(request(token, ""), options([]));
  await assert.rejects(authorizeCaller(request(token, "mcp://calc"), options([])), AuthorizationError);
});

test("a service the role was not granted is denied, and the refusal names the check", async () => {
  await assert.rejects(
    authorizeCaller(request(nodeToken(CALLER), "mcp://other"), options(NODE_ROLE_GRANTS)),
    (err: unknown) => err instanceof AuthorizationError && /FailedLogic/.test(err.message) && !/\[object Object\]/.test(err.message),
  );
});

test("a role with no grants at all is denied", async () => {
  const guest = mint([`node(${JSON.stringify(CALLER)})`, `client_peer_id(${JSON.stringify(CALLER)})`, `role("sam:role:guest")`]);
  await assert.rejects(authorizeCaller(request(guest), options(NODE_ROLE_GRANTS)), AuthorizationError);
});

test("a token presented by a peer it is not bound to is denied", async () => {
  // Minted for someone else, replayed by CALLER over CALLER's connection.
  const stolen = nodeToken("12D3KooWVictim000000000000000000000000000000000000", [`granted_service_all_types(true)`, `target_unrestricted(true)`]);
  await assert.rejects(authorizeCaller(request(stolen), options([])), AuthorizationError);
});

test("client_peer_id must match the connection peer", async () => {
  // node() binds to CALLER but client_peer_id names another peer: the replay check fails.
  const token = mint([
    `node(${JSON.stringify(CALLER)})`,
    `client_peer_id("12D3KooWSomeoneElse000000000000000000000000000000")`,
    `granted_service_all_types(true)`,
    `target_unrestricted(true)`,
  ]);
  await assert.rejects(authorizeCaller(request(token), options([])), AuthorizationError);
});

test("an expired token is denied", async () => {
  const token = mint([`node(${JSON.stringify(CALLER)})`, `client_peer_id(${JSON.stringify(CALLER)})`, `granted_service_all_types(true)`, `target_unrestricted(true)`], cpKeyPair, "2020-01-01T00:00:00Z");
  await assert.rejects(authorizeCaller(request(token), options([])), AuthorizationError);
});

test("a token under an untrusted key is denied", async () => {
  const token = nodeToken(CALLER, [`granted_service_all_types(true)`, `target_unrestricted(true)`], otherKeyPair);
  await assert.rejects(authorizeCaller(request(token), options([])), AuthorizationError);
});

test("target grants are matched against the provider's own identity", async () => {
  const provider = nodeToken(PROVIDER, [`group("backend")`]);
  const rules = [`granted_service_all_types(true) <- role("sam:role:node")`, `target_restricted(true) <- role("sam:role:node")`];
  // Granted group:backend, which the provider carries: allowed.
  await authorizeCaller(request(nodeToken(CALLER, [`granted_target_set("group", ["backend"])`])), options(rules, provider));
  // Granted group:frontend only: the provider is not an intended target.
  await assert.rejects(
    authorizeCaller(request(nodeToken(CALLER, [`granted_target_set("group", ["frontend"])`])), options(rules, provider)),
    AuthorizationError,
  );
  // No target grant and no target_unrestricted: denied.
  await assert.rejects(authorizeCaller(request(nodeToken(CALLER)), options(rules, provider)), AuthorizationError);
});

test("an agent claim is accepted only inside a granted namespace", async () => {
  const rules = [...NODE_ROLE_GRANTS, `granted_agent_suffix(".acme.example") <- role("sam:role:node")`];
  await authorizeCaller(request(nodeToken(CALLER), "mcp://calc", "reviewer.acme.example"), options(rules));
  await assert.rejects(authorizeCaller(request(nodeToken(CALLER), "mcp://calc", "reviewer.evil.example"), options(rules)), AuthorizationError);
  // No agent grant at all: any claim is refused, no claim is fine.
  await assert.rejects(authorizeCaller(request(nodeToken(CALLER), "mcp://calc", "reviewer.acme.example"), options(NODE_ROLE_GRANTS)), AuthorizationError);
  await authorizeCaller(request(nodeToken(CALLER)), options(NODE_ROLE_GRANTS));
});

test("every baseline item parses in biscuit-wasm", () => {
  for (const c of [BASELINE_DATALOG.time_check, BASELINE_DATALOG.replay_check, BASELINE_DATALOG.target_check, BASELINE_DATALOG.agent_check]) {
    wasm.Check.fromString(c);
  }
  for (const r of [...BASELINE_DATALOG.rules, ...BASELINE_DATALOG.agent_rules, ...BASELINE_DATALOG.target_fact_rules]) {
    wasm.Rule.fromString(r);
  }
  for (const p of [...BASELINE_DATALOG.policies, BASELINE_DATALOG.allow_if_true]) {
    wasm.Policy.fromString(p);
  }
});
