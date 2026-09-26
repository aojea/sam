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

// A mesh identity is an ed25519 key pair. Its peer ID is the one libp2p
// derives, so the same key works in the SDK, in sam-node and on the wire.
// The arithmetic is @noble/curves, the implementation libp2p itself uses,
// so an identity is built and used the same way in Node and in a browser.

import { ed25519 } from "@noble/curves/ed25519.js";
import { peerIdFromString } from "@libp2p/peer-id";
import { encodeBase58 } from "./base58.ts";
import { bytesEqual, concatBytes as concat } from "./bytes.ts";

const PUBLIC_KEY_SIZE = 32;
const SEED_SIZE = 32;
const SIGNATURE_SIZE = 64;

// libp2p crypto.proto PublicKey{Type: Ed25519 (1), Data: <32 bytes>}.
const LIBP2P_PUBLIC_KEY_PREFIX = Uint8Array.of(0x08, 0x01, 0x12, 0x20);
// libp2p crypto.proto PrivateKey{Type: Ed25519 (1), Data: <seed || public>}.
const LIBP2P_PRIVATE_KEY_PREFIX = Uint8Array.of(0x08, 0x01, 0x12, 0x40);
// multihash: identity function (0x00), digest length 36.
const IDENTITY_MULTIHASH_PREFIX = Uint8Array.of(0x00, 0x24);

function startsWith(bytes: Uint8Array, prefix: Uint8Array): boolean {
  return prefix.every((b, i) => bytes[i] === b);
}

// RFC 8032 verification, as Go crypto/ed25519 and node:crypto do it; the
// looser ZIP 215 rules noble defaults to accept signatures those reject.
function verifyRaw(publicKeyRaw: Uint8Array, data: Uint8Array, signature: Uint8Array): boolean {
  if (publicKeyRaw.length !== PUBLIC_KEY_SIZE || signature.length !== SIGNATURE_SIZE) {
    return false;
  }
  try {
    return ed25519.verify(signature, data, publicKeyRaw, { zip215: false });
  } catch {
    return false;
  }
}

/** The libp2p protobuf encoding of an ed25519 public key. */
export function libp2pPublicKey(publicKeyRaw: Uint8Array): Uint8Array {
  if (publicKeyRaw.length !== PUBLIC_KEY_SIZE) {
    throw new Error(`ed25519 public key must be ${PUBLIC_KEY_SIZE} bytes, got ${publicKeyRaw.length}`);
  }
  return concat(LIBP2P_PUBLIC_KEY_PREFIX, publicKeyRaw);
}

/** The peer ID libp2p derives from an ed25519 public key (base58btc, "12D3Koo..."). */
export function peerIdFromPublicKey(publicKeyRaw: Uint8Array): string {
  return encodeBase58(concat(IDENTITY_MULTIHASH_PREFIX, libp2pPublicKey(publicKeyRaw)));
}

/**
 * The base58btc form of a peer ID written in any encoding libp2p accepts
 * (base58btc multihash, CIDv1). Every key, ban set and comparison in SAM is
 * on this form, as peer.ID.String() in Go; a string read off the wire or
 * from a caller goes through here before it is used as one. Throws when the
 * text is not a peer ID at all.
 */
export function canonicalPeerId(text: string): string {
  try {
    return peerIdFromString(text).toString();
  } catch (err) {
    throw new Error(`${JSON.stringify(text)} is not a peer ID: ${err instanceof Error ? err.message : String(err)}`);
  }
}

export class Identity {
  readonly #seed: Uint8Array;
  /** Raw 32-byte ed25519 public key. */
  readonly publicKeyRaw: Uint8Array;
  readonly peerId: string;

  private constructor(seed: Uint8Array) {
    if (seed.length !== SEED_SIZE) {
      throw new Error(`ed25519 seed must be ${SEED_SIZE} bytes, got ${seed.length}`);
    }
    this.#seed = new Uint8Array(seed);
    this.publicKeyRaw = ed25519.getPublicKey(this.#seed);
    this.peerId = peerIdFromPublicKey(this.publicKeyRaw);
  }

  static generate(): Identity {
    const seed = new Uint8Array(SEED_SIZE);
    crypto.getRandomValues(seed);
    return new Identity(seed);
  }

  static fromSeed(seed: Uint8Array): Identity {
    return new Identity(seed);
  }

  /** Loads the libp2p protobuf private key encoding, the format sam-node persists. */
  static fromLibp2pPrivateKey(bytes: Uint8Array): Identity {
    if (bytes.length !== LIBP2P_PRIVATE_KEY_PREFIX.length + 64 || !startsWith(bytes, LIBP2P_PRIVATE_KEY_PREFIX)) {
      throw new Error("not a libp2p ed25519 private key");
    }
    const seed = bytes.subarray(LIBP2P_PRIVATE_KEY_PREFIX.length, LIBP2P_PRIVATE_KEY_PREFIX.length + SEED_SIZE);
    const pub = bytes.subarray(LIBP2P_PRIVATE_KEY_PREFIX.length + SEED_SIZE);
    const id = new Identity(seed);
    if (!bytesEqual(pub, id.publicKeyRaw)) {
      throw new Error("libp2p private key: public half does not match the seed");
    }
    return id;
  }

  /** The libp2p protobuf private key encoding (seed || public key). */
  toLibp2pPrivateKey(): Uint8Array {
    return concat(LIBP2P_PRIVATE_KEY_PREFIX, this.#seed, this.publicKeyRaw);
  }

  /** The libp2p protobuf public key encoding, what the control plane stores. */
  get libp2pPublicKey(): Uint8Array {
    return libp2pPublicKey(this.publicKeyRaw);
  }

  sign(data: Uint8Array): Uint8Array {
    return ed25519.sign(data, this.#seed);
  }

  verify(data: Uint8Array, signature: Uint8Array): boolean {
    return verifyRaw(this.publicKeyRaw, data, signature);
  }
}

/** Verifies an ed25519 signature with a raw 32-byte public key. */
export function verifyEd25519(publicKeyRaw: Uint8Array, data: Uint8Array, signature: Uint8Array): boolean {
  return verifyRaw(publicKeyRaw, data, signature);
}
