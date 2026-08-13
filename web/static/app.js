// MapLibre v6 ships ESM with named exports and no default, so this is a
// namespace import rather than a default one.
import * as maplibregl from "/vendor/maplibre-gl@6.3.0/maplibre-gl.mjs";

// Freshness thresholds. liveFor mirrors the server's constant of the same name
// and bounds extrapolation: past it we stop moving an aircraft rather than
// invent a position for it. See DESIGN-DECISIONS.md §5.
const LIVE_FOR_MS = 15_000;

const body = document.body;
const conn = document.getElementById("conn");
const listItems = new Map();
for (const li of document.querySelectorAll("#fleet li")) {
  listItems.set(li.dataset.hex, li);
}

// state per aircraft: the last real fix plus when we received it, which is all
// dead reckoning needs.
const fleet = new Map();
let follow = null; // hex the map is tracking, or null

// ---------------------------------------------------------------- formatting

const pad3 = (n) => String(Math.round(n)).padStart(3, "0");

function describe(s) {
  if (!s.fix) return "";
  const f = s.fix;
  const alt = f.on_ground ? "ground" : `${f.alt_ft.toLocaleString()} ft`;
  return `${alt} · ${Math.round(f.speed_kt)} kt · ${pad3(f.track_deg)}°`;
}

// Ages are coarse on purpose: a number ticking every second draws the eye to
// the least important thing on screen. What matters is live/stale, and the
// status word already says that.
function age(seconds) {
  if (seconds < 60) return `${Math.round(seconds)}s ago`;
  if (seconds < 3600) return `${Math.round(seconds / 60)}m ago`;
  return `${Math.round(seconds / 3600)}h ago`;
}

// --------------------------------------------------------------- dead reckoning

// Advance a fix along its ground track. Aircraft move in straight lines over
// the seconds involved, so a great-circle step is unnecessary precision; the
// only correction that matters is that a degree of longitude shrinks with
// latitude.
function extrapolate(fix, elapsedMs) {
  if (fix.on_ground || !fix.speed_kt) return [fix.lon, fix.lat];
  const metres = (fix.speed_kt * 1852 / 3600) * (elapsedMs / 1000);
  const rad = (fix.track_deg * Math.PI) / 180;
  const dLat = (metres * Math.cos(rad)) / 111_320;
  const dLon = (metres * Math.sin(rad)) / (111_320 * Math.cos((fix.lat * Math.PI) / 180));
  return [fix.lon + dLon, fix.lat + dLat];
}

// position returns where to draw an aircraft now, and whether that position is
// extrapolated. Extrapolation stops at LIVE_FOR_MS: an aircraft gliding across
// the map on a stale velocity vector is inventing data, which is worse than one
// that visibly stops.
function position(entry, now) {
  const elapsed = now - entry.receivedAt;
  if (entry.status !== "live" || elapsed > LIVE_FOR_MS) {
    return [entry.fix.lon, entry.fix.lat];
  }
  return extrapolate(entry.fix, elapsed);
}

function features(now) {
  const out = [];
  for (const [hex, entry] of fleet) {
    if (!entry.fix) continue;
    out.push({
      type: "Feature",
      geometry: { type: "Point", coordinates: position(entry, now) },
      properties: {
        hex,
        rego: entry.rego,
        status: entry.status,
        reference: Boolean(entry.reference),
        watched: Boolean(entry.watched),
        track: entry.fix.track_deg || 0,
        onGround: Boolean(entry.fix.on_ground),
      },
    });
  }
  return { type: "FeatureCollection", features: out };
}

// -------------------------------------------------------------------- the map

const bounds = body.dataset.bounds.split(",").map(Number);

const panelWidth = () => document.getElementById("panel")?.offsetWidth ?? 0;

// MapLibre v6 rejects root-relative sprite and glyph URLs -- they must carry a
// scheme and host. The template emits paths, so make them absolute here rather
// than teaching the server its own public origin, which it cannot know behind a
// proxy.
// Plain concatenation, not new URL(): the glyphs path contains {fontstack} and
// {range} placeholders that MapLibre matches literally, and URL parsing would
// percent-encode the braces.
const absolute = (p) => (p.startsWith("/") ? location.origin + p : p);

