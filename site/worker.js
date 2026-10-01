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
// 1280w capture, 25,360 bytes for 722 pixels of it. 1920w is the 2x desktop
// slot.
// The img omits decoding=async so the browser does not postpone the LCP decode.
// Its width and height are the fallback PNG's own 3240x1900, not a width the
// page draws it at: they only supply the aspect ratio the box is reserved with
// (.shot img is width:100%; height:auto), and stating a size the file does not
// have reserves the right ratio by coincidence rather than by construction.
// worker.test.js reads the PNG header and fails when the two disagree.
const HERO_SIZES =
  "(max-width: 640px) calc(100vw - 1.7rem - 2px), calc(min(76rem, 100vw - 2.5rem) - 2px)";
const HERO_AVIF_SRCSET =
  "/dashboard-768.avif 768w, /dashboard-1280.avif 1280w, /dashboard.avif 1920w";
const HERO_WEBP_SRCSET =
  "/dashboard-768.webp 768w, /dashboard-1280.webp 1280w, /dashboard.webp 1920w";

// The share card is a separate file, not another srcset candidate: a crawler
// reading og:image gets one URL and renders it at its own size, so the only
// question the width answers is how many pixels it is worth shipping. 1200px
// is the width a summary_large_image card is laid out at, and the capture is
// 262 flat colors, so a downscaled palette PNG shows the same frame in 69 KB
// where the 3240px original takes 304 KB.
const SHARE_CARD_PATH = "/dashboard-card.png";

// The palette, named once. toktop is a terminal: the page is a picture of one,
// and the same hexes are the TUI's (internal/ui/theme.go) and the capture
// renderer's (scripts/screenshot.py). Spelling a hex a second time here is a
// hex that can drift from the product it depicts, so the CSS, the light scheme
// and the favicon all read these names. site/worker.test.js pins the agreement
// across the three files, and the two schemes apart.
const DARK = {
  bg: "#0d1117",
  panel: "#11161d",
  line: "#222b36",
  fg: "#d7dde5",
  dim: "#7d8895",
  accent: "#4cc38a",
  warm: "#e3b341",
};
// LIGHT is a designed scheme, not DARK inverted. The identity holds across
// both: the green accent and the amber keep the hues the terminal draws,
// dropped to a lightness that reads as ink on paper. The neutrals move the
// other way, on purpose. The dark scheme is a cool near-black, because
// phosphor sits on cold glass, and a light scheme reusing that coolness would
// be the same terminal seen through a projector. This is paper instead: red
// and green sit two channels above blue, the way a terminal emulator's own
// light theme is warm rather than the dark colors run backwards. The ink
// stays cool in both, so a reader who switches is still reading inside one
// world, on cold glass or on warm paper.
//
// Neither decision moves a contrast number, so both are invisible in a diff
// and to every measurement the suite already makes. site/worker.test.js reads
// the temperature of each scheme and holds the two apart: #ffffff passes
// every ratio on the page and erases all of this.
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
const FAVICON_SVG =
  '<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 100 100">' +
  `<rect width="100" height="100" fill="${DARK.panel}"/>` +
  `<rect x="37" y="25" width="26" height="50" fill="${DARK.accent}"/>` +
  "</svg>";

// Inlined as a data URI, so a browser that reads the <link> fetches no second
// file. The same bytes answer /favicon.ico for the ones that ask blind, and
// that answer has to be here: with the icon only in the document, /favicon.ico
// fell through to the one-page catch-all and answered 3,606 bytes of
// text/html, a full page body for a request that wants an image.
const FAVICON = `data:image/svg+xml,${encodeURIComponent(FAVICON_SVG)}`;
const FAVICON_BYTES = new TextEncoder().encode(FAVICON_SVG);
const FAVICON_PATH = "/favicon.ico";

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
     the product the page is about. The card is the capture at 1200px, the
     width a summary_large_image is laid out at: the og crawlers that fetch
     this URL would otherwise pull the 3240px original, 304 KB where 69 KB
     shows exactly the same frame. -->
<meta property="og:image" content="https://toktop.ai${SHARE_CARD_PATH}">
<meta property="og:image:width" content="1200">
<meta property="og:image:height" content="704">
<meta property="og:image:alt" content="toktop running in a terminal: engine rows with throughput and KV-cache pressure beside an agent feed">
<meta name="twitter:card" content="summary_large_image">
<meta name="twitter:image" content="https://toktop.ai${SHARE_CARD_PATH}">
<!-- The browser's own chrome, painted from the same two palettes: without it
     a phone paints its address bar and its overscroll glow in its default,
     which is a second visual system over the page rather than around it. One
     tag per scheme, because a single tag cannot follow prefers-color-scheme. -->
