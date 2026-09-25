# toktop.ai

One Worker, one page, no build step.

```sh
bunx wrangler@4.126.0 deploy  # from this directory; pin, not whatever npm returns today
```

`worker.js` holds the HTML: it is a template literal, so there is nothing to
bundle. The Worker answers `/health` with `ok` for uptime checks, serves the
dashboard capture from `public/` at `/dashboard.png` and `/dashboard.webp`,
and answers every other path with the page (a one-page site should not 404
on a typo). Wrong methods are `405` with `Allow: GET, HEAD`. Image paths
without an asset binding, and 404/5xx from the asset store, are `no-store`
so a missing file is not cached as a day-long success. Error bodies are
`text/plain`, matching `/health`.

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
transfers 3,698 bytes rather than 10,027 bytes in the local Worker response test.
An explicit identity preference is respected. Refusing all available encodings
returns an uncacheable 406, including conditional requests; HEAD has no body.
Source comments
in the HTML and CSS stay in `worker.js` and are stripped before the page is
hashed, compressed, or sent. Every response carries `Vary: Accept-Encoding`,
so caches never hand a compressed body to a client that cannot decode it.

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
against the current source with Bun 1.4.2: 10,027 bytes identity / 3,698 gzip /
3,086 brotli for the HTML (previously 6,696 / 2,743 / 2,224), still inside the
~14 KB initial congestion window. The budget
is pinned by a test, so drift fails `bun test site/`; numbers above are
re-measurable with it:

```sh
bun test site/    # or `make site-check` from the repo root
```

The bun version is pinned in `.bun-version`; CI reads the same file.

Routing is by custom domain (`toktop.ai`, `www.toktop.ai`) rather than a
route pattern, so Cloudflare manages the DNS record for both names. The zone's
MX and SPF records are Namecheap's email forwarding and are left alone.