async function buildStyle() {
  const layers = await fetch(body.dataset.layers).then((r) => r.json());
  return {
    version: 8,
    glyphs: absolute(body.dataset.glyphs),
    sprite: absolute(body.dataset.sprite),
    sources: {
      protomaps: {
        type: "vector",
        url: `pmtiles://${absolute(body.dataset.tiles)}`,
        attribution: '© <a href="https://www.openstreetmap.org/copyright">OpenStreetMap</a>',
      },
    },
    layers,
  };
}

async function initMap() {
  // Register the pmtiles:// scheme so MapLibre range-requests into the archive
  // instead of asking for individual tile URLs.
  maplibregl.addProtocol("pmtiles", new pmtiles.Protocol().tile);

  const map = new maplibregl.Map({
    container: "map",
    style: await buildStyle(),
    bounds: [[bounds[0], bounds[1]], [bounds[2], bounds[3]]],
    // The panel floats over the map, so pad the initial fit by its width --
    // otherwise the east coast, where the fleet actually operates, opens up
    // hidden underneath it.
    fitBoundsOptions: { padding: { top: 20, bottom: 20, left: 20, right: panelWidth() + 20 } },
    // Generous, on purpose. maxBounds constrains the whole viewport, so a
    // tight box cannot be satisfied at a zoom where the viewport is wider than
    // the box -- MapLibre then clamps the centre, silently undoing the panel
    // padding above and pinning setCenter near the middle of the box. Wide
    // enough to stop anyone wandering to Europe, never tight enough to bind at
    // Australian zoom levels.
    maxBounds: [[bounds[0] - 20, bounds[1] - 20], [bounds[2] + 20, bounds[3] + 20]],
    maxZoom: 14,
    attributionControl: false,
  });
  map.addControl(new maplibregl.NavigationControl({ showCompass: false }), "top-left");
  map.addControl(new maplibregl.ScaleControl({ unit: "nautical" }), "bottom-left");

  await new Promise((resolve) => map.on("load", resolve));

  map.addSource("fleet", { type: "geojson", data: features(Date.now()) });

  // sdf: true is what makes icon-color work below -- without it MapLibre draws
  // the image as-is and every aircraft is the same colour, silently.
  map.addImage("aircraft", aircraftIcon(), { pixelRatio: 2, sdf: true });

  map.addLayer({
    id: "fleet-trail",
    type: "circle",
    source: "fleet",
    filter: ["all", ["==", ["get", "status"], "stale"], ["!", ["get", "reference"]]],
    paint: {
      "circle-radius": 14,
      "circle-color": ["case", ["get", "watched"], "#c084fc", "#fbbf24"],
      "circle-opacity": 0.12,
    },
  });
  map.addLayer({
    id: "fleet-icon",
    type: "symbol",
    source: "fleet",
    layout: {
      "icon-image": "aircraft",
      "icon-rotate": ["get", "track"],
      "icon-rotation-alignment": "map",
      "icon-allow-overlap": true,
      "icon-size": 0.9,
    },
    paint: {
      // Status must be legible at a glance: an aircraft nobody can hear cannot
      // look like one being tracked.
      // Three families, deliberately far apart in hue so which is which is
      // readable without a legend:
      //
      //   fleet    green when live, amber when stale  -- yours
      //   watched  violet                             -- added by hand, this session
      //   reference muted grey                        -- scenery proving the feed works
      //
      // Watched aircraft are bright rather than muted: you typed a registration
      // in order to look at it, so it should be the easiest thing on the map to
      // find. Only its brightness varies with freshness, not its hue, or it
      // would be mistaken for one of yours.
      "icon-color": [
        "case",
        ["get", "watched"], "#c084fc",
        ["get", "reference"], "#64748b",
        ["match", ["get", "status"],
          "live", "#4ade80",
          "stale", "#fbbf24",
          "#64748b"],
      ],
      "icon-opacity": [
        "case",
        ["get", "watched"], ["match", ["get", "status"], "live", 1, "stale", 0.8, 0.5],
        ["get", "reference"], 0.5,
        ["match", ["get", "status"], "no_contact", 0.45, 1],
      ],
    },
  });
  map.addLayer({
    id: "fleet-label",
    type: "symbol",
    source: "fleet",
    layout: {
      "text-field": ["get", "rego"],
      "text-font": ["Noto Sans Medium"],
      "text-size": 12,
      "text-offset": [0, 1.4],
      "text-anchor": "top",
      "text-allow-overlap": true,
    },
    paint: {
      "text-color": [
        "case",
        ["get", "watched"], "#e9d5ff",
        ["get", "reference"], "#8b98a8",
        "#dfe6ee",
      ],
      "text-halo-color": "#10141a",
      "text-halo-width": 1.5,
    },
  });

  // History track: a wide translucent line under a narrow bright one, which
  // reads clearly against a dark basemap without needing a glow filter.
  map.addSource("track", { type: "geojson", data: emptyGeoJSON() });
  map.addLayer({
    id: "track-halo", type: "line", source: "track",
    layout: { "line-cap": "round", "line-join": "round" },
    paint: { "line-color": "#38bdf8", "line-width": 7, "line-opacity": 0.18 },
  });
  map.addLayer({
    id: "track-line", type: "line", source: "track",
    layout: { "line-cap": "round", "line-join": "round" },
    paint: { "line-color": "#38bdf8", "line-width": 2 },
  });
  map.addSource("cursor", { type: "geojson", data: emptyGeoJSON() });
  map.addLayer({
    id: "cursor-icon", type: "symbol", source: "cursor",
    layout: {
      "icon-image": "aircraft", "icon-rotate": ["get", "track"],
      "icon-rotation-alignment": "map", "icon-allow-overlap": true, "icon-size": 0.9,
    },
    paint: { "icon-color": "#e2f2ff" },
  });

  map.on("click", "fleet-icon", (e) => selectAircraft(e.features[0].properties.hex, map));
  map.on("mouseenter", "fleet-icon", () => (map.getCanvas().style.cursor = "pointer"));
  map.on("mouseleave", "fleet-icon", () => (map.getCanvas().style.cursor = ""));
  // Panning by hand means you want to look somewhere else; stop chasing.
  map.on("dragstart", () => setFollow(null));

  return map;
}

