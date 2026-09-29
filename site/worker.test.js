// Copyright (C) 2026 Marcel W. Wysocki
// SPDX-License-Identifier: MIT

// Run with `bun test site/` from the repository root (no other deps needed).

import { expect, test } from "bun:test";
import { readFileSync, statSync } from "node:fs";
import { join } from "node:path";

import worker from "./worker.js";

const ORIGIN = "https://toktop.ai";
// Regexes live at the top level: a literal inside a function is recompiled on
// every call, and the worker's own regexes are hoisted for the same reason.
const METHOD_RE = /^(GET|HEAD)$/;
const FONT_SIZE_RE = /font-size:\s*([^;}]+)/g;
const FONT_STEP_RE = /^var\(--fs-([\w-]+)\)$/;
// The selector opening the rule a bare font-size sits in, and the media block
// closing on a line of its own: the two ends of the phone query, which is
// where the one size written outside the scale is allowed to live.
const RULE_OPEN_RE = /([^{};]+)\{\s*$/;
const BLOCK_CLOSE_RE = /^\s*\}$/m;
const PALETTE_ENTRY_RE = /\d+/g;
const ANSI16_BLOCK_RE = /^ANSI16: dict\[int, RGB\] = \{([\s\S]*?)^\}/m;
const ANSI16_ENTRY_RE = /^\s*(\d+): (\(\d+, \d+, \d+\))/gm;
const EDGE_DUR_RE = /^edge;dur=(\d+)$/;
// A border on :hover draws a second line under a link's underline.
const HOVER_BORDER_RE = /a:hover[^}]*border-/;
const DUR_SUFFIX_RE = /dur=\d+(?:\.\d+)?$/;
const IMG_TAG_RE = /<img\b[^>]*>/g;
// The transfer sizes site/README.md states as prose, and the phone visit it
// derives from them.
const README_SIZES_RE = /([\d,]+) bytes identity \/ ([\d,]+) gzip \/\s*([\d,]+) brotli/;
const README_VISIT_RE =
  /whole visit is those ([\d,]+) bytes[\s\S]*?([\d,]+) bytes in two requests/g;
const thousandsStripped = (value) => Number(value.replaceAll(",", ""));
// The h1 cursor's animation rule, and the parts of it that say whether the
// blink ever stops. Named apart from the tests that read them so a rule
// rewritten under them is a readable failure rather than a silent pass.
const BLINK_CURSOR_BLOCK_RE = /h1 \.cursor \{[^}]*animation:[^}]*\}/;
const INFINITE_RE = /\binfinite\b/;
const BLINK_CYCLES_RE = /step-end\s+(\d+)/;
const BLINK_PERIOD_RE = /blink\s+([\d.]+)s/;
const NAV_BLOCK_RE = /<nav\b[\s\S]*?<\/nav>/;
const NAV_LABELED_RE = /<nav aria-label="[^"]+">/;
const ANCHOR_TAG_RE = /<a\b[^>]*>/g;
const ARIA_LABEL_ATTR_RE = /aria-label=/;
const ARIA_LABELLEDBY_ATTR_RE = /aria-labelledby=/;
// A rule that takes the focus indicator off the skip link's target, and the
// shell prompt in the capture caption, which is decoration rather than part
// of the command it introduces.
const FOCUS_KILLED_RE = /main\s*:\s*focus[^{]*\{\s*outline:\s*none/;
const SHELL_PROMPT_RE = /<figcaption><span class="dim" aria-hidden="true">\$<\/span>/;
const KBD_RULE_RE = /kbd\s*\{[^}]*\}/;
const BAR_RULE_RE = /\.bar \{[^}]*\}/;
const NAV_CSS_RE = /nav \{[^}]*\}/;
const PRE_RULE_RE = /pre \{[^}]*\}/;
const paletteVarRE = (name) => new RegExp(`--dark-${name}: (#[0-9a-f]{6});`, "i");

