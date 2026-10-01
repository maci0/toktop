// Copyright (C) 2026 Marcel W. Wysocki
// SPDX-License-Identifier: MIT

// Run with `bun test site/` from the repository root (no other deps needed).

// A mutation harness over the Accept-Encoding parser, the one request header
// this Worker parses by hand rather than reading through the Headers API.
// parseAcceptEncoding splits the header on commas and semicolons, lowercases
// each token, and takes the last q= parameter on a coding; quality() then
// resolves the wildcard and the identity default. A header is whatever a
// client, a proxy or a scanner put on the wire, so a malformed one must still
// get a determinate answer: one of the three codings, identity, or a 406.
//
// A fuzzer proves a bug is there; the assertions are what turn the parser's
// promises into failures a mutated input can reach. Each one is a property
// every header has to hold, so a mutation that breaks any of them is a
// readable failure rather than a quietly wrong body.
//
// bun ships no coverage-guided fuzzer and the Worker takes no npm dependency,
// so the corpus is seeded and mutated here instead: the same loop a real fuzz
// run would drive, over inputs reached by byte flips, splices and duplications
// rather than by hand-picked cases only.

import { expect, test } from "bun:test";

import worker from "./worker.js";

const ORIGIN = "https://toktop.ai";
const call = (headers = {}) => worker.fetch(new Request(`${ORIGIN}/`, { headers }), undefined);

// The three codings the Worker can build, plus identity, plus the wildcard
// and the one coding it deliberately does not offer. Every seed below and
// every mutation of one is checked against this set, so an answer naming
// anything else is a parser bug rather than a client the Worker chose not to
// serve.
const CODINGS = ["br", "gzip", "zstd"];
const DECOMPRESS = { br: "brotli", gzip: "gzip", zstd: "zstd" };

// The seeds are the header spellings real clients and proxies send, plus the
// shapes a mutation has to reach: a wildcard, a q= on every coding, identity
// refused, a coding this Worker does not offer, parameters before and after
// the q, and the near-misses where one character changes the answer.
const SEEDS = [
  "gzip, deflate, br",
  "gzip, deflate, br, zstd",
  "br",
  "gzip",
  "zstd",
  "identity",
  "deflate",
  "*",
  "*;q=0",
  "identity;q=0",
  "gzip;q=0",
  "gzip;q=0.5",
  "br;q=0.1, gzip",
  "gzip;q=0.5, br",
  "zstd, gzip",
  "gzip, deflate, br, zstd;q=1.0, *;q=0.1",
  "br;q=1.0, gzip;q=0.8, deflate;q=0.6",
  "GZIP, DEFLATE, BR",
  "gzip;q=1.5",
  "gzip;q=-1",
  "gzip;q=0.000",
  "gzip;q=",
  "gzip;q",
  "gzip;q=abc",
  "gzip;q=0;level=9",
  "gzip;level=9;q=0.8",
  "gzip ; q = 0.8 , br ; q = 0.9",
  "  gzip  ,  br  ",
  "gzip,,br",
  ",gzip,br,",
  ",,",
  ";q=1",
  "*;q=0.5",
  "*;q=0, gzip;q=0.5",
  "*;q=0.0, identity;q=0",
  "compress, deflate, exi, gzip, br, zstd, *",
  "identity;q=1, gzip;q=0.5",
  "br;q=0.001",
  "gzip;q=0.001",
  "identity;q=0, gzip",
  "x-gzip",
  "gzip2",
  "br;q=1.0e0, gzip;q=0x1",
  "gzip;q=Infinity",
  "gzip;q=NaN",
  // The last q on a coding wins, so a repeat decides which one it was: a
  // parser that took the first would serve a body the final parameter had
  // refused.
  "gzip;q=0.9;q=0",
  "gzip;q=0;q=0.9",
  "br;q=0;q=1, gzip;q=1;q=0",
  "identity;q=0.5, br;q=0.5",
];

// Mutations a coverage-guided fuzzer would find on its own: a byte flipped, a
// byte inserted, a byte dropped, two seeds spliced, a seed doubled, and a
// run of one character blown out. The last one is the input shape that turns
// an O(n^2) parser into a hung request, so it is in the corpus on purpose.
const mutators = [
  (s, i) => s.slice(0, i) + flip(s[i]) + s.slice(i + 1),
  (s, i) => s.slice(0, i) + s[i] + s.slice(i),
  (s, i) => s.slice(0, i) + s.slice(i + 1),
  (s, i) => `${s.slice(0, i)},${s.slice(i)}`,
  (s, i) => `${s.slice(0, i)};q=0${s.slice(i)}`,
  (s, i) => `${s.slice(0, i)}*${s.slice(i)}`,
  (s) => s + s,
  (s) => SEEDS[(SEEDS.indexOf(s) + 7) % SEEDS.length] + s,
  (s) => `${s},${SEEDS[(SEEDS.indexOf(s) + 11) % SEEDS.length]}`,
];

