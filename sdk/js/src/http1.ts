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

// HTTP/1.1 on a libp2p stream, as go-libp2p-http speaks it: one request and
// one response per stream, the body framed by Content-Length or chunked
// (RFC 9112 section 6), a response without either running to the end of the
// stream. This is the whole of what a member needs to call a service or
// answer as one, so it is written here rather than borrowed from Node and
// runs wherever the SDK does, a browser included.

import type { Stream } from "@libp2p/interface";
import { concatBytes } from "./bytes.ts";

/** Longest request or response head (start line and headers) accepted. */
export const MAX_HEAD_BYTES = 64 * 1024;
const MAX_CHUNK_LINE_BYTES = 1024;

const CR = 0x0d;
const LF = 0x0a;
const encoder = new TextEncoder();
const decoder = new TextDecoder();

/** Where bytes come from: a chunk, or null when no more will arrive. */
export interface ByteSource {
  read(): Promise<Uint8Array | null>;
}

/** Reads a libp2p stream, holding the remote back while nobody is reading. */
export function streamSource(stream: Stream): ByteSource {
  const queue: Uint8Array[] = [];
  let ended = false;
  let waiter: (() => void) | undefined;
  const wake = () => {
    waiter?.();
    waiter = undefined;
  };
  stream.addEventListener("message", (evt) => {
    queue.push(evt.data.subarray());
    stream.pause();
    wake();
  });
  const end = () => {
    ended = true;
    wake();
  };
  // 'end' fires once the read buffer has drained after the remote closed its
  // writable end; 'remoteCloseWrite' can fire while data is still buffered.
  stream.addEventListener("end", end);
  stream.addEventListener("close", end);
  if (stream.readableEnded) {
    ended = true;
  }
  return {
    async read() {
      for (;;) {
        const next = queue.shift();
        if (next !== undefined) {
          if (queue.length === 0 && !ended) {
            stream.resume();
          }
          return next;
        }
        if (ended) {
          return null;
        }
        stream.resume();
        await new Promise<void>((resolve) => {
          waiter = resolve;
        });
      }
    },
  };
}

/** Sends to a libp2p stream, waiting for the remote when the send buffer is full. */
export async function sendAll(stream: Stream, data: Uint8Array): Promise<void> {
  if (data.length === 0) {
    return;
  }
  if (!stream.send(data)) {
    await stream.onDrain();
  }
}

export class HTTPParseError extends Error {
  constructor(message: string) {
    super(message);
    this.name = "HTTPParseError";
  }
}

/** Bytes from a source with lookahead: lines and exact counts. */
export class ByteReader {
  #source: ByteSource;
  #buf: Uint8Array = new Uint8Array(0);
  #eof = false;

  constructor(source: ByteSource) {
    this.#source = source;
  }

