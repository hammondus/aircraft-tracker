# aircraft-tracker

A live map and growing position archive for a personal list of aircraft, built
on crowd-sourced ADS-B networks.

Single Go binary. One dependency (`modernc.org/sqlite`); everything else —
HTTP, SSE, sessions, password hashing, templating — is standard library. The
map, its fonts and its tiles are all self-hosted, so nothing is fetched from a
CDN at runtime.

The reasoning behind every decision here, including the ones that turned out to
be wrong, is in [DESIGN-DECISIONS.md](DESIGN-DECISIONS.md).

## What it does

- Polls **adsb.lol**, **airplanes.live** and **adsb.fi**, merging the freshest
  fix per aircraft. Three networks means three independent sets of receivers.
- Shows the fleet on a MapLibre map with client-side dead reckoning, over a
  Protomaps archive of Australia served from disk.
- Distinguishes **live**, **stale** and **no contact**, because an aircraft
  nobody can hear must not look like one parked on a taxiway.
- Records every position to SQLite, forever, and replays any flight on the same
  map.

## Configuration

Two files. `fleet.json` is the aircraft list and is committed:

```json
[
  { "rego": "VH-YSO", "type": "B190", "desc": "Beech 1900C-1" }
]
```

No hex is needed — three-letter `VH-` registrations derive their ICAO address
arithmetically. Anything else needs an explicit `"hex"`.

Check the list against the Australian civil aircraft register:

```sh
./aircraft-tracker -casa fleet.json
```

It reads CASA's published CSV, fills in any missing description, and flags
anything whose description does not match the register or that is no longer
registered at all. It reports rather than rewrites: hand-written descriptions
carry popular names ("Baron", "Turbo Commander") that the register does not
have.

`config.json` holds everything else, including the password hash, and is **not**
committed. Start from `config.example.json`. Every field except the fleet has a
sensible default; the defaults are the tested path.

## Running it locally

```sh
cp config.example.json config.json
printf '%s' 'your-password' | go run . -hashpw     # paste the result into config.json
make tiles                                          # ~1 GB, see below
make run
```

Then <http://localhost:8099>.

`-hashpw` reads from stdin rather than taking a flag, because an argument would
land in your shell history and the process list. Piping it also avoids the
terminal echoing it.

## Map tiles

```sh
make tiles
```

Extracts an Australia-clipped [Protomaps](https://protomaps.com) archive
(~1 GB at zoom 14) from the planet build. It transfers only the byte ranges it
needs, not the 137 GB source, and takes a couple of minutes. Requires the
[`pmtiles`](https://github.com/protomaps/go-pmtiles) CLI.

Protomaps purges daily planet builds after about a week, so the pinned date in
the `Makefile` will eventually 404 — pick a current one from
<https://build.protomaps.com> when that happens.

## Deploying

Built for a small linux/arm64 box behind nginx proxy manager.

```sh
git clone <your-repo> && cd aircraft-tracker
cp config.example.json config.json
printf '%s' 'your-password' | docker run --rm -i aircraft-tracker -hashpw
$EDITOR config.json          # paste the hash; set the paths below
make tiles                   # or rsync the .pmtiles up from your laptop
mkdir -p history
sudo chown -R 10001:10001 history     # see below
docker compose up -d --build
```

The container runs as uid 10001, and `./history` is bind mounted, so on Linux
that directory must be writable by that user. Skip the `chown` and the recorder
cannot create its database, the process exits, and `restart: unless-stopped`
turns it into a restart loop:

```
history: /data/history/history.db: unable to open database file (14)
```

Docker Desktop on macOS maps bind-mount ownership and hides this, so it will
not reproduce on a Mac. If you would rather not `chown`, run the container as
yourself instead by adding `user: "1000:1000"` to the service.

Server `config.json`:

```json
{
  "listen": ":8099",
  "password_hash": "pbkdf2-sha256$...",
  "fleet_file": "fleet.json",
  "tiles_path": "tiles/australia.pmtiles",
  "history_path": "history/history.db"
}
```

All paths are relative to the config file itself, so these work unchanged
whether you run in Docker or directly. **Set `history_path` explicitly.** Its
default lands beside the working directory, which inside the container is `/data`
— part of the image rather than the mounted volume, so the recorder cannot write
there and exits into a restart loop.

Then point nginx proxy manager at port 8099. The app sets
`X-Accel-Buffering: no` on the event stream, so SSE works without touching the
proxy config — but if events arrive in bursts rather than steadily, set
`proxy_buffering off` on that location.

`make deploy` pulls and rebuilds on the server; `make logs` follows the output.

The container restarts unless stopped, which is the point: the archive only
grows while it is running.

## Backing up the archive

**The database is the part you cannot recreate.** Everything else is a
`git clone` and a tile download; the history only exists because the recorder
was running at the time.

Do not copy `history.db` while the container runs — WAL journalling means you
can catch a torn copy. Either stop it briefly:

```sh
docker compose stop && cp history/history.db /backup/ && docker compose start
```

or take a consistent snapshot in place:

```sh
sqlite3 history/history.db "VACUUM INTO '/backup/history-$(date +%F).db'"
```

## Development

```sh
make test      # go vet + tests
make build     # host binary
make release   # stripped static linux/arm64
```

100 tests, and they are worth reading: several encode behaviour that is easy to
"fix" back into a bug, such as why idle polling may only ever slow down and why
a date-only range end has to cover the whole day.

## A caution

This is a situational-awareness toy, not a flight-following system. Coverage
from volunteer receiver networks is patchy over Australia, and gaps are normal
rather than exceptional. An aircraft showing "no contact" very often means
nobody could hear it, not that it is on the ground.

## Attribution

Positions from [adsb.lol](https://adsb.lol), [airplanes.live](https://airplanes.live)
and [adsb.fi](https://adsb.fi) — all free community networks run on donated
receivers. Map data © [OpenStreetMap](https://www.openstreetmap.org/copyright)
contributors via [Protomaps](https://protomaps.com).
