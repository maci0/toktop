// Copyright (C) 2026 Marcel W. Wysocki
// SPDX-License-Identifier: MIT

// toktop.ai: one page, served from the edge. The whole site is this file, so
// there is no build step, no bucket, and nothing to keep in sync.

// Source comments stay in this file as the rationale for the CSS and markup.
// They are stripped once at isolate start so they never go over the wire.
function htmlForWire(source) {
  return source.replace(/<!--[\s\S]*?-->/g, "").replace(/\/\*[\s\S]*?\*\//g, "");
}

// A phone lays the figure out at about 360 CSS px, so a 2x screen asks for
// roughly 720 device pixels: 768w is that slot, and 1280w is what a 3x phone
// and a 1x desktop need. Without the 768w candidate every phone fetched the
// 1280w capture, 39,708 bytes for 722 pixels of it. 1920w is the 2x desktop
// slot.
// The img omits decoding=async so the browser does not postpone the LCP decode.
const HERO_SIZES =
  "(max-width: 640px) calc(100vw - 1.7rem - 2px), calc(min(76rem, 100vw - 2.5rem) - 2px)";
const HERO_AVIF_SRCSET =
  "/dashboard-768.avif 768w, /dashboard-1280.avif 1280w, /dashboard.avif 1920w";
const HERO_WEBP_SRCSET =
  "/dashboard-768.webp 768w, /dashboard-1280.webp 1280w, /dashboard.webp 1920w";

// The palette, named once. toktop is a terminal: the page is a picture of one,
// and the same hexes are the TUI's (internal/ui/theme.go) and the capture
// renderer's (scripts/screenshot.py). Spelling a hex a second time here is a
// hex that can drift from the product it depicts, so the CSS, the light scheme
// and the favicon all read these names. site/worker.test.js pins the agreement
// across the three files.
const DARK = {
  bg: "#0d1117",
  panel: "#11161d",
  line: "#222b36",
  fg: "#d7dde5",
  dim: "#7d8895",
  accent: "#4cc38a",
  warm: "#e3b341",
};
const LIGHT = {
  bg: "#fbfbf9",
  panel: "#f3f3ee",
  line: "#e3e3de",
  fg: "#1b1f24",
  dim: "#5c6570",
  accent: "#1a7f4b",
  warm: "#8c5f00",
};

// The h1 cursor block, in the panel and accent colors: the icon is the mark the
// page already ends on, not a placeholder glyph.
const FAVICON = `data:image/svg+xml,${encodeURIComponent(
  `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 100 100">` +
    `<rect width="100" height="100" fill="${DARK.panel}"/>` +
    `<rect x="37" y="25" width="26" height="50" fill="${DARK.accent}"/>` +
    `</svg>`,
)}`;

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
<link rel="icon" href="${FAVICON}">
<style>
  :root {
    /* Both schemes are styled here; declaring them lets the browser match
       its own chrome to the active one. Without this the scrollable <pre>
       blocks grow light-styled scrollbars on the dark theme, invisible
       against --panel (WCAG 1.4.11). */
    color-scheme: dark light;
    /* The dark scheme is named separately because the capture frame below
       wants it in both schemes; the page tokens just point at it. */
    --dark-bg: ${DARK.bg}; --dark-panel: ${DARK.panel}; --dark-line: ${DARK.line};
    --dark-fg: ${DARK.fg}; --dark-dim: ${DARK.dim};
    --dark-accent: ${DARK.accent}; --dark-warm: ${DARK.warm};
    --bg: var(--dark-bg); --panel: var(--dark-panel); --line: var(--dark-line);
    --fg: var(--dark-fg); --dim: var(--dark-dim);
    --accent: var(--dark-accent); --warm: var(--dark-warm);
    --mono: ui-monospace, "SF Mono", "JetBrains Mono", Menlo, Consolas, monospace;
  }
  @media (prefers-color-scheme: light) {
    :root {
      --bg: ${LIGHT.bg}; --panel: ${LIGHT.panel}; --line: ${LIGHT.line};
      --fg: ${LIGHT.fg}; --dim: ${LIGHT.dim};
      --accent: ${LIGHT.accent}; --warm: ${LIGHT.warm};
    }
  }
  * { box-sizing: border-box; }
  @media (prefers-reduced-motion: no-preference) {
    html { scroll-behavior: smooth; }
  }
  body {
    margin: 0; padding: 0 1.25rem 5rem;
    background: var(--bg); color: var(--fg);
    font-family: var(--mono); font-size: 15px; line-height: 1.6;
  }
  .skip-link {
    position: absolute; top: -100px; left: 1.25rem; z-index: 100;
    padding: .5rem 1rem; background: var(--panel); color: var(--fg);
    border: 1px solid var(--accent); text-decoration: none; font-size: 13.5px;
  }
  .skip-link:focus, .skip-link:focus-visible {
    top: .7rem; outline: 2px solid var(--accent); outline-offset: 2px;
  }
  main { max-width: 76rem; margin: 0 auto; }
  main:focus { outline: none; }
  /* Anchor bar: brand + section jumps, sticky. */
  .bar { position: sticky; top: 0; z-index: 10; display: flex; gap: 1.25rem;
    align-items: center; padding: .7rem 0; margin: 0 -1.25rem; padding-inline: 1.25rem;
    background: var(--bg); border-bottom: 1px solid var(--line); }
  .brand { font-weight: 700; font-size: 1.1rem; text-decoration: none; color: var(--fg);
    border-bottom: 0; white-space: nowrap; }
  .brand .cursor { color: var(--accent); }
  nav { display: flex; gap: 1.1rem; font-size: 13.5px; margin-left: auto; }
  nav a { color: var(--dim); white-space: nowrap; padding: .3rem 0; }
  .hero { padding-top: 2.6rem; }
  h1 { font-size: 2.6rem; margin: 0; }
  /* Blinking content that starts automatically must be pausable/stoppable
     (WCAG 2.2.2); honoring prefers-reduced-motion is the static-page remedy,
     so the cursor only blinks for users who have not asked for stillness. */
  @media (prefers-reduced-motion: no-preference) {
    h1 .cursor { color: var(--accent); animation: blink 1.2s step-end infinite; }
    @keyframes blink { 50% { opacity: 0; } }
  }
  .tag { color: var(--dim); margin: .6rem 0 2rem; font-size: 1.05rem; max-width: 62ch; }
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
  :focus-visible { outline: 2px solid var(--accent); outline-offset: 2px; }
  code { color: inherit; }
  .dim { color: var(--dim); }
  /* The capture needs the 76rem column; copy does not. */
  p, ul { max-width: 62ch; }
  ul { padding-left: 1.1rem; margin: 0; }
  li { margin-bottom: .5rem; }
  li b { font-weight: 600; }
  /* Feature grid: four panes, each a name, a job, a specimen. */
  .grid { display: grid; grid-template-columns: repeat(2, 1fr); gap: .75rem;
    max-width: none; margin: 0 0 1rem; padding: 0; list-style: none; }
  .grid li { margin: 0; background: var(--panel); border: 1px solid var(--line);
    border-top: 2px solid var(--accent); padding: .9rem 1rem; }
  .grid li:nth-child(2n) { border-top-color: var(--warm); }
  .grid b { display: block; font-size: .95rem; margin-bottom: .3rem; }
  .grid p { margin: 0 0 .5rem; font-size: 13.5px; color: var(--dim); max-width: none; }
  .grid code { display: block; font-size: 12.5px; white-space: normal; }
  /* Key table: chips left, action right. */
  .keys { display: grid; grid-template-columns: auto 1fr; gap: .3rem .9rem;
    max-width: 62ch; margin: 0 0 1rem; font-size: 13.5px; }
  .keys dt, .keys dd { margin: 0; }
  .keys dd { color: var(--dim); }
  kbd { border: 1px solid var(--line); border-radius: 4px;
    padding: 0 .4rem; font-family: inherit; font-size: 12.5px; background: var(--bg); }
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
     frame so the capture never sits on the light-scheme paper. The frame
     re-points the page tokens at the dark scheme, so it is a terminal in
     both schemes without repeating a hex, and the light page can never
     recolor the product. */
  .shot {
    --bg: var(--dark-bg); --panel: var(--dark-panel);
    --line: var(--dark-line); --dim: var(--dark-dim);
    margin: 0; border: 1px solid var(--line);
    background: var(--bg); overflow: hidden;
  }
  .shot figcaption {
    margin: 0; padding: .55rem 1rem; font-size: 13px;
    color: var(--dim); background: var(--panel); border-bottom: 1px solid var(--line);
  }
  .shot img { display: block; width: 100%; height: auto; }
  section { scroll-margin-top: 4rem; }
  @media (max-width: 640px) {
    body { padding: 0 .85rem 4rem; }
    .bar { margin: 0 -.85rem; padding-inline: .85rem; gap: .8rem; }
    nav { gap: .8rem; font-size: 12.5px; overflow-x: auto; }
    h1 { font-size: 2rem; }
    .hero { padding-top: 2rem; }
    .grid { grid-template-columns: 1fr; }
  }
</style>
</head>
<body>
<a class="skip-link" href="#top">Skip to content</a>
<header class="bar">
  <a class="brand" href="#top">toktop<span class="cursor" aria-hidden="true">_</span></a>
  <nav aria-label="Sections">
    <a href="#install">Install</a>
    <a href="#run">Run</a>
    <a href="#shows" aria-label="What it shows">Shows</a>
    <a href="#keys">Keys</a>
    <a href="#feed">Feed</a>
  </nav>
</header>
<main id="top" tabindex="-1">
  <div class="hero">
  <h1>toktop<span class="cursor" aria-hidden="true">_</span></h1>
  <p class="tag"><code>btop</code> for AI: a terminal dashboard for LLM inference
  engines and the coding agents hammering them. One static binary, no daemon,
  no telemetry, no account.</p>
  </div>

  <figure class="shot">
    <figcaption><span class="dim">$</span> toktop --demo</figcaption>
    <picture>
      <source type="image/avif" srcset="${HERO_AVIF_SRCSET}" sizes="${HERO_SIZES}">
      <source type="image/webp" srcset="${HERO_WEBP_SRCSET}" sizes="${HERO_SIZES}">
      <img src="/dashboard.png" width="1920" height="1126"
           alt="toktop dashboard: five local inference engines with throughput, context length and KV-cache pressure, probe time-to-first-token beside them, a GPU and host strip, and an agent feed reporting two coding agents"
           fetchpriority="high">
    </picture>
  </figure>

  <section id="install" aria-labelledby="install-heading">
  <h2 id="install-heading">Install</h2>
<pre tabindex="0" role="region" aria-label="Install commands"><code>go install -tags sqlite github.com/maci0/toktop/cmd/toktop@latest
<span class="dim"># or a binary: linux / macos / windows, amd64 + arm64</span></code></pre>
  <p class="dim">The <code>sqlite</code> tag matches the release binaries: without it
  crush and opencode stores are unreadable.
  <a href="https://github.com/maci0/toktop/releases">Releases</a> ·
  <code>toktop update</code> self-updates.</p>
  </section>

  <section id="run" aria-labelledby="run-heading">
  <h2 id="run-heading">Run</h2>
<pre tabindex="0" role="region" aria-label="Run commands"><code>toktop --demo             <span class="dim"># simulated fleet, works instantly</span>
toktop                    <span class="dim"># auto-discovers local engines</span>
toktop --agents           <span class="dim"># also watch coding agents on this machine</span>
toktop ssh://you@box      <span class="dim"># watch another host over ssh</span></code></pre>
  </section>

  <section id="shows" aria-labelledby="shows-heading">
  <h2 id="shows-heading">What it shows</h2>
  <ul class="grid">
    <li><b>Engines</b><p>Found by port and process, fingerprinted by HTTP.</p><code>Ollama · vLLM · llama.cpp · SGLang · LM Studio · MLX · +9</code></li>
    <li><b>Agents</b><p>Read from their own session logs. No cooperation needed.</p><code>claude · codex · qwen · copilot · dsh · +2 stores</code></li>
    <li><b>Probes</b><p>Real generations measuring TTFT and decode speed.</p><code>press p · or --probe N to auto-probe</code></li>
    <li><b>System</b><p>GPU/NPU, VRAM, temps, power beside the throughput.</p><code>nv0 82° 69% · vram 57G/80G · 397W</code></li>
  </ul>
  <p class="dim">Agents read from transcripts: claude, codex,
  qwen, copilot, pi, prime-agent, feynman, clanker and dsh keep JSONL;
  opencode and crush keep SQLite (needs the <code>sqlite</code> tag).
  Agents on a watched engine show <code>via &lt;engine&gt;</code>, counted once.</p>
  </section>

  <section id="keys" aria-labelledby="keys-heading">
  <h2 id="keys-heading">Keys</h2>
  <dl class="keys">
    <dt><kbd>space</kbd></dt><dd>pause / resume</dd>
    <dt><kbd>p</kbd></dt><dd>probe every engine</dd>
    <dt><kbd>t</kbd></dt><dd>toggle compressed timescale</dd>
    <dt><kbd>a</kbd></dt><dd>focus engines or agents</dd>
    <dt><kbd>?</kbd></dt><dd>key help, in the app</dd>
    <dt><kbd>esc</kbd></dt><dd>back to engines, or quit</dd>
    <dt><kbd>q</kbd></dt><dd>quit</dd>
  </dl>
  <p class="dim">The footer carries the same list, and drops the keys that
  have nothing to act on: no engines yet means no probe and nothing to
  swap to.</p>
  </section>

  <section id="feed" aria-labelledby="feed-heading">
  <h2 id="feed-heading">Agent feed</h2>
<pre tabindex="0" role="region" aria-label="Agent feed event payload"><code>curl -X POST localhost:8420/v1/events -d \
  '{"agent":"coder","output_tokens":310,"prompt_tokens":4200}'</code></pre>
  <p class="dim">Any harness can POST usage to the ingest endpoint
  (<code>127.0.0.1:8420</code>, <code>--no-ingest</code> disables it).
  <code>--once --plain</code> prints a linear report for screen readers:
  no braille, no borders, no columns.</p>
  </section>

  <section id="measured" aria-labelledby="measured-heading">
  <h2 id="measured-heading">Measured, or nothing</h2>
  <p class="dim">Every number is one an engine or an agent actually
  reported. Nothing estimated or inferred. An agent that reports nothing
  shows no rate, not a zero. An agent on a watched engine is counted once.</p>
  </section>

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
    // biome-ignore lint/suspicious/noBitwiseOperators: FNV-1a is defined by xor, not arithmetic.
    hash ^= HTML.charCodeAt(i);
    hash = Math.imul(hash, 0x01000193);
  }
  // biome-ignore lint/suspicious/noBitwiseOperators: the unsigned shift is how FNV-1a normalizes to 32 bits.
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
  if (headerValue === null || !headerValue.trim()) return qByCoding;
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

// The in-flight build, or the finished one. A promise, not the resolved
// value: a cold isolate interleaves concurrent requests at every await, so a
// value-only cache is a check-then-act that lets a whole burst of them run
// the compression pipeline together. Assigning the promise synchronously
// means the second caller awaits the first one's work instead of repeating it.
let representations;

function pageRepresentations(request) {
  representations ??= buildRepresentations(request).catch((err) => {
    // Only completed bytes are ever cached, so a failed build has to clear
    // the slot: a rejection left in place would answer every later request
    // with the same failure until the isolate was recycled.
    representations = undefined;
    throw err;
  });
  return representations;
}

// The request rides along only so a dropped coding names the edge request
// that hit it: the build is shared, so the first caller's ray is the one on
// the line, not a claim about every request it served.
//
// The three run concurrently. A cold isolate pays this build inside the first
// request, before the page it is answering, and awaiting them in sequence
// makes that request wait for the sum of the three rather than the slowest
// one. The bytes are the same either way and the CPU is the same either way;
// only the time to the first byte of that request changes.
async function buildRepresentations(request) {
  const compressed = await Promise.all(
    COMPRESSIBLE.map(async ([coding, format]) => {
      try {
        return { coding, bytes: await compressFormat(format) };
      } catch (err) {
        // A runtime without the format is the ordinary case. Anything else
        // (out of memory, a stream that dies mid-pipeline) would ship the
        // page at its uncompressed size forever without a word, so name it.
        logFailure(request, "coding-dropped", {
          coding,
          // A throw carries any value, null included, so err may have no message.
          error: String(err?.message ?? err),
        });
        return null;
      }
    }),
  );
  return [{ coding: null, bytes: IDENTITY }, ...compressed.filter((rep) => rep !== null)];
}

// Highest q the client offered, then the smallest body at that q. A Chrome
// `gzip, deflate, br, zstd` request therefore gets brotli rather than gzip,
// and a `br;q=0.1, gzip` request still gets gzip.
async function representationFor(acceptEncoding, request) {
  const reps = await pageRepresentations(request);
  const qByCoding = parseAcceptEncoding(acceptEncoding);
  let best = null;
  for (const rep of reps) {
    const q = quality(qByCoding, rep.coding ?? "identity");
    if (q <= 0) continue;
    if (
      best === null ||
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
// biome-ignore lint/security/noSecrets: a Cache-Control directive list, not a credential.
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

// Every error answer carries the same Server-Timing the page and image
// answers do: a failed request is the one a visitor reports, and its time at
// the edge is part of that report. Without it the timing series only describes
// the requests that worked.
function errorResponse(started, status, body, extraHeaders = {}) {
  return new Response(body, {
    status,
    headers: {
      ...ERROR_HEADERS,
      "server-timing": serverTiming(started),
      ...extraHeaders,
    },
  });
}

// One failure, one line and one answer. The line names the status the client
// got and the milliseconds the edge spent, so one pivot off a visitor's
// report says whether it succeeded and how slowly; the answer carries the
// same numbers as headers.
function failRequest(request, started, status, event, body, fields, extraHeaders) {
  logFailure(request, event, {
    status,
    duration_ms: Date.now() - started,
    ...fields,
  });
  return errorResponse(started, status, body, extraHeaders);
}

// What the edge spent on the answer, in Server-Timing (RFC 8941), so the
// number a visitor or a RUM script reads is the time to first byte from this
// Worker rather than an unbreakable share of a round trip. The page is the
// only surface that has no client-side timing to fall back on, and a
// regression here is invisible in a byte-count test: it is a change in how
// long the edge takes, not in how much it sends.
function serverTiming(started) {
  return `edge;dur=${Math.max(0, Date.now() - started)}`;
}

// One JSON object per line, so Workers Logs can filter on a field rather than
// parse prose, and the edge's cf-ray rides along so a failure a visitor
// reports pivots from the line to that edge request. Only failures log: a
// served page, its 304s and its images are the steady state, and a line per
// visit would bury the few that name a broken deploy.
function logFailure(request, event, fields) {
  // biome-ignore lint/suspicious/noConsole: Workers Logs is the only place a failure reaches an operator.
  console.error(
    JSON.stringify({
      event,
      ray: request.headers.get("cf-ray") ?? "",
      ...fields,
    }),
  );
}

// The reason line for a failure the asset store reported. The store's own
// body is not repeated: it is an HTML page that names no image path.
function assetErrorBody(status) {
  if (status === 404 || status === 410) return "not found\n";
  return "asset store error\n";
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
// The captures are served under stable names, not content-hashed ones, so
// nothing but a revalidation can retire the copy a browser is holding when a
// deploy re-captures. max-age covers the repeat visit, which is nearly all of
// them; stale-while-revalidate is what runs after it, and a day of that would
// put a week-old screenshot of the dashboard on the page. An hour bounds how
// long a re-capture takes to reach a returning browser, at the cost of one
// cheap conditional request on a visit that is already past max-age.
const IMAGE_CACHE = "public, max-age=86400, stale-while-revalidate=3600";

export default {
  // Every throw below would otherwise reach the client as the edge's opaque
  // 1101 page with nothing in Workers Logs to explain it. Name the request,
  // the reason and how long it took, then answer with the same plain-text
  // envelope every other failure here uses, so a client can still act on it.
  async fetch(request, env) {
    const started = Date.now();
    try {
      return await handle(request, env, started);
    } catch (err) {
      // HEAD carries the GET headers and no body (RFC 9110), on the failure
      // path as on the served one: a HEAD that throws is as reachable as a
      // GET that does, and a body under it is the one answer the runtime
      // will not strip for us.
      return failRequest(
        request,
        started,
        500,
        "unhandled",
        request.method === "HEAD" ? null : "internal error",
        {
          method: request.method,
          path: new URL(request.url).pathname,
          error: String(err?.message ?? err),
        },
      );
    }
  },
};

async function handle(request, env, started) {
  const url = new URL(request.url);
  if (IMAGE_PATHS.has(url.pathname)) {
    if (request.method !== "GET" && request.method !== "HEAD") {
      return errorResponse(started, 405, "method not allowed", { allow: "GET, HEAD" });
    }
    if (!env?.ASSETS) {
      // Every image on the page is now a 404 and /health reports the missing
      // binding, so this is the line that names the request behind it.
      return failRequest(request, started, 404, "assets-unbound", "not found", {
        path: url.pathname,
      });
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
    // An asset-store failure is the one answer whose body the Worker does
    // not write: the store ships an HTML error page under its own headers.
    // Every other status here is text/plain like /health, so a missing
    // capture stays a one-line reason a client can act on instead of a
    // document at an image path. The status passes through unchanged.
    if (asset.status >= 400) {
      // The client sees "not found" and cannot tell a capture that was never
      // uploaded from a store that is failing. Both are deploy-level, so both
      // get a line: the status names which one it was.
      return failRequest(
        request,
        started,
        asset.status,
        asset.status === 404 || asset.status === 410 ? "asset-missing" : "asset-store-error",
        request.method === "HEAD" ? null : assetErrorBody(asset.status),
        { path: url.pathname },
      );
    }
    const headers = new Headers(asset.headers);
    for (const [name, value] of Object.entries(SECURITY_HEADERS)) {
      headers.set(name, value);
    }
    headers.set("server-timing", serverTiming(started));
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
    return errorResponse(started, 405, "method not allowed", { allow: "GET, HEAD" });
  }
  if (url.pathname === "/health") {
    // Uptime probes hit this continuously; caching it would only blur
    // what the last probe actually saw. HEAD must carry the GET headers
    // and no body (RFC 9110).
    //
    // The probe reports degraded while the asset binding is missing rather
    // than ok: the page still serves, but every capture it shows is a 404,
    // so a probe that keeps saying ok describes a site nobody can use. That
    // is the same call the ingest /healthz makes when it is refusing every
    // event, and it is what makes `make site-deploy` fail a deploy that
    // shipped without its assets instead of waiting out a green probe.
    const degraded = !env?.ASSETS;
    const healthBody = degraded
      ? "degraded: no asset binding; the dashboard captures are not served\n"
      : "ok\n";
    const healthHeaders = {
      "content-type": "text/plain; charset=utf-8",
      "cache-control": "no-store",
      "content-length": String(new TextEncoder().encode(healthBody).byteLength),
      "server-timing": serverTiming(started),
      ...SECURITY_HEADERS,
    };
    if (request.method === "HEAD") {
      return new Response(null, { status: degraded ? 503 : 200, headers: healthHeaders });
    }
    return new Response(healthBody, {
      status: degraded ? 503 : 200,
      headers: healthHeaders,
    });
  }
  // One page: anything else is that page too, rather than a 404 nobody
  // learns anything from.
  const chosen = await representationFor(request.headers.get("accept-encoding"), request);
  if (chosen === null) {
    return errorResponse(started, 406, request.method === "HEAD" ? null : "not acceptable", {
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
        "server-timing": serverTiming(started),
        ...SECURITY_HEADERS,
      },
    });
  }
  const headers = {
    ...PAGE_HEADERS,
    "content-length": String(chosen.bytes.byteLength),
    "server-timing": serverTiming(started),
  };
  if (chosen.coding) headers["content-encoding"] = chosen.coding;
  if (request.method === "HEAD") {
    return new Response(null, { headers, encodeBody: "manual" });
  }
  return new Response(chosen.bytes, { headers, encodeBody: "manual" });
}
