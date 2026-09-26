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

// How a member on Node reaches the mesh: TCP and WebSocket, the transports
// the routers listen on. TLS 1.3 first, as every Go peer and the Python SDK
// offer it (internal/node/node.go); Noise accepted, so a member in a
// browser, which speaks Noise alone, is reached end to end through a relay.

import { noise } from "@chainsafe/libp2p-noise";
import { tcp } from "@libp2p/tcp";
import { tls } from "@libp2p/tls";
import { webSockets } from "@libp2p/websockets";
import type { Libp2pOptions } from "libp2p";

export function transports(): NonNullable<Libp2pOptions["transports"]> {
  return [tcp(), webSockets()];
}

export function connectionEncrypters(): NonNullable<Libp2pOptions["connectionEncrypters"]> {
  return [tls(), noise()];
}