<meta name="theme-color" content="${DARK.bg}" media="(prefers-color-scheme: dark)">
<meta name="theme-color" content="${LIGHT.bg}" media="(prefers-color-scheme: light)">
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
    /* One token, the whole page in it. The Latin faces come first, then one
       named family per script, because a run no family covers goes to the
       generic keyword and the platform's monospace is the one thing the code
       blocks cannot survive: a proportional face, or boxes. Naming the
       families loads nothing, they are what the reader already has. */
    --mono: ui-monospace, "SF Mono", "JetBrains Mono", Menlo, Consolas,
      "Sarasa Mono SC", "Noto Sans Mono CJK SC", "Noto Sans Mono CJK JP",
      "Noto Sans Mono", "Noto Sans Mono Devanagari", "Noto Sans Arabic",
      monospace;
    /* The type scale, one named step per level. Size is what separates the
       levels: no uppercase, no tracking, no color change, so a section start
       has to be legible as one. The page is monospaced, so a level is read as
       size. The h1 is bold, the one weight on the page, and it marks the top
       of the scale rather than any step within it. The h1 is exactly double
       the h2, and every
       step below that one is within a few pixels of the next, so no level on
       the page can be read as the level under it. Every step is in rem, the
       last three included: a step in pixels holds its size while the three
       above it follow the reader's browser text size, and the ratio the scale
       is built on, the one that makes a level findable, is the first thing
       such a reader loses. */
    --fs-h1: 2.6rem; --fs-h2: 1.3rem; --fs-lead: 1.05rem;
    --fs-body: 0.9375rem; --fs-small: 0.84375rem; --fs-micro: 0.78125rem;
    /* The vertical rhythm, three named steps. A page whose every gap is a
       literal in the rule that wants it has no rhythm to tune, only a set of
       numbers copied around: retuning the page then means finding each one.
       --space-section separates two sections, --space-tight the smaller gap
       where a section opens right under something already boxed, so the
       tighter step is a statement about what follows rather than a value that
       happens to be smaller, and --space-runout is the last one before the
       footer. */
    --space-tight: 1.5rem; --space-section: 2.8rem; --space-runout: 4rem;
    /* The height the sticky bar occupies, which is the one thing on the page
       that can sit on top of content. It is named rather than written into
       the scroll offset below because the two have to agree: an offset that
       under-reads the bar is the bar covering the thing it was meant to
       clear. The phone breakpoint raises it, the bar wrapping to two rows. */
    --bar-h: 4rem;
    /* The one radius on the page, on the one control that is a physical key.
       Every other box here is a rule and a background, because that is what a
       terminal draws: a rounded corner on a code block would be the page
       borrowing a web app's soft-edged cards to describe a frame that has no
       corner radius at all. The keycap earns its one, because a key is the
       one object a reader already holds a shape for. Named rather than
       written into the rule, so a second rounded box has to be argued for
       here instead of typed in beside whatever rule wanted it. */
    --radius-key: 4px;
  }
  @media (prefers-color-scheme: light) {
    :root {
      --bg: ${LIGHT.bg}; --panel: ${LIGHT.panel}; --line: ${LIGHT.line};
      --fg: ${LIGHT.fg}; --dim: ${LIGHT.dim};
      --accent: ${LIGHT.accent}; --warm: ${LIGHT.warm};
    }
  }
  * { box-sizing: border-box; }
  /* The bar is sticky at z-index 10, so anything the browser scrolls to can
     land underneath it. scroll-padding-top on the scrollport is the offset for
     every way the page is scrolled: an anchor jump, the skip link landing on
     main, and the browser bringing a focus stop into view on Tab, which no
     per-element margin covers. Without it a keyboard user tabbing through the
     bar's own row of links watched the focus ring slide under the bar and
     vanish, and the section each nav link names opened behind it (WCAG 2.4.11
     Focus Not Obscured, 2.4.7 Focus Visible). */
  html { scroll-padding-top: var(--bar-h); }
  @media (prefers-reduced-motion: no-preference) {
    html { scroll-behavior: smooth; }
  }
  body {
    margin: 0; padding: 0 1.25rem 5rem;
    background: var(--bg); color: var(--fg);
    font-family: var(--mono); font-size: var(--fs-body); line-height: 1.6;
  }
  .skip-link {
    position: absolute; top: -100px; inset-inline-start: 1.25rem; z-index: 100;
    padding: .5rem 1rem; background: var(--panel); color: var(--fg);
    border: 1px solid var(--accent); text-decoration: none; font-size: var(--fs-small);
  }
  .skip-link:focus, .skip-link:focus-visible {
    top: .7rem; outline: 2px solid var(--accent); outline-offset: 2px;
  }
  main { max-width: 76rem; margin: 0 auto; }
  /* Anchor bar: brand + section jumps, sticky. The rule is the bar's only
     edge: its background is --bg, the same token the page scrolls under it
     on, so no color separates the bar from the section passing behind. At
     --line that edge sits at 1.3:1, under the 3:1 that identifies a
     component's boundary (WCAG 1.4.11), and a reader scrolling could not
     see the content entering or leaving the bar. Same call the keycaps make
     below, for the same reason: --fg names a boundary, --line divides the
     page. */
  .bar { position: sticky; top: 0; z-index: 10; display: flex; gap: 1.25rem;
    row-gap: .3rem; flex-wrap: wrap; align-items: center; padding: .7rem 0;
    margin: 0 -1.25rem; padding-inline: 1.25rem;
    background: var(--bg); border-bottom: 1px solid var(--fg); }
  .brand { font-weight: 700; font-size: var(--fs-lead); text-decoration: none; color: var(--fg);
    white-space: nowrap; }
  .brand .cursor { color: var(--accent); }
  /* The section list wraps rather than shrinking or scrolling. The bar has no
     horizontal scroller, so a nav that did not wrap had two ways out at
     1.4.4 text sizes above the default: it pushed the wordmark off the left
     edge, or the last link past the right one with nothing to scroll to it.
     Every label in the list is the only route to its section, so a link that
     cannot be seen is a section that cannot be reached. The row gap is the
     phone step; the wrap itself is on at every width, because a 700px pane
     with enlarged text is as tight as a 360px phone with the default. */
  nav { display: flex; gap: 1.1rem; flex-wrap: wrap; font-size: var(--fs-small);
    margin-inline-start: auto; }
  nav a { color: var(--dim); white-space: nowrap; padding: .3rem 0; }
  .hero { padding-top: 2.6rem; }
  /* The h1 is bold for the same reason the wordmark above it is: the dashboard
     draws its title in a bold face (internal/ui/theme.go, wordmark), and the
     page is a picture of that terminal. At the default weight the largest type
     on the page read lighter than the brand line directly above it, so the
     page's first line had less presence than its own navigation. Weight marks
     the top of the scale only, and only there; every level below the h1 is
     still size alone, so the section structure reads without it. */
  h1 { font-size: var(--fs-h1); font-weight: 700; margin: 0; }
  /* The blinking cursor is decorative, so it stops rather than looping
     (WCAG 2.2.2). A preference is not a control: a user who never set
     prefers-reduced-motion would otherwise face a page that blinks at them
     for as long as they read it, with nothing on the page to stop it. Four
     cycles is under five seconds of motion, and the cursor is still there
     afterwards, solid, which is the state the wordmark above it already
     uses. */
  @media (prefers-reduced-motion: no-preference) {
    h1 .cursor { color: var(--accent); animation: blink 1.2s step-end 4; }
    @keyframes blink { 50% { opacity: 0; } }
  }
  .tag { color: var(--dim); margin: .6rem 0 2rem; font-size: var(--fs-lead); max-width: 62ch; }
  /* Section titles are sentence case on their own step of the scale, not
     uppercase micro-labels: the h2 is large enough to find while scrolling
     and small enough to stay the same idea as the wordmark above it.
     Install/Run sit tight under the capture; the manifesto heading after
     the list keeps the larger gap. */
  h2 { font-size: var(--fs-h2); color: var(--fg); font-weight: 600;
       margin: var(--space-section) 0 .7rem; }
  .shot + h2, h2 + pre + h2 { margin-top: var(--space-tight); }
  /* A code block is a focusable scroller (tabindex and role="region" below),
     and its border is the only cue that it scrolls: --panel against --bg is
     itself under 1.5:1, and a scrollbar is absent until it is hovered or
     dragged. A reader who tabbed in and found the rule at 1.3:1 had no way
     to tell the block from a clipped one. --fg names the edge, for the same
     reason the keycaps below do. */
  pre {
    background: var(--panel); border: 1px solid var(--fg);
    padding: 1rem 1.15rem; overflow-x: auto; margin: 0 0 1rem; font-size: var(--fs-small);
  }
  /* Narrow viewports clip code lines into a scroll container; a mouse-only
     scrollbar would lock keyboard users out (WCAG 2.1.1). The ring is the
     focus-visible default for everything, main included: main is the skip
     link's target and takes focus from the keyboard, so it needs the same
     landing marker every other focus stop gets (WCAG 2.4.7). It only ever
     holds focus right after that one key press, so the ring marks where
     focus landed rather than framing the page. */
  :focus-visible { outline: 2px solid var(--accent); outline-offset: 2px; }
  code { color: inherit; }
  .dim { color: var(--dim); }
  /* The capture needs the 76rem column; copy does not. */
  p, ul { max-width: 62ch; }
  ul { padding-inline-start: 1.1rem; margin: 0; }
  li { margin-bottom: .5rem; }
  li b { font-weight: 600; }
  /* Feature grid: four panes, each a name, a job, a specimen. The System
     pane runs the full width because that is where the dashboard puts it: the
     SYS strip is a full-width row under the charts, not a quarter of the
     frame, and its specimen is the longest line on the page. Four equal
     cells would be the card grid, not this product. */
  .grid { display: grid; grid-template-columns: repeat(2, 1fr); gap: .75rem;
    max-width: none; margin: 0 0 1rem; padding: 0; list-style: none; }
  /* The markup carries role="list" to answer for it: Safari drops the list
     semantics of any list styled list-style: none, so VoiceOver would read the
     four panes as four loose paragraphs and never say "list of four" (WCAG
     1.3.1). The role restores what the marker removal took away. */
  .grid li:last-child { grid-column: 1 / -1; }
  .grid li { margin: 0; background: var(--panel); border: 1px solid var(--line);
    border-top: 2px solid var(--accent); padding: .9rem 1rem; }
  /* Warm marks the System pane and nothing else: it is cYellow in the
     terminal, where amber is pressure, and that pane is the one about
     pressure (temps, VRAM, power). A second accent alternated by position
     would say nothing and read as decoration. */
  .grid li:last-child { border-top-color: var(--warm); }
  /* The pane name is body size in bold, its job the small step, its specimen
     the micro step: a pane reads as name, then sentence, then terminal line,
     which is the order the dashboard itself prints them in. */
  .grid b { display: block; font-size: var(--fs-body); margin-bottom: .3rem; }
  .grid p { margin: 0 0 .5rem; font-size: var(--fs-small); color: var(--dim); max-width: none; }
  .grid code { display: block; font-size: var(--fs-micro); white-space: normal; }
  /* Key table: chips left, action right. */
  .keys { display: grid; grid-template-columns: auto 1fr; gap: .3rem .9rem;
    max-width: 62ch; margin: 0 0 1rem; font-size: var(--fs-small); }
  .keys dt, .keys dd { margin: 0; }
  .keys dd { color: var(--dim); }
  /* A keycap is a UI component whose only boundary is this border, and
     --line sits at 1.3:1 on the page background, under the 3:1 that
     identifies a component's edges (WCAG 1.4.11). The border takes --fg so
     the key reads as a key; the box-drawing --line stays on the rules that
     divide the page rather than name a control. The sticky bar and the code
     blocks take --fg for that same reason, each at the point that needs it. */
  kbd { border: 1px solid var(--fg); border-radius: var(--radius-key);
    padding: 0 .4rem; font-family: inherit; font-size: var(--fs-micro); background: var(--bg); }
  /* Links must not be identified by color alone (WCAG 1.4.1): underline at
     rest, not just on hover. One device carries it. A transparent border that
     filled on hover drew a second rule two pixels under the underline already
     there, so every link the pointer crossed read as a rendering fault rather
     than as a link; hover thickens the underline it already has instead. */
  a { color: var(--accent); text-decoration: underline; text-underline-offset: 3px;
      text-decoration-thickness: 1px; }
  a:hover { text-decoration-thickness: 2px; }
  /* The footer is a sibling of main, not a child: only a footer outside the
     landmark scopes is exposed as the contentinfo landmark (WCAG 1.3.1), and
     the column width main sets is repeated here so moving it out of main
     moved nothing on screen. */
  footer { margin-top: var(--space-runout); max-width: 76rem; margin-inline: auto;
           padding-top: 1.25rem; border-top: 1px solid var(--line);
           color: var(--dim); font-size: var(--fs-small); display: flex; gap: 1.5rem; flex-wrap: wrap; }
  /* A small/1.6 line box is ~21px tall, under the 24px target-size floor
     (WCAG 2.2 AA SC 2.5.8); vertical padding makes each footer item a real
     target instead of leaning on the spacing exception. */
  footer > * { padding: .3rem 0; }
  /* The screenshot is the product, not a decoration: a dark terminal
     frame so the capture never sits on the light-scheme paper. The frame
     re-points the page tokens at the dark scheme, so it is a terminal in
     both schemes without repeating a hex, and the light page can never
     recolor the product. Every scheme token the frame's own text can name
     is in that list, not only the ones it paints surfaces with: --fg and
     --accent left pointing at the light values put light-scheme text on
     the dark frame, unreadable rather than merely wrong. */
  .shot {
    --bg: var(--dark-bg); --panel: var(--dark-panel);
    --line: var(--dark-line); --fg: var(--dark-fg);
    --dim: var(--dark-dim); --accent: var(--dark-accent);
    margin: 0; border: 1px solid var(--line);
    background: var(--bg); overflow: hidden;
  }
  .shot figcaption {
    margin: 0; padding: .55rem 1rem; font-size: var(--fs-small);
    color: var(--dim); background: var(--panel); border-bottom: 1px solid var(--line);
  }
  .shot img { display: block; width: 100%; height: auto; }
  /* A phone is one screen wide, so only the h1 drops a step, to the 2rem
     the type test exempts by name: the sticky wordmark keeps --fs-lead,
     and the section titles stay where the scale puts them, because they are
     read one at a time and every one of them fits a 360px column at 1.3rem.
     The section list wraps to a second row here rather than scrolling
     sideways: at the micro step the six labels need about 320px beside a
     71px wordmark, which is more than a 360px phone has, so an overflow-x
     scroller put the last link past the edge with no scrollbar on a phone
     to reveal it. A second row of links costs the reader one line of the
     screen; a section that cannot be reached from the bar costs the whole
     section. The wrap itself is not this breakpoint's to grant: it is on
     in the base rules, because enlarged text makes a wide pane as tight as
     a phone. */
  @media (max-width: 640px) {
    body { padding: 0 .85rem 4rem; }
    .bar { margin: 0 -.85rem; padding-inline: .85rem; gap: .8rem; row-gap: .25rem; }
    nav { gap: .8rem; row-gap: .15rem; font-size: var(--fs-micro);
      justify-content: flex-end; }
    h1 { font-size: 2rem; }
    .hero { padding-top: 2rem; }
    .grid { grid-template-columns: 1fr; }
    /* The bar wraps to two rows at this width, so it is taller than the 4rem
       the desktop rule clears: an anchor jump landed the section heading
       under the bar it was meant to clear, and on a phone the heading was
       the only thing naming the section. Two rows of .7rem padding over the
       brand line and the micro-step nav come to about 4.6rem. Raising the
       token is all it takes: the scroll offset above reads it, so no rule
       spells the height a second time. */
    :root { --bar-h: 6rem; }
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
    <a href="#shows">Shows</a>
    <a href="#keys">Keys</a>
    <a href="#feed">Feed</a>
    <a href="#measured">Measured</a>
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
    <!-- The prompt glyph is a shell decoration, not a character in the
         command: aria-hidden keeps a screen reader from announcing
         "dollar" ahead of the command it introduces. -->
    <figcaption><span class="dim" aria-hidden="true">$</span> toktop --demo</figcaption>
    <picture>
      <source type="image/avif" srcset="${HERO_AVIF_SRCSET}" sizes="${HERO_SIZES}">
      <source type="image/webp" srcset="${HERO_WEBP_SRCSET}" sizes="${HERO_SIZES}">
      <img src="/dashboard.png" width="3240" height="1900"
           alt="The toktop terminal dashboard: a row of inference engines, two throughput charts, a GPU and host strip, and an agent feed."
           fetchpriority="high">
    </picture>
  </figure>

  <section id="install" aria-labelledby="install-heading">
  <h2 id="install-heading">Install</h2>
<pre tabindex="0" role="region" aria-label="Install commands"><code>go install -tags sqlite github.com/maci0/toktop/cmd/toktop@latest</code></pre>
  <p class="dim">The <code>sqlite</code> tag matches the release binaries: without it
  crush and opencode stores are unreadable. No Go toolchain?
  <a href="https://github.com/maci0/toktop/releases">Releases</a> has a binary for
  linux, macos or windows (amd64 and arm64), and <code>toktop update</code> keeps it
  current.</p>
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
  <ul class="grid" role="list">
    <li><b>Engines</b><p>Found by port and process, fingerprinted by HTTP.</p><code>Ollama · vLLM · llama.cpp · SGLang · LM Studio · MLX · +9</code></li>
    <li><b>Agents</b><p>Read from their own session logs. No cooperation needed.</p><code>claude · codex · qwen · copilot · dsh · +2 stores</code></li>
    <li><b>Probes</b><p>Real generations measuring TTFT and decode speed.</p><code>press p · or --probe N to auto-probe</code></li>
    <li><b>System</b><p>GPU/NPU, VRAM, temps, power beside the throughput.</p><code>nv0 82° 69% · vram 57G/80G · 397W</code></li>
  </ul>
  <p class="dim">Transcripts: claude, codex, qwen, copilot, kimi, gemini, grok,
  agy, cursor-agent, pi, prime-agent, feynman, omp, clanker, and dsh
  (zstd or plain JSONL);
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
<pre tabindex="0" role="region" aria-label="Agent feed event payload"><code>curl -X POST localhost:8420/v1/events \
  -H "Idempotency-Key: coder-turn-1042" -d \
  '{"agent":"coder","output_tokens":310,"prompt_tokens":4200}'</code></pre>
  <p class="dim">Any harness can POST usage to the ingest endpoint
  (<code>127.0.0.1:8420</code>, <code>--no-ingest</code> disables it).
  The <code>Idempotency-Key</code> names one turn, so a resend after a lost
  answer counts once: mint it per turn, never from the clock, which would
  make every retry a new turn.</p>
  <p class="dim"><code>--once --plain</code> prints a linear report for screen
  readers: no braille, no borders, no columns.</p>
  </section>

  <section id="measured" aria-labelledby="measured-heading">
  <h2 id="measured-heading">Measured, or nothing</h2>
  <p class="dim">Every number is one an engine or an agent actually
  reported. Nothing estimated or inferred. An agent that reports nothing
  shows no rate, not a zero. An agent on a watched engine is counted once.</p>
  </section>
</main>

<footer>
  <a href="https://github.com/maci0/toktop">github.com/maci0/toktop</a>
  <span>MIT licensed</span>
</footer>
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
// One tag across the codings is safe because every copy of this page is keyed
// on the coding it was built for: Vary: Accept-Encoding is on the 200, the 304
// and the 406 alike, so a cache holding the brotli body never answers a zstd
// client, and the revalidation that reaches this Worker carries no coding of
// its own to contradict.
// FNV-1a over source text, as the 32-bit hex string every derived validator
// here is built from. One hash for every constant in this file, so a second
// validator cannot be a second implementation of it.
function fnv1a(source) {
  let hash = 0x811c9dc5;
  for (let i = 0; i < source.length; i++) {
    // biome-ignore lint/suspicious/noBitwiseOperators: FNV-1a is defined by xor, not arithmetic.
    hash ^= source.charCodeAt(i);
    hash = Math.imul(hash, 0x01000193);
  }
  // biome-ignore lint/suspicious/noBitwiseOperators: the unsigned shift is how FNV-1a normalizes to 32 bits.
  return (hash >>> 0).toString(16);
}

const ETAG_HASH = fnv1a(HTML);

const ETAG = `W/"${ETAG_HASH}"`;

// The icon is a second constant in this file with the same deploy-time
// lifetime as the page, so it answers the same way: a client that asks for
// /favicon.ico blind asks again on every visit, and without a validator each
// of those is a full 200. The tag is strong, unlike the page's, because the
// icon ships as one encoding under one URL, so one strong tag describes every
// representation of it.
const ICON_ETAG_HASH = fnv1a(FAVICON_SVG);

const ICON_ETAG = `"${ICON_ETAG_HASH}"`;

// RFC 9110 weak comparison for If-None-Match: any list member counts, an
// optional W/ prefix is ignored, and * matches whatever is held. Comparing
// the stripped forms means validators sent back by older deploys (which used
// a strong tag over the same hash) still revalidate to a 304. The expected
// hash is a parameter because the page and the icon each answer with their
// own: a client holding one must not be told the other is fresh.
function ifNoneMatchMatches(headerValue, expectedHash) {
  const value = headerValue?.trim();
  if (!value) return false;
  if (value === "*") return true;
  return value.split(",").some((raw) => {
    let candidate = raw.trim();
    if (candidate.startsWith("W/")) candidate = candidate.slice(2);
    return candidate === `"${expectedHash}"`;
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
const COMPRESSIBLE = new Map([
  ["br", "brotli"],
  ["zstd", "zstd"],
  ["gzip", "gzip"],
]);

// The same three codings, smallest body of this page first. The page is a
// constant, so those sizes are constants too, and ranking by them lets a
// request build only the coding it is about to send instead of all three to
// compare them. zstd lands behind gzip here because the page is short English
// words and markup, which is not what a zstd dictionary is for. The measured
// sizes are asserted in worker.test.js; they are not repeated here.
const CODING_BY_SIZE = ["br", "gzip", "zstd"];

async function compressFormat(format) {
  return new Uint8Array(
    await new Response(
      new Response(HTML).body.pipeThrough(new CompressionStream(format)),
    ).arrayBuffer(),
  );
}

const IDENTITY = new TextEncoder().encode(HTML);

// The built body of each coding, or the in-flight build. A promise, not the
// resolved value: a cold isolate interleaves concurrent requests at every
// await, so a value-only cache is a check-then-act that lets a whole burst of
// them start the same pipeline together. Assigning the promise synchronously
// means the second caller awaits the first one's work instead of repeating it.
// A coding is built when a client asks for it, so an isolate that only ever
// sees brotli never pays the gzip and zstd builds, and one that only ever
// revalidates builds nothing at all.
const BODIES = new Map();

// The request rides along only so a dropped coding names the edge request
// that hit it: the build is shared, so the first caller's ray is the one on
// the line, not a claim about every request it served. It carries the same
// method, path and duration every other failure line does, so one filter
// covers this event with the rest.
function pageBody(coding, request, started) {
  if (!BODIES.has(coding)) {
    BODIES.set(
      coding,
      compressFormat(COMPRESSIBLE.get(coding)).catch((err) => {
        // A runtime without the format is the ordinary case, and the bytes
        // will not turn up later, so the slot keeps that answer rather than
        // retrying a build this isolate cannot make. Anything else (out of
        // memory, a stream that dies mid-pipeline) would ship the page at
        // its uncompressed size forever without a word, so name it, with the
        // frames that say where the build died.
        const stack = stackLine(err);
        logFailure(request, "coding-dropped", {
          ...requestFields(request, started),
          coding,
          // A throw carries any value, null included, so err may have no message.
          error: String(err?.message ?? err),
          ...(stack === "" ? {} : { stack }),
        });
        return null;
      }),
    );
  }
  return BODIES.get(coding);
}

// The codings this Worker offers, for the acceptability check that runs
// before a body exists. A coding the runtime then fails to build is still
// caught in representationFor, which moves on to the next one.
const OFFERED_CODINGS = [null, ...COMPRESSIBLE.keys()];

function refusesEveryCoding(acceptEncoding) {
  const qByCoding = parseAcceptEncoding(acceptEncoding);
  return OFFERED_CODINGS.every((coding) => quality(qByCoding, coding ?? "identity") <= 0);
}

// Highest q the client offered, then the smallest body at that q, which for
// this page is the CODING_BY_SIZE order. A Chrome `gzip, deflate, br, zstd`
// request therefore gets brotli rather than gzip, and a `br;q=0.1, gzip`
// request still gets gzip. Identity comes last: a client that named no
// coding the Worker can build still gets the page.
function acceptableCodings(qByCoding) {
  return [...CODING_BY_SIZE, null]
    .map((coding, rank) => ({ coding, rank, q: quality(qByCoding, coding ?? "identity") }))
    .filter((entry) => entry.q > 0)
    .sort((a, b) => b.q - a.q || a.rank - b.rank)
    .map((entry) => entry.coding);
}

// The first acceptable coding this isolate can build, built here when no
// earlier request wanted it. A coding the runtime cannot build falls through
// to the next rather than costing the client the page.
async function representationFor(acceptEncoding, request, started) {
  const qByCoding = parseAcceptEncoding(acceptEncoding);
  for (const coding of acceptableCodings(qByCoding)) {
    if (coding === null) return { coding, bytes: IDENTITY };
    // Sequential on purpose: the next coding is the fallback for one that
    // cannot be built, so building them together would spend the bytes of a
    // body this request would then throw away.
    // biome-ignore lint/performance/noAwaitInLoops: a fallback chain, not a fan-out.
    const bytes = await pageBody(coding, request, started);
    if (bytes !== null) return { coding, bytes };
  }
  return null;
}

// Fresh for five minutes, then served from the browser's copy while a cheap
// 304 revalidation runs in the background: repeat visitors paint instantly
// and never wait on the edge. The revalidate window is an hour, the same one
// the captures use: it is how long a returning browser can keep painting a
// page from before a deploy, and a day of that would leave a visitor reading
// yesterday's install commands. Past the hour the copy is still served while
// the check runs, so the cost is one conditional request on a visit that is
// already past max-age, and never a blank screen.
const PAGE_CACHE_CONTROL = "public, max-age=300, stale-while-revalidate=3600";

// Several encodings live under one URL, so every cached copy must be keyed on
// what the accepting client asked for; without Vary a shared cache could hand
// a compressed body to a client that cannot decode it.
const VARY = "Accept-Encoding";

const SECURITY_HEADERS = {
  "x-content-type-options": "nosniff",
  "x-frame-options": "DENY",
  // includeSubDomains pins every subdomain of a name the first time the name
  // itself is visited, instead of waiting for each subdomain to serve the
  // header itself. Both custom domains here are covered by their own header,
  // but any subdomain added later (a preview host, a redirect target) is
  // served nothing until someone remembers to configure it, and the first
  // request to it is exactly the one an on-path attacker waits for.
  "strict-transport-security": "max-age=31536000; includeSubDomains",
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
//
// The length is computed here rather than left to the runtime, for the same
// reason /health computes it: a HEAD carries the GET's headers and no body
// (RFC 9110), and a null-bodied Response reports a length of zero, which is
// not the length of the answer the client would have got. Every answer on this
// surface states its length, so a client sizing a body reads one across all of
// them, and the HEAD rule sits in one place rather than at every call site.
// The newline is added here so every failure is a whole line. Three of them
// ended one already and the rest did not, so a client reading failures the way
// it reads /health, a line at a time, got a truncated reason on a 405 and a
// whole one on an asset 404. /health answers "ok\n" and the ingest server's
// reasons are lines too, so one trailing newline is what a caller can rely on
// across both surfaces.
function errorResponse(request, started, status, body, extraHeaders = {}) {
  const bytes = new TextEncoder().encode(body.endsWith("\n") ? body : `${body}\n`);
  return new Response(request.method === "HEAD" ? null : bytes, {
    status,
    headers: {
      ...ERROR_HEADERS,
      "content-length": String(bytes.byteLength),
      "server-timing": serverTiming(started),
      ...extraHeaders,
    },
  });
}

// A client that refuses every coding gets an uncacheable 406, conditional
// requests included: it cannot read any representation of the page, so a 304
// would leave it holding a copy it still cannot decode. The Vary rides along
// so a shared cache keys the refusal on what the client asked for. It is rare
// next to the served requests and the reason a visitor would report, so it
// logs like the rest of the failures rather than passing in silence.
function notAcceptable(request, started) {
  return failRequest(request, started, 406, "not-acceptable", "not acceptable", { vary: VARY });
}

// The edge's own clock, for measuring how long this Worker took.
//
// Date.now() is not that clock. The runtime freezes time across a synchronous
// stretch and only advances it at I/O, so a request that spends its work in
// env.ASSETS.fetch or a compression stream reads the same millisecond before
// and after, and the log line and the Server-Timing header both report 0 for an
// answer that took a second to reach the client. performance.now() is the
// monotonic reading the runtime keeps running across those waits, and it cannot
// step backwards the way a wall clock can. Fractional milliseconds are rounded
// because the header is an integer and the logs are read as whole numbers.
function edgeNow() {
  return performance.now();
}

function edgeElapsedMs(started) {
  return Math.round(edgeNow() - started);
}

// The request fields every failure line carries, so a filter on method, path
// or duration works across every event rather than only the ones that
// happened to pass them in. The duration is what the edge spent to reach the
// failure, which is the number the answer's Server-Timing reports.
function requestFields(request, started) {
  return {
    method: request.method,
    path: new URL(request.url).pathname,
    duration_ms: edgeElapsedMs(started),
  };
}

// One failure, one line and one answer. The line names the status the client
// got and the milliseconds the edge spent, so one pivot off a visitor's
// report says whether it succeeded and how slowly; the answer carries the
// same numbers as headers.
function failRequest(request, started, status, event, body, extraHeaders = {}, fields = {}) {
  logFailure(request, event, {
    ...requestFields(request, started),
    status,
    ...fields,
  });
  return errorResponse(request, started, status, body, extraHeaders);
}

// What the edge spent on the answer, in Server-Timing (RFC 8941), so the
// number a visitor or a RUM script reads is the time to first byte from this
// Worker rather than an unbreakable share of a round trip. The page is the
// only surface that has no client-side timing to fall back on, and a
// regression here is invisible in a byte-count test: it is a change in how
// long the edge takes, not in how much it sends.
function serverTiming(started) {
  return `edge;dur=${edgeElapsedMs(started)}`;
}

// How many lines one isolate writes for one request-driven event before it
// stops writing them. The refusals a client can repeat at will (a wrong
// method, an Accept-Encoding it refuses, a missing image on a hot path) are one
// request away from a log stream nobody sent, and a line per request buries
// the unhandled lines an operator reads. Honest traffic never reaches the cap:
// a 405 or a 406 next to served traffic is one line, and one line still says
// it.
const REFUSAL_LOG_CAP = 20;

// The one event a client cannot drive: /health logs it from a probe on a
// timer, not from a request, so its volume is the probe interval rather than
// traffic. It is the deploy-level line an operator reads when a site ships
// without its captures, and on such a site it is often the only line written
// at all (no image traffic, no errors, every page a 200). Capping it spends
// the whole budget in the first few minutes of every isolate and then reports
// a day-long broken deploy for the minutes before the isolate recycles, which
// is the blind spot the cap exists to prevent: the cap is here to keep a
// request flood from burying a state, and applying it to the state itself
// buries it. Every other event is request-driven or fault-driven and stays
// capped; this one is neither, and one line per probe interval is not a flood.
const UNCAPPED_EVENTS = new Set(["health-degraded"]);

// Lines written per event in this isolate. Keyed by event name, of which there
// is a fixed handful, so the map does not grow with traffic.
const loggedPerEvent = new Map();

// One JSON object per line, so Workers Logs can filter on a field rather than
// parse prose, and the edge's cf-ray rides along so a failure a visitor
// reports pivots from the line to that edge request. Only failures log: a
// served page, its 304s and its images are the steady state, and a line per
// visit would bury the few that name a broken deploy.
//
// Past the cap the line is written once more, without the request fields, and
// it says the rest are dropped: a total nobody will read is not reported, and
// a count that stopped counting would be worse than none. An event in
// UNCAPPED_EVENTS skips that budget and is written every time it happens.
function logFailure(request, event, fields) {
  if (UNCAPPED_EVENTS.has(event)) {
    writeLine({ event, ray: request.headers.get("cf-ray") ?? "", ...fields });
    return;
  }
  const seen = (loggedPerEvent.get(event) ?? 0) + 1;
  loggedPerEvent.set(event, seen);
  if (seen > REFUSAL_LOG_CAP + 1) {
    return;
  }
  writeLine(
    seen > REFUSAL_LOG_CAP
      ? { event, dropped_after: REFUSAL_LOG_CAP }
      : { event, ray: request.headers.get("cf-ray") ?? "", ...fields },
  );
}

function writeLine(line) {
  // biome-ignore lint/suspicious/noConsole: Workers Logs is the only place a failure reaches an operator.
  console.error(JSON.stringify(line));
}

// The reason line for a failure the asset store reported. The store's own
// body is not repeated: it is an HTML page that names no image path.
function assetErrorBody(status) {
  if (status === 404 || status === 410) return "not found\n";
  return "asset store error\n";
}

// The capture a health check reads. One file answers for the set, and this is
// the one every page view depends on from outside the page: the social
// crawlers that fetch og:image ask for exactly this path, so a deploy that
// shipped without the captures is a deploy whose share cards are broken even
// while the page itself answers 200. A binding that exists is not the same
// thing as a file behind it, and only a read of the store tells them apart.
const captureProbePath = SHARE_CARD_PATH;

// reasonLine folds a store's answer into a single bounded line: the reason
// rides a probe body and a log line, and the text it comes from is off the
// wire (an error message from the store), so a newline in it would split the
// line and a control character in it would write over the terminal reading it.
// biome-ignore lint/suspicious/noControlCharactersInRegex: matching the control characters is the point.
const CONTROL_CHARS_RE = /[\u0000-\u001f\u007f]+/g;

// capCodePoints cuts text to at most max code points, never between the two
// halves of one. String.prototype.slice counts UTF-16 code units, and every
// astral character (an emoji in an error message, a CJK extension ideograph)
// is two of them, so a cut at the cap can land between the halves and leave a
// line holding a lone surrogate: it prints as U+FFFD, and JSON.stringify emits
// it as an escape an operator reads as mojibake in the store's own words. The
// length test comes first because a code point is one or two units, so a
// string already within the cap in units is within it in points and the array
// is never built for a line that fits.
function capCodePoints(text, max) {
  if (text.length <= max) return text;
  const points = Array.from(text);
  return points.length <= max ? text : points.slice(0, max).join("");
}

function reasonLine(text) {
  return capCodePoints(text.replace(CONTROL_CHARS_RE, " ").trim(), 200);
}

// How much of a stack one line carries. Bounded like the reason beside it and
// for the same reason: the line is one JSON object, so the stack is folded
// onto it rather than left to break the format Workers Logs reads.
const maxStackLength = 2048;

// The stack behind an unhandled throw, folded and capped, or an empty string
// when the thrown value carries none. A throw that reaches the top of fetch is
// the one failure on this site an operator cannot reproduce: the request that
// caused it is a visitor's, on an isolate that is gone by the time anyone reads
// the line, and a message alone ("x is not a function") names neither the call
// nor the deploy it came from. Folded to one line because the log format is one
// JSON object per line, which a raw multi-line stack would split.
function stackLine(err) {
  if (typeof err?.stack !== "string") return "";
  return capCodePoints(err.stack.replace(CONTROL_CHARS_RE, " ").trim(), maxStackLength);
}

// captureUnavailable names why the captures are not being served, or null
// when they are. The read is one HEAD against a store that answers it from the
// edge cache, once per probe, which is what a probe interval is measured in;
// caching the verdict per isolate would only buy a stale answer, since the
// verdict changes when a deploy changes the files and a deploy arrives as a
// new isolate with nothing cached.
//
// The store's own failure is answered here rather than left to the caller's
// catch: a probe must not answer 500 for a store that is down, since 500 says
// the Worker is broken and the answer it needs to give is that the site is
// degraded while the page still serves.
async function captureUnavailable(request, env) {
  if (!env?.ASSETS) return "no asset binding";
  try {
    const res = await env.ASSETS.fetch(
      new Request(new URL(captureProbePath, request.url).toString(), { method: "HEAD" }),
    );
    if (res.status < 400) return null;
    return reasonLine(`the share card answered ${res.status} from the asset store`);
  } catch (err) {
    // The reason does not carry the exception's own text. It is returned to
    // /health, which is unauthenticated and public, and a runtime exception
    // message names the deploy's internals (bindings, paths, the runtime's
    // own wording) to whoever asks. The detail goes to the log instead, the
    // way the top-level catch in fetch already does, so an operator reads it
    // there and a visitor reads only that the store is down.
    logFailure(request, "asset-store-unreadable", {
      path: new URL(request.url).pathname,
      error: reasonLine(`the asset store could not be read: ${String(err?.message ?? err)}`),
      stack: stackLine(err),
    });
    return reasonLine("the asset store could not be read");
  }
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

// srcset candidates are "url descriptor" pairs separated by commas.
const SRCSET_SEPARATORS = /\s+/;

function srcsetPaths(srcset) {
  return srcset.split(",").map((part) => part.trim().split(SRCSET_SEPARATORS)[0]);
}

const IMAGE_PATHS = new Set([
  "/dashboard.png",
  SHARE_CARD_PATH,
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
  // the reason, the stack it unwound through and how long it took, then answer
  // with the same plain-text envelope every other failure here uses, so a
  // client can still act on it.
  async fetch(request, env) {
    const started = edgeNow();
    try {
      return await handle(request, env, started);
    } catch (err) {
      // A throw carries any value, null included, so err may have no message
      // and no stack; the line carries the stack only when there is one rather
      // than an empty field that reads like a stack of nothing.
      const stack = stackLine(err);
      return failRequest(request, started, 500, "unhandled", "internal error", undefined, {
        error: String(err?.message ?? err),
        ...(stack === "" ? {} : { stack }),
      });
    }
  },
};

async function handle(request, env, started) {
  const url = new URL(request.url);
  if (IMAGE_PATHS.has(url.pathname)) {
    if (request.method !== "GET" && request.method !== "HEAD") {
      return failRequest(request, started, 405, "method-not-allowed", "method not allowed", {
        allow: "GET, HEAD",
      });
    }
    if (!env?.ASSETS) {
      // Every image on the page is now a 404 and /health reports the missing
      // binding, so this is the line that names the request behind it.
      return failRequest(request, started, 404, "assets-unbound", "not found");
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
        assetErrorBody(asset.status),
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
    return failRequest(request, started, 405, "method-not-allowed", "method not allowed", {
      allow: "GET, HEAD",
    });
  }
  if (url.pathname === "/health") {
    // Uptime probes hit this continuously; caching it would only blur
    // what the last probe actually saw. HEAD must carry the GET headers
    // and no body (RFC 9110).
    //
    // The probe reports degraded rather than ok whenever the captures are not
    // being served: the page still answers, but every image on it is a 404,
    // so a probe that keeps saying ok describes a site nobody can use. That
    // is the same call the ingest /healthz makes when it is refusing every
    // event, and it is what makes `make site-deploy` fail a deploy that
    // shipped without its assets instead of waiting out a green probe.
    //
    // A binding is not a capture. A deploy that shipped the Worker and left
    // the files behind answers 200 here with every capture missing, and the
    // page's own og:image broken, so the store is read as well: the share
    // card is the one capture a request from outside the page always asks
    // for, and its answer is what separates a bound store from a stocked one.
    // That read is one HEAD per probe, against a store that answers it from
    // the edge cache, which is the unit a probe interval is counted in.
    //
    // A degraded answer is a failure, so it is logged like one: the missing
    // binding or the missing files are deploy-level things, and on a site
    // taking no image traffic the 503 is the only thing that says it. This
    // event is the one in UNCAPPED_EVENTS, so the probe's own interval bounds
    // the volume and a site broken for hours keeps saying so instead of
    // spending the refusal budget in the first minutes of every isolate. The
    // healthy answer logs nothing, as every served answer does.
    const reason = await captureUnavailable(request, env);
    const degraded = reason !== null;
    if (degraded) {
      logFailure(request, "health-degraded", {
        ...requestFields(request, started),
        status: 503,
        reason,
      });
    }
    const healthBody = degraded
      ? `degraded: ${reason}; the dashboard captures are not served\n`
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
  // The icon, for the clients that ask the path instead of reading the <link>:
  // a crawler, a bookmark, a browser that ignored the data URI. Answered from
  // the bytes already in this file, so it costs no request of its own and no
  // asset binding, and it is not compressed: an SVG is markup the Worker can
  // hand over as it stands, and a few hundred bytes do not earn a pipeline.
  // The one-page catch-all below would otherwise answer this with the whole
  // page, under text/html, to a request for an image.
  if (url.pathname === FAVICON_PATH) {
    // Revalidation is answered before the bytes are written, the same order the
    // page uses: a 304 carries no body, so it must not cost a body either.
    if (ifNoneMatchMatches(request.headers.get("if-none-match"), ICON_ETAG_HASH)) {
      return new Response(null, {
        status: 304,
        headers: {
          etag: ICON_ETAG,
          // The icon's bytes change only at a deploy, exactly like the page's,
          // so it carries the page's freshness window rather than a longer one.
          "cache-control": PAGE_CACHE_CONTROL,
          "server-timing": serverTiming(started),
          ...SECURITY_HEADERS,
        },
      });
    }
    const iconHeaders = {
      "content-type": "image/svg+xml",
      "content-length": String(FAVICON_BYTES.byteLength),
      etag: ICON_ETAG,
      "cache-control": PAGE_CACHE_CONTROL,
      "server-timing": serverTiming(started),
      ...SECURITY_HEADERS,
    };
    if (request.method === "HEAD") {
      return new Response(null, { headers: iconHeaders });
    }
    return new Response(FAVICON_BYTES, { headers: iconHeaders });
  }
  // One page: anything else is that page too, rather than a 404 nobody
  // learns anything from.
  const acceptEncoding = request.headers.get("accept-encoding");
  // Refusability is answered from the offer alone, before any compressed
  // body is built, so a client that can read nothing never waits for the
  // three codings to exist. Revalidation is answered next, and for the same
  // reason: a 304 carries no body, so it must not wait on a build it will not
  // use. An isolate that only ever sees revalidations never builds at all,
  // and a cold one answers the reload that follows a deploy without paying
  // brotli, zstd and gzip first. The build still runs for a client that needs
  // the bytes, and its 406 still applies when the only coding a client wanted
  // is one the runtime could not produce.
  if (refusesEveryCoding(acceptEncoding)) {
    return notAcceptable(request, started);
  }
  if (ifNoneMatchMatches(request.headers.get("if-none-match"), ETAG_HASH)) {
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
  const chosen = await representationFor(acceptEncoding, request, started);
  if (chosen === null) {
    return notAcceptable(request, started);
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
