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

// biscuit-wasm is built for bundlers and imports its .wasm as a module,
// which Node only does behind a flag. Instantiating it by hand avoids the
// flag; the module's import list says what it needs.

import { readFile } from "node:fs/promises";
import { fileURLToPath } from "node:url";

export type BiscuitWasm = typeof import("@biscuit-auth/biscuit-wasm");

export async function loadBiscuitWasm(): Promise<BiscuitWasm> {
  const dir = new URL("./", import.meta.resolve("@biscuit-auth/biscuit-wasm"));
  const bindings = (await import(new URL("biscuit_bg.js", dir).href)) as BiscuitWasm & {
    __wbg_set_wasm(exports: WebAssembly.Exports): void;
  };
  const module = await WebAssembly.compile(await readFile(fileURLToPath(new URL("biscuit_bg.wasm", dir))));
  const imports: WebAssembly.Imports = {};
  for (const imp of WebAssembly.Module.imports(module)) {
    const ns = (imports[imp.module] ??= {}) as Record<string, WebAssembly.ImportValue>;
    if (imp.module === "./biscuit_bg.js") {
      ns[imp.name] = (bindings as unknown as Record<string, WebAssembly.ImportValue>)[imp.name] as WebAssembly.ImportValue;
    } else if (imp.name === "performance_now") {
      ns[imp.name] = () => performance.now();
    } else {
      throw new Error(`biscuit-wasm needs an unknown import ${imp.module}:${imp.name}`);
    }
  }
  const instance = await WebAssembly.instantiate(module, imports);
  bindings.__wbg_set_wasm(instance.exports);
  // The module's start hook logs a greeting to the console.
  const log = console.log;
  console.log = () => {};
  try {
    (instance.exports as { __wbindgen_start(): void }).__wbindgen_start();
  } finally {
    console.log = log;
  }
  return bindings;
}
