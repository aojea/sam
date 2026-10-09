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

// A fake control plane behind an injected fetch: enough of /enroll,
// /enroll/status, /register, /refresh and /keys to check what the client
// sends and how it treats every answer. The real one is exercised by
// tests/integration/sdk_enroll_test.go.

import { create, fromBinary, toBinary } from "@bufbuild/protobuf";
import { timestampFromMs } from "@bufbuild/protobuf/wkt";
import assert from "node:assert/strict";
import { test } from "node:test";
import {
  egressChallenge,
  enrollChallenge,
  enrollStatusChallenge,
  nodesCatalogChallenge,
  policiesChallenge,
  refreshChallenge,
  registerChallenge,
  revocationsChallenge,
} from "./challenges.ts";
import {
  ControlPlaneClient,
  ControlPlaneError,
  EnrollmentRejectedError,
  HEADER_CHALLENGE_SIGNATURE,
  HEADER_CHALLENGE_TIMESTAMP,
  InsecureControlPlaneURLError,
  KEYS_RESPONSE_FRESHNESS_MS,
  ROLE_NODE,
  STALE_CHALLENGE_TIMESTAMP_MESSAGE,
  validateControlPlaneURL,
  verifyKeysResponse,
} from "./controlplane.ts";
import {
  BootstrapEnrollRequestSchema,
  BootstrapEnrollResponseSchema,
  EnrollRequestSchema,
  EnrollResponseSchema,
  EnrollmentStatus,
  KeysResponseSchema,
  PolicyConfigGetResponseSchema,
  TokenRefreshRequestSchema,
  TokenRefreshResponseSchema,
  type KeysResponse,
} from "./gen/agentmesh_pb.ts";
import { Identity, verifyEd25519 } from "./identity.ts";

type Handler = (req: Request, body: Uint8Array) => Response | Promise<Response>;

function proto(bytes: Uint8Array, status = 200): Response {
  return new Response(Buffer.from(bytes), { status, headers: { "Content-Type": "application/x-protobuf" } });
}

function fakeFetch(routes: Record<string, Handler>): typeof fetch {
  return (async (input: Parameters<typeof fetch>[0], init?: RequestInit) => {
    const req = new Request(input, init);
    const path = new URL(req.url).pathname;
    const handler = routes[`${req.method} ${path}`];
    if (!handler) {
      return new Response(`no route for ${req.method} ${path}`, { status: 404 });
    }
    return handler(req, new Uint8Array(await req.arrayBuffer()));
  }) as typeof fetch;
}

/** Signs a KeysResponse the way api.SignKeysResponse does. */
function signedKeys(signers: Identity[], timestamp = Date.now()): KeysResponse {
  const unsigned = create(KeysResponseSchema, { publicKeys: signers.map((s) => s.publicKeyRaw), signTime: timestampFromMs(timestamp) });
  const payload = toBinary(KeysResponseSchema, unsigned);
  return create(KeysResponseSchema, { ...unsigned, signatures: signers.map((s) => s.sign(payload)) });
}

const cpKey = Identity.generate();
const biscuit = new TextEncoder().encode("not-really-a-biscuit");

test("validateControlPlaneURL: https always, http only to loopback unless allowed", () => {
  assert.equal(validateControlPlaneURL("https://hub.example").hostname, "hub.example");
  assert.equal(validateControlPlaneURL("http://127.0.0.1:8080").port, "8080");
  assert.equal(validateControlPlaneURL("http://localhost").hostname, "localhost");
  assert.equal(validateControlPlaneURL("http://[::1]:9").hostname, "[::1]");
  assert.throws(() => validateControlPlaneURL("http://hub.example"), InsecureControlPlaneURLError);
  assert.equal(validateControlPlaneURL("http://hub.example", true).hostname, "hub.example");
  assert.throws(() => validateControlPlaneURL("ftp://hub.example"), /must use http/);
  assert.throws(() => validateControlPlaneURL("not a url"), /invalid control plane URL/);
});

