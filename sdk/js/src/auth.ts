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

// The /mesh/auth/1.0.0 handshake, both sides. One varint-length-prefixed
// AuthFrame, one AuthResponse back; go-msgio framing on the Go side is
// the same unsigned varint prefix lpStream uses.

import { create, fromBinary, toBinary } from "@bufbuild/protobuf";
import type { Connection, Stream, StreamHandler } from "@libp2p/interface";
import { lpStream } from "@libp2p/utils";
import { BiscuitVerificationError, verifyPeerBiscuit, type VerifiedBiscuit } from "./biscuit.ts";
import { decodeAuthResponse } from "./credential.ts";
import { AuthFrameSchema, AuthResponseSchema } from "./gen/agentmesh_pb.ts";

export const AUTH_PROTOCOL = "/mesh/auth/1.0.0";
export const MCP_PROTOCOL = "/mesh/mcp/1.0.0";

/** The first frame on a stream is capped, as msgio.NewVarintReaderSize(s, 64 KiB). */
export const MAX_AUTH_FRAME_BYTES = 64 * 1024;
/** How long either side waits for the other's frame. */
export const AUTH_HANDSHAKE_TIMEOUT_MS = 10_000;

export class AuthRejectedError extends Error {
  constructor(peerId: string, reason: string) {
    super(`peer ${peerId} rejected the auth handshake: ${reason}`);
    this.name = "AuthRejectedError";
  }
}

function framed(stream: Stream) {
  return lpStream(stream, { maxDataLength: MAX_AUTH_FRAME_BYTES });
}

/**
 * Client side: presents frame on a new /mesh/auth/1.0.0 stream and returns
 * the peer's verified credential. trustedKeys are the control plane keys.
 */
export async function authenticateWithPeer(conn: Connection, frame: Uint8Array, trustedKeys: Uint8Array[]): Promise<VerifiedBiscuit> {
  const signal = AbortSignal.timeout(AUTH_HANDSHAKE_TIMEOUT_MS);
  // A peer behind a router is reached over a relayed, limited connection.
  const stream = await conn.newStream(AUTH_PROTOCOL, { signal, runOnLimitedConnection: true });
  try {
    const lp = framed(stream);
    await lp.write(frame, { signal });
    const resp = decodeAuthResponse((await lp.read({ signal })).subarray());
    if (!resp.success) {
      throw new AuthRejectedError(conn.remotePeer.toString(), resp.error || "no reason given");
    }
    if (resp.biscuit.length === 0) {
      throw new AuthRejectedError(conn.remotePeer.toString(), "empty credential in the mutual response");
    }
    // Discovery names whoever answered; only this says the peer is enrolled.
    return await verifyPeerBiscuit(resp.biscuit, conn.remotePeer.toString(), trustedKeys);
  } finally {
    await stream.close().catch(() => stream.abort(new Error("auth stream close failed")));
  }
}

export interface AuthServerOptions {
  /** This member's current biscuit, read per handshake so a refresh takes effect. */
  ownBiscuit(): Uint8Array;
  trustedKeys(): Uint8Array[];
  /** Peers the control plane has banned; a banned peer's frame gets no answer. */
  isBanned?(peerId: string): boolean;
  /** Called with every peer that passes; the session keeps the admitted set. */
  onAuthenticated?(peerId: string, verified: VerifiedBiscuit): void;
}

/** Options for libp2p.handle() so the handshake also runs over relayed connections. */
export const AUTH_HANDLER_OPTIONS = { runOnLimitedConnection: true };

/**
 * Server side of /mesh/auth/1.0.0, mirroring agentmesh-node's HandleAuthHandshake:
 * verify the caller's biscuit against the control plane keys and its
 * connection peer ID, then answer with our own. A failed verification gets
 * no answer, only a closed stream, as on the Go side.
 */
export function authStreamHandler(options: AuthServerOptions): StreamHandler {
  return async (stream: Stream, connection: Connection) => {
    const peerId = connection.remotePeer.toString();
    const signal = AbortSignal.timeout(AUTH_HANDSHAKE_TIMEOUT_MS);
    try {
      const lp = framed(stream);
      const frame = fromBinary(AuthFrameSchema, (await lp.read({ signal })).subarray());
      if (options.isBanned?.(peerId) === true) {
        stream.log?.("auth handshake from %s refused: peer is revoked", peerId);
        return;
      }
      let verified: VerifiedBiscuit;
      try {
        verified = await verifyPeerBiscuit(frame.biscuit, peerId, options.trustedKeys());
      } catch (err) {
        if (err instanceof BiscuitVerificationError) {
          stream.log?.("auth handshake from %s refused: %s", peerId, err.message);
          return;
        }
        throw err;
      }
      options.onAuthenticated?.(peerId, verified);
      await lp.write(toBinary(AuthResponseSchema, create(AuthResponseSchema, { success: true, biscuit: options.ownBiscuit() })), { signal });
    } finally {
      await stream.close().catch(() => stream.abort(new Error("auth stream close failed")));
    }
  };
}
