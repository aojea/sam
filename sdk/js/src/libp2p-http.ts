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

// The /libp2p-http protocol (go-libp2p-http): the client that calls inference
// and A2A services on the mesh, and the ingress that accepts A2A requests for
// this member's own agent, gated by the authorizer the way sam-node gates its
// ingress (StartIngressServer in internal/node). Node's own HTTP server and
// client run over the libp2p stream, so bodies stream in both directions and
// an A2A message/stream (SSE) works.

import type { Connection, Stream, StreamHandler } from "@libp2p/interface";
import http from "node:http";
import { Duplex, Readable } from "node:stream";
import { AUTH_HANDSHAKE_TIMEOUT_MS } from "./auth.ts";
import { AuthorizationError, authorizeCaller, type ProviderAuthorizerOptions } from "./authorizer.ts";
import type { VerifiedBiscuit } from "./biscuit.ts";
import { canonicalPeerId } from "./identity.ts";

/** go-libp2p-http's protocol: plain HTTP/1.1 on a stream, one request per stream. */
export const HTTP_PROTOCOL = "/libp2p-http";

/** Headers of the mesh HTTP datapath (api/network.go). */
export const HEADER_SAM_BISCUIT = "x-sam-biscuit";
export const HEADER_SAM_AGENT = "x-sam-agent";
export const HEADER_PEER_ID = "x-peer-id";
export const HEADER_SAM_NO_TRAILING_SLASH = "x-sam-no-trailing-slash";

/** Options for libp2p.handle() so the agent is reachable over relayed connections. */
export const HTTP_HANDLER_OPTIONS = { runOnLimitedConnection: true };

/** The service name an agent answers under unless it picks another. */
export const DEFAULT_A2A_NAME = "agent";

/**
 * The path prefix of a mesh URL, http://mesh/sam/<peer-id>/<type>/<name>/<path>:
 * the shape of sam-node's egress proxy and of an agent card it rewrote. The
 * host is ignored; the peer ID is in the path because URL parsers lowercase
 * the host and a peer ID is case-sensitive.
 */
export const MESH_PATH_PREFIX = "/sam/";

const MAX_INGRESS_BODY_BYTES = 8 * 1024 * 1024;
const REQUEST_TIMEOUT_MS = 60_000;

/** A fetch-style handler in this process. */
export type HTTPHandler = (request: Request, caller: VerifiedBiscuit) => Promise<Response> | Response;

/** A Node request listener in this process, such as an Express app with the A2A SDK's handlers mounted. */
export type NodeRequestListener = (req: http.IncomingMessage, res: http.ServerResponse) => void;

/**
 * This member's agent as other members reach it: `a2a://<name>`, answered by
 * exactly one of url (an A2A server beside this process), handler or
 * listener (in this process). Authorized requests arrive with the biscuit and
 * agent headers stripped, X-Peer-Id naming the verified caller and the path
 * relative to /a2a/<name>, as sam-node forwards them. The endpoint is not
 * announced anywhere; a caller reaches it by peer ID.
 */
export interface A2AEndpointSpec {
  name?: string;
  url?: string;
  handler?: HTTPHandler;
  listener?: NodeRequestListener;
}

export interface A2AEndpoint {
  name: string;
  service: string;
  target: { url: string } | { handler: HTTPHandler } | { listener: NodeRequestListener };
}

export function a2aEndpoint(spec: A2AEndpointSpec): A2AEndpoint {
  const name = spec.name ?? DEFAULT_A2A_NAME;
  const given = [spec.url, spec.handler, spec.listener].filter((t) => t !== undefined).length;
  if (given !== 1) {
    throw new Error("an A2A endpoint is exactly one of url, handler or listener");
  }
  const target = spec.url !== undefined ? { url: spec.url } : spec.handler !== undefined ? { handler: spec.handler } : { listener: spec.listener as NodeRequestListener };
  return { name, service: `a2a://${name}`, target };
}

