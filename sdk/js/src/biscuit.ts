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

// Verification of a peer's biscuit, mirroring internal/identity.verifyBiscuit:
// signed by a trusted control plane key, authority block only, unexpired,
// and bound to the peer at the other end of the connection.

import { loadBiscuitWasm, type BiscuitWasm } from "./platform/wasm.ts";

let loading: Promise<BiscuitWasm> | undefined;

/** The biscuit-wasm module, loaded once, the way the runtime loads it. */
export function loadBiscuit(): Promise<BiscuitWasm> {
  loading ??= loadBiscuitWasm();
  return loading;
}

/** Datalog evaluation budget, as in internal/identity.AuthorizerOptions. */
export const AUTHORIZER_LIMITS = { max_facts: 1000, max_iterations: 100, max_time_micro: 1_000_000 };

export const ROLE_ROUTER = "sam:role:router";

export class BiscuitVerificationError extends Error {
  constructor(message: string, options?: ErrorOptions) {
    super(message, options);
    this.name = "BiscuitVerificationError";
  }
}

/** What a verified peer biscuit says about its holder. */
export interface VerifiedBiscuit {
  /** The peer the token is bound to (its node() fact). */
  peerId: string;
  /** When the token lapses; the earliest expiration() fact. */
  expiration: Date;
  /** The trusted key that verified the signature. */
  verifyingKey: Uint8Array;
  roles: string[];
  labels: Record<string, string>;
}

function describe(err: unknown): string {
  if (err instanceof Error) {
    return err.message;
  }
  try {
    return JSON.stringify(err);
  } catch {
    return String(err);
  }
}

type QueriedFact = { terms(): unknown[] };

/**
 * Verifies a biscuit received from expectedPeerId over an authenticated
 * connection. Every trusted key is tried, so a token minted under a
 * retiring key still verifies during rotation.
 */
export async function verifyPeerBiscuit(
  biscuitBytes: Uint8Array,
  expectedPeerId: string,
  trustedKeys: Uint8Array[],
  now: Date = new Date(),
): Promise<VerifiedBiscuit> {
  const wasm = await loadBiscuit();
  if (trustedKeys.length === 0) {
    throw new BiscuitVerificationError("no trusted control plane key to verify against");
  }

  let token: ReturnType<BiscuitWasm["Biscuit"]["fromBytes"]> | undefined;
  let verifyingKey: Uint8Array | undefined;
  let lastErr: unknown;
  for (const key of trustedKeys) {
    try {
      token = wasm.Biscuit.fromBytes(biscuitBytes, wasm.PublicKey.fromBytes(key, wasm.SignatureAlgorithm.Ed25519));
      verifyingKey = key;
      break;
    } catch (err) {
      lastErr = err;
    }
  }
  if (!token || !verifyingKey) {
    throw new BiscuitVerificationError(`biscuit is not signed by a trusted control plane key: ${describe(lastErr)}`);
  }

  // Appending needs no root key, so appended blocks are the one place a
  // holder can put Datalog of their own. SAM tokens are authority-only.
  if (token.countBlocks() !== 1) {
    throw new BiscuitVerificationError(`biscuit carries appended blocks; SAM tokens are authority-block only (${token.countBlocks() - 1})`);
  }

  const builder = new wasm.AuthorizerBuilder();
  builder.addFact(wasm.Fact.fromString(`time(${now.toISOString().replace(/\.\d{3}Z$/, "Z")})`));
  builder.addCheck(wasm.Check.fromString("check if time($t), expiration($e), $t <= $e"));
  builder.addPolicy(wasm.Policy.fromString("allow if true"));
  const authorizer = builder.buildAuthenticated(token);
  try {
    authorizer.authorizeWithLimits(AUTHORIZER_LIMITS);
  } catch (err) {
    throw new BiscuitVerificationError(`biscuit is expired or fails its checks: ${describe(err)}`);
  }

  const query = (rule: string) => authorizer.queryWithLimits(wasm.Rule.fromString(rule), AUTHORIZER_LIMITS) as QueriedFact[];
  const strings = (facts: QueriedFact[]) => facts.map((f) => f.terms()[0]).filter((t): t is string => typeof t === "string");

  const bound = strings(query("p($p) <- node($p)"));
  if (!bound.includes(expectedPeerId)) {
    throw new BiscuitVerificationError(`biscuit is not bound to peer ${expectedPeerId}`);
  }

  const expirations = query("e($e) <- expiration($e)")
    .map((f) => f.terms()[0])
    .filter((t): t is Date => t instanceof Date && !Number.isNaN(t.getTime()));
  if (expirations.length === 0) {
    throw new BiscuitVerificationError("biscuit carries no expiration fact");
  }
  const expiration = new Date(Math.min(...expirations.map((d) => d.getTime())));

  const labels: Record<string, string> = {};
  for (const f of query("l($k, $v) <- label($k, $v)")) {
    const [k, v] = f.terms();
    if (typeof k === "string" && typeof v === "string") {
      labels[k] = v;
    }
  }

  return { peerId: expectedPeerId, expiration, verifyingKey, roles: strings(query("r($r) <- role($r)")), labels };
}

/** Requires role(<role>) on an already verified token, as identity.RequireRole. */
export function requireRole(verified: VerifiedBiscuit, role: string): void {
  if (!verified.roles.includes(role)) {
    throw new BiscuitVerificationError(`biscuit lacks expected role ${JSON.stringify(role)}`);
  }
}
