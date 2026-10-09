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

// Service discovery as agentmesh-node does it (internal/node/service.go): a
// provider record in the mesh DHT under a CID derived from the service key.

import { CID } from "multiformats/cid";
import * as raw from "multiformats/codecs/raw";
import { sha256 } from "multiformats/hashes/sha2";

/** The DHT protocol, go-libp2p-kad-dht with dht.ProtocolPrefix("/mesh"). */
export const DHT_PROTOCOL = "/mesh/kad/1.0.0";

export type ServiceType = "mcp" | "inference" | "a2a" | "egress";

export function isServiceType(value: unknown): value is ServiceType {
  return value === "mcp" || value === "inference" || value === "a2a" || value === "egress";
}

/** The DHT key of a service: by type and name, or by type alone when name is omitted. */
export async function serviceCID(type: ServiceType, name?: string): Promise<CID> {
  const key = ["mesh:service", type, ...(name !== undefined && name !== "" ? [name] : [])].join(":");
  return CID.createV1(raw.code, await sha256.digest(new TextEncoder().encode(key)));
}

/** Splits "mcp://calculator" into its type and name; "" is the node's own catalog. */
export function parseServiceTarget(target: string): { type: ServiceType | "" ; name: string } {
  if (target === "") {
    return { type: "", name: "" };
  }
  const m = /^(mcp|inference|a2a|egress):\/\/(.+)$/.exec(target);
  if (!m) {
    throw new Error(`service target must look like mcp://<name>, got ${JSON.stringify(target)}`);
  }
  return { type: m[1] as ServiceType, name: m[2] as string };
}