export interface ProviderOptions extends ProviderAuthorizerOptions {
  /** Peers the control plane has banned; refused before their token is looked at. */
  isBanned?(peerId: string): boolean;
  /** Called after a caller is authorized, e.g. for the session's admitted set. */
  onAuthorized?(peerId: string, verified: VerifiedBiscuit, targetService: string): void;
}

interface StreamSocket extends Duplex {
  remotePeer: string;
}

/**
 * Bridges a libp2p stream to a Node Duplex so Node's own HTTP parser and
 * client can run over it.
 */
export function streamToNodeDuplex(stream: Stream, remotePeer: string): StreamSocket {
  const duplex = new Duplex({
    read() {
      stream.resume();
    },
    write(chunk: Uint8Array, _encoding, callback) {
      if (stream.send(chunk)) {
        callback();
      } else {
        stream.addEventListener("drain", () => callback(), { once: true });
      }
    },
    final(callback) {
      stream.close().then(
        () => callback(),
        (err: Error) => callback(err),
      );
    },
    destroy(err, callback) {
      if (err !== null) {
        stream.abort(err);
      } else {
        void stream.close().catch(() => {});
      }
      callback(err);
    },
  }) as StreamSocket;
  duplex.remotePeer = remotePeer;
  stream.addEventListener("message", (evt) => {
    if (!duplex.push(Buffer.from(evt.data.subarray()))) {
      stream.pause();
    }
  });
  const end = () => {
    if (!duplex.readableEnded) {
      duplex.push(null);
    }
  };
  stream.addEventListener("remoteCloseWrite", end);
  stream.addEventListener("close", end);
  return duplex;
}

/** Reads a whole body, bounded. */
async function readBody(readable: Readable, limit: number): Promise<Buffer> {
  const chunks: Buffer[] = [];
  let size = 0;
  for await (const chunk of readable) {
    const buf = Buffer.isBuffer(chunk) ? chunk : Buffer.from(chunk as Uint8Array);
    size += buf.length;
    if (size > limit) {
      throw new Error(`body exceeds ${limit} bytes`);
    }
    chunks.push(buf);
  }
  return Buffer.concat(chunks);
}

function hasDotSegment(path: string): boolean {
  return path.split("/").some((seg) => seg === "." || seg === "..");
}

/**
 * Server side of /libp2p-http, as sam-node's StartIngressServer: the path is
 * /<type>/<name>[/<upstream>], the caller's biscuit is X-Sam-Biscuit, and the
 * request is authorized for <type>://<name> before anything is forwarded. Only
 * the agent's own endpoint is answered; anything else is 404 after
 * authorization, so an unauthorized caller learns nothing about it.
 */
export function httpIngressHandler(endpoint: A2AEndpoint, options: ProviderOptions): StreamHandler {
  const server = http.createServer({ keepAlive: false }, (req, res) => {
    void handleIngress(req, res, endpoint, options).catch((err: unknown) => {
      if (!res.headersSent) {
        res.writeHead(500, { "content-type": "text/plain" });
      }
      res.end(`ingress error: ${err instanceof Error ? err.message : String(err)}\n`);
    });
  });
  server.headersTimeout = AUTH_HANDSHAKE_TIMEOUT_MS;
  return (stream: Stream, connection: Connection) => {
    server.emit("connection", streamToNodeDuplex(stream, connection.remotePeer.toString()));
  };
}