  async #fill(): Promise<boolean> {
    if (this.#eof) {
      return false;
    }
    const next = await this.#source.read();
    if (next === null) {
      this.#eof = true;
      return false;
    }
    this.#buf = this.#buf.length === 0 ? next : concatBytes(this.#buf, next);
    return true;
  }

  /** One CRLF-terminated line without its terminator, at most limit bytes; null at end of input before any byte. */
  async readLine(limit: number): Promise<string | null> {
    for (;;) {
      const lf = this.#buf.indexOf(LF);
      if (lf !== -1) {
        const end = lf > 0 && this.#buf[lf - 1] === CR ? lf - 1 : lf;
        if (end > limit) {
          throw new HTTPParseError(`line exceeds ${limit} bytes`);
        }
        const line = decoder.decode(this.#buf.subarray(0, end));
        this.#buf = this.#buf.subarray(lf + 1);
        return line;
      }
      // Room for the CRLF of a line exactly at the limit.
      if (this.#buf.length > limit + 2) {
        throw new HTTPParseError(`line exceeds ${limit} bytes`);
      }
      if (!(await this.#fill())) {
        if (this.#buf.length === 0) {
          return null;
        }
        throw new HTTPParseError("unexpected end of input in a line");
      }
    }
  }

  /** Exactly n bytes, or throws at end of input. */
  async readExactly(n: number): Promise<Uint8Array> {
    while (this.#buf.length < n) {
      if (!(await this.#fill())) {
        throw new HTTPParseError(`unexpected end of input after ${this.#buf.length} of ${n} bytes`);
      }
    }
    const out = this.#buf.slice(0, n);
    this.#buf = this.#buf.subarray(n);
    return out;
  }

  /** Whatever is available next, at most n bytes; null at end of input. */
  async readSome(n: number): Promise<Uint8Array | null> {
    if (this.#buf.length === 0 && !(await this.#fill())) {
      return null;
    }
    const take = Math.min(n, this.#buf.length);
    const out = this.#buf.slice(0, take);
    this.#buf = this.#buf.subarray(take);
    return out;
  }
}

export interface RequestHead {
  method: string;
  target: string;
  headers: Headers;
}

export interface ResponseHead {
  status: number;
  statusText: string;
  headers: Headers;
}

const TOKEN = /^[!#$%&'*+\-.^_`|~0-9A-Za-z]+$/;

async function readHeaders(reader: ByteReader, budget: number): Promise<Headers> {
  const headers = new Headers();
  for (;;) {
    const line = await reader.readLine(budget);
    if (line === null) {
      throw new HTTPParseError("unexpected end of input in headers");
    }
    if (line === "") {
      return headers;
    }
    budget -= line.length + 2;
    if (budget < 0) {
      throw new HTTPParseError(`head exceeds ${MAX_HEAD_BYTES} bytes`);
    }
    const colon = line.indexOf(":");
    if (colon <= 0 || line[0] === " " || line[0] === "\t") {
      throw new HTTPParseError(`malformed header line ${JSON.stringify(line)}`);
    }
    const name = line.slice(0, colon);
    if (!TOKEN.test(name)) {
      throw new HTTPParseError(`malformed header name ${JSON.stringify(name)}`);
    }
    headers.append(name, line.slice(colon + 1).trim());
  }
}

/** The request line and headers, as a server reads them. */
export async function readRequestHead(reader: ByteReader): Promise<RequestHead | null> {
  const line = await reader.readLine(MAX_HEAD_BYTES);
  if (line === null) {
    return null;
  }
  const parts = line.split(" ");
  if (parts.length !== 3 || !TOKEN.test(parts[0] as string) || parts[1] === "" || !/^HTTP\/1\.[01]$/.test(parts[2] as string)) {
    throw new HTTPParseError(`malformed request line ${JSON.stringify(line)}`);
  }
  const headers = await readHeaders(reader, MAX_HEAD_BYTES - line.length - 2);
  return { method: parts[0] as string, target: parts[1] as string, headers };
}

/** The status line and headers, as a client reads them. */
export async function readResponseHead(reader: ByteReader): Promise<ResponseHead> {
  const line = await reader.readLine(MAX_HEAD_BYTES);
  if (line === null) {
    throw new HTTPParseError("no response");
  }
  const m = /^HTTP\/1\.[01] (\d{3})(?: (.*))?$/.exec(line);
  if (m === null) {
    throw new HTTPParseError(`malformed status line ${JSON.stringify(line)}`);
  }
  const headers = await readHeaders(reader, MAX_HEAD_BYTES - line.length - 2);
  return { status: Number(m[1]), statusText: m[2] ?? "", headers };
}

/** How the bytes after a head are delimited. */
export type BodyFraming = { kind: "none" } | { kind: "length"; length: number } | { kind: "chunked" } | { kind: "until-close" };

function framingOf(headers: Headers, whenUnframed: BodyFraming): BodyFraming {
  const te = headers.get("transfer-encoding");
  if (te !== null) {
    const codings = te.split(",").map((c) => c.trim().toLowerCase());
    if (codings.at(-1) !== "chunked" || codings.length !== 1) {
      throw new HTTPParseError(`unsupported transfer-encoding ${JSON.stringify(te)}`);
    }
    return { kind: "chunked" };
  }
  const cl = headers.get("content-length");
  if (cl !== null) {
    const values = new Set(cl.split(",").map((v) => v.trim()));
    if (values.size !== 1 || !/^\d{1,15}$/.test(cl.split(",")[0]?.trim() ?? "")) {
      throw new HTTPParseError(`malformed content-length ${JSON.stringify(cl)}`);
    }
    const length = Number(values.values().next().value);
    return length === 0 ? { kind: "none" } : { kind: "length", length };
  }
  return whenUnframed;
}

/** A request body without Content-Length or Transfer-Encoding is empty (RFC 9112 section 6.3). */
export function requestBodyFraming(head: RequestHead): BodyFraming {
  return framingOf(head.headers, { kind: "none" });
}

/** A response body ends where the sender says, or with the stream. */
export function responseBodyFraming(method: string, head: ResponseHead): BodyFraming {
  if (method === "HEAD" || head.status === 204 || head.status === 304 || (head.status >= 100 && head.status < 200)) {
    return { kind: "none" };
  }
  return framingOf(head.headers, { kind: "until-close" });
}

async function* chunkedBody(reader: ByteReader): AsyncGenerator<Uint8Array> {
  for (;;) {
    const line = await reader.readLine(MAX_CHUNK_LINE_BYTES);
    if (line === null) {
      throw new HTTPParseError("unexpected end of input in chunked body");
    }
    // The whole size token must be hex, else "5g" would read as 5 and the
    // two ends would disagree on where the chunk ends. Eight digits bound
    // the size to 4 GiB, well within what parseInt represents exactly.
    const sizeText = (line.split(";")[0] as string).trim();
    if (!/^[0-9A-Fa-f]{1,8}$/.test(sizeText)) {
      throw new HTTPParseError(`malformed chunk size ${JSON.stringify(line)}`);
    }
    const size = parseInt(sizeText, 16);
    if (size === 0) {
      // Trailer fields, then the empty line that ends the message.
      for (let t = await reader.readLine(MAX_HEAD_BYTES); t !== ""; t = await reader.readLine(MAX_HEAD_BYTES)) {
        if (t === null) {
          throw new HTTPParseError("unexpected end of input in trailers");
        }
      }
      return;
    }
    let remaining = size;
    while (remaining > 0) {
      const part = await reader.readSome(remaining);
      if (part === null) {
        throw new HTTPParseError("unexpected end of input in a chunk");
      }
      remaining -= part.length;
      yield part;
    }
    if ((await reader.readLine(2)) !== "") {
      throw new HTTPParseError("chunk data not followed by CRLF");
    }
  }
}

/** The body bytes, as the framing delimits them, in the pieces they arrive. */
export async function* bodyChunks(reader: ByteReader, framing: BodyFraming): AsyncGenerator<Uint8Array> {
  switch (framing.kind) {
    case "none":
      return;
    case "length": {
      let remaining = framing.length;
      while (remaining > 0) {
        const part = await reader.readSome(remaining);
        if (part === null) {
          throw new HTTPParseError(`body ended ${remaining} bytes short of content-length`);
        }
        remaining -= part.length;
        yield part;
      }
      return;
    }
    case "chunked":
      yield* chunkedBody(reader);
      return;
    case "until-close":
      for (let part = await reader.readSome(64 * 1024); part !== null; part = await reader.readSome(64 * 1024)) {
        yield part;
      }
      return;
  }
}

/** A whole body, refused past the limit. */
export async function readBody(reader: ByteReader, framing: BodyFraming, limit: number): Promise<Uint8Array<ArrayBuffer>> {
  if (framing.kind === "length" && framing.length > limit) {
    throw new HTTPParseError(`body of ${framing.length} bytes exceeds ${limit}`);
  }
  const parts: Uint8Array[] = [];
  let size = 0;
  for await (const part of bodyChunks(reader, framing)) {
    size += part.length;
    if (size > limit) {
      throw new HTTPParseError(`body exceeds ${limit} bytes`);
    }
    parts.push(part);
  }
  return concatBytes(...parts);
}

/** The body as a web stream; onDone runs once it is drained, failed or cancelled. */
export function bodyStream(reader: ByteReader, framing: BodyFraming, onDone: (err?: Error) => void): ReadableStream<Uint8Array> {
  const chunks = bodyChunks(reader, framing);
  let done = false;
  const finish = (err?: Error) => {
    if (!done) {
      done = true;
      onDone(err);
    }
  };
  return new ReadableStream<Uint8Array>({
    async pull(controller) {
      try {
        const next = await chunks.next();
        if (next.done) {
          controller.close();
          finish();
        } else {
          controller.enqueue(next.value);
        }
      } catch (err) {
        const e = err instanceof Error ? err : new Error(String(err));
        controller.error(e);
        finish(e);
      }
    },
    async cancel(reason) {
      // The stream is torn down first, so a read the generator is blocked on ends.
      finish(reason instanceof Error ? reason : new Error(String(reason)));
      await chunks.return(undefined).catch(() => {});
    },
  });
}

function headerLines(headers: Headers): string {
  let out = "";
  headers.forEach((value, name) => {
    if (/[\r\n]/.test(value)) {
      throw new HTTPParseError(`header ${name} contains a line break`);
    }
    out += `${name}: ${value}\r\n`;
  });
  return out;
}

/** A request head on the wire. */
export function encodeRequestHead(method: string, target: string, headers: Headers): Uint8Array {
  return encoder.encode(`${method} ${target} HTTP/1.1\r\n${headerLines(headers)}\r\n`);
}

/** A response head on the wire. */
export function encodeResponseHead(status: number, statusText: string, headers: Headers): Uint8Array {
  return encoder.encode(`HTTP/1.1 ${status} ${statusText.replace(/[\r\n]/g, " ")}\r\n${headerLines(headers)}\r\n`);
}

/** One chunk of a chunked body. */
export function encodeChunk(data: Uint8Array): Uint8Array {
  return concatBytes(encoder.encode(`${data.length.toString(16)}\r\n`), data, encoder.encode("\r\n"));
}

/** The end of a chunked body. */
export const LAST_CHUNK = encoder.encode("0\r\n\r\n");