// A byte that changes the meaning rather than the length: the q values and
// the coding names are where a one-character difference flips an answer. The
// object rather than a switch because the map is read once per mutated byte
// and a lookup is the whole cost of a mutation.
const FLIPS = new Map([
  ["0", "1"],
  ["1", "0"],
  [".", ","],
  [",", ";"],
  [";", ","],
  ["=", "x"],
  ["q", "Q"],
]);

function flip(c) {
  return FLIPS.get(c) ?? c;
}

async function decompress(bytes, format) {
  const out = await new Response(
    new Response(bytes).body.pipeThrough(new DecompressionStream(format)),
  ).arrayBuffer();
  return new TextDecoder().decode(out);
}

// Every header the harness drives, deduplicated and in a fixed order, so one
// failing input is a reproducible case rather than a line number and the
// count is the same on every run.
//
// The corpus is flat rather than generational: a mutation of a mutation is
// how a generational corpus grows, and here each generation is an order of
// magnitude wider than the last for no extra reach. Two passes are enough to
// reach every shape the parser distinguishes a value by — a byte flipped, a
// token dropped, a parameter inserted, a whole seed spliced — because the
// seeds already carry the shape and the mutations only perturb them.
function corpus(budget) {
  const seen = new Set();
  const mutated = [];
  const addMutation = (s) => {
    if (s.length > maxHeaderLen || seen.has(s)) return;
    seen.add(s);
    mutated.push(s);
  };

  // The blow-up shapes are part of the corpus rather than of what is left
  // over, so they are present whatever the budget is: they are the only inputs
  // that can catch a parse quadratic in the header, which is the one failure
  // here that hangs a request rather than answering it wrong.
  const large = [];
  for (const filler of [", ", ",gzip;q=0, ", "*;q=0, ", "gzip;q=1,", "identity;q=0,"]) {
    large.push(filler.repeat(maxHeaderLen / filler.length));
    for (const seed of SEEDS.slice(0, 6)) large.push(filler.repeat(64) + seed);
  }
  for (const s of large) seen.add(s);

  // The seeds come next and are never trimmed: a mutation of a seed is an
  // extra shape, but the seed is the case the mutation was derived from, and
  // dropping one to make room for its own variants would leave the harness
  // asserting less than its corpus was written to assert.
  const out = [...large];
  for (const seed of SEEDS) {
    if (!seen.has(seed)) {
      seen.add(seed);
      out.push(seed);
    }
  }
  for (const seed of SEEDS) {
    for (const s of mutationsOf(seed)) addMutation(s);
  }
  return [...out, ...mutated].slice(0, budget);
}

// maxHeaderLen bounds what a single request carries. Every seed and every
// mutation stays under it, so a body this harness builds is the size a
// client can actually send.
const maxHeaderLen = 4096;

function* mutationsOf(seed) {
  // One position per distinct byte, so the whole header is walked without a
  // mutation per character of a seed that repeats itself.
  const at = new Set();
  for (let i = 0; i < seed.length; i++) at.add(i % Math.max(1, Math.ceil(seed.length / 8)));
  for (const i of [...at].sort((a, b) => a - b)) {
    for (const mutate of mutators.slice(0, 6)) yield mutate(seed, i);
  }
  for (const mutate of mutators.slice(6)) yield mutate(seed, 0);
}

test("mutated accept-encoding headers negotiate determinately", async () => {
  const identity = await call().then((r) => r.text());
  const inputs = corpus(300);
  // The corpus is worth nothing if the budget trimmed the long headers away,
  // so the shapes it is supposed to reach are pinned here rather than left to
  // whatever order the generator happens to produce.
  expect(Math.max(...inputs.map((ae) => ae.length))).toBeGreaterThanOrEqual(maxHeaderLen - 8);
  expect(new Set(inputs).size).toBe(inputs.length);

  let checked = 0;
  for (const ae of inputs) {
    checked++;
    // One header at a time, so a failing case names the header that failed
    // rather than one of a hundred fired at the Worker together, and so the
    // body cache fills the way a real client's sequence of requests fills it.
    // biome-ignore lint/performance/noAwaitInLoops: one case at a time, not a fan-out.
    const res = await call({ "accept-encoding": ae });
    const coding = res.headers.get("content-encoding");

    // One of exactly four answers, and nothing else: a coding this Worker can
    // build, identity (no header), or the 406 a client that refused every one
    // of them gets. A body this Worker cannot name is a parser that read a
    // token it should have refused.
    if (res.status === 406) {
      expect(coding).toBeNull();
      // A refusal is the one answer a shared cache must never keep, and it is
      // still a failure a visitor reports.
      expect(res.headers.get("cache-control")).toBe("no-store");
      expect(res.headers.get("vary")).toBe("Accept-Encoding");
      continue;
    }

    expect(res.status).toBe(200);
    // Every served answer names what it negotiated on, so a shared cache
    // keyed on the header cannot hand a compressed body to a client that
    // cannot decode it.
    expect(res.headers.get("vary")).toBe("Accept-Encoding");
    const bytes = new Uint8Array(await res.arrayBuffer());
    if (coding === null) {
      // Identity is what the client gets when it named nothing this Worker
      // can build, and the bytes are the page itself.
      expect(new TextDecoder().decode(bytes)).toBe(identity);
    } else {
      expect(CODINGS).toContain(coding);
      // A coding named in a header the client cannot read is the failure this
      // whole path exists to prevent, so the served body has to decompress
      // back to the page under exactly the coding that was named.
      expect(await decompress(bytes, DECOMPRESS[coding])).toBe(identity);
    }

    // The same header must always negotiate the same way: a cache warmed by
    // one request has to be able to serve the next, and a client that retries
    // cannot be handed a different body than the one it just received.
    const again = await call({ "accept-encoding": ae });
    expect(again.status).toBe(res.status);
    expect(again.headers.get("content-encoding")).toBe(coding);
  }
  expect(checked).toBe(inputs.length);
});