test("enrollBootstrap sends a bound proof of possession and polls until approved", async () => {
  const id = Identity.generate();
  const seen: string[] = [];
  const routerAddrs = ["/ip4/10.0.0.1/tcp/4001/p2p/12D3KooWP8iKhDf3iCMo2H3butNVfdTUtYwYWYQ75jTGnynXPFMp"];
  const fetch = fakeFetch({
    "POST /enroll": (req, body) => {
      assert.equal(req.headers.get("content-type"), "application/x-protobuf");
      const r = fromBinary(BootstrapEnrollRequestSchema, body);
      assert.equal(r.bootstrapToken, "sbt_secret");
      assert.equal(r.peerId, id.peerId);
      assert.deepEqual(r.publicKey, id.libp2pPublicKey);
      assert.equal(r.requestedRole, ROLE_NODE);
      assert.deepEqual(r.labels, { region: "eu" });
      assert.ok(Math.abs(Number(r.challengeUnixMs) - Date.now()) < 5000);
      assert.ok(verifyEd25519(id.publicKeyRaw, enrollChallenge(r.peerId, Number(r.challengeUnixMs)), r.challengeSignature));
      seen.push("enroll");
      return proto(toBinary(BootstrapEnrollResponseSchema, create(BootstrapEnrollResponseSchema, { status: EnrollmentStatus.PENDING, pollIntervalSeconds: 30 })));
    },
    "GET /enroll/status": (req) => {
      const url = new URL(req.url);
      assert.equal(url.searchParams.get("peer_id"), id.peerId);
      const ts = Number(req.headers.get(HEADER_CHALLENGE_TIMESTAMP));
      const sig = new Uint8Array(Buffer.from(req.headers.get(HEADER_CHALLENGE_SIGNATURE) ?? "", "base64url"));
      assert.ok(verifyEd25519(id.publicKeyRaw, enrollStatusChallenge(id.peerId, ts), sig));
      seen.push("status");
      // Still pending once, then approved.
      const approved = seen.filter((s) => s === "status").length > 1;
      return proto(
        toBinary(
          BootstrapEnrollResponseSchema,
          approved
            ? create(BootstrapEnrollResponseSchema, {
                status: EnrollmentStatus.APPROVED,
                biscuitToken: biscuit,
                controlPlanePublicKey: cpKey.publicKeyRaw,
                routerAddresses: routerAddrs,
                expireTime: timestampFromMs(1_800_000_000_000),
              })
            : create(BootstrapEnrollResponseSchema, { status: EnrollmentStatus.PENDING, pollIntervalSeconds: 30 }),
        ),
      );
    },
  });

  const client = new ControlPlaneClient({ url: "http://127.0.0.1:1", fetch });
  const enrollment = await client.enrollBootstrap({ identity: id, bootstrapToken: "sbt_secret", labels: { region: "eu" }, pollIntervalMs: 1 });
  assert.deepEqual(seen, ["enroll", "status", "status"]);
  assert.deepEqual(enrollment.biscuit, biscuit);
  assert.deepEqual(enrollment.controlPlanePublicKey, cpKey.publicKeyRaw);
  assert.deepEqual(enrollment.routerAddresses, routerAddrs);
  assert.equal(enrollment.expiration, 1_800_000_000);
});

test("enrollBootstrap surfaces a rejection and an HTTP error distinctly", async () => {
  const id = Identity.generate();
  const rejected = new ControlPlaneClient({
    url: "http://127.0.0.1:1",
    fetch: fakeFetch({
      "POST /enroll": () => proto(toBinary(BootstrapEnrollResponseSchema, create(BootstrapEnrollResponseSchema, { status: EnrollmentStatus.REJECTED, errorMessage: "Bootstrap token expired, revoked or exhausted" }))),
    }),
  });
  await assert.rejects(rejected.enrollBootstrap({ identity: id, bootstrapToken: "x" }), (err: unknown) => err instanceof EnrollmentRejectedError && /exhausted/.test(err.message));

  const limited = new ControlPlaneClient({
    url: "http://127.0.0.1:1",
    fetch: fakeFetch({ "POST /enroll": () => new Response("Rate limit exceeded", { status: 429 }) }),
  });
  await assert.rejects(limited.enrollBootstrap({ identity: id, bootstrapToken: "x" }), (err: unknown) => err instanceof ControlPlaneError && err.status === 429 && /Rate limit/.test(err.message));

  const empty = new ControlPlaneClient({
    url: "http://127.0.0.1:1",
    fetch: fakeFetch({
      "POST /enroll": () => proto(toBinary(BootstrapEnrollResponseSchema, create(BootstrapEnrollResponseSchema, { status: EnrollmentStatus.APPROVED, controlPlanePublicKey: cpKey.publicKeyRaw }))),
    }),
  });
  await assert.rejects(empty.enrollBootstrap({ identity: id, bootstrapToken: "x" }), /empty biscuit/);
});

