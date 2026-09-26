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

// The HTTP/1.1 codec on its own: what the ingress and the client agree on
// with go-libp2p-http, fed byte by byte so every split point is exercised.

import assert from "node:assert/strict";
import { test } from "node:test";
import {
  ByteReader,
  HTTPParseError,
  LAST_CHUNK,
  MAX_HEAD_BYTES,
  bodyStream,
  encodeChunk,
  encodeRequestHead,
  encodeResponseHead,
  readBody,
  readRequestHead,
  readResponseHead,
  requestBodyFraming,
  responseBodyFraming,
  type ByteSource,
} from "./http1.ts";

const enc = new TextEncoder();
const dec = new TextDecoder();

/** A source that hands out the text in pieces of the given size. */
function source(text: string | Uint8Array, pieceSize = 1): ByteSource {
  const bytes = typeof text === "string" ? enc.encode(text) : text;
  let offset = 0;
  return {
    async read() {
      if (offset >= bytes.length) {
        return null;
      }
      const next = bytes.subarray(offset, offset + pieceSize);
      offset += pieceSize;
      return next;
    },
  };
}

test("a request head parses however the bytes are split", async () => {
  const wire = "POST /a2a/agent/tasks?x=1 HTTP/1.1\r\nHost: 12D3KooW\r\nX-Sam-Biscuit: abc=\r\nContent-Length: 2\r\n\r\n{}";
  for (const size of [1, 3, 7, 1000]) {
    const reader = new ByteReader(source(wire, size));
    const head = await readRequestHead(reader);
    assert.ok(head);
    assert.equal(head.method, "POST");
    assert.equal(head.target, "/a2a/agent/tasks?x=1");
    assert.equal(head.headers.get("host"), "12D3KooW");
    assert.equal(head.headers.get("x-sam-biscuit"), "abc=");
    assert.deepEqual(requestBodyFraming(head), { kind: "length", length: 2 });
    assert.equal(dec.decode(await readBody(reader, requestBodyFraming(head), 1024)), "{}");
  }
  // Nothing at all is a closed stream, not an error.
  assert.equal(await readRequestHead(new ByteReader(source(""))), null);
});

test("a request without a length has no body; malformed heads are refused", async () => {
  const head = await readRequestHead(new ByteReader(source("GET /a2a/agent HTTP/1.1\r\nHost: x\r\n\r\n")));
  assert.deepEqual(requestBodyFraming(head!), { kind: "none" });
  for (const bad of ["GET /x\r\n\r\n", "GET /x HTTP/2\r\n\r\n", "GET /x HTTP/1.1\r\nbad header\r\n\r\n", "GET /x HTTP/1.1\r\n bad: fold\r\n\r\n", "GET /x HTTP/1.1\r\nHost: x"]) {
    await assert.rejects(readRequestHead(new ByteReader(source(bad, 64))), HTTPParseError, bad);
  }
  const huge = "GET /x HTTP/1.1\r\n" + "X-Pad: " + "a".repeat(MAX_HEAD_BYTES) + "\r\n\r\n";
  await assert.rejects(readRequestHead(new ByteReader(source(huge, 4096))), HTTPParseError);
  await assert.rejects(readRequestHead(new ByteReader(source("GET /x HTTP/1.1\r\nContent-Length: 1, 2\r\n\r\n"))).then((h) => requestBodyFraming(h!)), HTTPParseError);
  await assert.rejects(readRequestHead(new ByteReader(source("GET /x HTTP/1.1\r\nTransfer-Encoding: gzip, chunked\r\n\r\n"))).then((h) => requestBodyFraming(h!)), HTTPParseError);
});

test("a chunked body is reassembled, extensions and trailers skipped, and streamed piece by piece", async () => {
  const wire = "HTTP/1.1 200 OK\r\nContent-Type: text/event-stream\r\nTransfer-Encoding: chunked\r\n\r\n" + "5;ext=1\r\nhello\r\n" + "6\r\n world\r\n" + "0\r\nX-Trailer: t\r\n\r\n";
  for (const size of [1, 2, 5, 64]) {
    const reader = new ByteReader(source(wire, size));
    const head = await readResponseHead(reader);
    assert.equal(head.status, 200);
    assert.equal(head.statusText, "OK");
    const framing = responseBodyFraming("GET", head);
    assert.deepEqual(framing, { kind: "chunked" });
    assert.equal(dec.decode(await readBody(reader, framing, 1024)), "hello world");
  }
  let finished = false;
  const reader = new ByteReader(source(wire, 1000));
  const head = await readResponseHead(reader);
  const pieces: string[] = [];
  const stream = bodyStream(reader, responseBodyFraming("GET", head), () => (finished = true));
  for await (const piece of stream) {
    pieces.push(dec.decode(piece));
  }
  assert.deepEqual(pieces, ["hello", " world"]);
  assert.ok(finished);
  // Cancelling the stream reports the reason once and closes the generator,
  // so nothing is read after the caller has gone.
  const slow = new ByteReader(source("HTTP/1.1 200 OK\r\nTransfer-Encoding: chunked\r\n\r\n5\r\nhello\r\n6\r\n world\r\n0\r\n\r\n", 3));
  await readResponseHead(slow);
  const reasons: (Error | undefined)[] = [];
  const cancelled = bodyStream(slow, { kind: "chunked" }, (err) => reasons.push(err));
  const r = cancelled.getReader();
  assert.ok("hello".startsWith(dec.decode((await r.read()).value)));
  await r.cancel(new Error("gone"));
  assert.equal(reasons.length, 1);
  assert.equal(reasons[0]?.message, "gone");
  assert.deepEqual(await r.read(), { done: true, value: undefined });
  // Each malformed size line is followed by a body that would otherwise
  // parse, so only the size check can be what refuses it.
  for (const bad of ["zz\r\nhello\r\n0\r\n\r\n", "5g\r\nhello\r\n0\r\n\r\n", "5 g\r\nhello\r\n0\r\n\r\n", "-5\r\nhello\r\n0\r\n\r\n", "\r\nhello\r\n0\r\n\r\n", "123456789\r\n0\r\n\r\n"]) {
    await assert.rejects(readBody(new ByteReader(source(bad)), { kind: "chunked" }, 1024), HTTPParseError, bad);
  }
  for (const bad of ["5\r\nhello\r\n", "5\r\nhelloXX", "5\r\nhel"]) {
    await assert.rejects(readBody(new ByteReader(source(bad)), { kind: "chunked" }, 1024), HTTPParseError, bad);
  }
  // Whitespace around the extension separator is allowed (RFC 9112 section 7.1.1).
  assert.equal(dec.decode(await readBody(new ByteReader(source("5 ; ext=1\r\nhello\r\n0\r\n\r\n")), { kind: "chunked" }, 1024)), "hello");
});

