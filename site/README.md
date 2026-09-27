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

The Worker answers `/health` with `ok` for uptime checks, serves the
dashboard capture from `public/` at `/dashboard.png`, `/dashboard.avif`,
`/dashboard-1280.avif`, `/dashboard-768.avif`, `/dashboard.webp`,
`/dashboard-1280.webp` and `/dashboard-768.webp`,
and answers every other path with the page (a one-page site should not 404
on a typo). Wrong methods are `405` with `Allow: GET, HEAD`. `/health` reports
`degraded` with a `503` while the asset binding is missing, rather than `ok`:
the page still serves then, but every capture it shows is a 404, so a probe
saying `ok` describes a site nobody can use. A deploy that shipped without its
assets therefore fails `make site-deploy` instead of passing on a green probe.
Image paths
without an asset binding, and 404/5xx from the asset store, are `no-store`
so a missing file is not cached as a day-long success. Error bodies are
`text/plain`, matching `/health`: a failure the asset store reports is
rewritten into that envelope, status kept, rather than passing the store's
own HTML error page through at an image path. A `HEAD` that fails answers with
those same headers and no body, as a served `HEAD` does.

The page carries an ETag derived from its own bytes: reloads and visits
past the five-minute freshness window answer with an empty 304 instead of
resending the body, and `stale-while-revalidate` lets returning browsers paint
from their copy while that check runs. The validator is weak (`W/`) because
the page ships in several encodings under one URL, which one strong tag may
not span.

## Encoding

Clients that advertise brotli, zstd, or gzip get a cached compressed body.
Compression starts inside a request, not during module initialization, so
Worker stream APIs run in a request context. Only completed bytes are shared
across requests; simultaneous cold requests compress independently rather
than sharing request-owned stream work. Clients that advertise none of those
get the identity bytes. Among the encodings a client accepts,
the smallest body at the highest q-value wins, so a typical `gzip, deflate,
br, zstd` request is answered with brotli rather than gzip. Unlisted identity
is a fallback, not a preference over accepted compression: `gzip;q=0.5` now
transfers 4,124 bytes rather than 11,869 bytes in the local Worker response test.
An explicit identity preference is respected. Refusing all available encodings
returns an uncacheable 406, including conditional requests; HEAD has no body.

Source comments
in the HTML and CSS stay in `worker.js` and are stripped before the page is
hashed, compressed, or sent. Every response carries `Vary: Accept-Encoding`,
so caches never hand a compressed body to a client that cannot decode it.

The three codings are built concurrently. A cold isolate pays that build
inside the first request it is answering, so awaiting them in sequence makes
that one request wait for the sum of the three rather than the slowest one.

## Timing

Every answer carries `Server-Timing: edge;dur=<ms>`, the time the
Worker spent before writing the response, failures included: a failed request
is the one a visitor reports, and a timing series that covered only the served
requests would describe exactly the ones nobody is asking about. A byte-count
test cannot see a
regression here: the page can send the same 3,444 bytes slowly. With the
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

Each line that answers a request carries the request's `cf-ray` (empty off
Cloudflare), the `status` the client was given, the `duration_ms` the edge
spent getting there, plus the path, method or reason as the event needs. That
is the pivot from a failure
a visitor reports to the edge request behind it: filter Workers Logs on
`event`, then search the ray in the visitor's response headers. A `405` or a
`406` is not logged: the client did something the route does not do, the
answer says so, and neither names a broken deploy.

## Performance budget

One request for the page, no JavaScript, no webfonts, inline CSS only. The
hero is the real dashboard capture: AVIF (72,812 bytes at 1920px, 39,708 at
1280px, 20,231 at 768px), then WebP (148,050 / 81,540 / 36,130 bytes), then
the PNG share-card original. A phone lays the figure out at about 360 CSS px,
so the 768w candidate is the slot a 2x screen takes: without it every phone
rounded up to 1280w and fetched 39,708 bytes to fill 722 of them, which is the
49% the 768w AVIF saves. A 3x phone (1083 device pixels) and a 1x desktop
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
against the current source with Bun 1.4.2: 11,869 bytes identity / 4,124 gzip /
3,444 brotli for the HTML, still inside the
~14 KB initial congestion window. A phone's whole visit is those 3,444 bytes
plus the 20,231-byte 768w capture, 23,675 bytes in two requests; that pair has
a ceiling of its own in the same test, next to the per-asset ones, because
each half can pass its own limit while the visit still gets heavy. The PNG
original is the one download no
srcset narrows, so it carries a ceiling of its own in the same test. The budget
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
