# toktop.ai

One Worker, one page, no build step.

```
make site-deploy    # from the repo root: deploy, then wait for /health to answer ok
make site-rollback  # undo the last deploy, back to the version before it, then wait for /health
```

Both targets pin the wrangler version (the Makefile's `WRANGLER`, read by the
target) and the bun version (`.bun-version`, the same file CI installs), so a
deploy cannot depend on whatever happens to be on the operator's PATH. Both
take `dist/site.lock` and both wait for `/health` when they finish, so neither
a rollback nor a deploy can report success over a site that is not answering.
Deploy credentials come from the environment (`CLOUDFLARE_API_TOKEN`, or a
`wrangler` login already on the machine); nothing about them is written to
this repo.

`make site-rollback` runs once. `wrangler rollback` with no version undoes
whichever deployment is most recent, whoever shipped it, so running the undo
twice rolls back a rollback and puts the version that broke back on the site.
A deploy that reported success leaves `dist/site.deployed` behind, and a
rollback moves it to `dist/site.rolled-back`: a second rollback finds nothing
of this tree's to undo, says so, and exits 0 without calling wrangler at all.
The markers are directories under `dist/`, so `make dist-clean` leaves them
alone and `make clean` takes them with the rest of `dist/`. They record what
this tree did, not what the site is serving, so on a machine that never ran
`make site-deploy` a rollback is a no-op rather than a guess.

`worker.js` holds the HTML: it is a template literal, so there is nothing to
bundle. The palette lives in the `DARK` and `LIGHT` objects at the top of that
file, and the CSS, the light scheme and the favicon all interpolate from them:
toktop is a terminal, and a hex written into a rule is the start of a second
palette. The same colors are the TUI's (`internal/ui/theme.go`) and the capture
renderer's (`scripts/screenshot.py`); the three files are in three languages, so
`bun test site/` is what keeps them one palette, and it fails on a hex that
drifts, on a one-off hex in a rule, and on a violet anywhere in the three.

The type is the same idea: the `--fs-*` steps in `:root` are the only place
the page writes a size, one per level from `--fs-micro` to `--fs-h1`, and
`bun test site/` checks the steps descend and that no rule sizes text in
its own rems (the wordmark's bare `2rem` inside the `max-width: 640px` query
is the one exemption it allows). The page has no uppercase, no tracking and
no color change on a level, so size is what marks one; a step out of order or
a size written in a rule is a heading the eye can no longer find. The h1 is
the one weight on the page: the terminal draws its own title bold
(`internal/ui/theme.go`), and at the default weight the largest type on the
page read lighter than the wordmark directly above it. Every level below the
h1 is size alone.

The vertical rhythm is the same idea turned down a notch: `--space-tight`,
`--space-section` and `--space-runout` in `:root` are the only places the page
writes a gap between sections. A literal in each rule that wants one leaves a
page with nothing to retune, and the copies drift; `bun test site/` fails on
a section gap written as a rem.

The layout follows the capture it is showing: the four panes under "What it
shows" are laid out the way the dashboard lays them out, with System running
the full width where the dashboard's SYS strip does, because its specimen is
the longest line on the page. Four equal cells would be a card grid, and a
card grid is not this product. The browser's own chrome is the page's
palette too, through one `theme-color` per scheme, so a phone's address bar
and overscroll glow are the terminal's background rather than a second
system drawn over it. Neither costs a byte of JavaScript, and the page still
ships without one.

The Worker answers `/health` with `ok` for uptime checks, serves the
dashboard capture from `public/` at `/dashboard.png`, `/dashboard.avif`,
`/dashboard-1280.avif`, `/dashboard-768.avif`, `/dashboard.webp`,
`/dashboard-1280.webp`, `/dashboard-768.webp` and `/dashboard-card.png`,
and answers every other path with the page (a one-page site should not 404
on a typo). `/favicon.ico` is the one exception, and it has to be: a crawler, a
bookmark or a client that ignored the `<link rel="icon">` data URI asks for
that path blind, and the catch-all answered it with the whole page, 3,606 bytes
of `text/html` for a request that wants an image. The Worker answers it with
the icon the page already carries inline, from the same bytes, with no asset
binding and no second request, and a browser that reads the `<link>` still
fetches nothing at all. Wrong methods are `405` with `Allow: GET, HEAD`.
`/health` reports
`degraded` with a `503` while the asset binding is missing, rather than `ok`:
the page still serves then, but every capture it shows is a 404, so a probe
saying `ok` describes a site nobody can use. A deploy that shipped without its
assets therefore fails `make site-deploy` instead of passing on a green probe.
That degraded answer is a failure, so it is logged as one (`health-degraded`,
capped like the rest): the missing binding is a deploy-level fault, and on a
site taking no image traffic the probe's own answer is the only thing naming
it. The healthy answer logs nothing, as every served answer does.
Image paths
without an asset binding, and 404/5xx from the asset store, are `no-store`
so a missing file is not cached as a day-long success. Error bodies are
`text/plain`, matching `/health`: a failure the asset store reports is
rewritten into that envelope, status kept, rather than passing the store's
own HTML error page through at an image path. A `HEAD` that fails answers with
those same headers and no body, as a served `HEAD` does, and reports the
length the body it withheld would have had, as `/health` and the page already
do.