// A simple arrowhead, drawn once into a canvas and registered as an SDF so
// MapLibre can tint it per status. Drawing our own avoids vendoring a sprite
// sheet for a single symbol.
function aircraftIcon() {
  const size = 40;
  const c = document.createElement("canvas");
  c.width = c.height = size;
  const ctx = c.getContext("2d");
  ctx.fillStyle = "#fff";
  ctx.beginPath();
  ctx.moveTo(size / 2, 4);
  ctx.lineTo(size - 8, size - 6);
  ctx.lineTo(size / 2, size - 14);
  ctx.lineTo(8, size - 6);
  ctx.closePath();
  ctx.fill();
  return ctx.getImageData(0, 0, size, size);
}

const emptyGeoJSON = () => ({ type: "FeatureCollection", features: [] });

// ------------------------------------------------------------------ selection

function setFollow(hex) {
  follow = hex;
  for (const [h, li] of listItems) li.classList.toggle("selected", h === follow);
}

function selectAircraft(hex, map) {
  const entry = fleet.get(hex);
  if (!entry?.fix) return;
  setFollow(hex);
  // Offset the centre so the aircraft lands in the visible half rather than
  // under the panel.
  map.easeTo({
    center: position(entry, Date.now()),
    zoom: Math.max(map.getZoom(), 8),
    offset: [-panelWidth() / 2, 0],
  });
}

// keepInView nudges the map only when the followed aircraft drifts out of the
// comfortable middle of the visible area.
//
// Recentring every frame would be both jittery and wrong: it fights the easeTo
// started by a click, and it means the map never sits still even when the
// aircraft has barely moved.
function keepInView(map, lonLat) {
  const p = map.project(lonLat);
  const { width, height } = map.getCanvas().getBoundingClientRect();
  // The panel floats over the right of the map, so the usable area stops there.
  const usableRight = width - panelWidth();
  const marginX = usableRight * 0.2;
  const marginY = height * 0.2;
  const outside =
    p.x < marginX || p.x > usableRight - marginX || p.y < marginY || p.y > height - marginY;
  if (outside) map.easeTo({ center: lonLat, duration: 600 });
}

// ---------------------------------------------------------------------- feed

