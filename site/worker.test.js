// Copyright (C) 2026 Marcel W. Wysocki
// SPDX-License-Identifier: MIT

// Run with `bun test site/` from the repository root (no other deps needed).

import { expect, test } from "bun:test";
import { readFileSync, statSync } from "node:fs";
import { join } from "node:path";

import worker from "./worker.js";

const ORIGIN = "https://toktop.ai";
const call = (headers = {}, init = {}) =>
  worker.fetch(
    new Request(ORIGIN + (init.path ?? "/"), {
      method: init.method ?? "GET",
      headers,
    }),
  );

const identityBody = await call().then((r) => r.text());

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
    const request = () => new Request(ORIGIN, {
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
    // instead of starting a second pipeline.
    const afterFirst = constructions;
    expect(afterFirst).toBe(3);
    const second = await freshWorker.fetch(request());
    expect(new Uint8Array(await second.arrayBuffer())).toEqual(bytes);
    expect(constructions).toBe(afterFirst);
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
          expect(format ? await decompress(bytes, format) : new TextDecoder().decode(bytes)).toBe(identityBody);
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
    ["gzip;q=0.5, br", "br"],
    ["br;q=0.1, gzip", "gzip"],
    ["br", "br"],
    ["zstd", "zstd"],
    ["gzip;q=0", null],
    ["deflate", null],
  ]) {
    const res = await call({ "accept-encoding": ae });
    expect(res.headers.get("content-encoding")).toBe(want);
    if (want == null) expect(await res.text()).toBe(identityBody);
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
    expect(bytes.byteLength).toBe(4081);
    expect(res.headers.get("content-encoding")).toBe("gzip");
    expect(await decompress(bytes, "gzip")).toBe(identityBody);
  }
  const preferred = await call({ "accept-encoding": "identity;q=1, gzip;q=0.5" });
  expect(preferred.headers.get("content-encoding")).toBeNull();
  expect(await preferred.text()).toBe(identityBody);
});

test("unacceptable encodings return an uncacheable 406, including conditional requests", async () => {
  const etag = (await call()).headers.get("etag");
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
      }
    }
  }
  const accepted = await call({ "accept-encoding": "*;q=0, gzip;q=0.5" });
  expect(accepted.status).toBe(200);
  expect(accepted.headers.get("content-encoding")).toBe("gzip");
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
  const denied = await call({}, { method: "POST" });
  expect(denied.status).toBe(405);
  expect(denied.headers.get("allow")).toBe("GET, HEAD");
  expect(denied.headers.get("content-type")).toBe("text/plain; charset=utf-8");
  expect(denied.headers.get("cache-control")).toBe("no-store");
  const health = await call({}, { path: "/health" });
  expect(health.status).toBe(200);
  expect(health.headers.get("cache-control")).toBe("no-store");
  const healthHead = await call({}, { method: "HEAD", path: "/health" });
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
});

