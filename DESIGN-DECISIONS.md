# Design Decisions

Live map of a small set of aircraft, sourced from crowd-sourced ADS-B networks.
A personal, non-commercial project, behind a password and not public. Live view
is the primary goal; track history is a secondary one.

Aircraft tracked at time of writing (change freely in `config.json` -- VH-
registrations derive their ICAO address automatically, see §3):

| Rego | ICAO hex | Type | |
|---|---|---|---|
| VH-YSO | `7C7C16` | Beech 1900C-1 | B190 |
| VH-TAV | `7C6045` | Vulcanair P.68C | P68 |

---

## 1. Data sources: three networks, merged

**adsb.lol, airplanes.live and adsb.fi.** All three are free, keyless, and
serve the same JSON schema (all derive from readsb/tar1090's `aircraft.json`),
so each additional source costs one URL and nothing else.

We poll **all three and take the freshest fix per aircraft**. The dominant
failure mode of this project is not knowing where an aircraft is because no
volunteer receiver could hear it, and each network has a different feeder
population — so the union of their coverage is strictly better than any one.
Redundancy is the whole design, and it has already paid for itself twice: once
when airplanes.live blocked us, once when adsb.lol went dark.

Rejected:

- **OpenSky Network** — not the commercial clause but the operational one:
  *"Use of the REST API in any operational capacity — including … an automated
  system (even if only internal) — requires a previous written agreement."* A
  polling tracker is exactly that. Also coarser (5–10 s) and credit-limited.
- **ADSBexchange** — its Community API is paid rather than free. Worth
  revisiting if coverage proves inadequate; feeding also earns access.
- **ADSBHub** — requires feeding a station to receive anything, and serves
  TCP/SBS rather than REST.
- **Running our own receiver** — would give unlimited 1 Hz data with no rate
  limit and would fill local coverage gaps, but only within line of sight of
  wherever it is installed. The single best upgrade available, and the one that
  also unlocks feeder tiers elsewhere.

### Rate limits and poll scheduling

Both providers publish their policy. Neither advertises it in response headers,
which is what makes it easy to skip reading the documentation — a mistake that
cost this project its API access, see the incident below.

| Provider | Documented limit | Source |
|---|---|---|
| airplanes.live | "The Airplanes.live REST API is rate limited to **1 request per second**." | <https://airplanes.live/api-guide/> |
| adsb.lol | "Rate limits are **dynamic based on the environment load**." | <https://github.com/adsblol/api> |

airplanes.live also documents a cap of 1000 hex codes or 8000 characters per
query, which our fleet will never approach, and links terms of use.

adsb.lol's answer is the more interesting one: there is no fixed number to
discover, because the limit moves with their load. Any attempt to measure it
yields a number that is only true for that moment. This is the direct
justification for the adaptive cooldown below — responding to refusals is the
only correct strategy against a limit that is defined as variable.

**We poll every 10 s per provider, ten times slower than airplanes.live
permits.** That is a deliberate choice, not a measured constraint:

- A handful of aircraft need nothing faster. Staggered across three providers
  this refreshes fleet state roughly every 3 s, and dead reckoning covers the
  gap.
- adsb.lol's limit is dynamic, so headroom is worth more than throughput.
- These are free community services carrying our production dependency.

### Idle polling

A small fleet is on the ground for most of the day, and confirming that six
times a minute spends a free service's capacity to learn nothing. Polling
rate therefore follows **fleet activity first, clock second**:

| Fleet state | Time | Interval |
|---|---|---|
| Any aircraft detected within 10 min | any | **10 s** |
| Nothing detected | 20:00–12:00 UTC (day, AEST) | **2 min** |
| Nothing detected | 12:00–20:00 UTC (night, AEST) | **5 min** |

Time of day only matters while idle. An aircraft detected at 3am is tracked at
the full rate like any other, because the reason to watch it is identical.

**Being on the ground counts as detected.** An aircraft parked with its
transponder powered appears with `alt_baro: "ground"`, and that is a strong
signal of an imminent departure — exactly when the fast rate earns its keep.
The failure mode is a maintenance ground-run holding the fast rate for an hour,
which costs nothing that matters.

Estimated effect: roughly a **70% reduction** in upstream requests against a
flat 10 s poll, assuming four active hours a day.

**The cost is wake-up latency.** A departure is invisible until the next idle
poll — up to two minutes by day, five overnight. That is an accepted trade for
a situational-awareness display, not an oversight. If it ever matters, shorten
`idle_interval`; the mechanism is already there.

Coverage gaps longer than `idle_timeout` will drop an airborne aircraft back to
idle mid-flight, which is a real scenario for a light aircraft at low level in
poorly covered country. The cost is
small: the aircraft has already been invisible for ten minutes at that point,
and re-acquisition adds at most one idle interval.

The window is expressed in UTC rather than local time deliberately. An
Australian local window would shift by an hour twice a year when daylight
saving starts and stops — a silent behaviour change nobody would connect to the
clocks going back. UTC costs a moment's mental arithmetic once and never
surprises anyone.

Idle rates may only ever *slow* polling. An idle interval faster than the
active interval, or a quiet-hours interval faster than the day idle interval,
is rejected at startup rather than silently applied — otherwise "idle" becomes
a route to polling harder while nothing is happening, which is precisely
backwards. The same guard applies to failure backoff: a failing provider waits
the longer of its backoff and the current idle rate, so it can never end up
polled faster than a healthy one.

Mode transitions are logged, once, from the broadcast goroutine rather than per
provider. Several minutes of silence between polls is otherwise
indistinguishable from a wedged poller at three in the morning.

**Known consequence:** `liveFor` is 15 s, so an aircraft airborne while idle
renders as `stale` until the mode flips back — at most one idle interval, since
the first detection restores the fast rate. Raising `liveFor` to compensate
would be the wrong fix: it also bounds client-side dead reckoning, and
extrapolating a velocity vector for minutes would put an airliner kilometres
from where it actually is. Inventing position is worse than admitting
staleness.

This drove three choices:

**Poll interval is per provider**, defaulting to 10 s, rather than one global
rate. All three happen to need the same interval today, but they are free
services whose limits change independently, and one should not be punished for
another.

**Sources are staggered across the interval.** Three providers at 10 s, offset
by 3.3 s, refresh fleet state about every 3 s while none exceeds 0.1 req/s. A
conservative per-provider interval therefore does not mean conservative
staleness — and client-side dead reckoning covers the remainder, since an
airliner travels roughly 700 m in that time, which interpolates cleanly from
ground speed and track.

**Client broadcast is decoupled from upstream polling.** A separate ticker emits
snapshots to connected browsers at a steady rate regardless of what upstream
allows or how badly a provider is misbehaving. Combined with client-side dead
reckoning, the display stays smooth even if every provider degrades to a fix
every few seconds.

Failures back off exponentially from the normal interval, capped at two minutes,
honouring `Retry-After` when present — but never retrying *faster* than the
configured interval, since a provider that just refused us is not one to push
harder. A provider that fails is logged and skipped; the other carries the fleet
alone, which is the entire reason for polling two.

This was observed working under real degradation during development: while
adsb.lol was rate-limited and backing off, airplanes.live carried all tracked
aircraft with no gap; when both backed off simultaneously the affected aircraft
correctly transitioned to `stale` at 15 s and recovered on the next successful
poll.

### The adaptive cooldown

Plain backoff turned out to be insufficient, and the reason is worth recording
because it is not obvious.

A provider whose quota is depleted produces a *cycle*, not a single failure:
back off, succeed once, snap straight back to the base interval, immediately
trip the limit again. Observed against airplanes.live, which while depleted
tolerated roughly one request per 15–20 s but was being polled every 5 s. Every
recovery was followed within one interval by another 429.

Each source therefore carries a `cooldown` — an adaptive penalty added to its
interval that **persists across recovery** and decays only after
`cooldownDecayAfter` consecutive successes, and then only by half. One good
response after a rate limit is not evidence the limit has lifted; that is
precisely what the oscillation looked like.

This makes each source settle at whatever rate the provider currently tolerates
and speed back up when it relents. That matters here because the sustainable
rate is evidently not a fixed number — it varies with how much of some
longer-window budget we have already spent. A hardcoded interval cannot track
that; this can.

It is the only adaptive machinery in the program, and it exists because
measurement demanded it rather than because it seemed prudent.

### Incident: airplanes.live blocked the development IP

**On 2026-08-12, characterising the rate limits above got this project's
development IP blocked by airplanes.live.** It now returns:

```
HTTP 403 {"error": "please contact us at contact@airplanes.live"}
```

adsb.lol was unaffected and still returns 200.

**The root cause was not reading the documentation.** airplanes.live publishes
its limit plainly — 1 request per second, at
<https://airplanes.live/api-guide/> — and it was never consulted. Instead the
limit was "discovered" empirically: several hundred requests in an evening
across a coverage probe, three characterisation runs and repeated live tests,
all from one address, escalating after each refusal.

Two things follow from that, and both are worse than the wasted effort:

1. **The whole exercise was unnecessary.** The number was a documented fact,
   one page fetch away.
2. **The measurements were wrong.** The recorded conclusion was that "2 s trips
   both providers" and "5 s is the safe rate". Neither is true as a general
   fact. Those runs were measuring a state this project had itself degraded —
   an accumulating penalty and then an outright block — not the providers'
   steady-state behaviour. The documented limit is five times *faster* than
   what was concluded. Empirical numbers gathered while provoking a service
   describe the provocation, not the service.

Consequences and handling:

1. **Clearing it needs a human**, at `contact@airplanes.live`. Waiting will not
   fix it.
2. **The production server has a different IP** and is very unlikely to be
   affected. At the shipped 5 s interval it will not repeat this.
3. **The application degrades rather than fails** — adsb.lol carries the fleet
   alone. This is the redundancy in §1 doing its job, and is the second time
   during development that one provider covered for the other.

Two code changes came out of it:

- **403/401 is distinguished from 429.** A block does not clear on a two-minute
  timer, so it jumps straight to a 15 minute `blockedBackoff` and logs a
  distinct, actionable message once — rather than repeating `http 403` every
  half-minute, which reads like an ordinary rate limit.
- **The response body is captured** (truncated) into the error. `http 403`
  alone gave no hint that this was a block rather than a limit; the body said
  so plainly and was being discarded.

**If a provider's limit is ever in question again:** read their documentation
first, and treat what it says as the answer. If it is genuinely undocumented,
pick a conservative rate and let the backoff adapt — do not go looking for the
ceiling. The first refusal is a stop signal, not a data point.

### Licensing and permitted use

**This is a personal, non-commercial project.** That is not a detail: almost
every ADS-B aggregator draws its line exactly there, and the same survey gives
opposite answers depending on which side of it you sit. An attribution line is
in the UI footer regardless.

Surveyed 2026-08-13, with the verdict for **personal, non-commercial** use:

| Provider | Personal use | Documented limit | Notes |
|---|---|---|---|
| **adsb.fi** | **Yes** | 1 req/s | *"for personal, non-commercial use only"* — exactly this case. Explicitly *"compatible with the ADSBexchange v2 API"*, so it shares our schema. Uses `/icao/` for a comma-separated list. |
| **airplanes.live** | **Yes** | 1 req/s | Commercial use needs a separate arrangement ([their page](https://airplanes.live/commercial-use/) says only "contact@airplanes.live"); personal use is the ordinary case. Currently blocking this IP — see the incident below. |
| **adsb.lol** | Presumed | "dynamic based on the environment load" | No licence or permitted-use statement found anywhere on their site or README. Silence is not permission, but nothing prohibits personal use either. The README warns: *"In the future, you will require an API key which you can obtain by feeding adsb.lol."* |
| ADSBexchange | Yes, paid | per plan | Community API is *"for personal and non-commercial use"*, low-cost rather than free. |
| **OpenSky** | **No** | 400–14,400 credits/day by tier | The commercial clause stops mattering, but a second one does not: *"Use of the REST API in any operational capacity — including integration into a live product, service, or automated system (even if only internal) — requires a previous written agreement, even for non-profit or governmental entities."* A polling tracker is exactly that. Their licence also grants use *"solely for the purpose of non-profit research and non-profit education"*, which hobby tracking is not. |
| ADSBHub | N/A | — | Requires feeding at least one station to receive anything, and serves TCP/SBS rather than REST. |

**OpenSky is the trap here.** The obvious reading — "it's non-commercial now, so
OpenSky is fine" — is wrong. Its operational-automation clause is independent
of commerciality and catches any automated poller.

An earlier version of this document asserted adsb.lol data was ODbL. That was
not verified and has been removed; the licence should be confirmed with them
rather than assumed.

We poll **adsb.lol, airplanes.live and adsb.fi**, all three, staggered. They
share a schema, so a third source costs one line and buys a third independent
receiver network — which is the only real defence against both the coverage
gaps in §13 and a single provider going dark.

**Actions outstanding:**

1. Email `contact@airplanes.live` to clear the IP block recorded below.
2. **Consider feeding.** A receiver would give unmetered local data with no
   rate limit at all, unlock feeder tiers (ADSBexchange grants feeders API
   access; adsb.lol intends to), and improve coverage exactly where you are —
   addressing the roadmap risk and §13's coverage limitation together.

## 2. Query by hex, not by geography

The obvious approach — poll a radius around Australia — is wrong here. The
providers cap radius queries at 250 nm, so covering the continent would need
dozens of stitched circles and would blow the rate limit.

Instead `/v2/hex/<comma-separated list>` takes the whole fleet in one request
and **is not geographically constrained** — verified with a single request that
returned aircraft near Sydney and Melbourne together. One request per second
per provider covers the fleet anywhere on earth, forever, regardless of how
many aircraft are added.

Consequence: no viewport tracking, no circle stitching, no per-client
rate-limit budgeting. All of that complexity is designed out rather than
solved.

## 3. Registration → hex is computed, not looked up

Australian VH- registrations map algorithmically onto ICAO 24-bit addresses:

```
hex = 0x7C0000 + (L1 * 1296) + (L2 * 36) + L3     where A=0 … Z=25
```

Verified against five aircraft (VH-BYG, VH-YID, VH-PVQ observed live;
VH-YSO, VH-TAV confirmed against adsbdb.com).

This matters because `/v2/registration/{reg}` is a **live** query — it only
answers while the aircraft is airborne and being received. Deriving the hex
offline means the fleet config can be written at any time, including for an
aircraft that is on the ground or newly acquired.

Limits: valid only for three-letter VH- registrations. Foreign-registered or
numeric registrations need a manual entry in `fleet.json`.

Static metadata (type, manufacturer, owner) comes from **adsbdb.com**, used
once by hand when adding an aircraft — not at runtime. We do not want a third
service in the live path for data that never changes.

## 4. "Not currently visible" is a first-class state

An aircraft the network cannot hear is simply absent from the API response.
Because our fleet is small and known, the UI always renders every aircraft, in
one of three states:

- **live** — fix newer than ~15 s, drawn normally and dead-reckoned
- **stale** — last fix 15 s to ~15 min old, ghosted at its last known position
  with the age shown
- **no contact** — nothing today, listed in the sidebar only

This is the most important UI decision in the project. A map that silently
omits aircraft it cannot hear looks identical to a map showing an aircraft
parked at its last known position, and for an ops display that is actively
misleading. The distinction must be visible at a glance.

For the same reason the UI shows whether a fix was **MLAT-derived** (the `mlat`
array is non-empty). MLAT positions are multilaterated from receiver timing
rather than reported by the aircraft; they are less accurate and only occur
where receiver density is high.

## 5. Client-side dead reckoning, with a hard cutoff

At a 1 Hz poll a Beech 1900 at 250 kt moves ~130 m between updates, which reads
as visible teleporting. The browser extrapolates each aircraft from its last
fix using ground speed and track on every animation frame, snapping to truth
when a new fix arrives.

**Extrapolation stops after 15 seconds.** Past that the aircraft is drawn as
stale at its last real position. An aircraft confidently gliding across the map
on a stale velocity vector is worse than one that visibly stops — it invents
data. This is a deliberate choice to look less smooth in exchange for never
lying.

## 6. SSE, not WebSocket; full state, not deltas

Traffic is one-directional server → client, which is exactly what Server-Sent
Events model. `EventSource` gives automatic reconnection for free, it is plain
HTTP so it passes through nginx proxy manager unmodified, and the server side
is a few dozen lines with no dependency. A WebSocket would buy bidirectionality
we do not need.

One deployment note: nginx needs `proxy_buffering off` on the SSE location, or
events queue invisibly until the buffer fills.

We send **the entire fleet state on every tick**, not deltas. Two aircraft is
well under a kilobyte. A delta protocol would add reconnection edge cases and
client-side state reconciliation to save bandwidth that does not need saving.
Revisit only if the fleet reaches a few hundred aircraft, which it will not.

## 7. History: append-only JSONL

One file per UTC day, one JSON object per line, in `history/YYYY-MM-DD.jsonl`.

Chosen over SQLite because at this volume the query advantage does not pay for
the dependency. Two aircraft flying a full day at one sample per five seconds
is well under a megabyte a day; a year fits comfortably in a few hundred
megabytes. The files are greppable, trivially backed up, survive a corrupted
write with the loss of one line, and need no schema migration.

The writer sits behind a small interface so that if reporting requirements grow
teeth — utilisation by aircraft, by month, across years — replaying the files
into SQLite is an afternoon's work and loses nothing. `modernc.org/sqlite`
would be the choice there: pure Go, so it still cross-compiles to linux/arm64
without cgo.

History is sampled at **one fix per 5 seconds**, not the full 1 Hz. Track
replay does not benefit from finer resolution and it cuts storage fivefold.

### Flight segmentation

Raw position streams are nearly useless to query, so the writer segments them
into flights: a flight closes after the aircraft has been on the ground, or out
of contact, for more than 10 minutes.

**Known limitation:** a coverage gap and a completed flight look identical from
the data. An aircraft that flies out of receiver range for 15 minutes will be
recorded as having landed and then departed again. Flights are therefore
labelled as inferred, and are not suitable as a source of truth for duty or
maintenance records.

## 8. Map: MapLibre GL JS + self-hosted Protomaps

MapLibre (BSD, no account, no token) over a Protomaps `.pmtiles` extract clipped
to an Australia bounding box, range-served from disk by our own binary. No
external tile service, no API key, no per-request dependency on anyone else.

Rejected: OSM's public tile server (its usage policy prohibits application
traffic), and hosted providers such as MapTiler (adds a key and an external
dependency for something we can serve ourselves).

Leaflet was rejected because it uses DOM markers and CSS transforms for
rotation; MapLibre rotates icons declaratively by track angle
(`icon-rotate: ['get', 'track']`) and the smooth per-frame position updates
that dead reckoning needs are much cheaper on a GPU-composited layer.

### Extract parameters

Built with `make tiles`, which pins all of the following:

- **Source:** `https://build.protomaps.com/20260810.pmtiles` (137 GB planet).
  `pmtiles extract` reads the tile directory and range-requests only the byte
  ranges it needs, so the extract transfers ~1 GB, not 137 GB.
- **Bounding box:** `112.9,-43.7,153.7,-10.6` — mainland plus Tasmania.
  Excludes Lord Howe, Norfolk, Christmas and Cocos; widen to
  `96,-44,169,-9` if the fleet ever operates to any of them. Ocean tiles are
  nearly empty in vector data, so the extra area costs far less than its size
  suggests.
- **Max zoom: 14**, giving individual taxiways — enough to see which apron an
  aircraft is parked on.

Measured archive sizes for this bbox, roughly doubling per level: z11 108 MB,
z12 234 MB, z13 506 MB, **z14 1.0 GB**.

**Protomaps purges daily planet builds after about a week.** The date above
will 404 before long. That is fine — the extract only needs rebuilding when we
want fresher map data, and `make tiles` is where the date is recorded so it can
be bumped deliberately rather than rediscovered.

### Style

A `.pmtiles` archive is vector geometry with no styling; MapLibre also needs a
style JSON. Generated once with `npx protomaps-themes-base` and committed, so
the build has no ongoing node dependency.

We use a muted/dark base and let the aircraft carry all the colour. On an ops
display a full-colour street map underneath the traffic is noise.

### Client-side vendoring

Everything the map needs is served from our own origin — MapLibre, pmtiles.js,
the Protomaps sprites, and the Noto Sans glyph ranges. About 2.4 MB embedded in
the binary. An internal ops display must keep working when a third party is
down, and must not tell anyone else who is looking at it.

Vendored assets live under **version-pinned** directories
(`/vendor/maplibre-gl@6.3.0/…`) rather than content-hashed filenames. MapLibre's
ESM build imports `./maplibre-gl-shared.mjs` relatively, and a hashed filename
would break that import. The version in the path is the cache key instead:
upgrading means a new directory, so the URL still changes with the bytes. Our
own `app.css`/`app.js`, which change constantly, keep content hashing.

Serving needs no work on our side: Go's `http.ServeContent` handles HTTP
`Range` correctly, which is exactly what the pmtiles protocol requires.

### Things that only surfaced in a browser

Recorded because each cost real time and none is discoverable from the Go side:

- **MapLibre v6 is ESM-only with named exports and no default.** It is a
  namespace import, not a default one.
- **It loads `maplibre-gl-worker.mjs` as a third file.** Missing it stalls tile
  parsing with no console error — the map simply never fires `load`, and
  because the client awaits that before connecting the SSE stream, the fleet
  panel silently never populates either. One missing file, two symptoms that
  look unrelated.
- **Sprite and glyph URLs must be absolute**, with scheme and host. The server
  cannot know its own public origin behind a proxy, so the client prefixes
  `location.origin`. Use string concatenation, not `new URL()` — the glyphs
  path contains `{fontstack}` and `{range}` placeholders that MapLibre matches
  literally, and URL parsing percent-encodes the braces.
- **The aircraft icon must be registered with `sdf: true`**, or `icon-color`
  silently does nothing and every aircraft draws the same colour.
- **`maxBounds` constrains the whole viewport, not the centre.** A box tight
  around Australia cannot be satisfied at a zoom where the viewport is wider
  than the box, so MapLibre clamps the centre — which silently undid the
  panel padding and pinned `setCenter` near the middle of the box, so the
  easternmost aircraft rendered underneath the panel and clicking to follow did
  nothing. The bounds are now deliberately generous: wide enough to stop anyone
  wandering to Europe, never tight enough to bind at Australian zoom levels.
- **First paint is slow.** Reading the pmtiles directory out of a 1 GB archive
  takes several seconds before anything renders. Not a bug, but long enough to
  look like one.

### Following an aircraft

Clicking a fleet row centres the map on that aircraft, offset by half the panel
width so it lands in the visible half rather than under the panel.

While following, the map is only nudged when the aircraft drifts out of the
middle 60% of the usable area. Recentring every frame would fight the easing
started by the click, and would mean the map never sits still even when the
aircraft has barely moved. Dragging the map cancels following — panning by hand
means you want to look somewhere else.

## 9. Layout: flat `package main`

All source in one package at the repository root, split across files by concern
(`adsb.go`, `store.go`, `sse.go`, `web.go`). The whole program is on the order
of a thousand lines. Package boundaries at this size add import ceremony and
exported-identifier noise without buying isolation, and the intent is that the
entire thing can be read end to end.

## 10. Authentication

Single shared password, exchanged for a session cookie
(`HttpOnly`, `Secure`, `SameSite=Lax`), sessions held in memory.

`crypto/pbkdf2` has been in the standard library since Go 1.24, so this needs
no `golang.org/x/crypto` dependency. 600,000 iterations, OWASP's current
guidance for PBKDF2-SHA256. That cost is paid once per login, not per request —
the cookie carries every request after it — so it is invisible to users and
expensive for anyone guessing.

Hashes are self-describing, `pbkdf2-sha256$<iterations>$<salt>$<key>`, so the
iteration count can be raised later without invalidating hashes already sitting
in config files.

Handled in the application rather than as HTTP basic auth at nginx proxy
manager: basic auth is unpleasant on phones, cannot be logged out of, and
leaves no room for per-user accounts later. Sessions in memory mean a restart
logs everyone out, which is acceptable for an internal tool and avoids a
session store.

### Setup

```sh
printf '%s' 'your-password' | ./aircraft-tracker -hashpw
```

Stdin rather than a flag, because an argument lands in shell history and in the
process list. This does not disable terminal echo — doing so without a
dependency means shelling out to `stty`, which is not worth it for a one-off
local command — so pipe the password rather than typing it at a prompt.

**An empty `password_hash` refuses to start** unless `-insecure` is also
passed. An accidentally unauthenticated deployment should not be one missing
config key away, and the error message says exactly how to fix it.

### Details worth knowing

- **Sessions slide.** Expiry is extended on use, so a display left open on the
  ops wall does not sign itself out mid-shift, while an abandoned session still
  lapses after `session_ttl` (default 7 days). Expired entries are swept on the
  next login rather than by a background timer — logins are rare and the map is
  tiny.
- **Concurrent password verification is capped at two.** PBKDF2 is deliberately
  expensive, which makes an unbounded login endpoint a way to exhaust the CPU.
  The same cap throttles brute force to a few attempts a second.
- **`Secure` is set from `X-Forwarded-Proto`.** Behind nginx proxy manager the
  connection to us is plain HTTP, so the proxy's header is the only evidence
  the client used TLS. Trusting a client-settable header is safe only because
  this always sits behind a proxy that overwrites it; exposed directly, the
  worst outcome is a cookie marked `Secure` that need not be.
- **Redirect versus status.** A browser navigating to a protected page is
  redirected to `/login`; anything else — `EventSource`, a tile request — gets
  `401`. Sending an HTML login page to a caller expecting `text/event-stream`
  produces a confusing body where a status code would be actionable. The client
  uses exactly this to detect an expired session and reload.
- **`?next=` cannot leave the site.** Only a local absolute path is honoured,
  so the login page is not an open redirect.

## 11. HTTP caching

Applied at the render chokepoint, per resource:

| Resource | Header | Why |
|---|---|---|
| HTML pages | `no-cache, private` | Authenticated; must revalidate so asset URLs are never stale |
| SSE stream | `no-store` | Live data, never reusable |
| `app.<hash>.js`, `app.<hash>.css` | `public, max-age=31536000, immutable` | URL names the bytes |
| `australia.<hash>.pmtiles` | `public, max-age=31536000, immutable` | Same, and it is large enough that caching matters a great deal |
| `/favicon.ico` | `max-age=3600` | Unhashed path whose content can change |

The default is set by one middleware wrapping the whole mux, rather than per
handler. Handlers needing something stronger set their own `Cache-Control`,
which replaces it — so the strict cases are opt-in and visible at the call site.

**CSS and JS are embedded** with `go:embed` and served from URLs containing a
hash of their own content (`/static/app.<hash>.css`). Hashing once at startup
is correct *because* they are embedded: the bytes are fixed at build time and
cannot change under a running process.

**Deviation worth flagging:** the standing rule is to hash runtime-read assets
per request so an edit without a restart cannot serve stale bytes. The
`.pmtiles` archive is a gigabyte, so hashing it per request is not viable.
Instead its URL carries a `size-mtime` token, re-stat'ed on each page render.
Tile data changes only on a deliberate `make tiles`, which changes both, so the
weaker guarantee is proportionate.

Range requests matter here: MapLibre reads the archive by byte range, and
serving it without range support would mean shipping a gigabyte per tile.
`http.ServeContent` implements this for us, which is why the tile handler is
about fifteen lines.

## 12. API schema gotchas

Handled explicitly because each one breaks naive decoding:

- **`alt_baro` is a number in flight but the string `"ground"` on the
  surface.** Requires a custom `UnmarshalJSON`; a plain `int` field fails.
- `flight` is space-padded to 8 characters (`"BYG     "`) — trim it.
- `seen_pos` is the age of the position fix in seconds, and is the freshness
  signal the whole UI depends on. `seen` is the age of *any* message and is not
  the same thing.
- `now` in the response envelope is milliseconds, not seconds.
- An aircraft can appear in the response with no `lat`/`lon` at all (heard, but
  not positionally resolved). Always check before plotting.

## 13. Known limitations

Recorded so nobody has to rediscover them:

1. **Coverage is not guaranteed.** These are volunteer receiver networks with
   line-of-sight reception. Coverage over Australia is good near capital cities
   and along airline routes at altitude, and poor over inland and regional
   areas at low level. VH-TAV in particular will drop out if it operates at low
   level away from population centres. There is no software fix.
2. **This is not a flight-following system**, and must not be relied on as
   one. Coverage gaps are normal, not exceptional. Operators who need
   dependable position reporting use satellite trackers (Spidertracks, TracPlus,
   SkyTrac) precisely because ADS-B ground coverage in Australia is inadequate
   for the purpose.
3. **Inferred flights are not records.** See §7.
4. Providers may change rate limits or terms without notice; both are run as
   free community services with no SLA.
5. **A provider outage is indistinguishable from a quiet sky.** Observed on
   2026-08-13: adsb.lol returned `HTTP 200` with an empty aircraft array for
   every query worldwide — Sydney, London, New York, military — for a sustained
   period. Because we query by hex, an empty response is exactly what a parked
   fleet looks like, so the UI would have shown "no contact" with complete
   confidence while the data source was simply down.

   This is the same class of problem as §4's visibility states, and matters for
   the same reason: on an ops display, "no contact" has to be trustworthy. The
   fix, if it proves necessary, is a canary — periodically query somewhere
   guaranteed busy (a radius over a capital city, or `/v2/mil`) and treat a
   zero result as *provider unhealthy* rather than *nothing flying*, so the UI
   can say so. Not built yet; recorded so the failure is recognised rather
   than rediscovered.