function applySnapshot(states) {
  const now = Date.now();
  for (const s of states) {
    // Watched aircraft have no server-rendered row, so make one on first sight.
    const li = s.watched ? ensureRow(s) : listItems.get(s.hex);
    if (!li) continue; // fleet changed under a stale tab

    const prev = fleet.get(s.hex);
    // Only restamp when the fix itself is new, or extrapolation would restart
    // from zero on every broadcast and the aircraft would crawl.
    const isNew = !prev || !prev.fix || prev.fix.at !== s.fix?.at;
    fleet.set(s.hex, {
      rego: s.rego,
      reference: Boolean(s.reference),
      watched: Boolean(s.watched),
      status: s.status,
      fix: s.fix ?? null,
      ageSec: s.age_sec ?? 0,
      receivedAt: isNew ? now - (s.age_sec ?? 0) * 1000 : prev.receivedAt,
    });

    li.dataset.status = s.status;
    li.querySelector(".status").textContent =
      s.status === "no_contact" ? "no contact" : s.status;
    li.querySelector(".detail").textContent = s.fix
      ? `${describe(s)} · ${age(s.age_sec ?? 0)}`
      : "";
  }
}

function connect() {
  const es = new EventSource("/events");
  es.addEventListener("open", () => {
    conn.dataset.state = "open";
    conn.textContent = "live";
  });
  es.addEventListener("fleet", (e) => {
    try {
      applySnapshot(JSON.parse(e.data));
    } catch (err) {
      console.error("bad fleet payload", err);
    }
  });
  es.addEventListener("error", () => {
    // EventSource retries by itself using the server's hint. The exception is
    // an expired session: /events answers 401 rather than HTML, and no amount
    // of retrying fixes that, so send the user to sign in.
    if (es.readyState === EventSource.CLOSED) {
      conn.dataset.state = "lost";
      conn.textContent = "signed out";
      location.href = "/login?next=" + encodeURIComponent(location.pathname);
      return;
    }
    conn.dataset.state = "lost";
    conn.textContent = "reconnecting";
  });
}

// ---------------------------------------------------------------------- start

const map = await initMap();

for (const [hex, li] of listItems) {
  li.querySelector(".row").addEventListener("click", () => selectAircraft(hex, map));
}

connect();

// Redraw every frame so extrapolated aircraft move smoothly rather than
// stepping once per broadcast.
function frame() {
  const now = Date.now();
  map.getSource("fleet")?.setData(features(now));
  if (follow) {
    const entry = fleet.get(follow);
    if (entry?.fix && entry.status === "live") keepInView(map, position(entry, now));
  }
  requestAnimationFrame(frame);
}
requestAnimationFrame(frame);

// ==================================================================== history
//
// The history view reuses the same map rather than living on its own page:
// seeing an old track against the current basemap, and against live traffic, is
// most of the point.

const modeButtons = document.querySelectorAll(".modes button");
const historyStatus = document.getElementById("h-status");
const flightList = document.getElementById("flights");
const pickAircraft = document.getElementById("h-aircraft");
const pickRange = document.getElementById("h-range");
const scrubber = document.getElementById("scrubber");
const slider = document.getElementById("slider");
const playButton = document.getElementById("play");
const readout = document.getElementById("readout");

let track = [];      // points of the selected flight
let playing = null;  // interval handle while replaying

const fmtWhen = (iso) =>
  new Date(iso).toLocaleString([], {
    day: "numeric", month: "short", year: "numeric", hour: "2-digit", minute: "2-digit",
  });
const fmtClock = (iso) =>
  new Date(iso).toLocaleTimeString([], { hour: "2-digit", minute: "2-digit", second: "2-digit" });

function fmtDuration(ms) {
  const mins = Math.round(ms / 60000);
  return mins < 60 ? `${mins}m` : `${Math.floor(mins / 60)}h ${String(mins % 60).padStart(2, "0")}m`;
}

function setMode(mode) {
  body.dataset.mode = mode;
  for (const b of modeButtons) {
    const on = b.dataset.mode === mode;
    b.classList.toggle("active", on);
    b.setAttribute("aria-selected", String(on));
  }
  // Live aircraft stay visible in history mode but recede, so a track can be
  // read against where the fleet is now without the two competing.
  const dim = mode === "history" ? 0.35 : 1;
  for (const id of ["fleet-icon", "fleet-label"]) {
    if (!map.getLayer(id)) continue;
    map.setPaintProperty(id, id === "fleet-icon" ? "icon-opacity" : "text-opacity", dim);
  }
  if (mode === "live") clearTrack();
  else if (!flightList.childElementCount) loadFlights();
}