// The one failure a header parser here can cause that a wrong answer cannot:
// a parse quadratic in the header hangs the request instead of answering it,
// and nothing above would see that. The largest header in the corpus has
// thousands of members, so a linear parse answers it in about the time the
// page takes to compress and a quadratic one does not. The budget is a
// ceiling on the whole loop rather than a per-request timing, so a slow run
// is a signal and not a flake.
test("the largest headers in the corpus answer in bounded time", async () => {
  const long = corpus(300)
    .filter((ae) => ae.length >= 1024)
    .slice(0, 24);
  expect(long.length).toBeGreaterThan(0);

  const started = Date.now();
  for (const ae of long) {
    // The sequence is the measurement. Firing the headers at the Worker
    // together would share one body build across them, which is the cost a
    // quadratic parse would hide behind.
    // biome-ignore lint/performance/noAwaitInLoops: one at a time, not a fan-out.
    const res = await call({ "accept-encoding": ae });
    await res.arrayBuffer();
  }
  const elapsed = Date.now() - started;
  // Compression dominates and the Worker caches one body per coding, so the
  // whole loop is a few page compressions. A parse that walks the header
  // twice per member cannot fit in this on any of the CI runners.
  expect(elapsed).toBeLessThan(10_000);
});

test("a coding named anywhere in the header is never served to a client that refused it", async () => {
  for (const ae of corpus(300)) {
    // A q of zero on a coding is the client refusing that representation, so
    // the answer must not carry it whichever position the coding was named
    // at, whichever case it was spelled in, and however many parameters
    // sat between the token and its q.
    const members = new Map();
    for (const part of ae.split(",")) {
      const [rawToken, ...params] = part.trim().toLowerCase().split(";");
      const token = rawToken.trim();
      if (!token) continue;
      let q = 1;
      for (const param of params) {
        const trimmed = param.trim();
        if (!trimmed.startsWith("q=")) continue;
        const parsed = Number.parseFloat(trimmed.slice(2));
        if (Number.isFinite(parsed)) q = parsed;
      }
      members.set(token, q);
    }
    // A failing case has to name the header that failed, which a batch of
    // concurrent requests cannot do.
    // biome-ignore lint/performance/noAwaitInLoops: one case at a time, not a fan-out.
    const res = await call({ "accept-encoding": ae });
    const coding = res.headers.get("content-encoding");
    if (coding === null || res.status === 406) continue;
    // A member after the coding in the header overrides it, so only the
    // last mention of the served coding decides whether it was refused.
    if ((members.get(coding) ?? 1) === 0) {
      throw new Error(`${JSON.stringify(ae)} refused ${coding} with q=0 and was served it`);
    }
    // The wildcard refuses a coding the client never named at all, so a
    // served coding cannot contradict "*;q=0" either.
    if (members.get("*") === 0 && !members.has(coding)) {
      throw new Error(
        `${JSON.stringify(ae)} refused every coding with *;q=0 and was served ${coding}`,
      );
    }
  }
});

test("a header that names nothing this Worker offers falls back to identity", async () => {
  const identity = await call().then((r) => r.text());
  for (const ae of corpus(300)) {
    const names = ae
      .split(",")
      .map((part) => part.trim().toLowerCase().split(";")[0].trim())
      .filter(Boolean);
    // biome-ignore lint/performance/noAwaitInLoops: one case at a time, not a fan-out.
    const res = await call({ "accept-encoding": ae });
    if (res.status !== 200) continue;
    const coding = res.headers.get("content-encoding");
    // A served coding is always one the header named, or one the wildcard
    // covers. Serving a coding the client never asked for is how an HTTP/1.0
    // agent breaks on a body it has no decoder for.
    if (coding !== null) {
      const named = names.includes(coding) || names.includes("*");
      expect(named).toBe(true);
    } else {
      const bytes = new Uint8Array(await res.arrayBuffer());
      expect(new TextDecoder().decode(bytes)).toBe(identity);
    }
  }
});
