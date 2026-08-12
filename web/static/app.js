// Fleet table driven by the SSE stream. The map layer replaces the table's
// role as the primary view later; the data plumbing here is what it will use.

const conn = document.getElementById("conn");
const rows = new Map();
for (const tr of document.querySelectorAll("#fleet tbody tr")) {
  rows.set(tr.dataset.hex, tr);
}

function setConn(state, text) {
  conn.dataset.state = state;
  conn.textContent = text;
}

const fmt = {
  pos: (f) => `${f.lat.toFixed(4)}, ${f.lon.toFixed(4)}`,
  alt: (f) => (f.on_ground ? "ground" : `${f.alt_ft.toLocaleString()} ft`),
  speed: (f) => `${Math.round(f.speed_kt)} kt`,
  track: (f) => `${String(Math.round(f.track_deg)).padStart(3, "0")}°`,
};

// Ages are shown coarsely on purpose. A number ticking up every second draws
// the eye to the least important thing on the page; what matters is whether a
// position is current, and the status column already says that.
function age(seconds) {
  if (seconds < 60) return `${Math.round(seconds)}s`;
  if (seconds < 3600) return `${Math.round(seconds / 60)}m`;
  return `${Math.round(seconds / 3600)}h`;
}

function render(states) {
  for (const s of states) {
    const tr = rows.get(s.hex);
    if (!tr) continue; // fleet changed under a stale tab; ignore rather than throw

    tr.dataset.status = s.status;
    tr.dataset.mlat = String(Boolean(s.fix?.mlat));
    const cell = (c) => tr.querySelector(`.${c}`);

    cell("status").textContent = s.status.replace("_", " ");

    if (!s.fix) {
      for (const c of ["pos", "alt", "gs", "track", "age", "src"]) {
        cell(c).textContent = "—";
      }
      continue;
    }
    cell("pos").textContent = fmt.pos(s.fix);
    cell("alt").textContent = fmt.alt(s.fix);
    cell("gs").textContent = fmt.speed(s.fix);
    cell("track").textContent = fmt.track(s.fix);
    cell("age").textContent = age(s.age_sec ?? 0);
    cell("src").textContent = s.fix.source;
  }
}

function connect() {
  const es = new EventSource("/events");

  es.addEventListener("open", () => setConn("open", "live"));

  es.addEventListener("fleet", (e) => {
    try {
      render(JSON.parse(e.data));
    } catch (err) {
      console.error("bad fleet payload", err);
    }
  });

  es.addEventListener("error", () => {
    // EventSource reconnects on its own using the server's retry hint, so we
    // only report the state. The exception is an expired session: the server
    // answers /events with 401 rather than HTML, and no amount of retrying
    // fixes that, so send the user to sign in again.
    if (es.readyState === EventSource.CLOSED) {
      setConn("lost", "signed out");
      window.location.href = "/login?next=" + encodeURIComponent(location.pathname);
      return;
    }
    setConn("lost", "reconnecting");
  });
}

connect();