for (const b of modeButtons) b.addEventListener("click", () => setMode(b.dataset.mode));
pickAircraft.addEventListener("change", loadFlights);
pickRange.addEventListener("change", loadFlights);

async function loadFlights() {
  historyStatus.hidden = false;
  historyStatus.textContent = "Loading…";
  flightList.replaceChildren();

  const params = new URLSearchParams();
  if (pickAircraft.value) params.set("hex", pickAircraft.value);
  const days = Number(pickRange.value);
  if (days > 0) {
    const from = new Date(Date.now() - days * 86400_000);
    params.set("from", from.toISOString());
  } else {
    params.set("from", "2000-01-01");
  }

  let data;
  try {
    const r = await fetch(`/api/flights?${params}`);
    if (r.status === 401) return signOut();
    if (!r.ok) throw new Error(await r.text());
    data = await r.json();
  } catch (err) {
    historyStatus.textContent = `Could not load flights: ${err.message}`;
    return;
  }

  if (!data.flights.length) {
    // Worth being specific: an empty archive is the normal state on day one,
    // and looks identical to a broken query if we only say "no flights".
    historyStatus.textContent =
      "No flights recorded for this period. History starts when the recorder does.";
    return;
  }
  historyStatus.hidden = true;

  for (const f of data.flights) {
    const li = document.createElement("li");
    const b = document.createElement("button");
    b.type = "button";
    b.className = "flight";
    b.innerHTML =
      `<span class="when"><span class="rego"></span><span class="date"></span></span>` +
      `<span class="stats"></span>`;
    b.querySelector(".rego").textContent = f.rego || f.hex;
    b.querySelector(".date").textContent = fmtWhen(f.started);
    b.querySelector(".stats").textContent =
      `${fmtDuration(new Date(f.ended) - new Date(f.started))} · ` +
      `${Math.round(f.distance_nm)} nm · ${f.max_alt_ft.toLocaleString()} ft`;
    b.addEventListener("click", () => selectFlight(f, li));
    li.append(b);
    flightList.append(li);
  }
  if (data.truncated) {
    historyStatus.hidden = false;
    historyStatus.textContent = `Showing the most recent ${data.flights.length}; narrow the period for more.`;
  }
}

async function selectFlight(f, li) {
  for (const other of flightList.children) other.classList.toggle("selected", other === li);
  stopPlaying();

  const params = new URLSearchParams({ hex: f.hex, from: f.started, to: f.ended });
  let data;
  try {
    const r = await fetch(`/api/track?${params}`);
    if (r.status === 401) return signOut();
    if (!r.ok) throw new Error(await r.text());
    data = await r.json();
  } catch (err) {
    historyStatus.hidden = false;
    historyStatus.textContent = `Could not load the track: ${err.message}`;
    return;
  }

  track = data.points;
  if (track.length < 2) return clearTrack();

  map.getSource("track").setData({
    type: "Feature",
    geometry: { type: "LineString", coordinates: track.map((p) => [p.lon, p.lat]) },
  });

  const lons = track.map((p) => p.lon);
  const lats = track.map((p) => p.lat);
  map.fitBounds(
    [[Math.min(...lons), Math.min(...lats)], [Math.max(...lons), Math.max(...lats)]],
    { padding: { top: 60, bottom: 90, left: 60, right: panelWidth() + 60 }, duration: 700 },
  );

  slider.max = String(track.length - 1);
  slider.value = "0";
  scrubber.hidden = false;
  showPoint(0);
}

function clearTrack() {
  stopPlaying();
  track = [];
  scrubber.hidden = true;
  map.getSource("track")?.setData(emptyGeoJSON());
  map.getSource("cursor")?.setData(emptyGeoJSON());
  for (const li of flightList.children) li.classList.remove("selected");
}

function showPoint(i) {
  const p = track[i];
  if (!p) return;
  map.getSource("cursor").setData({
    type: "Feature",
    geometry: { type: "Point", coordinates: [p.lon, p.lat] },
    properties: { track: p.track_deg || 0 },
  });
  const alt = p.on_ground ? "ground" : `${p.alt_ft.toLocaleString()} ft`;
  readout.textContent =
    `${fmtClock(p.at)} · ${alt} · ${Math.round(p.speed_kt)} kt · ` +
    `${String(Math.round(p.track_deg)).padStart(3, "0")}°`;
}