async function handleIngress(req: http.IncomingMessage, res: http.ServerResponse, endpoint: A2AEndpoint, options: ProviderOptions): Promise<void> {
  const remotePeer = (req.socket as unknown as StreamSocket).remotePeer;
  const reply = (status: number, text: string) => {
    res.writeHead(status, { "content-type": "text/plain; charset=utf-8" });
    res.end(text + "\n");
  };

  const rawURL = req.url ?? "/";
  // Policy is decided on the /<type>/<name> prefix; a dot segment in what
  // follows could resolve to a sibling path on the backend. Checked on the
  // raw path, before URL parsing normalizes it away.
  if (hasDotSegment(rawURL.split("?")[0] as string)) {
    reply(400, "Invalid path");
    return;
  }
  const url = new URL(rawURL, "http://mesh.invalid");
  const parts = url.pathname.replace(/^\//, "").split("/");
  if (parts.length < 2 || parts[0] === "" || parts[1] === "") {
    reply(400, "Invalid path");
    return;
  }
  const [serviceType, serviceName, ...rest] = parts as [string, string, ...string[]];
  if (serviceType !== "inference" && serviceType !== "a2a" && serviceType !== "mcp") {
    reply(400, "Invalid service type");
    return;
  }
  const upstreamPath = rest.join("/");

  const biscuitB64 = req.headers[HEADER_SAM_BISCUIT];
  if (typeof biscuitB64 !== "string" || biscuitB64 === "") {
    reply(401, "Missing X-Sam-Biscuit header");
    return;
  }
  let biscuit: Uint8Array;
  try {
    biscuit = new Uint8Array(Buffer.from(biscuitB64, "base64"));
    if (biscuit.length === 0) {
      throw new Error("empty");
    }
  } catch {
    reply(400, "Invalid X-Sam-Biscuit encoding");
    return;
  }

  const targetService = `${serviceType}://${serviceName}`;
  if (options.isBanned?.(remotePeer) === true) {
    reply(403, "Authorization failed");
    return;
  }
  const agentHeader = req.headers[HEADER_SAM_AGENT];
  let verified: VerifiedBiscuit;
  try {
    verified = await authorizeCaller(
      { biscuit, peerId: remotePeer, targetService, protocol: HTTP_PROTOCOL, agent: typeof agentHeader === "string" ? agentHeader : "" },
      options,
    );
  } catch (err) {
    if (err instanceof AuthorizationError) {
      reply(403, "Authorization failed");
      return;
    }
    throw err;
  }
  options.onAuthorized?.(remotePeer, verified, targetService);

  // Under the type the policy was evaluated on.
  if (targetService !== endpoint.service) {
    reply(404, "Service not found");
    return;
  }

  const path = "/" + upstreamPath + url.search;
  const noTrailingSlash = upstreamPath === "" && rest.length === 0;

  if ("listener" in endpoint.target) {
    // The listener sees the request as a backend behind sam-node would: the
    // path relative to the service, the verified caller, never the biscuit.
    // X-Peer-Id is set, not added, so an inbound value cannot pose as the
    // verified peer.
    req.url = path;
    delete req.headers[HEADER_SAM_BISCUIT];
    delete req.headers[HEADER_SAM_AGENT];
    req.headers[HEADER_PEER_ID] = remotePeer;
    if (noTrailingSlash) {
      req.headers[HEADER_SAM_NO_TRAILING_SLASH] = "true";
    } else {
      delete req.headers[HEADER_SAM_NO_TRAILING_SLASH];
    }
    endpoint.target.listener(req, res);
    return;
  }

  const headers = new Headers();
  for (const [k, v] of Object.entries(req.headers)) {
    if (v === undefined || k === HEADER_SAM_BISCUIT || k === HEADER_SAM_AGENT || k === HEADER_SAM_NO_TRAILING_SLASH || k === HEADER_PEER_ID || k === "host" || k === "connection" || k === "transfer-encoding" || k === "content-length") {
      continue;
    }
    for (const value of Array.isArray(v) ? v : [v]) {
      headers.append(k, value);
    }
  }
  headers.set(HEADER_PEER_ID, remotePeer);
  if (noTrailingSlash) {
    headers.set(HEADER_SAM_NO_TRAILING_SLASH, "true");
  }

  const method = req.method ?? "GET";
  const body: Uint8Array<ArrayBuffer> | undefined =
    method === "GET" || method === "HEAD" ? undefined : new Uint8Array(await readBody(req, MAX_INGRESS_BODY_BYTES));
  const init: RequestInit = { method, headers };
  if (body !== undefined) {
    init.body = body;
  }

  let response: Response;
  if ("url" in endpoint.target) {
    const base = endpoint.target.url.replace(/\/$/, "");
    response = await fetch(base + path, { ...init, redirect: "manual" });
  } else {
    response = await endpoint.target.handler(new Request("http://" + endpoint.name + path, init), verified);
  }

  const outHeaders: Record<string, string | string[]> = {};
  response.headers.forEach((value, key) => {
    if (key === "content-length" || key === "transfer-encoding" || key === "connection") {
      return;
    }
    outHeaders[key] = value;
  });
  res.writeHead(response.status, outHeaders);
  if (response.body === null) {
    res.end();
    return;
  }
  await new Promise<void>((resolve, reject) => {
    Readable.fromWeb(response.body as import("node:stream/web").ReadableStream)
      .on("error", reject)
      .pipe(res)
      .on("finish", resolve)
      .on("error", reject);
  });
}

/** The request target for a service on a peer: /<type>/<name>/<path>. */
export function meshHTTPTarget(targetService: string, path = ""): string {
  const m = /^(inference|a2a|mcp):\/\/(.+)$/.exec(targetService);
  if (m === null) {
    throw new Error(`service target must look like inference://<name>, got ${JSON.stringify(targetService)}`);
  }
  if (path === "") {
    return `/${m[1]}/${m[2]}`;
  }
  return `/${m[1]}/${m[2]}${path.startsWith("/") ? path : "/" + path}`;
}

/**
 * The URL a fetch bound to a session (session.fetch()) takes for a service
 * on a peer: http://mesh/sam/<peer-id>/<type>/<name>/<path>.
 */
export function meshURL(peerId: string, targetService: string, path = ""): string {
  return "http://mesh" + MESH_PATH_PREFIX + canonicalPeerId(peerId) + meshHTTPTarget(targetService, path);
}

/** (peer ID, request target) of a mesh URL; the target is what the peer's ingress takes. */
export function splitMeshURL(url: URL): { peerId: string; target: string } {
  if (!url.pathname.startsWith(MESH_PATH_PREFIX)) {
    throw new TypeError(`a mesh URL looks like http://mesh${MESH_PATH_PREFIX}<peer-id>/<type>/<name>/..., got ${url.toString()}`);
  }
  const [peerId = "", ...rest] = url.pathname.slice(MESH_PATH_PREFIX.length).split("/");
  if (peerId === "" || rest.length < 2 || rest[0] === "" || rest[1] === "") {
    throw new TypeError(`a mesh URL names a peer, a service type and a name, got ${url.toString()}`);
  }
  return { peerId, target: "/" + rest.join("/") + url.search };
}

export interface HTTPStreamOptions {
  /** The agent this request is made for. */
  agent?: string;
  /** Bounds the whole exchange; without one, the response headers must arrive within a minute and the body is unbounded. */
  signal?: AbortSignal;
}

/**
 * Client side of /libp2p-http, as go-libp2p-http's RoundTripper: one stream
 * per request, plain HTTP/1.1 with Host set to the peer ID and the biscuit in
 * X-Sam-Biscuit. Resolves once the response headers are in; the body streams
 * after, so an SSE response is consumed as the peer sends it.
 */
export async function fetchOverStream(conn: Connection, biscuit: Uint8Array, request: Request, options: HTTPStreamOptions = {}): Promise<Response> {
  const { target } = splitMeshURL(new URL(request.url));
  const peerId = conn.remotePeer.toString();
  // One controller ends the exchange: the caller's signal at any time, the
  // headers timeout only until the response headers are in.
  const ctl = new AbortController();
  let headersIn = false;
  const forward = (s: AbortSignal | undefined) => s?.addEventListener("abort", () => ctl.abort(s.reason), { once: true });
  forward(options.signal);
  forward(request.signal);
  if (options.signal === undefined) {
    const timer = setTimeout(() => {
      if (!headersIn) {
        ctl.abort(new DOMException(`no response headers from ${peerId} within ${REQUEST_TIMEOUT_MS}ms`, "TimeoutError"));
      }
    }, REQUEST_TIMEOUT_MS);
    timer.unref?.();
  }
  const signal = ctl.signal;
  const stream = await conn.newStream(HTTP_PROTOCOL, { signal, runOnLimitedConnection: true });
  const socket = streamToNodeDuplex(stream, peerId);
  const headers: Record<string, string> = {};
  request.headers.forEach((value, key) => {
    if (key !== "host" && key !== "content-length" && key !== HEADER_SAM_BISCUIT && key !== HEADER_PEER_ID) {
      headers[key] = value;
    }
  });
  headers.host = peerId;
  headers[HEADER_SAM_BISCUIT] = Buffer.from(biscuit).toString("base64");
  if (options.agent) {
    headers[HEADER_SAM_AGENT] = options.agent;
  }
  const body = request.body === null ? undefined : Buffer.from(await request.arrayBuffer());
  headers["content-length"] = String(body?.length ?? 0);

  return new Promise<Response>((resolve, reject) => {
    const req = http.request({ method: request.method, path: target, headers, createConnection: () => socket, signal }, (res) => {
      headersIn = true;
      const responseHeaders = new Headers();
      for (const [k, v] of Object.entries(res.headers)) {
        if (typeof v === "string") {
          responseHeaders.set(k, v);
        } else if (Array.isArray(v)) {
          responseHeaders.set(k, v.join(", "));
        }
      }
      res.on("close", () => socket.destroy());
      const status = res.statusCode ?? 0;
      const bodyStream = Readable.toWeb(res) as ReadableStream<Uint8Array>;
      resolve(new Response(status === 204 || status === 304 || request.method === "HEAD" ? null : bodyStream, { status, statusText: res.statusMessage ?? "", headers: responseHeaders }));
    });
    req.on("error", (err) => {
      socket.destroy();
      reject(err);
    });
    if (body !== undefined) {
      req.write(body);
    }
    req.end();
  });
}

export interface HTTPRequestOptions {
  method?: string;
  headers?: Record<string, string>;
  body?: Uint8Array | string;
  /** The agent this request is made for. */
  agent?: string;
  signal?: AbortSignal;
}

export interface HTTPResponse {
  status: number;
  headers: Record<string, string>;
  body: Uint8Array;
  text(): string;
}

/** One request to /<type>/<name>/<path> on a peer, body read whole. */
export async function httpRequestOverStream(
  conn: Connection,
  biscuit: Uint8Array,
  targetService: string,
  path: string,
  options: HTTPRequestOptions = {},
): Promise<HTTPResponse> {
  const signal = options.signal ?? AbortSignal.timeout(REQUEST_TIMEOUT_MS);
  const init: RequestInit = { method: options.method ?? "GET", headers: options.headers ?? {}, signal };
  if (options.body !== undefined) {
    init.body = options.body as BodyInit;
  }
  const request = new Request(meshURL(conn.remotePeer.toString(), targetService, path), init);
  const streamOptions: HTTPStreamOptions = { signal };
  if (options.agent !== undefined) {
    streamOptions.agent = options.agent;
  }
  const response = await fetchOverStream(conn, biscuit, request, streamOptions);
  const buf = new Uint8Array(await response.arrayBuffer());
  if (buf.length > MAX_INGRESS_BODY_BYTES) {
    throw new Error("response body too large");
  }
  const flat: Record<string, string> = {};
  response.headers.forEach((value, key) => {
    flat[key] = value;
  });
  return { status: response.status, headers: flat, body: buf, text: () => new TextDecoder().decode(buf) };
}