const IMAGE_CACHE = "public, max-age=86400, stale-while-revalidate=604800";

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
  const head = await worker.fetch(
    new Request(`${ORIGIN}/dashboard.png`, { method: "HEAD" }),
    env,
  );
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
  const res = await worker.fetch(
    new Request(`${ORIGIN}/dashboard.png`, { method: "POST" }),
    env,
  );
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
  expect(identityBody.includes("https://toktop.ai/dashboard.png")).toBe(true);
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
      .match(/\d+/g)
      .map((n) => Number(n).toString(16).padStart(2, "0"))
      .join("")}`;
  const named = (name) => {
    const m = src.match(new RegExp(`^${name}: RGB = (\\(\\d+, \\d+, \\d+\\))`, "m"));
    if (m == null) throw new Error(`${name} not found in scripts/screenshot.py`);
    return toHex(m[1]);
  };
  const block = src.match(/^ANSI16: dict\[int, RGB\] = \{([\s\S]*?)^\}/m);
  if (block == null) throw new Error("ANSI16 not found in scripts/screenshot.py");
  const ansi = {};
  for (const [, index, tuple] of block[1].matchAll(/^\s*(\d+): (\(\d+, \d+, \d+\))/gm)) {
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
    if (sgr != null) {
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
      (max === r
        ? ((g - b) / delta) % 6
        : max === g
          ? (b - r) / delta + 2
          : (r - g) / delta + 4);
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
});

// RFC 6928 initcwnd: ten ~1460-byte segments (~14 KB). Identity bytes plus
// inline CSS are everything there is, so staying under this keeps first paint
// at one round trip. Exact sizes are the record: a copy or compression
// change that grows the payload fails here instead of hiding under the
// window ceiling.
test("recorded transfer sizes stay inside the initial congestion window", async () => {
  const budget = 10 * 1460;
  const identity = new Uint8Array(await (await call()).arrayBuffer()).byteLength;
  const gzipped = new Uint8Array(
    await (await call({ "accept-encoding": "gzip" })).arrayBuffer(),
  ).byteLength;
  const brotli = new Uint8Array(
    await (await call({ "accept-encoding": "br" })).arrayBuffer(),
  ).byteLength;
  expect(identity).toBe(11741);
  expect(gzipped).toBe(4081);
  expect(brotli).toBe(3414);
  expect(identity).toBeLessThan(budget);
  expect(gzipped).toBeLessThan(budget);
  expect(brotli).toBeLessThan(budget);
});

const PUBLIC = join(import.meta.dir, "public");
const assetBytes = (name) => statSync(join(PUBLIC, name)).size;

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
  expect(assetBytes("dashboard.avif")).toBeLessThan(80_000);
  expect(assetBytes("dashboard-1280.avif")).toBeLessThan(50_000);
  expect(assetBytes("dashboard-768.avif")).toBeLessThan(25_000);
  expect(assetBytes("dashboard-1280.webp")).toBeLessThan(100_000);
  expect(assetBytes("dashboard-768.webp")).toBeLessThan(45_000);
  expect(assetBytes("dashboard.webp")).toBeLessThan(160_000);
});

// The slot a phone actually takes: a 2x screen at the 360 CSS px the figure
// occupies needs 722 device pixels, so the srcset hands it the 768w candidate.
// Without that entry the browser rounds up to 1280w and downloads 39,708 bytes
// to fill 722 of them. A 768w capture covers 36% of the 1280w area and lands
// at 51% of its weight; the ceiling is set past that, so a re-capture that
// drops or fattened the phone candidate fails here instead of quietly
// doubling the weight of the visit that matters most.
test("the phone slot is served by the 768w capture, not the 1280w one", () => {
  expect(assetBytes("dashboard-768.avif")).toBeLessThan(
    assetBytes("dashboard-1280.avif") * 0.6,
  );
  expect(assetBytes("dashboard-768.webp")).toBeLessThan(
    assetBytes("dashboard-1280.webp") * 0.6,
  );
});

// The <img src> fallback and the og:image both point at the PNG original, so
// it is the one download on the page that no srcset narrows: a client with
// neither AVIF nor WebP pays all of it. Nothing else measures that path, so a
// re-capture at a higher scale would double the worst-case hero weight with CI
// green. The ceiling is above today's 303,865 and well under the ~14 KB the
// rest of the page fits in, so it records the gap instead of hiding it.
test("the PNG fallback stays bounded", () => {
  expect(assetBytes("dashboard.png")).toBeLessThan(320_000);
});

function assetsEnv(bodies) {
  return {
    ASSETS: {
      fetch(request) {
        const url = new URL(request.url);
        const body = bodies[url.pathname];
        if (body == null) {
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

const imageCall = (path, headers = {}, init = {}) =>
  worker.fetch(
    new Request(ORIGIN + path, {
      method: init.method ?? "GET",
      headers,
    }),
    init.env,
  );

test("image paths 404 without ASSETS instead of falling through to the page", async () => {
  const logs = captureLogs();
  try {
    const res = await imageCall("/dashboard.avif");
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
    const res = await imageCall("/dashboard.png", {}, { env });
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
      const res = await imageCall("/dashboard.png", {}, { env });
      expect(res.status).toBe(status);
      expect(await res.text()).toBe(body);
      expect(res.headers.get("content-type")).toBe("text/plain; charset=utf-8");
      expect(res.headers.get("cache-control")).toBe("no-store");
      for (const name of SECURITY_HEADER_NAMES) {
        expect(res.headers.get(name)).not.toBeNull();
      }
    }
    const head = await imageCall("/dashboard.png", {}, { env: missing, method: "HEAD" });
    expect(head.status).toBe(404);
    expect((await head.arrayBuffer()).byteLength).toBe(0);
  } finally {
    logs.restore();
  }
});

test("image paths are served from ASSETS with cache and security headers", async () => {
  const env = assetsEnv({ "/dashboard.avif": "avif-bytes" });
  const res = await imageCall("/dashboard.avif", { "accept-encoding": "gzip, br" }, { env });
  expect(res.status).toBe(200);
  expect(await res.text()).toBe("avif-bytes");
  expect(res.headers.get("cache-control")).toBe(
    "public, max-age=86400, stale-while-revalidate=604800",
  );
  expect(res.headers.get("etag")).toBe('"/dashboard.avif"');
  for (const name of SECURITY_HEADER_NAMES) {
    expect(res.headers.get(name)).not.toBeNull();
  }
});

test("every dashboard URL in the HTML is served as an image asset", async () => {
  const paths = [
    ...new Set(
      [...identityBody.matchAll(/\/dashboard[-.\w]+/g)].map((m) => m[0]),
    ),
  ];
  expect(paths.length).toBeGreaterThanOrEqual(5);
  const env = assetsEnv(Object.fromEntries(paths.map((p) => [p, p])));
  for (const path of paths) {
    const res = await imageCall(path, {}, { env });
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
          return Promise.resolve(
            new Response(null, { status: 304, headers: { etag: '"abc"' } }),
          );
        }
        return Promise.resolve(
          new Response("full", { status: 200, headers: { etag: '"abc"' } }),
        );
      },
    },
  };
  const res = await imageCall(
    "/dashboard.png",
    { "if-none-match": '"abc"', "accept-encoding": "gzip" },
    { env },
  );
  expect(res.status).toBe(304);
  expect(res.headers.get("etag")).toBe('"abc"');
  expect(res.headers.get("cache-control")).toBe(IMAGE_CACHE);
  expect(await res.text()).toBe("");
});

test("image HEAD matches GET headers with no body, POST is 405", async () => {
  const env = assetsEnv({ "/dashboard.webp": "webp-bytes" });
  const get = await imageCall("/dashboard.webp", {}, { env });
  const head = await imageCall("/dashboard.webp", {}, { method: "HEAD", env });
  expect(head.status).toBe(200);
  expect(head.headers.get("cache-control")).toBe(get.headers.get("cache-control"));
  expect((await head.arrayBuffer()).byteLength).toBe(0);
  const posted = await imageCall("/dashboard.webp", {}, { method: "POST", env });
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
    expect(await res.text()).toBe("internal error");
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
    expect(typeof line.duration_ms).toBe("number");
  } finally {
    logs.restore();
  }
});

test("asset failures log their status; served requests log nothing", async () => {
  const logs = captureLogs();
  try {
    const env = assetsEnv({ "/dashboard.webp": "webp-bytes" });
    const served = await imageCall("/dashboard.webp", {}, { env });
    expect(served.status).toBe(200);
    expect((await call({ "cf-ray": "healthy-TOK" })).status).toBe(200);
    expect((await call({}, { path: "/health" })).status).toBe(200);
    expect(logs.parse()).toEqual([]);

    const unbound = await imageCall("/dashboard.avif", {}, { env: {} });
    expect(unbound.status).toBe(404);
    const missing = await imageCall(
      "/dashboard.avif",
      {},
      { env: { ASSETS: { fetch: () => new Response("missing", { status: 404 }) } } },
    );
    expect(missing.status).toBe(404);
    const broken = await imageCall(
      "/dashboard.avif",
      {},
      { env: { ASSETS: { fetch: () => new Response("boom", { status: 502 }) } } },
    );
    expect(broken.status).toBe(502);
    expect(logs.parse()).toEqual([
      { event: "assets-unbound", ray: "", path: "/dashboard.avif" },
      {
        event: "asset-missing",
        ray: "",
        path: "/dashboard.avif",
        status: 404,
      },
      {
        event: "asset-store-error",
        ray: "",
        path: "/dashboard.avif",
        status: 502,
      },
    ]);
  } finally {
    logs.restore();
  }
});
