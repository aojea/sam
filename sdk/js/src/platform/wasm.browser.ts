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

// biscuit-wasm is built for bundlers: its entry imports the .wasm as an ES
// module, which the bundler of the page resolves (webpack's
// asyncWebAssembly, vite-plugin-wasm, or the esbuild plugin in
// scripts/bundle-browser.mjs). The module is loaded once the page first
// needs it.

export type BiscuitWasm = typeof import("@biscuit-auth/biscuit-wasm");

export async function loadBiscuitWasm(): Promise<BiscuitWasm> {
  return import("@biscuit-auth/biscuit-wasm");
}