test("enrollBootstrap stops polling when the caller aborts", async () => {
  const id = Identity.generate();
  const client = new ControlPlaneClient({
    url: "http://127.0.0.1:1",
    fetch: fakeFetch({
      "POST /enroll": () => proto(toBinary(BootstrapEnrollResponseSchema, create(BootstrapEnrollResponseSchema, { status: EnrollmentStatus.PENDING, pollIntervalSeconds: 30 }))),
    }),
  });
  const controller = new AbortController();
  const pending = client.enrollBootstrap({ identity: id, bootstrapToken: "x", signal: controller.signal });
  controller.abort(new Error("operator never came"));
  await assert.rejects(pending, /operator never came/);
});

test("register carries the JWT and the register-bound challenge", async () => {
  const id = Identity.generate();
  const client = new ControlPlaneClient({
    url: "http://127.0.0.1:1",
    fetch: fakeFetch({
      "POST /register": (_req, body) => {
        const r = fromBinary(EnrollRequestSchema, body);
        assert.equal(r.jwt, "eyJ.fake.jwt");
        assert.equal(r.peerId, id.peerId);
        assert.equal(r.requestedRole, "mesh:role:custom");
        assert.ok(verifyEd25519(id.publicKeyRaw, registerChallenge(r.peerId, Number(r.challengeUnixMs)), r.challengeSignature));
        return proto(toBinary(EnrollResponseSchema, create(EnrollResponseSchema, { biscuitToken: biscuit, controlPlanePublicKey: cpKey.publicKeyRaw, expireTime: timestampFromMs(7_000) })));
      },
    }),
  });
  const e = await client.register({ identity: id, jwt: "eyJ.fake.jwt", role: "mesh:role:custom" });
  assert.deepEqual(e.biscuit, biscuit);
  assert.equal(e.expiration, 7);

  const denied = new ControlPlaneClient({
    url: "http://127.0.0.1:1",
    fetch: fakeFetch({ "POST /register": () => proto(toBinary(EnrollResponseSchema, create(EnrollResponseSchema, { errorMessage: "role not bound" }))) }),
  });
  await assert.rejects(denied.register({ identity: id, jwt: "j" }), EnrollmentRejectedError);
});

test("refresh presents the biscuit as a bearer and signs the refresh challenge", async () => {
  const id = Identity.generate();
  const fresh = new TextEncoder().encode("fresher-biscuit");
  const client = new ControlPlaneClient({
    url: "http://127.0.0.1:1",
    fetch: fakeFetch({
      "POST /refresh": (req, body) => {
        assert.equal(req.headers.get("authorization"), `Bearer ${Buffer.from(biscuit).toString("base64")}`);
        const r = fromBinary(TokenRefreshRequestSchema, body);
        assert.equal(r.peerId, id.peerId);
        assert.ok(verifyEd25519(id.publicKeyRaw, refreshChallenge(id.peerId, Number(r.challengeUnixMs)), r.challengeSignature));
        return proto(toBinary(TokenRefreshResponseSchema, create(TokenRefreshResponseSchema, { biscuitToken: fresh, expireTime: timestampFromMs(99_000) })));
      },
    }),
  });
  const result = await client.refresh({ identity: id, biscuit });
  assert.deepEqual(result.biscuit, fresh);
  assert.equal(result.expiration, 99);

  const replayed = new ControlPlaneClient({
    url: "http://127.0.0.1:1",
    fetch: fakeFetch({ "POST /refresh": () => new Response("Biscuit already rotated", { status: 401 }) }),
  });
  await assert.rejects(replayed.refresh({ identity: id, biscuit }), (err: unknown) => err instanceof ControlPlaneError && err.status === 401);
});