The page carries an ETag derived from its own bytes: reloads and visits
past the five-minute freshness window answer with an empty 304 instead of
resending the body, and `stale-while-revalidate` lets returning browsers paint
from their copy while that check runs. That window is an hour, the one the
captures use, and it is how long a returning browser can paint a page from
before the deploy that replaced it. The validator is weak (`W/`) because
the page ships in several encodings under one URL, which one strong tag may
not span.

## Encoding

Clients that advertise brotli, zstd, or gzip get a cached compressed body.
Compression starts inside a request, not during module initialization, so
Worker stream APIs run in a request context. Only completed bytes are shared
across requests; the in-flight build is shared too, because the cache holds the
promise rather than the value, so a burst of cold requests waits on one
pipeline instead of each starting its own. Clients that advertise none of those
get the identity bytes. Among the encodings a client accepts,
the smallest body at the highest q-value wins, so a typical `gzip, deflate,
br, zstd` request is answered with brotli rather than gzip. That ranking is a
constant list in the Worker rather than a comparison of bodies, because the
page is a constant too: brotli 3,643 bytes, gzip 4,328, zstd 4,566. zstd
lands behind gzip here, so a client that named only `zstd, gzip` still gets
gzip. Unlisted identity
is a fallback, not a preference over accepted compression: `gzip;q=0.5` now
transfers 4,328 bytes rather than 12,524 bytes in the local Worker response test.
An explicit identity preference is respected. Refusing all available encodings
returns an uncacheable 406, including conditional requests; HEAD has no body.

Source comments
in the HTML and CSS stay in `worker.js` and are stripped before the page is
hashed, compressed, or sent. Every page response carries
`Vary: Accept-Encoding` (the `406` included), so caches never hand a compressed
page to a client that cannot decode it. The image, favicon and health paths
serve a single fixed representation each and carry no `Vary`.

A coding is built the first time a client asks for it and kept for the
isolate's life, so a cold isolate that serves brotli pays the brotli build
alone: measured against this page, brotli takes 23 ms, gzip 0.7 ms and zstd
4 ms, and building all three inside the request that only needs one of them
charged every visitor the lot. The build a request wants is started before
the negotiation finishes, and one it will not send is never started.

Answers that carry no body are decided before any build runs. A 406 comes from
what the Worker offers (`identity` plus `br`, `zstd` and `gzip`) read against
`Accept-Encoding`, and a 304 from `If-None-Match`, so neither waits on a coding
it will not send. An isolate that only ever serves revalidations never
builds a representation at all, and the reload after a deploy is answered off
the isolate's first request rather than after a pipeline. A client whose only
acceptable coding the runtime cannot build falls through to the next one it
named, and gets its 406 only when none of them can be built; the offer check
answers the refusals a client can state up front.

## Timing

Every answer carries `Server-Timing: edge;dur=<ms>`, the time the
Worker spent before writing the response, failures included: a failed request
is the one a visitor reports, and a timing series that covered only the served
requests would describe exactly the ones nobody is asking about. A byte-count
test cannot see a
regression here: the page can send the same 3,643 bytes slowly. With the
header, a RUM script or a visitor's own devtools reads the edge's share of
time to first byte on the connection they actually had, and no third party
has to be added to the page to collect it.

## Logs

`wrangler.jsonc` turns on Cloudflare Workers observability, so `console.error`
lands in Workers Logs. The Worker writes one JSON object per line there, and
nothing else: a served page, its 304s and its images are the steady state, and
a line per visit would bury the few that name a broken deploy.

| `event` | means |
| --- | --- |
| `unhandled` | a throw reached the top of `fetch`; the client gets a plain 500 instead of the edge's opaque 1101 page |
| `asset-missing` | the asset store answered 404 or 410 for a capture, the client gets the same `not found` either way |
| `asset-store-error` | the asset store answered 5xx |
| `assets-unbound` | an image path was requested with no asset binding, so every capture is a 404 and `/health` reports `degraded` |
| `coding-dropped` | one compression format failed to build; the page is served at its uncompressed size |
| `health-degraded` | `/health` answered 503 because the asset binding is missing; the healthy answer logs nothing |
| `method-not-allowed` | a method the path does not take, on the page or on an image |
| `not-acceptable` | the client refused every encoding the isolate can produce, so the page cannot be sent to it at all |

