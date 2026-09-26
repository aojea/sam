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

// A member of the mesh in a browser page: the same Echo agent as
// a2a-agent.ts, answering with a fetch handler instead of an Express app,
// and the same caller as a2a-call.ts. Bundle it for the page with
//
//   npm run build && node scripts/bundle-browser.mjs examples/browser/app.js <outdir>
//
// and serve <outdir> together with index.html. The page's identity and
// credential are kept in IndexedDB, so a reload resumes the same peer
// without a token.

import { A2A_PROTOCOL_VERSION, AGENT_CARD_PATH, Message, Role } from "@a2a-js/sdk";
import { ClientFactory, DefaultAgentCardResolver, JsonRpcTransportFactory } from "@a2a-js/sdk/client";
import { AgentEvent, DefaultRequestHandler, InMemoryTaskStore, JsonRpcTransportHandler, ServerCallContext } from "@a2a-js/sdk/server";
import { AgentMesh, MeshSession } from "@sam-mesh/sdk";

const $ = (id) => document.getElementById(id);
const params = new URLSearchParams(location.search);
$("control-plane-url").value = params.get("controlPlaneUrl") ?? location.origin;
$("bootstrap-token").value = params.get("bootstrapToken") ?? "";
$("target-peer").value = params.get("peerId") ?? "";

let session;

function log(line) {
  $("log").textContent += line + "\n";
}

/** The Echo agent: answers every message with what it said and who sent it. */
class EchoExecutor {
  async execute(context, eventBus) {
    const said = context.userMessage.parts.map((p) => (p.content?.$case === "text" ? p.content.value : "")).join("");
    const caller = context.context.user?.userName ?? "someone";
    log(`message from ${caller}: ${said}`);
    eventBus.publish(
      AgentEvent.message({
        messageId: crypto.randomUUID(),
        contextId: context.contextId,
        taskId: "",
        role: Role.ROLE_AGENT,
        parts: [{ content: { $case: "text", value: `${caller} said: ${said}` }, filename: "", mediaType: "text/plain", metadata: undefined }],
        metadata: undefined,
        extensions: [],
        referenceTaskIds: [],
      }),
    );
    eventBus.finished();
  }

  async cancelTask() {}
}

function agentCard(url) {
  return {
    name: "Echo agent (browser)",
    description: "Answers every message with what it said and who sent it.",
    version: "1.0.0",
    supportedInterfaces: [{ url, protocolBinding: "JSONRPC", tenant: "", protocolVersion: A2A_PROTOCOL_VERSION }],
    provider: undefined,
    documentationUrl: "",
    capabilities: { streaming: true, pushNotifications: false, extendedAgentCard: false, extensions: [] },
    securitySchemes: {},
    securityRequirements: [],
    defaultInputModes: ["text/plain"],
    defaultOutputModes: ["text/plain"],
    skills: [{ id: "echo", name: "Echo", description: "Repeats the message.", tags: ["echo"], examples: ["hello"], inputModes: [], outputModes: [], securityRequirements: [] }],
    signatures: [],
    iconUrl: "",
  };
}

/**
 * The handler behind a2a://agent. The SDK has already authorized the caller;
 * `caller` is its verified biscuit, so the A2A user is the peer ID it is
 * bound to, never anything the request said about itself.
 */
function a2aHandler(card) {
  const transport = new JsonRpcTransportHandler(new DefaultRequestHandler(card, new InMemoryTaskStore(), new EchoExecutor()));
  return async (request, caller) => {
    const path = new URL(request.url).pathname;
    if (request.method === "GET" && path === `/${AGENT_CARD_PATH}`) {
      return Response.json(card);
    }
    if (request.method !== "POST") {
      return new Response("not found\n", { status: 404 });
    }
    const context = new ServerCallContext({ user: { isAuthenticated: true, userName: caller.peerId }, requestedVersion: request.headers.get("a2a-version") ?? undefined });
    const result = await transport.handle(await request.text(), context);
    if (Symbol.asyncIterator in result) {
      // message/stream: one SSE event per JSON-RPC response, as it is produced.
      const encoder = new TextEncoder();
      const body = new ReadableStream({
        async pull(controller) {
          const next = await result.next();
          if (next.done) {
            controller.close();
          } else {
            controller.enqueue(encoder.encode(`data: ${JSON.stringify(next.value)}\n\n`));
          }
        },
      });
      return new Response(body, { headers: { "content-type": "text/event-stream" } });
    }
    return Response.json(result);
  };
}

async function join(controlPlaneUrl, bootstrapToken) {
  $("status").textContent = "enrolling";
  const mesh = await AgentMesh.enroll({
    controlPlaneUrl,
    // Empty resumes the enrollment saved in IndexedDB under this name.
    ...(bootstrapToken !== "" ? { bootstrapToken } : {}),
    stateDir: "browser-agent",
    // A plaintext http:// control plane is otherwise accepted only on loopback.
    allowInsecure: params.get("allowInsecure") === "true",
  });
  $("status").textContent = "joining";
  session = await mesh.join();
  const url = MeshSession.meshURL(session.peerId, "a2a://agent");
  await session.acceptA2A({ handler: a2aHandler(agentCard(url)) });
  $("peer-id").textContent = session.peerId;
  $("status").textContent = `on the mesh, accepting a2a://agent`;
  window.addEventListener("pagehide", () => void session.close());
  return session.peerId;
}

async function call(peerId, text) {
  const fetchImpl = session.fetch();
  const factory = new ClientFactory({
    transports: [new JsonRpcTransportFactory({ fetchImpl })],
    cardResolver: new DefaultAgentCardResolver({ fetchImpl }),
  });
  const client = await factory.createFromUrl(MeshSession.meshURL(peerId, "a2a://agent") + "/");
  const card = await client.getAgentCard();
  const answer = await client.sendMessage({
    tenant: "",
    message: Message.fromJSON({ messageId: crypto.randomUUID(), role: Role[Role.ROLE_USER], parts: [{ text }] }),
    configuration: undefined,
    metadata: undefined,
  });
  const parts = "parts" in answer ? answer.parts : (answer.status?.message?.parts ?? []);
  const reply = parts.map((p) => (p.content?.$case === "text" ? p.content.value : "")).join("");
  $("answer").textContent = `${card.name}: ${reply}`;
  return reply;
}

$("join-form").addEventListener("submit", (event) => {
  event.preventDefault();
  join($("control-plane-url").value, $("bootstrap-token").value).catch((err) => {
    $("status").textContent = `failed: ${err.message}`;
  });
});

$("call-form").addEventListener("submit", (event) => {
  event.preventDefault();
  call($("target-peer").value, $("message").value).catch((err) => {
    $("answer").textContent = `failed: ${err.message}`;
  });
});

// For scripts and tests driving the page.
window.sam = { join, call, get session() { return session; } };