test("policyRules presents the biscuit and signs the policies challenge", async () => {
  const id = Identity.generate();
  const rules = ['granted_service("mcp", "echo") <- role("mesh:role:node")'];
  const client = new ControlPlaneClient({
    url: "http://127.0.0.1:1",
    fetch: fakeFetch({
      "GET /policies": (req) => {
        assert.equal(req.headers.get("authorization"), `Bearer ${Buffer.from(biscuit).toString("base64")}`);
        const ts = Number(req.headers.get(HEADER_CHALLENGE_TIMESTAMP));
        const sig = new Uint8Array(Buffer.from(req.headers.get(HEADER_CHALLENGE_SIGNATURE) ?? "", "base64url"));
        assert.ok(verifyEd25519(id.publicKeyRaw, policiesChallenge(id.peerId, ts), sig));
        return proto(toBinary(PolicyConfigGetResponseSchema, create(PolicyConfigGetResponseSchema, { datalogRules: rules })));
      },
    }),
  });
  assert.deepEqual(await client.policyRules(id, biscuit), rules);
  assert.deepEqual(await client.policyRules({ identity: id, biscuit }), rules);

  // Domain-separated challenge payloads match api/network.go.
  const dec = new TextDecoder();
  assert.equal(dec.decode(policiesChallenge("peer1", 42)), "mesh:policies:peer1:42");
  assert.equal(dec.decode(egressChallenge("peer1", 42)), "mesh:egress:peer1:42");
  assert.equal(dec.decode(revocationsChallenge("peer1", 42)), "mesh:revocations:peer1:42");
  assert.equal(dec.decode(nodesCatalogChallenge("peer1", 42)), "mesh:nodes-catalog:peer1:42");
});

test("clock skew: +10 min client clock retries once using response Date header; other 401s are not retried", async () => {
  const id = Identity.generate();
  const serverNowSec = Math.floor(Date.now() / 1000);
  const serverNowMs = serverNowSec * 1000;
  const skewedClientNowMs = serverNowMs + 10 * 60 * 1000;
  const dateHeader = new Date(serverNowMs).toUTCString();
  const fresh = new TextEncoder().encode("fresher-biscuit");
  const rules = ['granted_service("mcp", "echo") <- true'];

  let refreshCalls = 0;
  let policyCalls = 0;
  const skewedClient = new ControlPlaneClient({
    url: "http://127.0.0.1:1",
    now: () => skewedClientNowMs,
    fetch: fakeFetch({
      "POST /refresh": (_req, body) => {
        refreshCalls++;
        const r = fromBinary(TokenRefreshRequestSchema, body);
        const ts = Number(r.challengeUnixMs);
        assert.ok(verifyEd25519(id.publicKeyRaw, refreshChallenge(id.peerId, ts), r.challengeSignature));
        if (Math.abs(serverNowMs - ts) > 5 * 60 * 1000) {
          return new Response(STALE_CHALLENGE_TIMESTAMP_MESSAGE, { status: 401, headers: { Date: dateHeader } });
        }
        return proto(toBinary(TokenRefreshResponseSchema, create(TokenRefreshResponseSchema, { biscuitToken: fresh, expireTime: timestampFromMs(99_000) })));
      },
      "GET /policies": (req) => {
        policyCalls++;
        const ts = Number(req.headers.get(HEADER_CHALLENGE_TIMESTAMP));
        const sig = new Uint8Array(Buffer.from(req.headers.get(HEADER_CHALLENGE_SIGNATURE) ?? "", "base64url"));
        assert.ok(verifyEd25519(id.publicKeyRaw, policiesChallenge(id.peerId, ts), sig));
        if (Math.abs(serverNowMs - ts) > 5 * 60 * 1000) {
          return new Response(STALE_CHALLENGE_TIMESTAMP_MESSAGE, { status: 401, headers: { Date: dateHeader } });
        }
        return proto(toBinary(PolicyConfigGetResponseSchema, create(PolicyConfigGetResponseSchema, { datalogRules: rules })));
      },
    }),
  });

  const refreshed = await skewedClient.refresh({ identity: id, biscuit });
  assert.deepEqual(refreshed.biscuit, fresh);
  assert.equal(refreshCalls, 2);

  assert.deepEqual(await skewedClient.policyRules(id, biscuit), rules);
  assert.equal(policyCalls, 2);

  // Other 401s (even with a Date header) are not retried.
  let nonStaleCalls = 0;
  const nonStaleClient = new ControlPlaneClient({
    url: "http://127.0.0.1:1",
    now: () => skewedClientNowMs,
    fetch: fakeFetch({
      "GET /policies": () => {
        nonStaleCalls++;
        return new Response("invalid challenge signature", { status: 401, headers: { Date: dateHeader } });
      },
    }),
  });
  await assert.rejects(nonStaleClient.policyRules(id, biscuit), (err: unknown) => err instanceof ControlPlaneError && err.status === 401);
  assert.equal(nonStaleCalls, 1);
});

