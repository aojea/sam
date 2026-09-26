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

// How a member in a browser reaches the mesh: WebSocket, the one transport a
// page can open to a router (sam-one's single port, or a router behind a
// TLS-terminating proxy as wss), secured with Noise. A browser cannot run
// libp2p's TLS, which needs a self-signed certificate over a raw socket;
// routers and nodes accept Noise beside TLS, and both bind the connection to
// the peer ID.

import { noise } from "@chainsafe/libp2p-noise";
import { webSockets } from "@libp2p/websockets";
import type { Libp2pOptions } from "libp2p";

export function transports(): NonNullable<Libp2pOptions["transports"]> {
  return [webSockets()];
}

export function connectionEncrypters(): NonNullable<Libp2pOptions["connectionEncrypters"]> {
  return [noise()];
}