test("a response without framing runs to the end of the stream; some have no body at all", async () => {
  const reader = new ByteReader(source("HTTP/1.1 200 OK\r\nContent-Type: text/plain\r\n\r\nuntil the end", 3));
  const head = await readResponseHead(reader);
  const framing = responseBodyFraming("GET", head);
  assert.deepEqual(framing, { kind: "until-close" });
  assert.equal(dec.decode(await readBody(reader, framing, 1024)), "until the end");
  for (const [method, status] of [["HEAD", 200], ["GET", 204], ["GET", 304], ["GET", 101]] as const) {
    const h = await readResponseHead(new ByteReader(source(`HTTP/1.1 ${status} X\r\nContent-Length: 10\r\n\r\n`)));
    assert.deepEqual(responseBodyFraming(method, h), { kind: "none" }, `${method} ${status}`);
  }
  // A status line with no reason phrase is still a status line.
  assert.equal((await readResponseHead(new ByteReader(source("HTTP/1.1 204\r\n\r\n")))).status, 204);
  await assert.rejects(readResponseHead(new ByteReader(source("HTTP/1.1 twenty\r\n\r\n"))), HTTPParseError);
  await assert.rejects(readResponseHead(new ByteReader(source(""))), HTTPParseError);
});

test("a line past the limit is refused however the bytes arrive", async () => {
  assert.equal(await new ByteReader(source("abcde\r\nrest", 1000)).readLine(5), "abcde");
  assert.equal(await new ByteReader(source("abcde\r\nrest", 1)).readLine(5), "abcde");
  // In one piece, so the line is complete before the limit is compared.
  await assert.rejects(new ByteReader(source("abcdef\r\nrest", 1000)).readLine(5), HTTPParseError);
  await assert.rejects(new ByteReader(source("abcdef\r\nrest", 1)).readLine(5), HTTPParseError);
  await assert.rejects(new ByteReader(source("a".repeat(100), 1)).readLine(5), HTTPParseError);
});

test("bodies past the limit are refused, up front when the length says so", async () => {
  await assert.rejects(readBody(new ByteReader(source("x".repeat(20))), { kind: "length", length: 20 }, 10), HTTPParseError);
  await assert.rejects(readBody(new ByteReader(source("x".repeat(20))), { kind: "until-close" }, 10), HTTPParseError);
  await assert.rejects(readBody(new ByteReader(source("x".repeat(5))), { kind: "length", length: 20 }, 100), HTTPParseError);
});

test("heads and chunks are written as the other side reads them", async () => {
  const headers = new Headers({ Host: "peer", "X-Sam-Biscuit": "abc=", "Content-Length": "0" });
  assert.equal(dec.decode(encodeRequestHead("GET", "/a2a/agent/card", headers)), "GET /a2a/agent/card HTTP/1.1\r\ncontent-length: 0\r\nhost: peer\r\nx-sam-biscuit: abc=\r\n\r\n");
  assert.equal(dec.decode(encodeResponseHead(404, "Not Found", new Headers({ "Content-Type": "text/plain" }))), "HTTP/1.1 404 Not Found\r\ncontent-type: text/plain\r\n\r\n");
  assert.equal(dec.decode(encodeResponseHead(200, "", new Headers())), "HTTP/1.1 200 \r\n\r\n");
  // Headers itself refuses a line break in a value; a stray one in the reason phrase is flattened.
  assert.throws(() => new Headers({ "X-Bad": "a\r\nInjected: 1" }), TypeError);
  assert.equal(dec.decode(encodeResponseHead(200, "OK\r\nInjected: 1", new Headers())), "HTTP/1.1 200 OK  Injected: 1\r\n\r\n");
  const body = new Uint8Array([...encodeChunk(enc.encode("hello")), ...encodeChunk(enc.encode(" world")), ...LAST_CHUNK]);
  assert.equal(dec.decode(body), "5\r\nhello\r\n6\r\n world\r\n0\r\n\r\n");
  assert.equal(dec.decode(await readBody(new ByteReader(source(body, 4)), { kind: "chunked" }, 1024)), "hello world");
});