Every request line carries the same fields: `event`, the request's ray under
`ray` (empty off Cloudflare), its `method` and `path`, the `status` the
client was given, the `duration_ms` the edge spent getting there, and
whatever reason the event adds. A filter on method, path or status works
across every event, `coding-dropped` included: it names the build that failed
rather than the answer the client was given, and the request that asked for
it is the first one on the line, so the ray and the fields filter on. That is
the pivot from a failure
a visitor reports to the edge request behind it: filter Workers Logs on
`event`, then search the ray in the visitor's response headers. Past 20 lines
for one `event` in an isolate the Worker writes one more line, carrying
`dropped_after` instead of the request fields, and then stops: a client can
repeat a 405 or a 406 one request at a time, and an unearned line per request
buries the few lines an operator reads. The answer is unchanged, so the cap
costs the log nothing a client can act on. A `405` or a
`406` is rare next to the served requests and names a client the edge cannot
serve, which is a report an operator gets rather than a broken deploy, so
both are logged; the served page, its 304s and its images are the ones that
stay silent.

## Performance budget

One request for the page, no JavaScript, no webfonts, inline CSS only. The
hero is the real dashboard capture: AVIF (45,559 bytes at 1920px, 25,360 at
1280px, 10,577 at 768px), then WebP (148,050 / 81,540 / 36,130 bytes), then
the full-size PNG for a client that speaks neither. A phone lays the figure
out at about 360 CSS px,
so the 768w candidate is the slot a 2x screen takes: without it every phone
rounded up to 1280w and fetched 25,360 bytes to fill 722 of them, which is the
58% the 768w AVIF saves. A 3x phone (1083 device pixels) and a 1x desktop
(1216) still take 1280w, and 1920w remains the 2x desktop slot.
For public visitors, including mobile networks, `sizes` follows the body
gutters, figure borders, and 76rem column cap rather than declaring a desktop
width on tablets. Each format retains its 768w, 1280w and 1920w candidates.
Served from
this Worker so a deploy updates share cards and the page together.
`wrangler.jsonc` sets `run_worker_first` so those image paths hit the Worker
(cache headers, HSTS, 405s) instead of Cloudflare's asset pipeline, and
`public/` carries no `_headers` of its own: two files setting the same policy
is one of them drifting.

Captures are cached `max-age=86400, stale-while-revalidate=3600`. They are
served under stable names rather than content-hashed ones, so revalidation is
the only thing that can retire the copy a browser holds when a deploy
re-captures; the hour bounds how long a returning browser keeps showing the
previous screenshot, and costs one conditional request on a visit that is
already past `max-age`.
Measured
against the current source with Bun 1.4.2: 12,524 bytes identity / 4,328 gzip /
3,643 brotli for the HTML, still inside the
~14 KB initial congestion window. A phone's whole visit is those 3,643 bytes
plus the 10,577-byte 768w capture, 14,220 bytes in two requests; that pair has
a ceiling of its own in the same test, next to the per-asset ones, because
each half can pass its own limit while the visit still gets heavy. The PNG
original is the one download no
srcset narrows, so it carries a ceiling of its own in the same test.

The AVIF candidates are encoded at `-q 32`, which is where this capture stops
paying: 10,577 bytes at 768w against 13,563 at `-q 40`, at 30.0 dB PSNR
against the resized source. The page draws that candidate into about 662 device
pixels, so the browser downscales it and the difference is not on screen at the
size anyone reads it. 4:2:0 was measured for the same widths and is not
smaller: the frame is mostly flat dark background, so there is little chroma to
subsample, and it costs luma detail on the text that is the picture. WebP
stays at `-quality 82`; a browser that reaches for it has no AVIF at all and
gets the better-looking of the two.

The share card is a fourth file, not a fifth srcset candidate: the og
crawlers fetch the one URL in `og:image` and draw it at card size, so
`dashboard-card.png` is the capture at 1200px, the width a
`summary_large_image` is laid out at. The capture is 262 flat colors, so the
palette PNG shows the same frame in 68,924 bytes where the 3240px original
takes 303,865: the pixels past 1200 were bytes every share downloaded and
never drew. Its width and weight are pinned in the same test, so a re-capture
that stops producing it fails there. The budget
is pinned by a test, so drift fails `bun test site/`; numbers above are
re-measurable with it:

```sh
bun test site/    # or `make site-check` from the repo root
```

The Worker and its tests are formatted and linted by biome (`biome.jsonc` at the
repo root, run by `make site-lint`, applied by `make site-fmt`; the version pin
is the Makefile's `BIOME`, which CI reads). The bun version is pinned in
`.bun-version`; CI reads the same file.

Routing is by custom domain (`toktop.ai`, `www.toktop.ai`) rather than a
route pattern, so Cloudflare manages the DNS record for both names. The zone's
MX and SPF records are Namecheap's email forwarding and are left alone.
