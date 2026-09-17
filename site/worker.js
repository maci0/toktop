// Copyright (C) 2026 Marcel W. Wysocki
// SPDX-License-Identifier: MIT

// toktop.ai: one page, served from the edge. The whole site is this file, so
// there is no build step, no bucket, and nothing to keep in sync.

// Source comments stay in this file as the rationale for the CSS and markup.
// They are stripped once at isolate start so they never go over the wire.
function htmlForWire(source) {
  return source.replace(/<!--[\s\S]*?-->/g, "").replace(/\/\*[\s\S]*?\*\//g, "");
}

// 1280w covers 3x phones and 1x desktop; 1920w is the 2x desktop slot.
// The img omits decoding=async so the browser does not postpone the LCP decode.
const HERO_SIZES = "(max-width: 640px) calc(100vw - 1.7rem - 2px), calc(min(76rem, 100vw - 2.5rem) - 2px)";
const HERO_AVIF_SRCSET = "/dashboard-1280.avif 1280w, /dashboard.avif 1920w";
const HERO_WEBP_SRCSET = "/dashboard-1280.webp 1280w, /dashboard.webp 1920w";

const HTML = htmlForWire(`<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>toktop - btop for AI</title>
<meta name="description" content="A terminal dashboard for LLM inference engines and the coding agents hammering them.">
<meta property="og:title" content="toktop">
<meta property="og:description" content="btop for AI: a terminal dashboard for LLM inference engines and the coding agents hammering them.">
<meta property="og:type" content="website">
<meta property="og:url" content="https://toktop.ai">
<!-- the real dashboard, not a generated stand-in: share cards should show
     the product the page is about -->
<meta property="og:image" content="https://toktop.ai/dashboard.png">
<meta property="og:image:width" content="3240">
<meta property="og:image:height" content="1900">
<meta property="og:image:alt" content="toktop running in a terminal: engine rows with throughput and KV-cache pressure beside an agent feed">
<meta name="twitter:card" content="summary_large_image">
<meta name="twitter:image" content="https://toktop.ai/dashboard.png">
<!-- the icon is the h1 cursor block in the accent and panel colors, not a placeholder emoji -->
<link rel="icon" href="data:image/svg+xml,%3Csvg%20xmlns='http://www.w3.org/2000/svg'%20viewBox='0%200%20100%20100'%3E%3Crect%20width='100'%20height='100'%20fill='%2311161d'/%3E%3Crect%20x='37'%20y='25'%20width='26'%20height='50'%20fill='%234cc38a'/%3E%3C/svg%3E">
<style>
  :root {
    /* Both schemes are styled here; declaring them lets the browser match
       its own chrome to the active one. Without this the scrollable <pre>
       blocks grow light-styled scrollbars on the dark theme, invisible
       against --panel (WCAG 1.4.11). */
    color-scheme: dark light;
    --bg: #0d1117; --panel: #11161d; --line: #222b36;
    --fg: #d7dde5; --dim: #7d8895; --accent: #4cc38a; --warm: #e3b341;
    --mono: ui-monospace, "SF Mono", "JetBrains Mono", Menlo, Consolas, monospace;
  }
  @media (prefers-color-scheme: light) {
    :root {
      --bg: #fbfbf9; --panel: #f3f3ee; --line: #e3e3de;
      --fg: #1b1f24; --dim: #5c6570; --accent: #1a7f4b; --warm: #9a6b00;
    }
  }
  * { box-sizing: border-box; }
  body {
    margin: 0; padding: 3rem 1.25rem 5rem;
    background: var(--bg); color: var(--fg);
    font-family: var(--mono); font-size: 15px; line-height: 1.6;
  }
  main { max-width: 76rem; margin: 0 auto; }
  h1 { font-size: 2.6rem; margin: 0; }
  /* Blinking content that starts automatically must be pausable/stoppable
     (WCAG 2.2.2); honoring prefers-reduced-motion is the static-page remedy,
     so the cursor only blinks for users who have not asked for stillness. */
  @media (prefers-reduced-motion: no-preference) {
    h1 .cursor { color: var(--accent); animation: blink 1.2s step-end infinite; }
    @keyframes blink { 50% { opacity: 0; } }
  }
  .tag { color: var(--dim); margin: .6rem 0 2.4rem; font-size: 1.05rem; }
  /* Sentence-case titles on the tagline size, not uppercase micro-labels.
     Install/Run sit tight under the capture; the manifesto heading after
     the list keeps the larger gap. */
  h2 { font-size: 1.05rem; color: var(--fg); font-weight: 600; margin: 2.8rem 0 .7rem; }
  .shot + h2, h2 + pre + h2 { margin-top: 1.5rem; }
  pre {
    background: var(--panel); border: 1px solid var(--line);
    padding: 1rem 1.15rem; overflow-x: auto; margin: 0 0 1rem; font-size: 13.5px;
  }
  /* Narrow viewports clip code lines into a scroll container; a mouse-only
     scrollbar would lock keyboard users out (WCAG 2.1.1). */
  pre:focus-visible { outline: 2px solid var(--accent); outline-offset: 2px; }
  code { color: inherit; }
  .up { color: var(--accent); }
  .warm { color: var(--warm); }
  .dim { color: var(--dim); }
  /* The capture needs the 76rem column; copy does not. */
  p, ul { max-width: 62ch; }
  ul { padding-left: 1.1rem; margin: 0; }
  li { margin-bottom: .5rem; }
  li b { font-weight: 600; }
  /* Links must not be identified by color alone (WCAG 1.4.1): underline at
     rest, not just on hover. */
  a { color: var(--accent); text-decoration: underline; text-underline-offset: 3px;
      border-bottom: 1px solid transparent; }
  a:hover { border-bottom-color: currentColor; }
  footer { margin-top: 4rem; padding-top: 1.25rem; border-top: 1px solid var(--line);
           color: var(--dim); font-size: 13px; display: flex; gap: 1.5rem; flex-wrap: wrap; }
  /* A 13px/1.6 line box is ~21px tall, under the 24px target-size floor
     (WCAG 2.2 AA SC 2.5.8); vertical padding makes each footer item a real
     target instead of leaning on the spacing exception. */
  footer > * { padding: .3rem 0; }
  /* The screenshot is the product, not a decoration: a dark terminal
     frame so the capture never sits on the light-scheme paper. */
  .shot {
    margin: 0; border: 1px solid var(--line);
    background: #0d1117; overflow: hidden;
  }
  .shot figcaption {
    margin: 0; padding: .55rem 1rem; font-size: 13px;
    color: #7d8895; background: #11161d; border-bottom: 1px solid #222b36;
  }
  .shot img { display: block; width: 100%; height: auto; }
  @media (max-width: 640px) {
    body { padding: 2rem .85rem 4rem; }
    h1 { font-size: 2rem; }
  }
</style>
</head>
<body>
<main>
  <h1>toktop<span class="cursor" aria-hidden="true">_</span></h1>
  <p class="tag"><code>btop</code> for AI: a terminal dashboard for LLM inference
  engines and the coding agents hammering them.</p>

  <figure class="shot">
    <figcaption><span class="dim">$</span> toktop --demo</figcaption>
    <picture>
      <source type="image/avif" srcset="${HERO_AVIF_SRCSET}" sizes="${HERO_SIZES}">
      <source type="image/webp" srcset="${HERO_WEBP_SRCSET}" sizes="${HERO_SIZES}">
      <img src="/dashboard.png" width="1920" height="1126"
           alt="toktop dashboard: five local engines with throughput and KV-cache pressure, GPU vitals, two answered probes, and three coding agents"
           fetchpriority="high">
    </picture>
  </figure>

  <h2>Install</h2>
<pre tabindex="0"><code>go install -tags sqlite github.com/maci0/toktop/cmd/toktop@latest
<span class="dim"># or grab a binary: linux / macos / windows, amd64 + arm64</span></code></pre>

  <h2>Run</h2>
<pre tabindex="0"><code>toktop --demo             <span class="dim"># simulated fleet, works instantly</span>
toktop                    <span class="dim"># auto-discovers local engines</span>
toktop --agents           <span class="dim"># also watch coding agents on this machine</span>
toktop ssh://you@box      <span class="dim"># watch another host over ssh</span></code></pre>

  <h2>What it shows</h2>
  <ul>
    <li><b>Engines</b> found by port and process: Ollama, llama.cpp, vLLM,
      SGLang, TRT-LLM, LM Studio, MLX, KoboldCpp, LocalAI, TGI, LiteLLM,
      GPUStack, Lemonade, OmniRoute, plus a generic OpenAI-compatible
      fallback so nothing is missed.</li>
    <li><b>Agents</b> without any cooperation from them: claude, codex, qwen,
      copilot, opencode, pi, prime-agent, feynman, clanker and dsh all keep
      session logs carrying the provider's own token counts.</li>
    <li><b>Probes</b> that measure real time-to-first-token and decode speed,
      instead of guessing from averages.</li>
    <li><b>System</b> context: GPU/NPU enumeration, VRAM, temps, power, and
      KV-cache pressure next to the throughput that caused it.</li>
  </ul>

  <h2>Measured, or nothing</h2>
  <p class="dim">Every number here is one an engine or an agent actually
  reported. Nothing is estimated, interpolated, or inferred from character
  counts. An agent that reports nothing shows no rate, rather than a zero that
  reads like a measurement. An agent generating through an engine that is
  already being watched is counted once, not twice.</p>

  <footer>
    <a href="https://github.com/maci0/toktop">github.com/maci0/toktop</a>
    <span>MIT licensed</span>
    <span>no telemetry, no account, no daemon</span>
  </footer>
</main>
</body>
</html>
`);

// The page bytes change only at deploy time, so a derived ETag lets a browser
// that already holds a copy prove freshness with If-None-Match and be
// answered with a bodyless 304 instead of the whole page again on every
// reload and every visit past max-age. Computed once when the isolate starts.
//
// The validator is weak because the resource ships several encodings
// (identity plus whichever of brotli, zstd, gzip the isolate can produce)
// under one URL, and RFC 9110 forbids one strong ETag spanning multiple
// representations. If-None-Match compares weakly for GET revalidation either
// way, so nothing is lost: no ranges are offered on a page this small.
// Caches that pre-compress also match the stored coding by the identity tag:
// an accept-encoding transform that selects zstd (a newer, smaller coding)
// must not serve the older brotli or gzip copy as a cache hit while claiming
// its own ETag, so a zstd accept-encoding forces a full 200.
const ETAG_HASH = (() => {
  let hash = 0x811c9dc5;
  for (let i = 0; i < HTML.length; i++) {
    hash ^= HTML.charCodeAt(i);
    hash = Math.imul(hash, 0x01000193);
  }
  return (hash >>> 0).toString(16);
})();

const ETAG = `W/"${ETAG_HASH}"`;

// RFC 9110 weak comparison for If-None-Match: any list member counts, an
// optional W/ prefix is ignored, and * matches whatever is held. Comparing
// the stripped forms means validators sent back by older deploys (which used
// a strong tag over the same hash) still revalidate to a 304.
function ifNoneMatchMatches(headerValue) {
  const value = headerValue?.trim();
  if (!value) return false;
  if (value === "*") return true;
  return value.split(",").some((raw) => {
    let candidate = raw.trim();
    if (candidate.startsWith("W/")) candidate = candidate.slice(2);
    return candidate === `"${ETAG_HASH}"`;
  });
}

// Parse Accept-Encoding into coding -> q. A missing or blank header is an
// empty map, which quality() treats as identity-only: sending gzip to a
// client that never advertised it is how old HTTP/1.0 agents used to break.
function parseAcceptEncoding(headerValue) {
  const qByCoding = new Map();
  if (headerValue == null || !headerValue.trim()) return qByCoding;
  for (const part of headerValue.split(",")) {
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
    qByCoding.set(token, q);
  }
  return qByCoding;
}

function quality(qByCoding, coding) {
  if (coding === "identity") {
    if (qByCoding.has("identity")) return qByCoding.get("identity");
    return qByCoding.get("*") === 0 ? 0 : Number.EPSILON;
  }
  if (qByCoding.has(coding)) return qByCoding.get(coding);
  if (qByCoding.has("*")) return qByCoding.get("*");
  return 0;
}

// Content-Encoding token -> CompressionStream format. deflate is omitted on
// purpose: the zlib wrapper versus raw-deflate split is still a footgun, and
// every browser that speaks deflate also speaks gzip.
const COMPRESSIBLE = [
  ["br", "brotli"],
  ["zstd", "zstd"],
  ["gzip", "gzip"],
];

async function compressFormat(format) {
  return new Uint8Array(
    await new Response(
      new Response(HTML).body.pipeThrough(new CompressionStream(format)),
    ).arrayBuffer(),
  );
}

const IDENTITY = new TextEncoder().encode(HTML);

let representations;

async function pageRepresentations() {
  if (representations) return representations;
  const out = [{ coding: null, bytes: IDENTITY }];
  for (const [coding, format] of COMPRESSIBLE) {
    try {
      out.push({ coding, bytes: await compressFormat(format) });
    } catch {
      // Runtime lacks this format.
    }
  }
  representations = out;
  return out;
}

// Highest q the client offered, then the smallest body at that q. A Chrome
// `gzip, deflate, br, zstd` request therefore gets brotli rather than gzip,
// and a `br;q=0.1, gzip` request still gets gzip.
async function representationFor(acceptEncoding) {
  const reps = await pageRepresentations();
  const qByCoding = parseAcceptEncoding(acceptEncoding);
  let best = null;
  for (const rep of reps) {
    const q = quality(qByCoding, rep.coding ?? "identity");
    if (q <= 0) continue;
    if (
      best == null ||
      q > best.q ||
      (q === best.q && rep.bytes.byteLength < best.bytes.byteLength)
    ) {
      best = { ...rep, q };
    }
  }
  return best;
}

// Fresh for five minutes, then served from the browser's copy while a cheap
// 304 revalidation runs in the background: repeat visitors paint instantly
// and are never more than the first max-age behind a deploy.
const PAGE_CACHE_CONTROL = "public, max-age=300, stale-while-revalidate=86400";

// Several encodings live under one URL, so every cached copy must be keyed on
// what the accepting client asked for; without Vary a shared cache could hand
// a compressed body to a client that cannot decode it.
const VARY = "Accept-Encoding";

const SECURITY_HEADERS = {
  "x-content-type-options": "nosniff",
  "x-frame-options": "DENY",
  "strict-transport-security": "max-age=31536000",
  "referrer-policy": "strict-origin-when-cross-origin",
  "content-security-policy":
    "default-src 'none'; style-src 'unsafe-inline'; img-src 'self' data:; base-uri 'none'; form-action 'none'; frame-ancestors 'none'",
};

const ERROR_HEADERS = {
  "content-type": "text/plain; charset=utf-8",
  "cache-control": "no-store",
  ...SECURITY_HEADERS,
};

function errorResponse(status, body, extraHeaders = {}) {
  return new Response(body, {
    status,
    headers: { ...ERROR_HEADERS, ...extraHeaders },
  });
}

// Every page answer (200 any encoding, 304) carries these; the security
// headers ride along because a fresh load is exactly where they must apply.
const PAGE_HEADERS = {
  "content-type": "text/html; charset=utf-8",
  etag: ETAG,
  "cache-control": PAGE_CACHE_CONTROL,
  vary: VARY,
  ...SECURITY_HEADERS,
};

function srcsetPaths(srcset) {
  return srcset.split(",").map((part) => part.trim().split(/\s+/)[0]);
}

const IMAGE_PATHS = new Set([
  "/dashboard.png",
  ...srcsetPaths(HERO_AVIF_SRCSET),
  ...srcsetPaths(HERO_WEBP_SRCSET),
]);
const IMAGE_CACHE = "public, max-age=86400, stale-while-revalidate=604800";

export default {
  async fetch(request, env) {
    const url = new URL(request.url);
    if (IMAGE_PATHS.has(url.pathname)) {
      if (request.method !== "GET" && request.method !== "HEAD") {
        return errorResponse(405, "method not allowed", { allow: "GET, HEAD" });
      }
      if (!env?.ASSETS) {
        return errorResponse(404, "not found");
      }
      // Images are already compressed. Clone-with-headers keeps
      // Accept-Encoding (a forbidden header), so this is a new request
      // that only forwards revalidation fields.
      const assetHeaders = new Headers();
      const noneMatch = request.headers.get("if-none-match");
      if (noneMatch) assetHeaders.set("if-none-match", noneMatch);
      const modifiedSince = request.headers.get("if-modified-since");
      if (modifiedSince) assetHeaders.set("if-modified-since", modifiedSince);
      const asset = await env.ASSETS.fetch(
        new Request(request.url, {
          method: request.method,
          headers: assetHeaders,
        }),
      );
      const headers = new Headers(asset.headers);
      for (const [name, value] of Object.entries(SECURITY_HEADERS)) {
        headers.set(name, value);
      }
      // Success and revalidation can be stored; a missing or failed asset
      // must not inherit the day-long image policy or a 404 sticks.
      if (asset.status === 200 || asset.status === 304) {
        headers.set("cache-control", IMAGE_CACHE);
      } else {
        headers.set("cache-control", "no-store");
      }
      if (request.method === "HEAD") {
        return new Response(null, { status: asset.status, headers });
      }
      return new Response(asset.body, { status: asset.status, headers });
    }
    if (request.method !== "GET" && request.method !== "HEAD") {
      return errorResponse(405, "method not allowed", { allow: "GET, HEAD" });
    }
    if (url.pathname === "/health") {
      // Uptime probes hit this continuously; caching it would only blur
      // what the last probe actually saw. HEAD must carry the GET headers
      // and no body (RFC 9110).
      const healthBody = "ok\n";
      const healthHeaders = {
        "content-type": "text/plain; charset=utf-8",
        "cache-control": "no-store",
        "content-length": String(new TextEncoder().encode(healthBody).byteLength),
        ...SECURITY_HEADERS,
      };
      if (request.method === "HEAD") {
        return new Response(null, { headers: healthHeaders });
      }
      return new Response(healthBody, { headers: healthHeaders });
    }
    // One page: anything else is that page too, rather than a 404 nobody
    // learns anything from.
    const chosen = await representationFor(request.headers.get("accept-encoding"));
    if (chosen == null) {
      return errorResponse(406, request.method === "HEAD" ? null : "not acceptable", {
        vary: VARY,
      });
    }
    if (ifNoneMatchMatches(request.headers.get("if-none-match"))) {
      // Revalidation answers keep the validator and policy headers but no body.
      return new Response(null, {
        status: 304,
        headers: {
          etag: ETAG,
          "cache-control": PAGE_CACHE_CONTROL,
          vary: VARY,
          ...SECURITY_HEADERS,
        },
      });
    }
    const headers = {
      ...PAGE_HEADERS,
      "content-length": String(chosen.bytes.byteLength),
    };
    if (chosen.coding) headers["content-encoding"] = chosen.coding;
    if (request.method === "HEAD") {
      return new Response(null, { headers, encodeBody: "manual" });
    }
    return new Response(chosen.bytes, { headers, encodeBody: "manual" });
  },
};