slider.addEventListener("input", () => {
  stopPlaying();
  showPoint(Number(slider.value));
});

// Replay steps through recorded fixes at a fixed rate rather than in real time.
// A two-hour flight played back at 1x would be a two-hour wait; what you want is
// to watch the shape of it.
function stopPlaying() {
  if (playing) clearInterval(playing);
  playing = null;
  playButton.textContent = "▶";
  playButton.setAttribute("aria-label", "Play");
}

playButton.addEventListener("click", () => {
  if (playing) return stopPlaying();
  if (Number(slider.value) >= track.length - 1) slider.value = "0";
  playButton.textContent = "❚❚";
  playButton.setAttribute("aria-label", "Pause");
  playing = setInterval(() => {
    const next = Number(slider.value) + 1;
    if (next >= track.length) return stopPlaying();
    slider.value = String(next);
    showPoint(next);
  }, 60);
});

function signOut() {
  location.href = "/login?next=" + encodeURIComponent(location.pathname);
}

// ================================================================== watching
//
// Aircraft added by hand for this session only. They are tracked exactly like
// the fleet -- live, stale, dead-reckoned -- and drive the fast poll rate,
// because you added one in order to watch it. Nothing about them is recorded.

const watchForm = document.getElementById("watch-form");
const watchInput = document.getElementById("watch-input");
const watchNote = document.getElementById("watch-note");
const watchList = document.getElementById("watching");

function say(msg, isError) {
  watchNote.hidden = !msg;
  watchNote.textContent = msg || "";
  watchNote.dataset.error = String(Boolean(isError));
}

function renderWatched(list) {
  watchList.replaceChildren();
  for (const m of list) {
    const li = document.createElement("li");
    const rego = document.createElement("span");
    rego.className = "rego";
    rego.textContent = m.rego || m.hex;
    const what = document.createElement("span");
    what.className = "what";
    what.textContent = [m.type, m.desc].filter(Boolean).join(" · ");
    const drop = document.createElement("button");
    drop.type = "button";
    drop.textContent = "×";
    drop.setAttribute("aria-label", `Stop tracking ${m.rego || m.hex}`);
    drop.addEventListener("click", () => unwatch(m.hex));
    li.append(rego, what, drop);
    watchList.append(li);
  }
}

async function unwatch(hex) {
  const r = await fetch(`/api/watch?hex=${encodeURIComponent(hex)}`, { method: "DELETE" });
  if (r.status === 401) return signOut();
  if (r.ok) {
    renderWatched((await r.json()).watching || []);
    // Drop it from the map immediately rather than leaving a ghost until the
    // next broadcast.
    fleet.delete(hex);
    listItems.get(hex)?.remove();
    listItems.delete(hex);
  }
}

watchForm.addEventListener("submit", async (e) => {
  e.preventDefault();
  const query = watchInput.value.trim();
  if (!query) return;
  say("Looking…", false);
  try {
    const r = await fetch("/api/watch", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ query }),
    });
    if (r.status === 401) return signOut();
    if (!r.ok) return say((await r.text()).trim(), true);
    const data = await r.json();
    renderWatched(data.watching || []);
    watchInput.value = "";
    say(`Tracking ${data.added?.rego || query} for this session.`, false);
  } catch (err) {
    say(String(err.message || err), true);
  }
});

// Watched aircraft arrive in the same snapshot as the fleet, but have no row in
// the server-rendered list, so build one the first time each appears.
function ensureRow(s) {
  if (listItems.has(s.hex)) return listItems.get(s.hex);
  const li = document.createElement("li");
  li.dataset.hex = s.hex;
  li.dataset.watched = "true";
  li.innerHTML =
    `<button type="button" class="row">` +
    `<span class="rego"></span><span class="type"></span>` +
    `<span class="status"></span><span class="detail"></span></button>`;
  li.querySelector(".rego").textContent = s.rego || s.hex;
  li.querySelector(".type").textContent = s.type || "";
  li.querySelector(".row").addEventListener("click", () => selectAircraft(s.hex, map));
  document.getElementById("fleet").append(li);
  listItems.set(s.hex, li);
  return li;
}

fetch("/api/watch")
  .then((r) => (r.ok ? r.json() : null))
  .then((d) => d && renderWatched(d.watching || []))
  .catch(() => {});
