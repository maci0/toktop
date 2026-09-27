# toktop.ai

One Worker, one page, no build step.

```
make site-deploy    # from the repo root: deploy, then wait for /health to answer ok
make site-rollback  # undo the last deploy, back to the version before it
```

Both targets pin the wrangler version (the Makefile's `WRANGLER`, read by the
target) and the bun version (`.bun-version`, the same file CI installs), so a
deploy cannot depend on whatever happens to be on the operator's PATH. Deploy
credentials come from the environment (`CLOUDFLARE_API_TOKEN`, or a `wrangler`
login already on the machine); nothing about them is written to this repo.

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
`/dashboard-1280.avif`, `/dashboard.webp` and `/dashboard-1280.webp`,
and answers every other path with the page (a one-page site should not 404
on a typo). Wrong methods are `405` with `Allow: GET, HEAD`. Image paths
without an asset binding, and 404/5xx from the asset store, are `no-store`
so a missing file is not cached as a day-long success. Error bodies are
`text/plain`, matching `/health`: a failure the asset store reports is
rewritten into that envelope, status kept, rather than passing the store's
own HTML error page through at an image path.

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
transfers 4,081 bytes rather than 11,741 bytes in the local Worker response test.
An explicit identity preference is respected. Refusing all available encodings
returns an uncacheable 406, including conditional requests; HEAD has no body.
Source comments
in the HTML and CSS stay in `worker.js` and are stripped before the page is
hashed, compressed, or sent. Every response carries `Vary: Accept-Encoding`,
so caches never hand a compressed body to a client that cannot decode it.

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
| `assets-unbound` | an image path was requested with no asset binding, so every capture is a 404 while `/health` still answers `ok` |
| `coding-dropped` | one compression format failed to build; the page is served at its uncompressed size |

Each line carries the request's `cf-ray` (empty off Cloudflare), plus the path,
method, status or reason as the event needs. That is the pivot from a failure
a visitor reports to the edge request behind it: filter Workers Logs on
`event`, then search the ray in the visitor's response headers. A `405` or a
`406` is not logged: the client did something the route does not do, the
answer says so, and neither names a broken deploy.

## Performance budget

One request for the page, no JavaScript, no webfonts, inline CSS only. The
hero is the real dashboard capture: AVIF (72,812 bytes at 1920px, 39,708 at
1280px), then WebP (148,050 / 81,540 bytes), then the PNG share-card original.
For public visitors, including mobile networks, `sizes` follows the body
gutters, figure borders, and 76rem column cap rather than declaring a desktop
width on tablets. Both formats retain their 1280w and 1920w candidates. Served from
this Worker so a deploy updates share cards and the page together.
`wrangler.jsonc` sets `run_worker_first` so those image paths hit the Worker
(cache headers, HSTS, 405s) instead of Cloudflare's asset pipeline. Measured
against the current source with Bun 1.4.2: 11,741 bytes identity / 4,081 gzip /
3,414 brotli for the HTML, still inside the
~14 KB initial congestion window. The PNG original is the one download no
srcset narrows, so it carries a ceiling of its own in the same test. The budget
is pinned by a test, so drift fails `bun test site/`; numbers above are
re-measurable with it:

```sh
bun test site/    # or `make site-check` from the repo root
```

The Worker and its tests are linted by biome (`biome.jsonc` at the repo root,
run by `make site-lint`; the version pin is the Makefile's `BIOME`, which CI
reads). The bun version is pinned in `.bun-version`; CI reads the same file.

Routing is by custom domain (`toktop.ai`, `www.toktop.ai`) rather than a
route pattern, so Cloudflare manages the DNS record for both names. The zone's
MX and SPF records are Namecheap's email forwarding and are left alone.