// WCAG 2.x relative contrast between two #rrggbb values, the measure the
// page's own palette is held to: 4.5:1 for text, 3:1 for a control boundary.
const relLuminance = (hex) => {
  const ch = [0, 2, 4].map((i) => Number.parseInt(hex.slice(1 + i, 3 + i), 16) / 255);
  return ch
    .map((c) => (c <= 0.03928 ? c / 12.92 : ((c + 0.055) / 1.055) ** 2.4))
    .reduce((sum, c, i) => sum + c * [0.2126, 0.7152, 0.0722][i], 0);
};
const relContrast = (a, b) => {
  const [hi, lo] = [relLuminance(a), relLuminance(b)].sort((x, y) => y - x);
  return (hi + 0.05) / (lo + 0.05);
};
const IMG_SIZE_RE = /width="(\d+)" height="(\d+)"/;
const INLINED_ICON_RE = /rel="icon" href="data:image\/svg\+xml,([^"]+)"/;
// The h1 is the terminal's own bold title; the brand above it is bold too.
const H1_RULE_RE = /h1 \{ font-size: var\(--fs-h1\); font-weight: 700;/;
const BRAND_RULE_RE = /\.brand \{ font-weight: 700;/;
const FONT_WEIGHT_RE = /font-weight:\s*(\d+)/g;
const H1_RULE_END = "font-weight: 700; margin: 0; }";
const SPACE_STEP_RE = /--space-(?:tight|section|runout):\s*([\d.]+rem);/g;
const SECTION_GAP_RE = /h2 \{[^}]*margin: var\(--space-section\) 0 \.7rem;/;
const TIGHT_GAP_RE = /\.shot \+ h2, h2 \+ pre \+ h2 \{ margin-top: var\(--space-tight\); \}/;
const RUNOUT_GAP_RE = /footer \{ margin-top: var\(--space-runout\);/;
// A section gap written as a literal instead of one of the three steps above.
const LITERAL_GAP_RE = /margin(?:-top)?: [^;]*\b(?:1\.5|2\.8)rem\b/;
// Out of main, the footer carries the body column width itself.
const FOOTER_MAXWIDTH_RE = /footer \{[^}]*max-width: 76rem/;
// The role is the documented remedy for a list whose markers are removed.
const GRID_LIST_RE = /<ul class="grid" role="list">/;
const GRID_LIST_STYLE_RE = /\.grid \{[^}]*list-style: none/;
// The terminal frame re-points the scheme it paints in; every scheme token its
// text can name has to be in that list.
const SHOT_FRAME_RE = /\.shot \{[^}]*\}/;
const call = (headers = {}, init = {}) =>
  worker.fetch(
    new Request(ORIGIN + (init.path ?? "/"), {
      method: init.method ?? "GET",
      headers,
    }),
    init.env,
  );

const identityBody = await call().then((r) => r.text());

// The page is set in one font token, so that token is where script coverage is
// decided. A family named in it is matched per run, so one run in a script no
// family covers goes to the generic keyword, and there the platform's own
// monospace is what renders: a proportional face, which is the one thing the
// code blocks on the page cannot survive, or boxes.
const FONT_TOKEN_RE = /--mono:\s*([^;]+);/;
// A script the page has no family for, and the generic keyword it falls to.
const UNCOVERED_SCRIPTS = [
  ["CJK", "Noto Sans Mono CJK"],
  ["Cyrillic", "Noto Sans Mono"],
  ["Devanagari", "Noto Sans Mono Devanagari"],
  ["Arabic", "Noto Sans Arabic"],
];

async function decompress(bytes, format) {
  const out = await new Response(
    new Response(bytes).body.pipeThrough(new DecompressionStream(format)),
  ).arrayBuffer();
  return new TextDecoder().decode(out);
}

test("compression starts in a request and only completed bytes are reused", async () => {
  const NativeCompressionStream = globalThis.CompressionStream;
  let inRequest = false;
  let constructions = 0;
  globalThis.CompressionStream = new Proxy(NativeCompressionStream, {
    construct(target, args) {
      constructions++;
      if (!inRequest) throw new Error("compression outside a request");
      return Reflect.construct(target, args);
    },
  });
  try {
    const { default: freshWorker } = await import("./worker.js?request-context");
    expect(constructions).toBe(0);
    inRequest = true;
    const request = () =>
      new Request(ORIGIN, {
        headers: { "accept-encoding": "gzip" },
      });
    const [first, concurrent] = await Promise.all([
      freshWorker.fetch(request()),
      freshWorker.fetch(request()),
    ]);
    expect(first.headers.get("content-encoding")).toBe("gzip");
    const bytes = new Uint8Array(await first.arrayBuffer());
    expect(await decompress(bytes, "gzip")).toBe(identityBody);
    expect(new Uint8Array(await concurrent.arrayBuffer())).toEqual(bytes);
    // One build for the pair, not one each: the cache holds the in-flight
    // promise, so the concurrent request awaited this one's compression
    // instead of starting a second pipeline. One, not three: a client that
    // asked for gzip never pays the brotli and zstd builds.
    const afterFirst = constructions;
    expect(afterFirst).toBe(1);
    const second = await freshWorker.fetch(request());
    expect(new Uint8Array(await second.arrayBuffer())).toEqual(bytes);
    expect(constructions).toBe(afterFirst);
    // A coding is built when a client asks for it, so the brotli body is
    // built now and the gzip body is served from the first build.
    const brotli = await freshWorker.fetch(
      new Request(ORIGIN, { headers: { "accept-encoding": "br" } }),
    );
    expect(brotli.headers.get("content-encoding")).toBe("br");
    expect(await decompress(new Uint8Array(await brotli.arrayBuffer()), "brotli")).toBe(
      identityBody,
    );
    expect(constructions).toBe(2);
  } finally {
    globalThis.CompressionStream = NativeCompressionStream;
  }
});

// A 304 and a 406 carry no body, so neither may wait on the three compressed
// representations it will not send. The construction count is the observable:
// an isolate that only ever sees revalidations never builds a coding at all.
test("revalidation and refusal answer without building any representation", async () => {
  const NativeCompressionStream = globalThis.CompressionStream;
  let constructions = 0;
  globalThis.CompressionStream = new Proxy(NativeCompressionStream, {
    construct(target, args) {
      constructions++;
      return Reflect.construct(target, args);
    },
  });
  try {
    const etag = (await call()).headers.get("etag");
    // The conditional request is the first one the fresh isolate sees, so a
    // build here would show up as a nonzero count.
    const { default: freshWorker } = await import("./worker.js?no-build");
    expect(constructions).toBe(0);

    for (const headers of [
      { "if-none-match": etag },
      { "if-none-match": "*", "accept-encoding": "gzip" },
    ]) {
      const res = await freshWorker.fetch(new Request(ORIGIN, { headers }), {});
      expect(res.status).toBe(304);
      expect(await res.text()).toBe("");
      expect(constructions).toBe(0);
    }

    for (const ae of ["identity;q=0", "*;q=0", "deflate, identity;q=0"]) {
      const res = await freshWorker.fetch(
        new Request(ORIGIN, { headers: { "accept-encoding": ae } }),
        {},
      );
      expect(res.status).toBe(406);
      expect(constructions).toBe(0);
    }

    // The bytes are still there for a client that wants them, and the
    // isolate builds the one coding that client asked for.
    const body = await freshWorker.fetch(
      new Request(ORIGIN, { headers: { "accept-encoding": "gzip" } }),
      {},
    );
    expect(constructions).toBe(1);
    expect(await decompress(new Uint8Array(await body.arrayBuffer()), "gzip")).toBe(identityBody);
  } finally {
    globalThis.CompressionStream = NativeCompressionStream;
  }
});

test("no accept-encoding: identity body, no content-encoding", async () => {
  const res = await call();
  expect(res.status).toBe(200);
  expect(res.headers.get("content-encoding")).toBeNull();
  expect(await res.text()).toBe(identityBody);
});

test("single-coding clients get a body that decompresses to the page", async () => {
  for (const [ae, format] of [
    ["gzip", "gzip"],
    ["zstd", "zstd"],
    ["br", "brotli"],
  ]) {
    const res = await call({ "accept-encoding": ae });
    expect(res.status).toBe(200);
    expect(res.headers.get("content-encoding")).toBe(ae);
    const bytes = new Uint8Array(await res.arrayBuffer());
    expect(await decompress(bytes, format)).toBe(identityBody);
  }
});

test("page responses disable Workers automatic body encoding", async () => {
  const NativeResponse = globalThis.Response;
  const options = new WeakMap();
  globalThis.Response = new Proxy(NativeResponse, {
    construct(target, args) {
      const response = Reflect.construct(target, args);
      options.set(response, args[1]);
      return response;
    },
  });
  try {
    for (const [coding, format] of [
      ["gzip", "gzip"],
      ["br", "brotli"],
      ["zstd", "zstd"],
      ["identity", null],
    ]) {
      for (const method of ["GET", "HEAD"]) {
        const res = await call({ "accept-encoding": coding }, { method });
        expect(options.get(res)?.encodeBody).toBe("manual");
        const bytes = new Uint8Array(await res.arrayBuffer());
        expect(res.headers.get("content-encoding")).toBe(format ? coding : null);
        if (method === "HEAD") {
          expect(bytes.byteLength).toBe(0);
        } else {
          expect(Number(res.headers.get("content-length"))).toBe(bytes.byteLength);
          expect(format ? await decompress(bytes, format) : new TextDecoder().decode(bytes)).toBe(
            identityBody,
          );
        }
      }
    }
  } finally {
    globalThis.Response = NativeResponse;
  }
});

test("brotli-capable clients are served br, not gzip", async () => {
  for (const ae of ["br", "gzip, deflate, br", "gzip, deflate, br, zstd"]) {
    const res = await call({ "accept-encoding": ae });
    expect(res.status).toBe(200);
    expect(res.headers.get("content-encoding")).toBe("br");
    const bytes = new Uint8Array(await res.arrayBuffer());
    expect(await decompress(bytes, "brotli")).toBe(identityBody);
  }
});

test("accept-encoding variants negotiate correctly", async () => {
  for (const [ae, want] of [
    ["gzip", "gzip"],
    ["GZIP", "gzip"],
    ["*", "br"],
    // zstd is bigger than gzip on this page, so the ranking the Worker
    // negotiates from is a list, not the order the codings are offered in,
    // and a client naming both gets the smaller.
    ["zstd, gzip", "gzip"],
    ["gzip, deflate, br, zstd", "br"],
    ["gzip;q=0.5, br", "br"],
    ["br;q=0.1, gzip", "gzip"],
    ["br", "br"],
    ["zstd", "zstd"],
    ["gzip;q=0", null],
    ["deflate", null],
  ]) {
    const res = await call({ "accept-encoding": ae });
    expect(res.headers.get("content-encoding")).toBe(want);
    if (want === null) expect(await res.text()).toBe(identityBody);
  }
});

// RFC 9110 12.5.3: identity is acceptable by default, so a header naming no
// page encoding at all (or refusing the listed ones with q=0) falls back to
// identity bytes. Only identity;q=0 itself refuses the raw bytes.
test("clients listing no compatible encoding get identity, per accept-encoding rules", async () => {
  for (const ae of ["deflate", "br;q=0, gzip;q=0, zstd;q=0", "deflate;q=1"]) {
    const res = await call({ "accept-encoding": ae });
    expect(res.headers.get("content-encoding")).toBeNull();
    expect(await res.text()).toBe(identityBody);
  }
});

test("implicit identity does not outweigh an accepted compressed representation", async () => {
  for (const ae of ["gzip;q=0.5", "br;q=0.1, gzip;q=0.5", "gzip;q=0.001"]) {
    const res = await call({ "accept-encoding": ae });
    const bytes = new Uint8Array(await res.arrayBuffer());
    expect(bytes.byteLength).toBe(4506);
    expect(res.headers.get("content-encoding")).toBe("gzip");
    expect(await decompress(bytes, "gzip")).toBe(identityBody);
  }
  const preferred = await call({ "accept-encoding": "identity;q=1, gzip;q=0.5" });
  expect(preferred.headers.get("content-encoding")).toBeNull();
  expect(await preferred.text()).toBe(identityBody);
});

test("unacceptable encodings return an uncacheable 406, including conditional requests", async () => {
  const logs = captureLogs();
  try {
    const etag = (await call()).headers.get("etag");
    let refused = 0;
    for (const ae of ["identity;q=0", "*;q=0", "deflate, identity;q=0"]) {
      for (const method of ["GET", "HEAD"]) {
        for (const conditional of [{}, { "if-none-match": etag }]) {
          const res = await call({ "accept-encoding": ae, ...conditional }, { method });
          expect(res.status).toBe(406);
          expect(res.headers.get("cache-control")).toBe("no-store");
          expect(res.headers.get("vary")).toBe("Accept-Encoding");
          expect(res.headers.get("content-encoding")).toBeNull();
          for (const name of SECURITY_HEADER_NAMES) {
            expect(res.headers.get(name)).not.toBeNull();
          }
          if (method === "HEAD") expect(await res.text()).toBe("");
          refused++;
        }
      }
    }
    // A 406 is a client the edge cannot serve at all, so it is one of the
    // failures a visitor reports: one line each, and none of the served
    // requests around them.
    expect(logs.parse()).toEqual(
      Array.from({ length: refused }, () => ({
        event: "not-acceptable",
        ray: "",
        method: expect.stringMatching(METHOD_RE),
        path: "/",
        status: 406,
        duration_ms: expect.any(Number),
      })),
    );
    const accepted = await call({ "accept-encoding": "*;q=0, gzip;q=0.5" });
    expect(accepted.status).toBe(200);
    expect(accepted.headers.get("content-encoding")).toBe("gzip");
  } finally {
    logs.restore();
  }
});

test("every variant carries Vary: Accept-Encoding", async () => {
  const fresh = await call({ "accept-encoding": "gzip" });
  expect(fresh.headers.get("vary")).toBe("Accept-Encoding");
  const plain = await call();
  expect(plain.headers.get("vary")).toBe("Accept-Encoding");
  const etag = plain.headers.get("etag");
  const revalidated = await call({ "if-none-match": etag });
  expect(revalidated.status).toBe(304);
  expect(revalidated.headers.get("vary")).toBe("Accept-Encoding");
});

test("weak ETag round trip, including validators from older strong-form deploys", async () => {
  const etag = (await call()).headers.get("etag");
  expect(etag.startsWith('W/"')).toBe(true);
  for (const echoed of [etag, etag.slice(2), `"x", ${etag}`, "*"]) {
    const res = await call({ "if-none-match": echoed });
    expect(res.status).toBe(304);
    expect(res.headers.get("etag")).toBe(etag);
    expect(await res.text()).toBe("");
  }
});

test("changed validator re-sends the body", async () => {
  const res = await call({ "if-none-match": '"deadbeef"' });
  expect(res.status).toBe(200);
  expect(await res.text()).toBe(identityBody);
});

test("HEAD answers with page headers and no body", async () => {
  const res = await call({}, { method: "HEAD" });
  expect(res.status).toBe(200);
  expect(res.headers.get("content-type")).toContain("text/html");
  expect(res.headers.get("content-encoding")).toBeNull();
  expect((await res.arrayBuffer()).byteLength).toBe(0);
});

test("HEAD with Accept-Encoding matches GET's coding and length, with no body", async () => {
  const get = await call({ "accept-encoding": "gzip, deflate, br" });
  const head = await call({ "accept-encoding": "gzip, deflate, br" }, { method: "HEAD" });
  expect(head.status).toBe(200);
  expect(head.headers.get("content-encoding")).toBe(get.headers.get("content-encoding"));
  expect(head.headers.get("content-length")).toBe(get.headers.get("content-length"));
  expect((await head.arrayBuffer()).byteLength).toBe(0);
});

const SECURITY_HEADER_NAMES = [
  "content-security-policy",
  "x-content-type-options",
  "x-frame-options",
  "strict-transport-security",
  "referrer-policy",
];

test("non-GET methods and /health keep their contract", async () => {
  const env = staticAssets();
  const denied = await call({}, { method: "POST", env });
  expect(denied.status).toBe(405);
  expect(denied.headers.get("allow")).toBe("GET, HEAD");
  expect(denied.headers.get("content-type")).toBe("text/plain; charset=utf-8");
  expect(denied.headers.get("cache-control")).toBe("no-store");
  const health = await call({}, { path: "/health", env });
  expect(health.status).toBe(200);
  expect(await health.text()).toBe("ok\n");
  expect(health.headers.get("cache-control")).toBe("no-store");
  const healthHead = await call({}, { method: "HEAD", path: "/health", env });
  expect(healthHead.status).toBe(200);
  expect(healthHead.headers.get("content-type")).toBe(health.headers.get("content-type"));
  expect(healthHead.headers.get("content-length")).toBe(health.headers.get("content-length"));
  expect((await healthHead.arrayBuffer()).byteLength).toBe(0);
  for (const res of [denied, health, healthHead]) {
    for (const name of SECURITY_HEADER_NAMES) {
      expect(res.headers.get(name)).not.toBeNull();
    }
  }
});

test("every error answer states its length, and a HEAD states the GET's", async () => {
  const refused = await call({}, { method: "POST", env: staticAssets() });
  const refusedBody = await refused.text();
  expect(refused.status).toBe(405);
  expect(refused.headers.get("content-length")).toBe(
    String(new TextEncoder().encode(refusedBody).byteLength),
  );

  const missing = await call({}, { path: "/dashboard.avif", env: {} });
  const missingBody = await missing.text();
  expect(missing.status).toBe(404);
  expect(missing.headers.get("content-length")).toBe(
    String(new TextEncoder().encode(missingBody).byteLength),
  );
  const missingHead = await call({}, { path: "/dashboard.avif", method: "HEAD", env: {} });
  expect(missingHead.status).toBe(404);
  expect(missingHead.headers.get("content-length")).toBe(missing.headers.get("content-length"));
  expect((await missingHead.arrayBuffer()).byteLength).toBe(0);

  const unreadable = await call({ "accept-encoding": "identity;q=0" });
  expect(unreadable.status).toBe(406);
  const unreadableHead = await call({ "accept-encoding": "identity;q=0" }, { method: "HEAD" });
  expect(unreadableHead.status).toBe(406);
  expect(unreadableHead.headers.get("content-length")).toBe(
    unreadable.headers.get("content-length"),
  );
  expect(unreadableHead.headers.get("vary")).toBe(unreadable.headers.get("vary"));
  expect((await unreadableHead.arrayBuffer()).byteLength).toBe(0);
});

test("every failure is a whole line, the way /health answers", async () => {
  const answers = [
    ["method not allowed", await call({}, { method: "POST", env: staticAssets() })],
    ["assets unbound", await call({}, { path: "/dashboard.png", env: {} })],
    [
      "asset store error",
      await call(
        {},
        {
          path: "/dashboard.png",
          env: { ASSETS: { fetch: () => Promise.resolve(new Response("", { status: 502 })) } },
        },
      ),
    ],
    ["not acceptable", await call({ "accept-encoding": "identity;q=0" })],
  ];
  for (const [what, res] of answers) {
    const body = await res.text();
    expect(res.status, what).toBeGreaterThanOrEqual(400);
    expect(res.headers.get("content-type"), what).toBe("text/plain; charset=utf-8");
    expect(body.endsWith("\n"), `${what}: ${JSON.stringify(body)}`).toBe(true);
    expect(body.slice(0, -1).includes("\n"), `${what}: ${JSON.stringify(body)}`).toBe(false);
    expect(res.headers.get("content-length"), what).toBe(
      String(new TextEncoder().encode(body).byteLength),
    );
  }
});

test("/health reports degraded while the asset binding is missing", async () => {
  const logs = captureLogs();
  try {
    const degraded = await call({ "cf-ray": "probe-TOK" }, { path: "/health", env: {} });
    expect(degraded.status).toBe(503);
    expect(await degraded.text()).toBe(
      "degraded: no asset binding; the dashboard captures are not served\n",
    );
    expect(degraded.headers.get("cache-control")).toBe("no-store");
    const head = await call({}, { method: "HEAD", path: "/health", env: {} });
    expect(head.status).toBe(503);
    expect(head.headers.get("content-length")).toBe(degraded.headers.get("content-length"));
    expect((await head.arrayBuffer()).byteLength).toBe(0);
    expect((await call({}, { path: "/health", env: staticAssets() })).status).toBe(200);
    // The 503 is a failure, so it carries the request behind it: the binding
    // is a deploy-level thing, and on a site taking no image traffic the
    // probe's answer is the only thing that says it. The healthy answer is a
    // served one and writes nothing.
    expect(logs.parse()).toEqual([
      {
        event: "health-degraded",
        ray: "probe-TOK",
        method: "GET",
        path: "/health",
        status: 503,
        duration_ms: expect.any(Number),
        reason: "no asset binding",
      },
      {
        event: "health-degraded",
        ray: "",
        method: "HEAD",
        path: "/health",
        status: 503,
        duration_ms: expect.any(Number),
        reason: "no asset binding",
      },
    ]);
  } finally {
    logs.restore();
  }
});

// A binding is not a capture. A deploy that shipped the Worker and left the
// files behind answers 200 here with every image on the page a 404, and the
// deploy gate polls this path right after shipping, so the store is read and
// a missing or failing capture is the 503 that stops the deploy.
test("/health reports degraded when the store cannot serve the share card", async () => {
  const missing = { ASSETS: { fetch: () => new Response("nope", { status: 404 }) } };
  const logs = captureLogs();
  try {
    const res = await call({ "cf-ray": "probe-TOK" }, { path: "/health", env: missing });
    expect(res.status).toBe(503);
    expect(await res.text()).toBe(
      "degraded: the share card answered 404 from the asset store; " +
        "the dashboard captures are not served\n",
    );
    expect(logs.parse()).toEqual([
      {
        event: "health-degraded",
        ray: "probe-TOK",
        method: "GET",
        path: "/health",
        status: 503,
        duration_ms: expect.any(Number),
        reason: "the share card answered 404 from the asset store",
      },
    ]);

    // A store that is down is the same answer, not a 500: the Worker is
    // serving, the captures are not, and a 500 would name the wrong one.
    const broken = { ASSETS: { fetch: () => Promise.reject(new Error("store unreachable")) } };
    const down = await call({}, { path: "/health", env: broken });
    expect(down.status).toBe(503);
    // /health is unauthenticated and public, so the reason it carries is fixed
    // text: a store exception message names the deploy's internals to whoever
    // asks. The detail is logged instead, where an operator reads it.
    const downBody = await down.text();
    expect(downBody).toBe(
      "degraded: the asset store could not be read; the dashboard captures are not served\n",
    );
    expect(downBody).not.toContain("store unreachable");
    const storeLine = logs.parse().find((l) => l.event === "asset-store-unreadable");
    expect(storeLine.error).toBe("the asset store could not be read: store unreachable");
  } finally {
    logs.restore();
  }
});

// The cap on a folded line counts code points, not UTF-16 code units. A store
// message long enough to reach the cap with an astral character astride the
// cut used to lose half of it, and the line then carried a lone surrogate: it
// reads as U+FFFD in the answer and as an escape in the log. The padding puts
// the high surrogate exactly on the boundary the old unit-counting cut fell on.
test("a folded reason caps on code points, never between the halves of one", async () => {
  const padding = "x".repeat(164);
  const broken = { ASSETS: { fetch: () => Promise.reject(new Error(`${padding}😀tail`)) } };
  const logs = captureLogs();
  try {
    const res = await call({}, { path: "/health", env: broken });
    expect(res.status).toBe(503);
    const body = await res.text();
    // The cap is on the logged detail, which is where the store's own text
    // lands; the answer carries fixed text and none of it.
    expect(body).not.toContain(padding);
    // 35 characters of prefix plus 164 of padding is 199 units, so the emoji's
    // high surrogate is unit 200 and the low one is past the old cut.
    const storeLine = logs.parse().find((l) => l.event === "asset-store-unreadable");
    expect(storeLine.error).toBe(`the asset store could not be read: ${padding}😀`);
    expect(storeLine.error).not.toContain("�");
    expect(logs.parse().at(-1).reason).toBe("the asset store could not be read");
  } finally {
    logs.restore();
  }
});

// A coding the runtime cannot build is a fallback, not a failed request: the
// next acceptable coding answers, and the client sees a page. It is logged
// anyway, because the slot then holds that failure for the isolate's life and
// the site answers uncompressed from then on. A fresh isolate so the coding
// cache starts empty.
test("a coding the runtime cannot build is logged with the request behind it", async () => {
  const NativeCompressionStream = globalThis.CompressionStream;
  globalThis.CompressionStream = class extends NativeCompressionStream {
    constructor(format) {
      super(format);
      if (format === "gzip") throw new Error("gzip unavailable");
    }
  };
  const { default: freshWorker } = await import("./worker.js?coding-dropped");
  const logs = captureLogs();
  try {
    const res = await freshWorker.fetch(
      new Request(ORIGIN, {
        headers: { "accept-encoding": "gzip, identity;q=0.5", "cf-ray": "zip-TOK" },
      }),
      staticAssets(),
    );
    expect(res.status).toBe(200);
    expect(res.headers.get("content-encoding")).toBeNull();
    expect(logs.parse()).toEqual([
      {
        event: "coding-dropped",
        ray: "zip-TOK",
        method: "GET",
        path: "/",
        duration_ms: expect.any(Number),
        coding: "gzip",
        error: "gzip unavailable",
        // The frames ride along for the same reason the unhandled line's do:
        // a build that dies mid-pipeline leaves the page uncompressed for the
        // isolate's life, and the message alone says which format, not where.
        stack: expect.stringContaining("gzip unavailable"),
      },
    ]);
    expect(logs.parse()[0].stack).not.toContain("\n");
  } finally {
    logs.restore();
    globalThis.CompressionStream = NativeCompressionStream;
  }
});

test("every page answer carries the security headers, not only revalidations", async () => {
  const fresh = await call();
  const gzipped = await call({ "accept-encoding": "gzip" });
  const brotli = await call({ "accept-encoding": "br" });
  const revalidated = await call({ "if-none-match": fresh.headers.get("etag") });
  expect(revalidated.status).toBe(304);
  for (const res of [fresh, gzipped, brotli, revalidated]) {
    for (const name of SECURITY_HEADER_NAMES) {
      expect(res.headers.get(name)).not.toBeNull();
    }
  }
  // includeSubDomains is what pins a subdomain nobody configured before its
  // first request, so dropping it is a silent weakening of the same header.
  const hsts = fresh.headers.get("strict-transport-security");
  expect(hsts).toContain("max-age=");
  expect(hsts).toContain("includeSubDomains");
});

const IMAGE_CACHE = "public, max-age=86400, stale-while-revalidate=3600";

function staticAssets(body = new Uint8Array([1, 2, 3, 4])) {
  return {
    ASSETS: {
      fetch: () =>
        new Response(body, {
          status: 200,
          headers: { "content-type": "image/png" },
        }),
    },
  };
}

test("dashboard images take cache and security headers from the worker", async () => {
  const body = new Uint8Array([1, 2, 3, 4]);
  const env = staticAssets(body);
  for (const path of ["/dashboard.png", "/dashboard.webp"]) {
    const res = await worker.fetch(new Request(ORIGIN + path), env);
    expect(res.status).toBe(200);
    expect(res.headers.get("cache-control")).toBe(IMAGE_CACHE);
    expect(res.headers.get("content-type")).toBe("image/png");
    for (const name of SECURITY_HEADER_NAMES) {
      expect(res.headers.get(name)).not.toBeNull();
    }
    expect(new Uint8Array(await res.arrayBuffer())).toEqual(body);
  }
});

test("dashboard image HEAD is bodyless with the same headers as GET", async () => {
  const env = staticAssets();
  const get = await worker.fetch(new Request(`${ORIGIN}/dashboard.png`), env);
  const head = await worker.fetch(new Request(`${ORIGIN}/dashboard.png`, { method: "HEAD" }), env);
  expect(head.status).toBe(200);
  expect(head.headers.get("cache-control")).toBe(get.headers.get("cache-control"));
  expect((await head.arrayBuffer()).byteLength).toBe(0);
  for (const name of SECURITY_HEADER_NAMES) {
    expect(head.headers.get(name)).not.toBeNull();
  }
});

test("dashboard images reject non-GET/HEAD without fetching assets", async () => {
  let fetched = false;
  const env = {
    ASSETS: {
      fetch: () => {
        fetched = true;
        return new Response("no");
      },
    },
  };
  const res = await worker.fetch(new Request(`${ORIGIN}/dashboard.png`, { method: "POST" }), env);
  expect(res.status).toBe(405);
  expect(res.headers.get("allow")).toBe("GET, HEAD");
  expect(fetched).toBe(false);
  for (const name of SECURITY_HEADER_NAMES) {
    expect(res.headers.get(name)).not.toBeNull();
  }
});

test("served HTML does not carry source comments", () => {
  expect(identityBody.includes("<!--")).toBe(false);
  expect(identityBody.includes("/*")).toBe(false);
});

test("hero is the captured dashboard, not an ASCII stand-in", () => {
  expect(identityBody.includes("<picture>")).toBe(true);
  expect(identityBody.includes('type="image/avif"')).toBe(true);
  expect(
    identityBody.includes(
      "/dashboard-768.avif 768w, /dashboard-1280.avif 1280w, /dashboard.avif 1920w",
    ),
  ).toBe(true);
  expect(
    identityBody.includes(
      "/dashboard-768.webp 768w, /dashboard-1280.webp 1280w, /dashboard.webp 1920w",
    ),
  ).toBe(true);
  expect(identityBody.includes('src="/dashboard.png"')).toBe(true);
  expect(identityBody.includes("decoding=")).toBe(false);
});

test("hero source sizes account for body gutters, column cap, and figure borders", () => {
  const sources = [...identityBody.matchAll(/<source\b[^>]*>/g)];
  expect(sources).toHaveLength(2);
  for (const [source] of sources) {
    expect(source).toContain(
      'sizes="(max-width: 640px) calc(100vw - 1.7rem - 2px), calc(min(76rem, 100vw - 2.5rem) - 2px)"',
    );
  }
});

test("section titles are sentence case on the body scale, not marketing labels", () => {
  expect(identityBody.includes("text-transform")).toBe(false);
  expect(identityBody.includes("letter-spacing")).toBe(false);
  expect(identityBody.includes("max-width: 62ch")).toBe(true);
});

// Every level of the page is a named step, and the steps descend. Size is the
// only thing marking a level on a page with no uppercase, no tracking and no
// color change, so a rule that sizes text in rems of its own, or a step that
// lands below the one under it, is a level the eye can no longer find.
test("the type scale is named, ordered, and the one place a size is written", () => {
  const steps = new Map(
    [...identityBody.matchAll(/--fs-([\w-]+):\s*([\d.]+)(px|rem)/g)].map(
      ([, name, value, unit]) => [name, Number(value) * (unit === "rem" ? 16 : 1)],
    ),
  );
  expect([...steps.keys()].sort()).toEqual(["body", "h1", "h2", "lead", "micro", "small"].sort());
  expect(steps.get("h1")).toBe(steps.get("h2") * 2);
  expect(steps.get("h2")).toBeGreaterThan(steps.get("lead"));
  expect(steps.get("lead")).toBeGreaterThan(steps.get("body"));
  expect(steps.get("body")).toBeGreaterThan(steps.get("small"));
  expect(steps.get("small")).toBeGreaterThan(steps.get("micro"));
  for (const [, value] of identityBody.matchAll(FONT_SIZE_RE)) {
    const token = FONT_STEP_RE.exec(value.trim());
    if (token) expect(steps.has(token[1]), `${token[1]} is not a step on the scale`).toBe(true);
  }
  // Every font-size in the page is one of the steps above, with exactly one
  // named exception: the h1 at a bare 2rem inside the max-width: 640px query,
  // the phone step. Checking only the value would let a second bare size
  // appear on any rule and still pass, and that the scale is the one place a
  // size is written is the whole thing this test holds.
  const bare = [...identityBody.matchAll(FONT_SIZE_RE)]
    .filter(([, value]) => !FONT_STEP_RE.test(value.trim()))
    .map((match) => ({ value: match[1].trim(), index: match.index }));
  expect(bare).toHaveLength(1);
  expect(bare[0].value).toBe("2rem");
  const phoneQuery = identityBody.indexOf("@media (max-width: 640px)");
  expect(phoneQuery).toBeGreaterThan(-1);
  const selector = identityBody.slice(0, bare[0].index).match(RULE_OPEN_RE);
  expect(selector[1].trim()).toBe("h1");
  // The rule sits inside that query rather than after it: the media block
  // closes on a line holding only its brace, so an exemption that drifted
  // out to the base rules would show one here.
  const inside = identityBody.slice(phoneQuery, bare[0].index);
  expect(inside).not.toMatch(BLOCK_CLOSE_RE);
});

// The order test above reads px and rem as the same 16px root, so a scale that
// mixed them still passed it. The ratios are the whole point of the scale, and
// they only hold while every step moves together: a step in pixels keeps its
// size while the three above it follow the reader's browser text size, which
// doubles the h1 over an unchanged body step and leaves the eye with a level
// it cannot place. One unit, all six steps.
test("every step of the type scale is in the same unit", () => {
  const steps = [...identityBody.matchAll(/--fs-([\w-]+):\s*([\d.]+)(px|rem)/g)];
  expect(steps).toHaveLength(6);
  expect([...new Set(steps.map(([, , , unit]) => unit))]).toEqual(["rem"]);
});

// The page is monospaced on purpose, and a reader in Arabic or Japanese is not
// a hypothetical one. A stack that names only Latin families leaves every other
// script to the generic monospace keyword, which is the platform's choice
// rather than the page's: a proportional-metrics face on some systems, boxes
// on a desktop with no matching family. Naming one family per script is the
// whole fix, and it costs no webfont, because the page ships none.
test("the font token names a family for every script the page can fall to", () => {
  const token = FONT_TOKEN_RE.exec(identityBody);
  expect(token).not.toBeNull();
  const stack = token[1].replace(/\s+/g, " ");
  // A family may only follow the Latin ones; leading with a script-specific
  // face would let it take over the Latin run too.
  expect(stack.indexOf("Noto Sans Mono CJK")).toBeGreaterThan(stack.indexOf("Consolas"));
  for (const [script, family] of UNCOVERED_SCRIPTS) {
    expect(stack, `no ${script} family in --mono`).toContain(family);
  }
  expect(stack.endsWith("monospace")).toBe(true);
});

// The hero is the terminal's own title, and the terminal draws that title bold
// (internal/ui/theme.go, wordmark). At the default weight the largest type on
// the page read lighter than the sticky wordmark directly above it, so the
// first thing a reader saw had less presence than the navigation. Weight marks
// the top of the scale and nothing below it: bold on an h2 or a paragraph
// would put a second idea beside size on the levels the scale has to carry.
test("the hero wordmark is the terminal's bold title, and weight stops there", () => {
  expect(identityBody).toMatch(H1_RULE_RE);
  expect(identityBody).toMatch(BRAND_RULE_RE);
  const afterH1 = identityBody.slice(identityBody.indexOf(H1_RULE_END) + H1_RULE_END.length);
  expect([...new Set([...afterH1.matchAll(FONT_WEIGHT_RE)].map(([, w]) => w))]).toEqual(["600"]);
});

// The gaps between sections are a rhythm, not a number per rule. A literal in
// each rule that wants one leaves a page with nothing to retune: changing the
// rhythm means finding every copy, and the copy drifts.
test("vertical rhythm is three named steps, never a literal in a margin", () => {
  expect([...new Set([...identityBody.matchAll(SPACE_STEP_RE)].map(([, v]) => v))].length).toBe(3);
  expect(identityBody).toMatch(SECTION_GAP_RE);
  expect(identityBody).toMatch(TIGHT_GAP_RE);
  expect(identityBody).toMatch(RUNOUT_GAP_RE);
  expect(identityBody).not.toMatch(LITERAL_GAP_RE);
});

// A link is underlined at rest so color is not its only cue, and hover
// thickens that underline. A second device alongside it, a border that filled
// on hover, drew a rule two pixels under the one already there: every link the
// pointer crossed read as a rendering fault. The underline is the whole cue, so
// nothing else may draw under a link.
test("one underline device carries the link, at rest and on hover", () => {
  expect(identityBody).not.toMatch(HOVER_BORDER_RE);
  expect([...identityBody.matchAll(/text-decoration-thickness/g)]).toHaveLength(2);
  expect([...identityBody.matchAll(/text-decoration:\s*underline/g)]).toHaveLength(1);
  // The wordmark and the skip link are the two links that opt out of the
  // underline, so the count above stays the count of the body links.
  expect([...identityBody.matchAll(/text-decoration:\s*none/g)]).toHaveLength(2);
});

// The hero's three refusals are its pitch, and the footer repeated the same
// list two screens lower, where a reader who had scrolled past the hero had
// already read it. A footer line that says nothing the page has not already
// said is there to fill the bar, which is what makes a footer read as
// furniture. The footer names the repository and the license and stops.
test("the footer says what the page has not already said", () => {
  const footer = identityBody.slice(identityBody.indexOf("<footer>"));
  expect(footer.includes("no telemetry, no account, no daemon")).toBe(false);
  expect(footer.includes("github.com/maci0/toktop")).toBe(true);
  expect(footer.includes("MIT licensed")).toBe(true);
});

// The footer is the page's contentinfo landmark, and a <footer> inside <main>
// is not one: its role is scoped to the main landmark and a screen reader
// finds no contentinfo to jump to. It sits after </main>, not inside it.
test("the footer is the contentinfo landmark, outside main", () => {
  expect(identityBody).toContain("</main>\n\n<footer>");
  const main = identityBody.slice(identityBody.indexOf("<main"), identityBody.indexOf("</main>"));
  expect(main.includes("<footer")).toBe(false);
  // Out of main, the footer is a body child, so it carries main's column width
  // itself. Without it the footer spans the padded body and the two no longer
  // line up on a wide screen.
  expect(identityBody).toMatch(FOOTER_MAXWIDTH_RE);
});

// Safari drops the list semantics of any list whose markers are removed, so
// VoiceOver reads the four feature panes as four loose paragraphs and never
// announces the list or its length (WCAG 1.3.1). The role is the documented
// remedy, and it is the only thing standing between the grid styling and that
// regression, which is why it is pinned here.
test("the feature grid keeps its list semantics under list-style: none", () => {
  expect(identityBody).toMatch(GRID_LIST_RE);
  expect(identityBody).toMatch(GRID_LIST_STYLE_RE);
});

// The second accent is cYellow in the terminal, where amber is pressure. On
// the page it marks the pane about pressure and nothing else: a second accent
// alternated by position is decoration, and decoration is what makes a page
// read as a template rather than as this product.
test("the second accent marks the pressure pane, and only it", () => {
  expect([...identityBody.matchAll(/var\(--warm\)/g)]).toHaveLength(1);
  expect(identityBody.includes(".grid li:last-child { border-top-color: var(--warm); }")).toBe(
    true,
  );
  expect(identityBody.includes("nth-child")).toBe(false);
  const panes = [...identityBody.matchAll(/<li><b>([^<]+)<\/b>/g)].map(([, name]) => name);
  expect(panes).toEqual(["Engines", "Agents", "Probes", "System"]);
});

// The panes are laid out the way the dashboard lays them out: the pressure
// pane is the full-width SYS strip under the charts, not a fourth equal cell.
// Four equal cells is the card grid, and a grid that one reflow "tidies" back
// into symmetry is a grid that stopped being a picture of the product.
test("the pressure pane runs the width the dashboard's SYS strip runs", () => {
  expect(identityBody.includes(".grid li:last-child { grid-column: 1 / -1; }")).toBe(true);
});

// The page is a picture of a terminal, so it has to be the same terminal.
// Three files carry the palette (this worker, internal/ui/theme.go,
// scripts/screenshot.py) in three languages, and nothing in the build ties
// them together: a hex edited in one leaves a site that no longer matches the
// dashboard it is showing. These are the checks that make the comments in
// those files true rather than aspirational.
const repoFile = (rel) => readFileSync(join(import.meta.dir, "..", ...rel), "utf8");
const repoHexes = (rel) => repoFile(rel).match(/#[0-9a-f]{6}/gi) ?? [];

const themeHex = (token) =>
  repoFile(["internal", "ui", "theme.go"]).match(
    new RegExp(`${token}\\s*=\\s*lipgloss\\.Color\\("(#[0-9a-f]{6})"\\)`),
  )?.[1];

// scripts/screenshot.py states its colors as RGB tuples; read them back as the
// hex the TUI uses so the two are compared in one notation.
const shotPalette = () => {
  const src = repoFile(["scripts", "screenshot.py"]);
  const toHex = (tuple) =>
    `#${tuple
      .match(PALETTE_ENTRY_RE)
      .map((n) => Number(n).toString(16).padStart(2, "0"))
      .join("")}`;
  const named = (name) => {
    const m = src.match(new RegExp(`^${name}: RGB = (\\(\\d+, \\d+, \\d+\\))`, "m"));
    if (m === null) throw new Error(`${name} not found in scripts/screenshot.py`);
    return toHex(m[1]);
  };
  const block = src.match(ANSI16_BLOCK_RE);
  if (block === null) throw new Error("ANSI16 not found in scripts/screenshot.py");
  const ansi = {};
  for (const [, index, tuple] of block[1].matchAll(ANSI16_ENTRY_RE)) {
    ansi[index] = toHex(tuple);
  }
  return { bg: named("BG"), fg: named("FG_DEFAULT"), ansi };
};
const SHOT = shotPalette();

// The five values internal/ui/theme.go names as shared with the site, by CSS
// token on the dark scheme and, where the renderer has an SGR slot for them,
// by that slot's index.
const SHARED = [
  ["cBase", "--dark-bg"],
  ["cText", "--dark-fg"],
  ["cDim", "--dark-dim"],
  ["cGreen", "--dark-accent", 2],
  ["cYellow", "--dark-warm", 3],
];

test("the page ships the terminal's palette, not one of its own", () => {
  for (const [token, cssVar, sgr] of SHARED) {
    const hex = themeHex(token);
    expect(hex, `${token} not found in internal/ui/theme.go`).toBeDefined();
    expect(
      identityBody.includes(`${cssVar}: ${hex}`),
      `${cssVar} does not carry theme.go ${token} (${hex})`,
    ).toBe(true);
    if (sgr !== undefined) {
      expect(SHOT.ansi[sgr], `scripts/screenshot.py ANSI16[${sgr}]`).toBe(hex);
    }
  }
  // The two the renderer states outside the ANSI table: the page background
  // behind the capture, and the text a cell sets no color for.
  expect(SHOT.bg).toBe(themeHex("cBase"));
  expect(SHOT.fg).toBe(themeHex("cText"));
});

// The renderer also paints the TUI's status colors, which the page has no
// token for: they are the heat ramp and the kind badges, in the capture the
// site is showing.
test("the capture renderer paints the status colors of the dashboard", () => {
  for (const [token, sgr] of [
    ["cBorder", 0],
    ["cRed", 1],
    ["cBlue", 4],
    ["cCyan", 6],
  ]) {
    expect(SHOT.ansi[sgr], `scripts/screenshot.py ANSI16[${sgr}]`).toBe(themeHex(token));
  }
});

// Every hex the page ships is a named token. A hex written straight into a
// rule is the start of a second palette, which is how a site stops being one.
test("the page paints in named tokens only", () => {
  const named = new Set(
    [...identityBody.matchAll(/--(?:dark-)?[\w-]+:\s*(#[0-9a-f]{6})/gi)].map(([, hex]) =>
      hex.toLowerCase(),
    ),
  );
  const loose = [...identityBody.matchAll(/#[0-9a-f]{6}/gi)]
    .map(([hex]) => hex.toLowerCase())
    .filter((hex) => !named.has(hex));
  expect(loose).toEqual([]);
  expect(named.size).toBe(14); // seven dark, seven light
});

// The identity is phosphor green on cool dark. Violet is the default this
// product does not have, and in a one-line palette edit it would be invisible
// in the diff, so it is named here rather than left to a reviewer's eye.
test("no purple or violet in the palette of any of the three files", () => {
  for (const rel of [["site"], ["internal", "ui", "theme.go"], ["scripts", "screenshot.py"]]) {
    const violets = rel.length === 1 ? servedVioletHexes() : repoHexes(rel).filter(isViolet);
    expect(violets, `${rel.join("/")} has a violet`).toEqual([]);
  }

  function servedVioletHexes() {
    return [...new Set(identityBody.match(/#[0-9a-f]{6}/gi) ?? [])].filter(isViolet);
  }

  function isViolet(hex) {
    const [r, g, b] = [1, 3, 5].map((i) => parseInt(hex.slice(i, i + 2), 16) / 255);
    const [max, min] = [Math.max(r, g, b), Math.min(r, g, b)];
    const delta = max - min;
    if (delta / max < 0.25) return false; // a neutral or a near-neutral has no hue to get wrong
    const hue =
      60 *
      (max === r ? ((g - b) / delta) % 6 : max === g ? (b - r) / delta + 2 : (r - g) / delta + 4);
    // 230-315 degrees covers indigo (239-245) through violet, which is the
    // band Tailwind's indigo-500 and violet-600 sit in. cBlue sits at 217 and
    // the sand ANSI16[5] the renderer uses at 26, so both stay clean.
    return hue > 230 && hue < 315;
  }
});

test("accessibility contracts: skip link, motion preferences, focus indicators, and landmarks", () => {
  expect(identityBody.includes('class="skip-link"')).toBe(true);
  expect(identityBody.includes("prefers-reduced-motion: no-preference")).toBe(true);
  expect(identityBody.includes(":focus-visible")).toBe(true);
  expect(identityBody.includes('role="region"')).toBe(true);
  expect(identityBody.includes('aria-labelledby="install-heading"')).toBe(true);
  expect(identityBody.includes('<html lang="en">')).toBe(true);
});

// The blink has to stop, not just be skippable by preference. A user who
// never set prefers-reduced-motion faces a page that blinks for as long as
// they read it, with nothing on the page to stop it, which 2.2.2 does not
// allow (WCAG). The animation is therefore bounded, and a rule that reopens
// it as an infinite loop is a test failure.
test("the blinking cursor stops instead of looping forever", () => {
  const block = identityBody.match(BLINK_CURSOR_BLOCK_RE)?.[0];
  expect(block).toBeDefined();
  expect(block).not.toMatch(INFINITE_RE);
  // The bound is a cycle count on the animation, not a duration elsewhere.
  const cycles = Number(block?.match(BLINK_CYCLES_RE)?.[1]);
  expect(cycles).toBeGreaterThan(0);
  // Under five seconds of motion at the declared period.
  const period = Number(block?.match(BLINK_PERIOD_RE)?.[1]);
  expect(cycles * period).toBeLessThanOrEqual(5);
});

// A nav link's accessible name has to contain the text it shows, because a
// voice-control user says what they see and the page must answer to it
// (WCAG 2.5.3). The section headings the labels used to repeat are already
// reachable, so overriding the visible text bought nothing.
test("nav links are named by their visible text, not an aria-label", () => {
  const nav = identityBody.match(NAV_BLOCK_RE)?.[0];
  expect(nav).toBeDefined();
  // The nav landmark keeps its own label; the links inside it carry none,
  // so each one is named by the text it shows.
  expect(nav).toMatch(NAV_LABELED_RE);
  const links = [...nav.matchAll(ANCHOR_TAG_RE)].map((match) => match[0]);
  expect(links.length).toBeGreaterThan(0);
  for (const link of links) {
    expect(link).not.toMatch(ARIA_LABEL_ATTR_RE);
    expect(link).not.toMatch(ARIA_LABELLEDBY_ATTR_RE);
  }
  for (const [href, text] of [
    ["#shows", "Shows"],
    ["#measured", "Measured"],
  ]) {
    expect(nav).toContain(`<a href="${href}">${text}</a>`);
  }
});

// The skip link is only useful if the reader can see where it put them. main
// takes focus from that key press, and a rule that removed the outline there
// left a keyboard user with no focus indicator at all (WCAG 2.4.7), so the
// page must not suppress it on the skip target.
test("the skip link's focus target keeps a visible focus indicator", () => {
  expect(identityBody.includes('<main id="top" tabindex="-1">')).toBe(true);
  expect(identityBody).not.toMatch(FOCUS_KILLED_RE);
});

// A shell prompt is decoration. Read out, the figcaption announced "dollar"
// ahead of the command it introduces (WCAG 1.1.1 / 4.1.2).
test("the shell prompt in the capture caption is hidden from assistive technology", () => {
  expect(identityBody).toMatch(SHELL_PROMPT_RE);
});

// The capture is the product, drawn as a terminal on the light paper too. The
// frame re-points the scheme tokens so nothing inside it can be recolored by
// the page it sits on, and the list is complete rather than the four tokens
// its surfaces happen to paint with: --fg and --accent still carrying the
// light values put near-black text on the dark frame and a light accent on a
// dark surface, unreadable rather than merely out of place. Each pair is
// checked in both directions, so dropping either re-point fails here.
test("the terminal frame carries the whole dark scheme, text tokens included", () => {
  const frame = identityBody.match(SHOT_FRAME_RE)?.[0] ?? "";
  for (const name of ["bg", "panel", "line", "fg", "dim", "accent"]) {
    expect(frame, `.shot does not re-point --${name}`).toContain(`--${name}: var(--dark-${name})`);
  }
});

// kbd is a UI component and this border is the only thing that draws it:
// --line measures about 1.3:1 against the page background, under the 3:1 that
// identifies a component's boundary (WCAG 1.4.11). Both schemes are measured,
// so a token edit that helps one scheme cannot quietly break the other.
test("a keycap's boundary meets the 3:1 non-text contrast floor in both schemes", () => {
  const kbd = identityBody.match(KBD_RULE_RE)?.[0] ?? "";
  expect(kbd).toContain("border: 1px solid var(--fg)");
  const token = (name) => identityBody.match(paletteVarRE(name))?.[1];
  for (const bg of [token("bg"), token("panel")]) {
    expect(relContrast(token("fg"), bg)).toBeGreaterThanOrEqual(3);
  }
  // The divider the keycap replaced, recorded so a future edit that puts it
  // back fails with the number it dropped to rather than only the inequality.
  expect(relContrast(token("line"), token("bg"))).toBeCloseTo(1.32, 2);
});

// The same 3:1 floor, on the two boundaries that name something rather than
// divide the page. The sticky bar is --bg, the token the page scrolls under
// it on, so its rule is the only edge between the bar and the section passing
// behind. A code block is a focusable scroller whose scrollbar does not appear
// until it is hovered, and --panel against --bg is itself under 1.5:1, so its
// rule is the only cue that it scrolls at all. Both schemes are measured, the
// way the keycap above is.
test("the sticky bar and the code block meet the 3:1 boundary floor", () => {
  const token = (name) => identityBody.match(paletteVarRE(name))?.[1];
  for (const rule of [BAR_RULE_RE, PRE_RULE_RE]) {
    const body = identityBody.match(rule)?.[0] ?? "";
    expect(body, `${rule} matched no rule`).toContain("solid var(--fg)");
    expect(body, `${rule} still draws its edge with --line`).not.toContain("solid var(--line)");
  }
  for (const bg of [token("bg"), token("panel")]) {
    expect(relContrast(token("fg"), bg)).toBeGreaterThanOrEqual(3);
  }
});

// Every label in the bar is the only route to its section, so no width may
// push one out of reach. Text is sized in rem, so a reader at a 1.4.4 text
// size gets the same layout at fewer pixels across, and the bar has no
// horizontal scroller: a nav that did not wrap pushed the last link past the
// right edge with nothing to scroll to it. The wrap belongs in the base rules
// rather than the phone breakpoint, because enlarged text makes a wide pane as
// tight as a narrow one.
test("the section list wraps at every width, not only on a phone", () => {
  for (const rule of [BAR_RULE_RE, NAV_CSS_RE]) {
    const body = identityBody.match(rule)?.[0] ?? "";
    expect(body, `${rule} matched no rule`).toContain("flex-wrap: wrap");
  }
});

// RFC 6928 initcwnd: ten ~1460-byte segments (~14 KB). Identity bytes plus
// inline CSS are everything there is, so staying under this keeps first paint
// at one round trip. The identity size is the record: a copy change that
// grows the document fails here instead of hiding under the window ceiling.
// The two compressed sizes are recorded as measured under the pinned bun.
// Re-measure them with this test when the page changes.
test("recorded transfer sizes stay inside the initial congestion window", async () => {
  const budget = 10 * 1460;
  const identity = new Uint8Array(await (await call()).arrayBuffer()).byteLength;
  const gzipped = new Uint8Array(await (await call({ "accept-encoding": "gzip" })).arrayBuffer())
    .byteLength;
  const brotli = new Uint8Array(await (await call({ "accept-encoding": "br" })).arrayBuffer())
    .byteLength;
  expect(identity).toBe(13093);
  expect(gzipped).toBe(4506);
  expect(brotli).toBe(3814);
  expect(identity).toBeLessThan(budget);
  expect(gzipped).toBeLessThan(budget);
  expect(brotli).toBeLessThan(budget);
  expect(brotli).toBeLessThan(gzipped);
  expect(gzipped).toBeLessThan(identity);
});

// The three numbers above are the only record anybody keeps of how heavy the
// page got, and site/README.md restates them as prose. Prose is not measured:
// a copy edit moved the page 30 bytes and the README kept quoting the old
// three, so the one document that says how fast the site is described a page
// this repo no longer serves. The README is read here and its figures are
// compared against the bodies just measured, so a re-record is a test failure
// with the new numbers in it rather than a silent drift.
test("the README records the transfer sizes the page actually ships", async () => {
  const readme = readFileSync(join(import.meta.dir, "README.md"), "utf8");
  const recorded = readme.match(README_SIZES_RE);
  expect(recorded).not.toBeNull();
  const stated = recorded.slice(1).map(thousandsStripped);
  const visit = [...readme.matchAll(README_VISIT_RE)].map((match) =>
    match.slice(1).map(thousandsStripped),
  );
  for (const [index, coding] of ["", "gzip", "br"].entries()) {
    const bytes = new Uint8Array(
      await (await call(coding ? { "accept-encoding": coding } : {})).arrayBuffer(),
    ).byteLength;
    expect(stated[index]).toBe(bytes);
  }
  // The same pair the phone test bounds above, stated as the whole visit.
  expect(visit).toEqual([[stated[2], stated[2] + assetBytes("dashboard-768.avif")]]);
  expect(visit[0][1]).toBe(14_391);
});

const PUBLIC = join(import.meta.dir, "public");
const assetBytes = (name) => statSync(join(PUBLIC, name)).size;

// The byte counts above say how much the page sends; they cannot see the edge
// taking longer to send it, which is the other half of time to first byte and
// the half a cold isolate controls. Every page and image answer therefore
// carries the edge's own cost in Server-Timing, where a RUM script or a
// visitor's own devtools can read it. The failures carry it too: a failed
// request is the one a visitor reports, and its time at the edge is part of
// that report, so a timing series that covered only the served requests would
// describe exactly the ones nobody is asking about.
test("every answer, served or failed, reports the edge cost in Server-Timing", async () => {
  const dur = (value) => EDGE_DUR_RE.exec(value ?? "")?.[1];
  for (const headers of [{}, { "accept-encoding": "br" }]) {
    const res = await call(headers);
    expect(dur(res.headers.get("server-timing"))).toBeDefined();
  }
  const etag = (await call()).headers.get("etag");
  const revalidated = await call({ "if-none-match": etag });
  expect(dur(revalidated.headers.get("server-timing"))).toBeDefined();
  const image = await call({}, { path: "/dashboard.png", env: staticAssets() });
  expect(dur(image.headers.get("server-timing"))).toBeDefined();
  const failures = [
    await call({}, { method: "POST" }),
    await call({ "accept-encoding": "identity;q=0" }),
    await call({}, { path: "/health", env: staticAssets() }),
  ];
  for (const res of failures) {
    expect(dur(res.headers.get("server-timing"))).toBeDefined();
  }
});

// Two requests, both from this origin: the document and the one hero image
// its srcset picks. Nothing else is fetched, because the page has no script,
// no webfont and no external stylesheet, and the favicon is a data URI rather
// than a file. The per-asset ceilings below bound each half; this bounds the
// pair, which is what a phone on a mobile network actually waits for.
test("a phone's visit is the document and the 768w capture, and fits in 25 KB", async () => {
  expect(identityBody.includes("<script")).toBe(false);
  expect(identityBody.includes('rel="stylesheet"')).toBe(false);
  // The one <link> is the data-URI favicon: a link to a file would be a
  // fourth thing on the critical path.
  const links = [...identityBody.matchAll(/<link\b[^>]*>/g)];
  expect(links).toHaveLength(1);
  expect(links[0][0]).toContain('rel="icon"');
  expect(links[0][0]).toContain("data:image/svg+xml,");

  const brotli = new Uint8Array(await (await call({ "accept-encoding": "br" })).arrayBuffer())
    .byteLength;
  const visit = brotli + assetBytes("dashboard-768.avif");
  expect(visit).toBe(14_391);
  expect(visit).toBeLessThan(25_000);
});

// The captures keep stable names, so a re-capture deploy cannot reach a
// browser that still holds the old one: revalidation is the whole invalidation
// story, and stale-while-revalidate is when it runs. A week of that window is
// a week-old screenshot of the dashboard on a public page, so the ceiling here
// is a day and the hour-long window is deliberate.
test("a re-capture reaches a returning browser within a day, not a week", () => {
  const directives = new Map(
    IMAGE_CACHE.split(",").map((part) => {
      const [name, value] = part.trim().split("=");
      return [name, Number(value)];
    }),
  );
  expect(directives.get("max-age")).toBe(86_400);
  expect(directives.get("stale-while-revalidate")).toBeLessThanOrEqual(86_400);
});

// The page carries the same window the captures do. Its bytes change only at a
// deploy, so a browser holding one serves it from cache and revalidates behind
// the paint: that is what keeps a repeat visit off the edge. The window is
// still how long a returning browser can paint a page from before the deploy
// that replaced it, so it is an hour rather than a day, and it is pinned here
// because nothing else measures the page's freshness.
test("the page's revalidate window is the hour the captures use", async () => {
  const policy = "public, max-age=300, stale-while-revalidate=3600";
  const cacheControl = (await call()).headers.get("cache-control");
  expect(cacheControl).toBe(policy);
  const revalidated = await call({ "if-none-match": (await call()).headers.get("etag") });
  expect(revalidated.headers.get("cache-control")).toBe(policy);
});

test("hero AVIF is smaller than WebP at every width, and each width beats the next", () => {
  for (const width of ["768", "1280", ""]) {
    const suffix = width ? `-${width}` : "";
    expect(assetBytes(`dashboard${suffix}.avif`)).toBeLessThan(
      assetBytes(`dashboard${suffix}.webp`),
    );
  }
  expect(assetBytes("dashboard-768.avif")).toBeLessThan(assetBytes("dashboard-1280.avif"));
  expect(assetBytes("dashboard-1280.avif")).toBeLessThan(assetBytes("dashboard.avif"));
  expect(assetBytes("dashboard-768.webp")).toBeLessThan(assetBytes("dashboard-1280.webp"));
  expect(assetBytes("dashboard-1280.webp")).toBeLessThan(assetBytes("dashboard.webp"));
  expect(assetBytes("dashboard.avif")).toBeLessThan(50_000);
  expect(assetBytes("dashboard-1280.avif")).toBeLessThan(30_000);
  expect(assetBytes("dashboard-768.avif")).toBeLessThan(12_000);
  expect(assetBytes("dashboard-1280.webp")).toBeLessThan(100_000);
  expect(assetBytes("dashboard-768.webp")).toBeLessThan(45_000);
  expect(assetBytes("dashboard.webp")).toBeLessThan(160_000);
});

// The slot a phone actually takes: a 2x screen at the 360 CSS px the figure
// occupies needs 722 device pixels, so the srcset hands it the 768w candidate.
// Without that entry the browser rounds up to 1280w and downloads 25,360 bytes
// to fill 722 of them. A 768w capture covers 36% of the 1280w area and lands
// at 42% of its weight; the ceiling is set past that, so a re-capture that
// drops or fattened the phone candidate fails here instead of quietly
// doubling the weight of the visit that matters most.
test("the phone slot is served by the 768w capture, not the 1280w one", () => {
  expect(assetBytes("dashboard-768.avif")).toBeLessThan(assetBytes("dashboard-1280.avif") * 0.6);
  expect(assetBytes("dashboard-768.webp")).toBeLessThan(assetBytes("dashboard-1280.webp") * 0.6);
});

// The <img src> fallback is the one download on the page that no srcset
// narrows: a client with neither AVIF nor WebP pays all of it. Nothing else
// measures that path, so a re-capture at a higher scale would double the
// worst-case hero weight with CI green. The ceiling is above today's 303,865
// and well under the ~14 KB the rest of the page fits in, so it records the
// gap instead of hiding it.
test("the PNG fallback stays bounded", () => {
  expect(assetBytes("dashboard.png")).toBeLessThan(320_000);
});

// The repository front page is a browser surface too, and a bigger download
// than the site it links to: GitHub's renderer drops srcset and picture, so
// the README gets exactly the one file it names, at whatever size that file
// is. It named the 3240px PNG, 303,865 bytes, where the same frame at 1920px
// in AVIF is 45,559. The ceiling and the identity of the bytes are both
// pinned: the capture is the site's 1920w candidate, so a re-capture cannot
// leave the repository showing one dashboard and the landing page another.
const REPO = join(import.meta.dir, "..");
const README_IMAGE = "docs/images/dashboard.avif";
const README_BUDGET = 60_000;
test("the README names a capture inside its own byte budget", () => {
  const readme = readFileSync(join(REPO, "README.md"), "utf8");
  const [tag] = readme.match(/<img\b[^>]*>/g) ?? [];
  expect(tag).toBeDefined();
  expect(tag).toContain(`src="${README_IMAGE}"`);
  // The height the box is reserved with, so the README's own render does not
  // shift the image down the page after it paints.
  expect(tag).toMatch(IMG_SIZE_RE);
  const bytes = readFileSync(join(REPO, README_IMAGE));
  expect(bytes.byteLength).toBeLessThan(README_BUDGET);
  expect(bytes.byteLength).toBeLessThan(assetBytes("dashboard.png") * 0.2);
  expect(bytes).toEqual(readFileSync(join(PUBLIC, "dashboard.avif")));
});

// The share card is the same frame at the width a card is laid out at. The
// og:image crawlers fetch it on every share, so the pixels past 1200 are
// bytes they download and never draw: the 3240px original is 304 KB of which
// a 1200px palette PNG shows 69 KB. This pins both halves, so a re-capture
// that stops producing the card (or stops quantizing it) fails here instead
// of shipping a share preview four times its size.
test("the share card is the capture at card width, not the full-size original", () => {
  const card = readFileSync(join(PUBLIC, "dashboard-card.png"));
  // IHDR width and height, big-endian at the fixed offset 16: every PNG
  // starts with an 8-byte signature, a 4-byte length, the "IHDR" type, then
  // the two fields.
  const view = new DataView(card.buffer, card.byteOffset, card.byteLength);
  expect([view.getUint32(16), view.getUint32(20)]).toEqual([1200, 704]);
  expect(assetBytes("dashboard-card.png")).toBeLessThan(80_000);
  expect(assetBytes("dashboard-card.png")).toBeLessThan(assetBytes("dashboard.png") * 0.3);
  expect(identityBody).toContain(`og:image" content="${ORIGIN}/dashboard-card.png"`);
  expect(identityBody).toContain(`twitter:image" content="${ORIGIN}/dashboard-card.png"`);
});

// The <img width height> is the aspect ratio the browser reserves the box with
// before the fallback arrives, so it has to be the fallback file's own size and
// not the width the page happens to draw it at. Stating a size the file does
// not have reserves the right ratio by coincidence: a re-capture at a slightly
// different ratio shifts the hero by a few pixels after it paints. Both halves
// are read off the file, so a re-capture that changes the capture without
// changing the markup fails here.
test("the hero's reserved box is the fallback capture's own size", () => {
  const png = readFileSync(join(PUBLIC, "dashboard.png"));
  const view = new DataView(png.buffer, png.byteOffset, png.byteLength);
  const declared = [...identityBody.matchAll(IMG_TAG_RE)][0][0].match(IMG_SIZE_RE);
  expect([Number(declared[1]), Number(declared[2])]).toEqual([
    view.getUint32(16),
    view.getUint32(20),
  ]);
});

// A browser that reads the <link> never asks for this, but a crawler, a
// bookmark or a client that ignored the data URI does, and the one-page
// catch-all answered it with the whole document: 3,606 bytes of text/html for
// a request that wants an image, on a page whose whole budget is two requests.
// The answer must be the icon itself, and it must not need the asset binding
// the captures need.
test("/favicon.ico answers with the icon, not the page", async () => {
  const res = await call({}, { path: "/favicon.ico" });
  expect(res.status).toBe(200);
  expect(res.headers.get("content-type")).toBe("image/svg+xml");
  const body = await res.text();
  expect(body.startsWith("<svg ")).toBe(true);
  expect(Number(res.headers.get("content-length"))).toBe(new TextEncoder().encode(body).byteLength);
  // The same mark the page inlines, so the two cannot drift.
  const inlined = identityBody.match(INLINED_ICON_RE)[1];
  expect(decodeURIComponent(inlined)).toBe(body);
  expect(res.headers.get("cache-control")).toBe((await call()).headers.get("cache-control"));
  for (const name of SECURITY_HEADER_NAMES) {
    expect(res.headers.get(name)).not.toBeNull();
  }
  // A client that asks for the path blind asks again on every visit, so the
  // answer carries a validator and a repeat visit costs a 304 rather than the
  // icon again. The tag is strong: the icon ships as one encoding under one
  // URL, so unlike the page's it describes every representation of it.
  const etag = res.headers.get("etag");
  expect(etag).toBe(`"${etag.slice(1, -1)}"`);
  const revalidated = await call({ "if-none-match": etag }, { path: "/favicon.ico" });
  expect(revalidated.status).toBe(304);
  expect(revalidated.headers.get("etag")).toBe(etag);
  expect(revalidated.headers.get("content-length")).toBeNull();
  expect(EDGE_DUR_RE.exec(revalidated.headers.get("server-timing") ?? "")?.[1]).toBeDefined();
  expect(await revalidated.text()).toBe("");
  // A client's page validator says nothing about the icon: they are separate
  // constants in separate responses, and answering the icon fresh against the
  // page's tag would hand back a 304 for bytes the client never holds.
  const pageEtag = (await call()).headers.get("etag");
  const wrongTag = await call({ "if-none-match": pageEtag }, { path: "/favicon.ico" });
  expect(wrongTag.status).toBe(200);
  expect(await wrongTag.text()).toBe(body);
  expect((await call({ "if-none-match": "*" }, { path: "/favicon.ico" })).status).toBe(304);
  // HEAD carries the GET headers and no body, like every other HEAD here.
  const head = await call({}, { method: "HEAD", path: "/favicon.ico" });
  expect(head.status).toBe(200);
  expect(head.headers.get("content-type")).toBe("image/svg+xml");
  expect(await head.text()).toBe("");
  // A POST to it is still the one the method check answers.
  expect((await call({}, { method: "POST", path: "/favicon.ico" })).status).toBe(405);
});

function assetsEnv(bodies) {
  return {
    ASSETS: {
      fetch(request) {
        const url = new URL(request.url);
        const body = bodies[url.pathname];
        if (!(url.pathname in bodies)) {
          return Promise.resolve(new Response("missing", { status: 404 }));
        }
        const headers = {
          "content-type": "application/octet-stream",
          etag: `"${url.pathname}"`,
        };
        if (request.headers.get("accept-encoding")) {
          return Promise.resolve(
            new Response("compressed-by-mistake", {
              status: 500,
              headers,
            }),
          );
        }
        if (request.method === "HEAD") {
          return Promise.resolve(new Response(null, { status: 200, headers }));
        }
        return Promise.resolve(new Response(body, { status: 200, headers }));
      },
    },
  };
}

test("image paths 404 without ASSETS instead of falling through to the page", async () => {
  const logs = captureLogs();
  try {
    const res = await call({}, { path: "/dashboard.avif" });
    expect(res.status).toBe(404);
    expect(await res.text()).not.toBe(identityBody);
    expect(res.headers.get("content-type")).toBe("text/plain; charset=utf-8");
    expect(res.headers.get("cache-control")).toBe("no-store");
    for (const name of SECURITY_HEADER_NAMES) {
      expect(res.headers.get(name)).not.toBeNull();
    }
  } finally {
    logs.restore();
  }
});

test("image 404s from ASSETS are not cached as successes", async () => {
  const logs = captureLogs();
  try {
    const env = {
      ASSETS: {
        fetch: () => new Response("missing", { status: 404 }),
      },
    };
    const res = await call({}, { path: "/dashboard.png", env });
    expect(res.status).toBe(404);
    expect(res.headers.get("cache-control")).toBe("no-store");
  } finally {
    logs.restore();
  }
});

test("an asset-store failure answers with the worker's own error envelope", async () => {
  const logs = captureLogs();
  try {
    const missing = {
      ASSETS: {
        fetch: () =>
          Promise.resolve(
            new Response("<html><body>not here</body></html>", {
              status: 404,
              headers: { "content-type": "text/html" },
            }),
          ),
      },
    };
    const failed = {
      ASSETS: {
        fetch: () => Promise.resolve(new Response("boom", { status: 502 })),
      },
    };
    for (const [env, status, body] of [
      [missing, 404, "not found\n"],
      [failed, 502, "asset store error\n"],
    ]) {
      const res = await call({}, { path: "/dashboard.png", env });
      expect(res.status).toBe(status);
      expect(await res.text()).toBe(body);
      expect(res.headers.get("content-type")).toBe("text/plain; charset=utf-8");
      expect(res.headers.get("cache-control")).toBe("no-store");
      for (const name of SECURITY_HEADER_NAMES) {
        expect(res.headers.get(name)).not.toBeNull();
      }
    }
    const head = await call({}, { path: "/dashboard.png", env: missing, method: "HEAD" });
    expect(head.status).toBe(404);
    expect((await head.arrayBuffer()).byteLength).toBe(0);
  } finally {
    logs.restore();
  }
});

test("image paths are served from ASSETS with cache and security headers", async () => {
  const env = assetsEnv({ "/dashboard.avif": "avif-bytes" });
  const res = await call({ "accept-encoding": "gzip, br" }, { path: "/dashboard.avif", env });
  expect(res.status).toBe(200);
  expect(await res.text()).toBe("avif-bytes");
  expect(res.headers.get("cache-control")).toBe(IMAGE_CACHE);
  expect(res.headers.get("etag")).toBe('"/dashboard.avif"');
  for (const name of SECURITY_HEADER_NAMES) {
    expect(res.headers.get(name)).not.toBeNull();
  }
});

test("every dashboard URL in the HTML is served as an image asset", async () => {
  const paths = [...new Set([...identityBody.matchAll(/\/dashboard[-.\w]+/g)].map((m) => m[0]))];
  expect(paths.length).toBeGreaterThanOrEqual(5);
  const env = assetsEnv(Object.fromEntries(paths.map((p) => [p, p])));
  for (const path of paths) {
    const res = await call({}, { path, env });
    expect(res.status).toBe(200);
    expect(await res.text()).toBe(path);
  }
});

test("image revalidation forwards If-None-Match without Accept-Encoding", async () => {
  const env = {
    ASSETS: {
      fetch(request) {
        if (request.headers.get("accept-encoding")) {
          return Promise.resolve(new Response("compressed-by-mistake", { status: 500 }));
        }
        if (request.headers.get("if-none-match") === '"abc"') {
          return Promise.resolve(new Response(null, { status: 304, headers: { etag: '"abc"' } }));
        }
        return Promise.resolve(new Response("full", { status: 200, headers: { etag: '"abc"' } }));
      },
    },
  };
  const res = await call(
    { "if-none-match": '"abc"', "accept-encoding": "gzip" },
    { path: "/dashboard.png", env },
  );
  expect(res.status).toBe(304);
  expect(res.headers.get("etag")).toBe('"abc"');
  expect(res.headers.get("cache-control")).toBe(IMAGE_CACHE);
  expect(await res.text()).toBe("");
});

test("image HEAD matches GET headers with no body, POST is 405", async () => {
  const env = assetsEnv({ "/dashboard.webp": "webp-bytes" });
  const get = await call({}, { path: "/dashboard.webp", env });
  const head = await call({}, { path: "/dashboard.webp", method: "HEAD", env });
  expect(head.status).toBe(200);
  expect(head.headers.get("cache-control")).toBe(get.headers.get("cache-control"));
  expect((await head.arrayBuffer()).byteLength).toBe(0);
  const posted = await call({}, { path: "/dashboard.webp", method: "POST", env });
  expect(posted.status).toBe(405);
  expect(posted.headers.get("allow")).toBe("GET, HEAD");
  expect(posted.headers.get("content-type")).toBe("text/plain; charset=utf-8");
  expect(posted.headers.get("cache-control")).toBe("no-store");
});

// Failure paths write one JSON line to console.error (Workers Logs). Captured
// rather than printed so the assertions can read the lines back and the run
// stays quiet.
function captureLogs() {
  const lines = [];
  // biome-ignore lint/suspicious/noConsole: the point of this helper is to intercept those writes.
  const original = console.error;
  console.error = (...args) => lines.push(args.join(" "));
  return {
    parse: () => lines.map((line) => JSON.parse(line)),
    restore: () => {
      console.error = original;
    },
  };
}

test("an unhandled throw answers 500 and logs the request that caused it", async () => {
  const logs = captureLogs();
  try {
    const env = {
      ASSETS: { fetch: () => Promise.reject(new Error("asset store unreachable")) },
    };
    const res = await worker.fetch(
      new Request(`${ORIGIN}/dashboard.png`, {
        headers: { "cf-ray": "9a1b2c3d4e5f-TOK" },
      }),
      env,
    );
    expect(res.status).toBe(500);
    expect(await res.text()).toBe("internal error\n");
    expect(res.headers.get("content-type")).toBe("text/plain; charset=utf-8");
    expect(res.headers.get("cache-control")).toBe("no-store");
    for (const name of SECURITY_HEADER_NAMES) {
      expect(res.headers.get(name)).not.toBeNull();
    }
    const [line] = logs.parse();
    expect(line.event).toBe("unhandled");
    expect(line.ray).toBe("9a1b2c3d4e5f-TOK");
    expect(line.method).toBe("GET");
    expect(line.path).toBe("/dashboard.png");
    expect(line.error).toBe("asset store unreachable");
    expect(line.status).toBe(500);
    expect(typeof line.duration_ms).toBe("number");
    // The stack rides along because nothing else on the line says where the
    // throw came from: the request that caused it belongs to a visitor and the
    // isolate is gone by the time anyone reads the line. Folded onto one line,
    // since a raw stack would split the JSON object Workers Logs reads.
    expect(line.stack).toContain("Error: asset store unreachable");
    expect(line.stack).toContain("worker.js");
    expect(line.stack).not.toContain("\n");
    // The failing request is measurable the same way a served one is, so a
    // visitor's report and the log agree on what it cost.
    expect(res.headers.get("server-timing")).toMatch(DUR_SUFFIX_RE);
  } finally {
    logs.restore();
  }
});

test("a HEAD that throws answers 500 with no body, like every other HEAD", async () => {
  const logs = captureLogs();
  try {
    const env = {
      ASSETS: { fetch: () => Promise.reject(new Error("asset store unreachable")) },
    };
    const res = await worker.fetch(new Request(`${ORIGIN}/dashboard.png`, { method: "HEAD" }), env);
    expect(res.status).toBe(500);
    expect(await res.text()).toBe("");
    expect(res.headers.get("content-type")).toBe("text/plain; charset=utf-8");
    const [line] = logs.parse();
    expect(line.event).toBe("unhandled");
    expect(line.method).toBe("HEAD");
    expect(line.status).toBe(500);
  } finally {
    logs.restore();
  }
});

test("asset failures log their status; served requests log nothing", async () => {
  const logs = captureLogs();
  try {
    // The share card is stocked along with the capture: /health reads it, and
    // a store without it is a degraded one, which is its own line.
    const env = assetsEnv({ "/dashboard.webp": "webp-bytes", "/dashboard-card.png": "card" });
    const served = await call({}, { path: "/dashboard.webp", env });
    expect(served.status).toBe(200);
    expect((await call({ "cf-ray": "healthy-TOK" })).status).toBe(200);
    expect((await call({}, { path: "/health", env })).status).toBe(200);
    expect(logs.parse()).toEqual([]);

    const unbound = await call({}, { path: "/dashboard.avif", env: {} });
    expect(unbound.status).toBe(404);
    const missing = await call(
      {},
      {
        path: "/dashboard.avif",
        env: { ASSETS: { fetch: () => new Response("missing", { status: 404 }) } },
      },
    );
    expect(missing.status).toBe(404);
    const broken = await call(
      {},
      {
        path: "/dashboard.avif",
        env: { ASSETS: { fetch: () => new Response("boom", { status: 502 }) } },
      },
    );
    expect(broken.status).toBe(502);
    expect(logs.parse()).toEqual([
      {
        event: "assets-unbound",
        ray: "",
        method: "GET",
        status: 404,
        path: "/dashboard.avif",
        duration_ms: expect.any(Number),
      },
      {
        event: "asset-missing",
        ray: "",
        method: "GET",
        status: 404,
        path: "/dashboard.avif",
        duration_ms: expect.any(Number),
      },
      {
        event: "asset-store-error",
        ray: "",
        method: "GET",
        status: 502,
        path: "/dashboard.avif",
        duration_ms: expect.any(Number),
      },
    ]);
  } finally {
    logs.restore();
  }
});

test("a rejected method on either surface logs the request behind it", async () => {
  const logs = captureLogs();
  try {
    const env = assetsEnv({ "/dashboard.webp": "webp-bytes" });
    const page = await call({ "cf-ray": "9a1b2c3d4e5f-TOK" }, { method: "POST", env });
    expect(page.status).toBe(405);
    const image = await call({}, { path: "/dashboard.webp", method: "PUT", env });
    expect(image.status).toBe(405);
    expect(logs.parse()).toEqual([
      {
        event: "method-not-allowed",
        ray: "9a1b2c3d4e5f-TOK",
        method: "POST",
        path: "/",
        status: 405,
        duration_ms: expect.any(Number),
      },
      {
        event: "method-not-allowed",
        ray: "",
        method: "PUT",
        path: "/dashboard.webp",
        status: 405,
        duration_ms: expect.any(Number),
      },
    ]);
  } finally {
    logs.restore();
  }
});

// The worker's per-isolate line cap, restated here so the test reads as the
// contract rather than a magic number: REFUSAL_LOG_CAP lines carry the
// request, the next one announces the drop, and everything after that is
// silent.
const REFUSAL_LOG_CAP = 20;
const REFUSALS = 40;

test("a repeated refusal stops writing lines and says so once", async () => {
  // A fresh isolate so the per-isolate cap starts at zero: the counters live
  // in module state, and the tests above have already spent some of it.
  const { default: cappedWorker } = await import("./worker.js?refusal-cap");
  const env = staticAssets();
  const logs = captureLogs();
  try {
    for (let i = 0; i < REFUSALS + 2; i += 1) {
      const res = await cappedWorker.fetch(
        new Request(`${ORIGIN}/`, { method: "POST", headers: { "cf-ray": "cap-TOK" } }),
        env,
      );
      // The answer does not change: the cap is on the log line, not on the
      // refusal. A client repeating it still gets its 405 every time.
      expect(res.status).toBe(405);
    }
    const lines = logs.parse();
    expect(lines).toHaveLength(REFUSAL_LOG_CAP + 1);
    // The first REFUSAL_LOG_CAP lines carry the request, as before.
    expect(lines[0]).toEqual({
      event: "method-not-allowed",
      ray: "cap-TOK",
      method: "POST",
      path: "/",
      status: 405,
      duration_ms: expect.any(Number),
    });
    expect(lines[REFUSAL_LOG_CAP - 1].ray).toBe("cap-TOK");
    // The line past the cap says the rest are dropped and names no request,
    // so a reader cannot mistake it for a failure of its own.
    expect(lines[REFUSAL_LOG_CAP]).toEqual({
      event: "method-not-allowed",
      dropped_after: REFUSAL_LOG_CAP,
    });
  } finally {
    logs.restore();
  }
});

// The cap exists to keep a request flood from burying a line an operator
// reads, so it applies to what a client drives. A deploy that shipped without
// the captures is the opposite: the probe's 503 is the only line such a site
// writes, and it is the line that says why every image is a 404. Capped, it
// spends the whole budget in the first minutes of every isolate and the rest
// of a day-long outage reports nothing. The probe's own interval bounds the
// volume, so this event skips the cap and keeps saying so.
test("a degraded health probe keeps logging past the refusal cap", async () => {
  // A fresh isolate so the per-isolate counters start at zero.
  const { default: cappedWorker } = await import("./worker.js?health-cap");
  const logs = captureLogs();
  const checks = REFUSAL_LOG_CAP * 2;
  try {
    for (let i = 0; i < checks; i += 1) {
      const res = await cappedWorker.fetch(
        new Request(`${ORIGIN}/health`, { headers: { "cf-ray": "health-TOK" } }),
        {},
      );
      // The answer does not change: what is under test is the log line, not
      // the probe. A site in this state keeps answering 503 every time.
      expect(res.status).toBe(503);
    }
    const lines = logs.parse();
    expect(lines).toHaveLength(checks);
    for (const line of lines) {
      expect(line).toEqual({
        event: "health-degraded",
        ray: "health-TOK",
        method: "GET",
        path: "/health",
        status: 503,
        duration_ms: expect.any(Number),
        reason: "no asset binding",
      });
    }
  } finally {
    logs.restore();
  }
});
