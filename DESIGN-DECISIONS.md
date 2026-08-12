# Design Decisions

Live map of the Southern Airlines fleet, sourced from crowd-sourced ADS-B
networks. Internal tool, not public. Live view is the primary goal; track
history is a secondary one.

Fleet at time of writing:

| Rego | ICAO hex | Type | |
|---|---|---|---|
| VH-YSO | `7C7C16` | Beech 1900C-1 | B190 |
| VH-TAV | `7C6045` | Vulcanair P.68C | P68 |

---

## 1. Data sources: adsb.lol + airplanes.live, merged

Both are free, keyless, and serve the same JSON schema (both derive from
readsb/tar1090's `aircraft.json`), so supporting both costs one extra URL and a
merge function.

We poll **both and take the freshest fix per aircraft** (lowest `seen_pos`).
The dominant failure mode of this project is not knowing where an aircraft is
because no volunteer receiver could hear it. The two networks have different
feeder populations, so the union of their coverage is strictly better than
either alone. At a two-aircraft fleet the cost of redundancy is negligible —
one request per second to each, each well inside its own limit.

`adsb.fi` (`https://opendata.adsb.fi/api/v2/`) serves the same schema and can
be added as a third source by appending one line to the provider list.

Rejected:

- **OpenSky Network** — free tier is non-commercial only, and this is a
  commercial operator. Also coarser (5–10s) and credit-limited.
- **ADSBexchange** — commercial via RapidAPI unless you feed data. We have no
  receivers.
- **Running our own receiver** — would give unlimited 1 Hz data with no rate
  limit and would fill local coverage gaps, but only within line of sight of
  wherever it is installed. Worth revisiting if it turns out the gaps cluster
  around one base.

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

- Two aircraft need nothing faster. Staggered across two providers this
  refreshes fleet state every 5 s, and dead reckoning covers the gap.
- adsb.lol's limit is dynamic, so headroom is worth more than throughput.
- These are free community services carrying our production dependency.

### Idle polling

A two-aircraft fleet is on the ground for most of the day, and confirming that
six times a minute spends a free service's capacity to learn nothing. Polling
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
idle mid-flight, which is a real scenario for VH-TAV at low level. The cost is
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

**Poll interval is per provider**, defaulting to 5 s, rather than one global
rate. Both providers happen to need the same interval today, but they are free
services whose limits can change independently, and one should not be punished
for the other.

**Sources are staggered across the interval.** Two providers at 5 s, offset by
2.5 s, refresh fleet state every 2.5 s while neither exceeds 0.2 req/s. A
conservative per-provider interval therefore does not mean conservative
staleness — and client-side dead reckoning covers the remainder, since an
airliner travels about 500 m in 2.5 s, which interpolates cleanly from ground
speed and track.

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

### Licensing

adsb.lol data is ODbL (attribution + share-alike on derived databases).
airplanes.live has its own terms. Both are satisfied by an attribution line in
the UI footer, which we include. Neither is a problem for internal use, but
that changes if this is ever exposed publicly.

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

MapLibre GL JS and `pmtiles.js` (~10 KB, registers the `pmtiles://` protocol
handler so MapLibre can range-request into the archive) are both vendored and
served from our own origin, not a CDN.

Serving needs no work on our side: Go's `http.ServeContent` handles HTTP
`Range` correctly, which is exactly what the pmtiles protocol requires.

## 9. Layout: flat `package main`

All source in one package at the repository root, split across files by concern
(`adsb.go`, `store.go`, `sse.go`, `web.go`). The whole program is on the order
of a thousand lines. Package boundaries at this size add import ceremony and
exported-identifier noise without buying isolation, and the intent is that the
entire thing can be read end to end.

## 10. Authentication

Single shared password, exchanged for a session cookie
(`HttpOnly`, `Secure`, `SameSite=Lax`), sessions held in memory. The password
is stored as a PBKDF2 hash in the config file.

`crypto/pbkdf2` has been in the standard library since Go 1.24, so this needs
no `golang.org/x/crypto` dependency.

Handled in the application rather than as HTTP basic auth at nginx proxy
manager: basic auth is unpleasant on phones, cannot be logged out of, and
leaves no room for per-user accounts later. Sessions in memory mean a restart
logs everyone out, which is acceptable for an internal tool and avoids a
session store.

## 11. HTTP caching

Applied at the render chokepoint, per resource:

| Resource | Header | Why |
|---|---|---|
| HTML pages | `no-cache, private` | Authenticated; must revalidate so asset URLs are never stale |
| SSE stream | `no-store` | Live data, never reusable |
| `app.<hash>.js`, `app.<hash>.css` | `public, max-age=31536000, immutable` | URL names the bytes |
| `australia.<hash>.pmtiles` | `public, max-age=31536000, immutable` | Same, and it is large enough that caching matters a great deal |
| `/favicon.ico` | `max-age=3600` | Unhashed path whose content can change |

**Deviation worth flagging:** the standing rule is to hash runtime-read assets
per request so an edit without a restart cannot serve stale bytes. The
`.pmtiles` file is on the order of a gigabyte, so hashing it per request is not
viable. We hash it once at startup and re-stat it on each request, falling back
to `size-mtime` as the cache-busting token if it has changed. Tile data changes
only on a deliberate map rebuild, so the weaker guarantee is proportionate.

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
2. **This is not a flight-following system.** Operators who need dependable
   position reporting for duty-of-care use satellite trackers (Spidertracks,
   TracPlus, SkyTrac) precisely because ADS-B ground coverage in Australia is
   inadequate for the purpose. This tool is a situational-awareness display. If
   a satellite tracker is ever fitted, its feed should become the authoritative
   source and ADS-B the supplement.
3. **Inferred flights are not records.** See §7.
4. Providers may change rate limits or terms without notice; both are run as
   free community services with no SLA.