test("verifyKeysResponse accepts a set vouched for by a trusted key and nothing else", () => {
  const retiring = Identity.generate();
  const resp = signedKeys([cpKey, retiring]);

  // Trusting either key yields the whole set.
  assert.deepEqual(verifyKeysResponse(resp, [cpKey.publicKeyRaw]), [cpKey.publicKeyRaw, retiring.publicKeyRaw]);
  assert.deepEqual(verifyKeysResponse(resp, [retiring.publicKeyRaw]), [cpKey.publicKeyRaw, retiring.publicKeyRaw]);

  assert.throws(() => verifyKeysResponse(resp, []), /no trusted control plane key/);
  assert.throws(() => verifyKeysResponse(resp, [Identity.generate().publicKeyRaw]), /not signed by any trusted/);
  assert.throws(() => verifyKeysResponse(resp, [cpKey.publicKeyRaw], Date.now() + KEYS_RESPONSE_FRESHNESS_MS + 1000), /freshness window/);

  const forged = create(KeysResponseSchema, { ...resp, publicKeys: [Identity.generate().publicKeyRaw, retiring.publicKeyRaw] });
  assert.throws(() => verifyKeysResponse(forged, [retiring.publicKeyRaw]), /not signed by any trusted/);

  const unsigned = create(KeysResponseSchema, { publicKeys: resp.publicKeys, signTime: resp.signTime });
  assert.throws(() => verifyKeysResponse(unsigned, [cpKey.publicKeyRaw]), /carries 0 signatures for 2 keys/);
});

test("keys() fetches and verifies against the enrollment key", async () => {
  const client = new ControlPlaneClient({
    url: "http://127.0.0.1:1",
    fetch: fakeFetch({ "GET /keys": () => proto(toBinary(KeysResponseSchema, signedKeys([cpKey]))) }),
  });
  assert.deepEqual(await client.keys([cpKey.publicKeyRaw]), [cpKey.publicKeyRaw]);
  await assert.rejects(client.keys([Identity.generate().publicKeyRaw]), /not signed by any trusted/);
});

test("an injected fetch is called unbound, as a browser's window.fetch requires", async () => {
  const inner = fakeFetch({ "GET /keys": () => proto(toBinary(KeysResponseSchema, signedKeys([cpKey]))) });
  let receiver: unknown = "unset";
  const strict = function (this: unknown, input: Parameters<typeof fetch>[0], init?: RequestInit) {
    receiver = this;
    return inner(input, init);
  } as typeof fetch;
  const client = new ControlPlaneClient({ url: "http://127.0.0.1:1", fetch: strict });
  await client.keys([cpKey.publicKeyRaw]);
  assert.equal(receiver, undefined);
});

