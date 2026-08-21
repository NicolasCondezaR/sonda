/* Sonda — the instrument face.
 *
 * The field is a coordinate space in milliseconds: every event keeps a static x
 * derived from its timestamp, and the whole track is translated once per frame
 * so time advances. Repositioning every event each frame would be the obvious
 * way to do it and would not survive fifteen busy channels.
 */
"use strict";

const MAX_CALLS = 2000;      // bounded so a long session cannot grow without limit
// Divisions per window, chosen so every tick lands on a round figure.
const WINDOWS = new Map([[60000, 4], [300000, 5], [1800000, 6]]);
const SEARCH_POLL_MS = 2000;
const PAGE_LIMIT = 500;
/* The diagnosis is only refreshed while the field is empty, which is the only
 * time it is on screen. A listener can come back and a client can finally
 * connect while somebody reads it, and a reading that froze at the moment of
 * confusion would be the wrong one by the time they acted on it. */
const DIAG_POLL_MS = 3000;

const state = {
  targets: [],
  byName: new Map(),
  calls: new Map(),          // id -> summary
  nodes: new Map(),          // id -> element
  filter: "failed",          // the reason he opened it comes first
  windowMs: 300000,
  search: "",
  channel: null,
  held: false,
  frozen: false,             // pointer resting in the field holds the trace
  frozenOffset: 0,
  selected: null,
  // The two cursors hold call ids, not screen positions or timestamps. The
  // field is a window sliding on its own, so a cursor parked at an x would
  // measure the view's pixel-to-time scale rather than the traffic — and a
  // reading that is about the screen has no business on an instrument whose
  // first principle is fidelity. Pinned to captures, every delta is the
  // difference between two recorded times.
  cursors: { a: null, b: null },
  detail: null,             // the call currently in the inspector
  // flowPin holds the run kept for comparison. Comparing two runs needs two of
  // them and the second is usually found minutes after the first, so the hold
  // outlives the search — a modal picker would not.
  flowPin: null,
  totals: { calls: 0, dropped: 0, byTarget: new Map() },
  stubbed: new Set(),        // services answering from recordings, not the wire
  broken: new Map(),         // service -> the rule Sonda is applying to it
  diagnosis: null,           // why nothing is being captured, when nothing is
  probes: new Map(),         // service -> the last upstream dial, with its clock time
  probing: false,
  pxPerMs: 0,
  epoch: Date.now(),
  laneWidth: 0,
};

const dom = {
  rail: document.getElementById("rail"),
  railFoot: document.getElementById("rail-foot"),
  lanes: document.getElementById("lanes"),
  axis: document.getElementById("axis"),
  fieldBody: document.getElementById("field-body"),
  empty: document.getElementById("field-empty"),
  emptyHeadline: document.getElementById("empty-headline"),
  emptyNote: document.getElementById("empty-note"),
  emptyDiag: document.getElementById("empty-diag"),
  diagList: document.getElementById("diag-list"),
  diagNote: document.getElementById("diag-note"),
  diagProbe: document.getElementById("diag-probe"),
  readout: document.getElementById("readout"),
  calipers: document.getElementById("calipers"),
  cursorA: document.getElementById("cursor-a"),
  cursorB: document.getElementById("cursor-b"),
  acquisition: document.getElementById("acquisition"),
  acquisitionText: document.getElementById("acquisition-text"),
  search: document.getElementById("search"),
  hold: document.getElementById("hold"),
  inspector: document.getElementById("inspector"),
  inspectorIdle: document.getElementById("inspector-idle"),
  inspectorBody: document.getElementById("inspector-body"),
  diffBody: document.getElementById("diff-body"),
  admin: document.getElementById("admin"),
  adminBody: document.getElementById("admin-body"),
  adminNote: document.getElementById("admin-note"),
  openProjects: document.getElementById("open-projects"),
  closeAdmin: document.getElementById("close-admin"),
};

/* ------------------------------------------------------------- helpers -- */

function el(tag, className, text) {
  const node = document.createElement(tag);
  if (className) node.className = className;
  if (text !== undefined && text !== null) node.textContent = String(text);
  return node;
}

/* The definition of failure, mirroring summaryFailed in internal/api and
 * Call.Fault in the terminal client, clause for clause and in the same order.
 * A GraphQL error arrives under HTTP 200 with no transport complaint, so it is
 * asked about before the status is trusted. */
function isFault(call) {
  if (call.error) return true;
  if (call.graphql_errors > 0) return true;
  /* A Postgres statement has no status at all: its failure is an ErrorResponse
   * inside the stream, counted when the capture was stored. */
  if (call.postgres_errors > 0) return true;
  if (call.grpc_status !== undefined && call.grpc_status !== null) return call.grpc_status !== 0;
  return call.status >= 400;
}

/* A measured value is reported as measured. Rounding 1201ms to "about a second"
 * would be a lie about what the instrument read. */
function duration(ms) {
  if (ms >= 1000) return (ms / 1000).toFixed(2) + "s";
  if (ms >= 1) return Math.round(ms) + "ms";
  return ms.toFixed(2) + "ms";
}

function bytes(n) {
  if (n < 1024) return n + " B";
  if (n < 1024 * 1024) return (n / 1024).toFixed(1) + " KB";
  return (n / (1024 * 1024)).toFixed(1) + " MB";
}

function clockTime(iso) {
  const d = new Date(iso);
  return d.toLocaleTimeString(undefined, { hour12: false }) +
    "." + String(d.getMilliseconds()).padStart(3, "0");
}

function outcome(call) {
  if (call.error) return call.protocol === "amqp" ? "AMQP ERROR" : "TRANSPORT";
  if (call.grpc_status_text) return call.grpc_status_text.toUpperCase();
  /* "200" here would be the truth about the transport and a lie about the call. */
  if (call.graphql_errors > 0) return "GRAPHQL ERROR";
  if (call.postgres_errors > 0) return "SQL ERROR";
  /* There is no status on a statement. Printing the zero would invent one, so
   * the kind of row stands in: a statement, or the connection that ran none. */
  if (call.protocol === "postgres" || call.protocol === "amqp") return call.method;
  return String(call.status);
}

/* The one sentence a capture gets about encryption, or "" when both ends were
 * in the clear and there is nothing to say. It mirrors Call.TLSNote in the
 * terminal client clause for clause: two hand-written readings of the same three
 * flags is how the two interfaces start disagreeing about what was verified. */
function tlsNote(call) {
  if (call.upstream_insecure) {
    return "upstream certificate NOT VERIFIED — this service is configured to skip the check";
  }
  if (call.tls && call.upstream_tls) return "client encrypted to Sonda, upstream verified";
  if (call.upstream_tls) return "upstream verified";
  if (call.tls) return "client encrypted to Sonda, upstream in the clear";
  return "";
}

/* How a call is named in one line. Every GraphQL call to a service is the same
 * method and path, and every Postgres capture against a database is too, so the
 * operation — or the statement — is the only part that says which one this was.
 * Without it a whole lane reads as one call repeated. */
function label(call) {
  const base = call.method + " " + call.path;
  const detail = call.graphql_op || call.postgres_summary;
  return detail ? base + " · " + detail : base;
}

/* ------------------------------------------------------------- geometry -- */

function divisions() {
  return WINDOWS.get(state.windowMs) || 4;
}

function measure() {
  state.laneWidth = dom.lanes.clientWidth || 1;
  state.pxPerMs = state.laneWidth / state.windowMs;
  dom.lanes.style.setProperty("--division", (state.laneWidth / divisions()) + "px");
  renderAxis();
}

function renderAxis() {
  dom.axis.replaceChildren();
  const count = divisions();
  const step = state.windowMs / count;
  for (let i = 0; i < count; i++) {
    const tick = el("span", "axis__tick", "-" + shortSpan(state.windowMs - i * step));
    tick.style.left = (i * (state.laneWidth / count)) + "px";
    dom.axis.appendChild(tick);
  }
  const now = el("span", "axis__tick", "NOW");
  now.style.right = "0";
  now.style.left = "auto";
  now.style.paddingLeft = "0";
  now.style.paddingRight = "5px";
  now.style.borderLeft = "0";
  dom.axis.appendChild(now);
}

function shortSpan(ms) {
  const s = Math.round(ms / 1000);
  if (s >= 60 && s % 60 === 0) return (s / 60) + "M";
  return s + "S";
}

function xFor(startedAt) {
  return (new Date(startedAt).getTime() - state.epoch) * state.pxPerMs;
}

/* ---------------------------------------------------------------- rail -- */

function renderRail() {
  dom.rail.replaceChildren();

  for (const target of state.targets) {
    const item = el("li");
    const row = el("button", "channel");
    row.type = "button";
    row.style.setProperty("--ch", target.color);
    row.setAttribute("aria-pressed", String(state.channel === target.name));
    row.title = target.name + " → " + target.upstream;

    const stubbed = state.stubbed.has(target.name);
    const swatch = el("span", "channel__swatch" +
      (target.hollow ? " channel__swatch--hollow" : "") +
      (stubbed ? " channel__swatch--stub" : ""));
    row.append(swatch, el("span", "channel__name", target.name));
    if (target.protocol === "grpc") row.appendChild(el("span", "channel__proto", "gRPC"));
    if (target.protocol === "amqp") row.appendChild(el("span", "channel__proto", "AMQP"));
    // Stubbing is a mode the service is in, not a property of one call, so it
    // is engraved on the channel rather than repeated on every event.
    if (stubbed) {
      row.appendChild(el("span", "channel__stub", "STUB"));
      row.title += " — answering from recordings, not being called";
    }
    // Being broken on purpose is a mode too, and the one most worth seeing
    // before spending an hour on the failures it is causing.
    if (state.broken.has(target.name)) {
      row.appendChild(el("span", "channel__stub channel__stub--broken", "BROKEN"));
      row.title += " — broken on purpose: " + state.broken.get(target.name);
    }
    /* Terminating TLS is a mode too, and not checking the upstream's
     * certificate is the one nobody may have to go looking for: the rail is
     * where a reader picks which service to look at. */
    if (target.tls) {
      row.appendChild(el("span", "channel__stub", "TLS"));
      const scheme = target.protocol === "amqp" ? "amqps://" : "https://";
      row.title += " — Sonda terminates TLS on this port; point the caller at " + scheme + target.listen;
    }
    if (target.insecure_skip_verify) {
      row.appendChild(el("span", "channel__stub channel__stub--broken", "NO VERIFY"));
      row.title += " — the upstream's certificate is not being checked";
    }
    row.append(
      el("span", "channel__calls", "0"),
      el("span", "channel__faults", "0"),
    );

    row.addEventListener("click", () => {
      state.channel = state.channel === target.name ? null : target.name;
      renderRail();
      reload();
    });

    item.appendChild(row);
    dom.rail.appendChild(item);
    target.row = row;
  }
  updateRailCounts();
}

function updateRailCounts() {
  const tally = state.totals.byTarget;

  for (const target of state.targets) {
    if (!target.row) continue;
    const t = tally.get(target.name) || { calls: 0, faults: 0 };
    const cells = target.row.querySelectorAll(".channel__calls, .channel__faults");
    cells[0].textContent = t.calls;
    cells[1].textContent = t.faults;
    cells[1].dataset.any = String(t.faults > 0);
  }
}

/* --------------------------------------------------------------- lanes -- */

function renderLanes() {
  dom.lanes.replaceChildren();
  for (const target of state.targets) {
    const lane = el("div", "lane");
    lane.style.setProperty("--ch", target.color);
    const track = el("div", "lane__track");
    lane.appendChild(track);
    dom.lanes.appendChild(lane);
    target.track = track;
  }
  // The cursor layer is stacked over every lane, so it needs to know how tall
  // the stack is; the lanes rebuild whenever the channel list changes.
  dom.lanes.style.setProperty("--lane-count", state.targets.length);
  dom.lanes.appendChild(caliperLayer());
  measure();
  renderCursors();
}

/* ------------------------------------------------------------- cursors -- */

let layer = null;
const caliperNodes = {};

function caliperLayer() {
  if (!layer) {
    layer = el("div", "calipers-layer");
    for (const which of ["a", "b"]) {
      const node = el("div", "caliper");
      node.hidden = true;
      node.appendChild(el("span", "caliper__letter", which.toUpperCase()));
      caliperNodes[which] = node;
      layer.appendChild(node);
    }
  }
  return layer;
}

// setCursor pins a cursor to the selected call, or lifts it when it is already
// on that one — the same key both places and clears, the way a latched control
// works.
function setCursor(which) {
  if (state.selected === null) return;
  state.cursors[which] = state.cursors[which] === state.selected ? null : state.selected;
  renderCursors();
}

function renderCursors() {
  for (const which of ["a", "b"]) {
    const node = caliperNodes[which];
    const call = state.calls.get(state.cursors[which]);
    if (!node) continue;
    if (!call) {
      // A cursor cannot point at a capture the field no longer holds. Dropping
      // it is the honest move: keeping the letter with nothing under it would
      // claim a measurement against something that scrolled out of the window.
      state.cursors[which] = null;
      node.hidden = true;
      continue;
    }
    node.hidden = false;
    node.style.left = xFor(call.started_at) + "px";
  }
  renderCaliperReading();
}

function renderCaliperReading() {
  const a = state.calls.get(state.cursors.a);
  const b = state.calls.get(state.cursors.b);

  dom.cursorA.setAttribute("aria-pressed", String(Boolean(a)));
  dom.cursorB.setAttribute("aria-pressed", String(Boolean(b)));
  const armed = state.selected !== null;
  dom.cursorA.disabled = !armed && !a;
  dom.cursorB.disabled = !armed && !b;
  const how = "Select a call, then place this cursor on it";
  dom.cursorA.title = a ? "Lift cursor A" : how;
  dom.cursorB.title = b ? "Lift cursor B" : how;

  if (!a && !b) {
    dom.calipers.hidden = true;
    dom.calipers.replaceChildren();
    return;
  }

  dom.calipers.hidden = false;
  dom.calipers.replaceChildren();

  if (!a || !b) {
    // One cursor down is not a measurement. Say which is missing rather than
    // showing a dash: naming the next key is the recovery.
    const placed = a ? "A" : "B";
    const missing = a ? "B" : "A";
    dom.calipers.appendChild(
      el("span", "calipers__hint", `${placed} SET · PRESS ${missing.toLowerCase()} ON ANOTHER CALL`));
    return;
  }

  const ta = new Date(a.started_at).getTime();
  const tb = new Date(b.started_at).getTime();
  // The arrow carries which cursor is earlier, so the reading never has to show
  // a negative span. Start to start, the reading that answers "and how long
  // after that did the next call go out" — each call's own duration is still on
  // screen as the width of its mark.
  const order = tb >= ta ? "A→B" : "B→A";
  dom.calipers.appendChild(document.createTextNode(`${order} ${duration(Math.abs(tb - ta))}`));
}

function addEvent(call, isNew) {
  const target = state.byName.get(call.target);
  if (!target || !target.track) return;

  const fault = isFault(call);
  const stub = Boolean(call.stub_of);
  const node = el("button", "ev" + (fault ? " ev--fault" : "") +
    (stub ? " ev--stub" : "") + (isNew ? " ev--new" : ""));
  node.type = "button";
  node.style.left = xFor(call.started_at) + "px";
  if (!fault) {
    node.style.width = Math.max(3, call.duration_ms * state.pxPerMs) + "px";
  }
  node.setAttribute("aria-pressed", "false");
  // Said in words as well: the hatch carries it at a glance, and a screen
  // reader has no hatch.
  const served = stub ? ", answered from a recording" : "";
  node.setAttribute("aria-label",
    `${label(call)} on ${call.target}, ${outcome(call)}${served}, ${duration(call.duration_ms)}`);
  node.title = `${outcome(call)}  ${label(call)}\n${duration(call.duration_ms)} · ${clockTime(call.started_at)}`;

  node.addEventListener("click", () => select(call.id));

  target.track.appendChild(node);
  state.nodes.set(call.id, node);
}

function repositionAll() {
  for (const [id, node] of state.nodes) {
    const call = state.calls.get(id);
    if (!call) continue;
    node.style.left = xFor(call.started_at) + "px";
    if (!isFault(call)) {
      node.style.width = Math.max(3, call.duration_ms * state.pxPerMs) + "px";
    }
  }
  // A changed window or width rescales the axis, so the cursors have to be
  // re-solved from their calls' times rather than kept at their old pixels.
  renderCursors();
}

/* One transform per frame moves the whole field; events keep static positions. */
const reducedMotion = window.matchMedia("(prefers-reduced-motion: reduce)");
let lastStep = 0;

function advance() {
  // Reduced motion gets the same information without the continuous sweep:
  // the field steps once a second instead of gliding.
  if (reducedMotion.matches && !state.frozen) {
    const now = Date.now();
    if (now - lastStep < 1000) {
      requestAnimationFrame(advance);
      return;
    }
    lastStep = now;
  }

  const offset = state.frozen
    ? state.frozenOffset
    : state.laneWidth - (Date.now() - state.epoch) * state.pxPerMs;

  for (const target of state.targets) {
    if (target.track) target.track.style.transform = `translateX(${offset}px)`;
  }
  // The cursors ride the same offset as the tracks. Anything else and a cursor
  // would drift off the call it is pinned to as the trace advances.
  if (layer) layer.style.transform = `translateX(${offset}px)`;
  if (!state.frozen) prune(offset);
  requestAnimationFrame(advance);
}

function freeze(on) {
  if (state.frozen === on) return;
  if (on) state.frozenOffset = state.laneWidth - (Date.now() - state.epoch) * state.pxPerMs;
  state.frozen = on;
  if (state.held) return;
  setAcquisition(on ? "held" : "live", on ? "TRACE HELD" : "LIVE");
}

function prune(offset) {
  if (state.nodes.size < 200) return;
  let dropped = false;
  for (const [id, node] of state.nodes) {
    if (parseFloat(node.style.left) + offset < -80) {
      node.remove();
      state.nodes.delete(id);
      state.calls.delete(id);
      if (state.cursors.a === id || state.cursors.b === id) dropped = true;
    }
  }
  // Only when a pruned call was carrying a cursor: this runs inside the frame
  // loop, and re-reading both cursors every frame to find nothing is work the
  // field does not need to do.
  if (dropped) renderCursors();
}

/* ------------------------------------------------------------- readout -- */

function refreshReadout() {
  const shown = state.calls.size;
  let faults = 0;
  for (const call of state.calls.values()) if (isFault(call)) faults++;

  dom.readout.replaceChildren();
  const parts = [];
  parts.push(`${state.totals.calls} CAPTURED`);
  if (state.filter === "failed") {
    parts.push(`${shown} FLAGGED`);
  } else {
    parts.push(`${shown} SHOWN`);
    if (faults) parts.push(`${faults} FLAGGED`);
  }
  if (state.totals.dropped) parts.push(`${state.totals.dropped} DROPPED`);

  dom.readout.appendChild(document.createTextNode(parts.join("  ·  ")));
  if (state.filter === "failed" && shown > 0) dom.readout.classList.add("readout__fault");
  else dom.readout.classList.remove("readout__fault");

  /* A rule in force is never folded into the counts. The bar is where someone
   * looks first when the field fills with failures, and the control that armed
   * one lives behind PROJECTS: if this line is not here, the way to find out
   * that Sonda is the cause is to remember doing it. */
  if (state.broken.size) {
    dom.readout.appendChild(el("span", "readout__broken",
      "  ·  " + state.broken.size + " BROKEN ON PURPOSE"));
  }

  const noneShown = shown === 0;
  dom.empty.hidden = !noneShown;

  /* Nothing captured at all is the only one of these three that is a question
   * about the instrument rather than about the filter, and it is the one that
   * gets an empty screen with no explanation. It gets the diagnosis; the other
   * two already say what is true in one line. */
  const unexplained = noneShown && state.totals.calls === 0;
  dom.emptyDiag.hidden = !unexplained;
  dom.empty.classList.toggle("field__empty--diagnosing", unexplained);
  watchDiagnosis(unexplained);

  if (!noneShown) return;
  if (unexplained) {
    dom.emptyHeadline.textContent = "ARMED · NOTHING CAPTURED";
    dom.emptyNote.textContent = state.diagnosis
      ? state.diagnosis.summary
      : "Reading what Sonda knows about each channel…";
  } else if (state.filter === "failed") {
    dom.emptyHeadline.textContent = "NO FAULTS";
    dom.emptyNote.textContent =
      `${state.totals.calls} calls captured, none flagged. Switch to ALL to see the whole field.`;
  } else {
    dom.emptyHeadline.textContent = "NO MATCH";
    dom.emptyNote.textContent = "Nothing in this window matches the current filter.";
  }
}

/* ----------------------------------------------------------- diagnosis -- */

let diagTimer = null;

function watchDiagnosis(on) {
  if (on === Boolean(diagTimer)) return;
  clearInterval(diagTimer);
  diagTimer = on ? setInterval(() => loadDiagnosis(false), DIAG_POLL_MS) : null;
  if (on) loadDiagnosis(false);
}

/* probe is a side effect — it dials the user's services — so it is only ever
 * passed by the button, never by the timer above. The GET is free of it by
 * construction rather than by a flag somebody could forget. */
async function loadDiagnosis(probe) {
  try {
    const res = await fetch("api/diagnose", probe ? { method: "POST" } : undefined);
    if (!res.ok) return;
    const report = await res.json();
    if (probe) {
      /* Kept with the clock time it was taken at, because the poll that follows
       * carries no probe and the reading must not be redrawn as if it were
       * current. A stale answer labelled stale is useful; one presented as now
       * is the tool lying. */
      const at = new Date().toLocaleTimeString(undefined, { hour12: false });
      for (const s of report.services || []) {
        state.probes.set(s.service, { at, reachable: s.upstream_reachable, error: s.upstream_error });
      }
    }
    state.diagnosis = report;

    /* The live stream only bumps the local total for calls the current filter
     * admits, so the first successful call under FAULTS leaves it at zero while
     * the store knows better — and this panel would keep saying nothing was
     * captured over a proxy that is demonstrably working. The report counts what
     * is stored, so it is the authority; reloading puts the field, the readout
     * and this panel back in step, and the panel goes away on its own. */
    if ((report.services || []).some((s) => s.captures > 0) && state.totals.calls === 0) {
      reload();
      return;
    }

    renderDiagnosis();
    if (state.totals.calls === 0) dom.emptyNote.textContent = report.summary;
  } catch (err) {
    console.error("sonda: could not read the diagnosis", err);
  }
}

/* A verdict is not a fault by default. `fault` is reserved for failure, so only
 * a port that never opened and an upstream refusing connections get it — a
 * client that has not connected yet is not a failure of anything. */
function verdictTone(verdict) {
  if (verdict === "listener_down" || verdict === "upstream_unreachable") return "fault";
  if (verdict === "capturing") return "ok";
  return "flat";
}

function renderDiagnosis() {
  const report = state.diagnosis;
  if (!report) return;

  dom.diagNote.textContent = report.note || "";
  dom.diagList.replaceChildren();

  for (const svc of report.services || []) {
    const row = el("div", "diag__row");
    row.appendChild(el("span", "diag__name", svc.service));
    row.appendChild(el("span", "diag__cell", svc.listen));
    row.appendChild(el("span", "diag__cell diag__cell--num", svc.connections + " CONN"));
    row.appendChild(el("span", "diag__cell diag__cell--num", svc.captures + " CAPT"));
    row.appendChild(el("span", "diag__state diag__state--" + verdictTone(svc.verdict),
      svc.verdict.replace(/_/g, " ").toUpperCase()));

    row.appendChild(el("p", "diag__detail", svc.detail));

    const probe = state.probes.get(svc.service);
    if (probe) {
      row.appendChild(el("p", "diag__probe" + (probe.reachable ? "" : " diag__probe--fault"),
        probe.reachable
          ? `upstream ${svc.upstream} accepted a connection at ${probe.at}`
          : `upstream ${svc.upstream} refused a connection at ${probe.at} — ${probe.error}`));
    }

    if (svc.cannot_distinguish && svc.cannot_distinguish.length) {
      row.appendChild(el("p", "diag__label", "SONDA CANNOT TELL THESE APART"));
      for (const cause of svc.cannot_distinguish) {
        row.appendChild(el("p", "diag__cause", cause));
      }
    }
    if (svc.what_to_check && svc.what_to_check.length) {
      row.appendChild(el("p", "diag__label", "WHAT TO CHECK, IN ORDER"));
      svc.what_to_check.forEach((step, i) => {
        row.appendChild(el("p", "diag__step", (i + 1) + ". " + step));
      });
    }
    dom.diagList.appendChild(row);
  }
}

function setAcquisition(stateName, text) {
  dom.acquisition.dataset.state = stateName;
  dom.acquisitionText.textContent = text;
}

/* ----------------------------------------------------------- data load -- */

function query() {
  const params = new URLSearchParams();
  params.set("limit", String(PAGE_LIMIT));
  if (state.filter === "failed") params.set("failed", "true");
  if (state.channel) params.set("target", state.channel);
  if (state.search) params.set("q", state.search);
  params.set("since", new Date(Date.now() - state.windowMs).toISOString());
  return params;
}

async function reload() {
  try {
    const [callsRes, statsRes, stubRes, faultRes] = await Promise.all([
      fetch("api/calls?" + query()),
      fetch("api/stats"),
      fetch("api/stub"),
      fetch("api/faults"),
    ]);
    if (!callsRes.ok) throw new Error("calls " + callsRes.status);

    const { calls } = await callsRes.json();

    // Which services are answering from recordings. Read on every reload
    // rather than remembered, because it can be changed from the terminal or
    // by an agent while this window is open.
    if (stubRes.ok) {
      const { stubbed } = await stubRes.json();
      const next = new Set(stubbed || []);
      const changed = next.size !== state.stubbed.size ||
        [...next].some((name) => !state.stubbed.has(name));
      state.stubbed = next;
      if (changed) renderRail();
    }

    if (faultRes.ok) {
      const { faults } = await faultRes.json();
      const next = new Map(Object.entries(faults || {}));
      const changed = next.size !== state.broken.size ||
        [...next].some(([name, rule]) => state.broken.get(name) !== rule);
      state.broken = next;
      if (changed) renderRail();
    }
    if (statsRes.ok) {
      const stats = await statsRes.json();
      state.totals.calls = stats.calls || 0;
      state.totals.dropped = stats.dropped || 0;
      state.totals.byTarget = new Map(
        (stats.by_target || []).map((t) => [t.target, { calls: t.calls, faults: t.faults }]));
    }

    state.calls.clear();
    state.nodes.clear();
    for (const target of state.targets) {
      if (target.track) target.track.replaceChildren();
    }
    // Oldest first, so the field is built left to right.
    for (const call of calls.slice().reverse()) {
      state.calls.set(call.id, call);
      addEvent(call, false);
    }
    updateRailCounts();
    refreshReadout();
  } catch (err) {
    setAcquisition("lost", "LOAD FAILED");
    console.error("sonda: could not load calls", err);
  }
}

function admits(call) {
  if (state.channel && call.target !== state.channel) return false;
  if (state.filter === "failed" && !isFault(call)) return false;
  return true;
}

function ingest(call) {
  if (state.held || !admits(call)) return;
  state.totals.calls++;
  let tally = state.totals.byTarget.get(call.target);
  if (!tally) state.totals.byTarget.set(call.target, (tally = { calls: 0, faults: 0 }));
  tally.calls++;
  if (isFault(call)) tally.faults++;
  state.calls.set(call.id, call);
  addEvent(call, true);
  if (state.calls.size > MAX_CALLS) {
    const oldest = state.calls.keys().next().value;
    const node = state.nodes.get(oldest);
    if (node) node.remove();
    state.nodes.delete(oldest);
    state.calls.delete(oldest);
  }
  updateRailCounts();
  refreshReadout();
}

let stream = null;
let searchTimer = null;
let pollTimer = null;

function connect() {
  if (stream) stream.close();
  stream = new EventSource("api/stream");

  stream.addEventListener("open", () => {
    if (!state.held) setAcquisition("live", "LIVE");
  });
  stream.addEventListener("call", (event) => {
    // While a search is running the server is the authority, because it matches
    // payload text this page never received. Polling covers it instead.
    if (state.search) return;
    try {
      ingest(JSON.parse(event.data));
    } catch (err) {
      console.error("sonda: bad event", err);
    }
  });
  stream.addEventListener("error", () => {
    if (!state.held) setAcquisition("lost", "RECONNECTING");
  });
}

function syncPolling() {
  clearInterval(pollTimer);
  pollTimer = null;
  if (state.search && !state.held) {
    pollTimer = setInterval(reload, SEARCH_POLL_MS);
  }
}

/* ----------------------------------------------------------- inspector -- */

async function select(id) {
  for (const [otherID, node] of state.nodes) {
    node.setAttribute("aria-pressed", String(otherID === id));
  }
  state.selected = id;
  // A call is now selected, so the cursor controls have something to be placed
  // on: they stop being disabled the moment that becomes true.
  renderCaliperReading();
  dom.inspector.hidden = false;
  dom.inspectorIdle.hidden = true;
  dom.inspectorBody.hidden = false;
  dom.inspectorBody.replaceChildren(el("div", "insp-sec__body insp-sec__body--loading", "Reading…"));

  try {
    const res = await fetch("api/calls/" + id);
    if (!res.ok) throw new Error("detail " + res.status);
    const detail = await res.json();
    state.detail = detail;
    renderInspector(detail);
  } catch (err) {
    dom.inspectorBody.replaceChildren(
      el("div", "insp-sec__body note note--fault", "Could not read this call: " + err.message));
  }
}

function section(title, aside) {
  const wrap = el("section", "insp-sec");
  const head = el("div", "insp-sec__head");
  head.appendChild(el("span", "label", title));
  if (aside) head.appendChild(el("span", "note", aside));
  wrap.appendChild(head);
  const body = el("div", "insp-sec__body");
  wrap.appendChild(body);
  return { wrap, body };
}

function kv(pairs) {
  const list = el("dl", "kv");
  for (const [key, value] of pairs) {
    if (value === undefined || value === null || value === "") continue;
    list.append(el("dt", null, key), el("dd", null, value));
  }
  return list;
}

function renderInspector(call) {
  const out = document.createDocumentFragment();

  const head = el("div", "insp-head");
  head.appendChild(el("div", "insp-head__path", label(call)));
  const meta = el("div", "insp-head__meta");
  meta.append(
    el("span", null, call.target),
    el("span", null, (call.protocol || "http").toUpperCase()),
  );
  /* Raw protocol captures have no HTTP status. "HTTP 0" would be a reading of
   * something that was never measured. */
  if (call.protocol !== "postgres" && call.protocol !== "amqp") {
    meta.appendChild(el("span", null, "HTTP " + call.status));
  }
  meta.append(
    el("span", null, duration(call.duration_ms)),
    el("span", null, clockTime(call.started_at)),
  );
  head.appendChild(meta);

  if (call.grpc_status_text) {
    const line = el("div", call.grpc_status === 0 ? "note" : "insp-head__fault",
      "gRPC " + call.grpc_status + " " + call.grpc_status_text +
      (call.grpc_message ? " — " + call.grpc_message : ""));
    head.appendChild(line);
  }
  /* Said in the header and not only in the block below: this call answered
   * HTTP 200, and a reader who takes the status line at its word closes the
   * inspector believing it worked. */
  if (call.graphql_errors > 0) {
    head.appendChild(el("div", "insp-head__fault",
      "GraphQL · " + call.graphql_errors +
      (call.graphql_errors === 1 ? " error" : " errors") + " under HTTP " + call.status));
  }
  /* Same reason again: nothing about a statement's transport says it failed. */
  if (call.postgres_errors > 0) {
    head.appendChild(el("div", "insp-head__fault",
      "Postgres · " + call.postgres_errors +
      (call.postgres_errors === 1 ? " server error" : " server errors")));
  }
  if (call.error) {
    head.appendChild(el("div", "insp-head__fault", call.error));
  }

  // Provenance before anything else. Everything below is a reading of something
  // recorded earlier, and a reader who scrolls straight to the payload would
  // otherwise take it for what just happened.
  // Said before anything else: this failure is Sonda's, not the service's, and
  // a reader who takes it for real spends an hour on a bug that is not there.
  if (call.injected) {
    const line = el("div", "insp-head__stub insp-head__stub--broken");
    line.append(el("b", null, "BROKEN ON PURPOSE"), document.createTextNode(" · "));
    line.appendChild(document.createTextNode(call.error || "Sonda injected this failure."));
    head.appendChild(line);
  }

  if (call.stub_of) {
    const line = el("div", "insp-head__stub");
    line.append(el("b", null, "FROM RECORDING"), document.createTextNode(" · "));
    line.appendChild(document.createTextNode("the service was not called. Answered from capture "));
    const link = el("button", null, "#" + call.stub_of);
    link.type = "button";
    link.addEventListener("click", () => select(call.stub_of));
    line.appendChild(link);
    head.appendChild(line);
  }

  /* Also before the payload. Whether the far end was ever identified decides
   * what the payload is worth, and going to read the configuration to find out
   * is exactly the trip this line saves. */
  const encryption = tlsNote(call);
  if (encryption) {
    const line = el("div", "insp-head__stub" + (call.upstream_insecure ? " insp-head__stub--broken" : ""));
    line.append(el("b", null, "TLS"), document.createTextNode(" · " + encryption));
    head.appendChild(line);
  }

  out.appendChild(head);
  out.appendChild(renderActions(call));
  out.appendChild(renderTracePlaceholder(call));
  out.appendChild(renderDriftPlaceholder(call));

  if (call.graphql) {
    out.appendChild(renderGraphQL(call.graphql));
    /* The raw body stays reachable: the decode is a reading of it, not a
     * replacement for it. */
    out.appendChild(renderSide("REQUEST", call.request).wrap);
    out.appendChild(renderSide("RESPONSE", call.response).wrap);
  } else if (call.socket) {
    out.appendChild(renderFrames("SENT", call.socket.sent, call.socket.sent_summary, call.socket.sent_incomplete));
    out.appendChild(renderFrames("RECEIVED", call.socket.received, call.socket.received_summary, call.socket.received_incomplete));
  } else if (call.postgres) {
    out.appendChild(renderPostgres("SENT", call.postgres.sent, call.postgres.sent_incomplete));
    out.appendChild(renderPostgres("RECEIVED", call.postgres.received, call.postgres.received_incomplete));
  } else if (call.amqp) {
    out.appendChild(renderAMQP("SENT", call.amqp.sent, call.amqp.sent_incomplete));
    out.appendChild(renderAMQP("RECEIVED", call.amqp.received, call.amqp.received_incomplete));
  } else if (call.stream) {
    out.appendChild(renderEvents(call.stream));
  } else if (call.grpc) {
    out.appendChild(renderGRPC(call.grpc));
  } else {
    out.appendChild(renderSide("REQUEST", call.request).wrap);
    out.appendChild(renderSide("RESPONSE", call.response).wrap);
  }

  if (call.response_trailers && Object.keys(call.response_trailers).length) {
    const s = section("TRAILERS");
    s.body.appendChild(kv(Object.entries(call.response_trailers).map(([k, v]) => [k, v.join(", ")])));
    out.appendChild(s.wrap);
  }

  dom.inspectorBody.replaceChildren(out);
  dom.inspectorBody.scrollTop = 0;
  dom.diffBody.hidden = true;
}

/* --------------------------------------------------------------- drift -- */

/* Whether this endpoint still answers the shape it used to. Fetched after the
   payload is on screen, like the tree: it is context, not the reason the
   inspector was opened. */

function renderDriftPlaceholder(call) {
  const s = section("CONTRACT");
  s.body.classList.add("insp-sec__body--loading");
  s.body.textContent = "comparing…";
  loadDrift(call, s);
  return s.wrap;
}

async function loadDrift(call, s) {
  let report;
  try {
    const q = new URLSearchParams({ target: call.target, path: call.path, method: call.method });
    const res = await fetch("api/drift?" + q);
    // Fewer than two captures, or a response that is not JSON. Neither is a
    // failure worth a red box; there is simply nothing to compare.
    if (!res.ok) { s.wrap.remove(); return; }
    report = await res.json();
  } catch {
    s.wrap.remove();
    return;
  }

  s.body.classList.remove("insp-sec__body--loading");
  s.body.replaceChildren();

  const head = s.wrap.querySelector(".insp-sec__head");
  head.appendChild(el("span", "note", "vs capture #" + report.baseline));

  if (!report.changes.length) {
    s.body.appendChild(el("p", "note", "The shape has not changed since then."));
    return;
  }

  const list = el("div", "tree");
  for (const c of report.changes) {
    const row = el("div", "tree__row" + (c.kind === "added" ? "" : " tree__row--fault"));
    const left = el("span");
    left.appendChild(el("span", "tree__pipe", c.kind === "gone" ? "− " : c.kind === "added" ? "+ " : "~ "));
    left.appendChild(el("span", "tree__name", c.path));
    row.append(left, el("span", "tree__dur",
      c.kind === "retyped" ? c.was + " → " + c.now : c.kind === "gone" ? "was " + c.was : c.now));
    list.appendChild(row);
  }
  s.body.appendChild(list);

  // Adding a field is safe. Losing one or changing its type is what takes a
  // caller down, and saying which is the difference between a report worth
  // acting on and a list worth ignoring.
  s.body.appendChild(el("p", "note",
    report.breaking.length
      ? report.breaking.length + " of these would break a caller."
      : "All additive — nothing here breaks a caller."));
}

/* ---------------------------------------------------------------- tree -- */

/* One call is a reading; the request it belonged to is the thing that broke.
   Fetched after the inspector is already on screen, because the payload is what
   the reader came for and it must not wait on this. */

function renderTracePlaceholder(call) {
  // Not "REQUEST": that name already belongs to this call's own payload, two
  // sections below. And not "TRACE", which on this surface is the moving line.
  const s = section("CALL TREE");
  s.body.classList.add("insp-sec__body--loading");
  s.body.textContent = "arranging…";
  loadTrace(call, s);
  return s.wrap;
}

async function loadTrace(call, s) {
  let tree;
  try {
    const res = await fetch("api/trace?call=" + call.id);
    if (!res.ok) throw new Error(String(res.status));
    ({ trace: tree } = await res.json());
  } catch {
    // Not a failure worth shouting about: the payload below is still the
    // answer, and the tree is context.
    s.wrap.remove();
    return;
  }

  // A lone call is not a tree, and drawing one would suggest Sonda found
  // relations it did not.
  if (!tree || tree.calls < 2) {
    s.wrap.remove();
    return;
  }

  s.body.classList.remove("insp-sec__body--loading");
  s.body.replaceChildren();

  const head = s.wrap.querySelector(".insp-sec__head");
  head.appendChild(el("span", "note",
    tree.calls + " calls" + (tree.failed ? " · " + tree.failed + " failed" : "")));

  // Principle three of the product: label the guesses. A tree grouped by
  // timing alone is an inference, and one presented as measurement is worse
  // than no tree.
  if (!tree.certain) {
    s.body.appendChild(el("div", "note",
      "grouped by timing — no trace id in these requests, so the shape is inferred"));
  }

  const list = el("div", "tree");
  drawNode(list, tree.root, "", true, true, call.id);
  s.body.appendChild(list);
}

function drawNode(list, node, prefix, last, root, currentID) {
  const c = node.call;
  const row = el("button", "tree__row" +
    (c.failed ? " tree__row--fault" : "") +
    (node.inferred ? " tree__row--guess" : ""));
  row.type = "button";
  row.setAttribute("aria-current", String(c.id === currentID));

  const target = state.byName.get(c.target);
  if (target) row.style.setProperty("--ch", target.color);

  const left = el("span");
  left.appendChild(el("span", "tree__pipe", prefix + (root ? "" : last ? "└─ " : "├─ ")));
  // The same hatch as the rail and the field: this branch is a recording, which
  // is also the answer to why it came back suspiciously fast.
  left.appendChild(el("span", "tree__swatch" + (c.stubbed ? " tree__swatch--stub" : "")));
  // Only a failure gets a mark. Giving a healthy row one too would put a second
  // coloured square on every line and turn the exception into wallpaper.
  if (c.failed) left.appendChild(el("span", "tree__mark", "█"));
  left.appendChild(el("span", "tree__name", c.target + (c.path ? " " + c.path : "")));
  if (c.stubbed) left.appendChild(el("span", "note", "  from recording"));
  if (node.ambiguous) left.appendChild(el("span", "note", "  may belong elsewhere"));

  row.append(left, el("span", "tree__dur", duration(c.duration_ms)));
  row.addEventListener("click", () => select(c.id));
  list.appendChild(row);

  if (c.detail) list.appendChild(el("div", "tree__detail", c.detail));

  const children = node.children || [];
  const indent = prefix + (root ? "" : last ? "   " : "│  ");
  children.forEach((child, i) => {
    drawNode(list, child, indent, i === children.length - 1, false, currentID);
  });
}

/* ------------------------------------------------------------- actions -- */

function renderActions(call) {
  const bar = el("div", "actions");

  // Replaying a socket would resend the handshake and open a new, empty
  // conversation — not the one being read. A database session is the same
  // shape of problem. Offering a control that cannot do what its label says is
  // worse than not offering it.
  if (call.protocol === "websocket") {
    bar.appendChild(el("span", "note", "A socket cannot be replayed: the handshake would open a new conversation, not this one."));
    return bar;
  }
  if (call.protocol === "postgres") {
    bar.appendChild(el("span", "note", "A statement cannot be replayed: it belongs to a connection and a transaction that are gone."));
    return bar;
  }
  if (call.protocol === "amqp") {
    bar.appendChild(el("span", "note", "An AMQP unit cannot be replayed: it belongs to a multiplexed connection and channel that are gone."));
    return bar;
  }

  const replay = el("button", "switch__key switch__key--lone", "REPLAY");
  replay.type = "button";

  // Only the head of a truncated body was stored, so replaying it would put a
  // different request on the wire. The control says why up front instead of
  // failing after the click.
  if (call.request.truncated) {
    replay.disabled = true;
    replay.title = "This request was truncated when captured, so it cannot be replayed faithfully.";
  }
  replay.addEventListener("click", () => runReplay(call, replay, bar));
  bar.appendChild(replay);

  if (call.replay_of) {
    const diff = el("button", "switch__key switch__key--lone", "DIFF vs ORIGINAL");
    diff.type = "button";
    diff.addEventListener("click", () => runDiff(call.replay_of, call.id));
    bar.appendChild(diff);
    bar.appendChild(el("span", "actions__note", "replay of #" + call.replay_of));
  }

  bar.appendChild(flowControl(call, bar));
  return bar;
}

// flowControl holds one run and then compares the next one against it. Two
// clicks rather than a picker, because the run you want to compare against was
// usually captured long before you knew you would want it.
function flowControl(call, bar) {
  if (state.flowPin === null || state.flowPin === call.id) {
    const hold = el("button", "switch__key switch__key--lone",
      state.flowPin === call.id ? "RUN HELD" : "HOLD RUN");
    hold.type = "button";
    hold.addEventListener("click", () => {
      state.flowPin = state.flowPin === call.id ? null : call.id;
      const note = el("span", "actions__note", state.flowPin
        ? "run #" + call.id + " held — open a call from the other run to compare"
        : "run released");
      for (const stale of bar.querySelectorAll(".actions__note--flow")) stale.remove();
      note.classList.add("actions__note--flow");
      bar.appendChild(note);
      hold.textContent = state.flowPin ? "RUN HELD" : "HOLD RUN";
    });
    return hold;
  }

  const compare = el("button", "switch__key switch__key--lone", "DIFF FLOW vs #" + state.flowPin);
  compare.type = "button";
  compare.addEventListener("click", () => {
    const pinned = state.flowPin;
    state.flowPin = null;
    runFlowDiff(pinned, call.id);
  });
  return compare;
}

async function runReplay(call, button, bar) {
  const previous = button.textContent;
  button.disabled = true;
  button.textContent = "SENDING";
  for (const stale of bar.querySelectorAll(".actions__note--fault")) stale.remove();

  try {
    const res = await fetch("api/calls/" + call.id + "/replay", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: "{}",
    });
    const out = await res.json();
    if (!res.ok) throw new Error(out.error || ("replay " + res.status));

    // The replay travels through the proxy, so it arrives as an ordinary
    // capture. Wait for it rather than guessing its id.
    const replayed = await waitForReplay(call.id);
    if (replayed) {
      await select(replayed);
      runDiff(call.id, replayed);
    } else {
      bar.appendChild(el("span", "actions__note",
        "sent to " + out.sent_to + " - " + (out.error || out.status)));
    }
  } catch (err) {
    bar.appendChild(el("span", "actions__note actions__note--fault", err.message));
  } finally {
    button.disabled = call.request.truncated;
    button.textContent = previous;
  }
}

// The recorder writes off the request path, so the row lands a moment after the
// response comes back.
async function waitForReplay(originalID) {
  for (let attempt = 0; attempt < 30; attempt++) {
    await new Promise((resolve) => setTimeout(resolve, 100));
    const res = await fetch("api/calls?limit=20");
    if (!res.ok) continue;
    const body = await res.json();
    const hit = body.calls.find((c) => c.replay_of === originalID);
    if (hit) return hit.id;
  }
  return null;
}

/* ---------------------------------------------------------------- diff -- */

async function runDiff(aID, bID) {
  dom.diffBody.hidden = false;
  dom.diffBody.replaceChildren(el("div", "insp-sec__body insp-sec__body--loading", "Comparing"));

  try {
    const res = await fetch("api/diff?a=" + aID + "&b=" + bID);
    const out = await res.json();
    if (!res.ok) throw new Error(out.error || ("diff " + res.status));
    renderDiff(out);
  } catch (err) {
    dom.diffBody.replaceChildren(
      el("div", "insp-sec__body note note--fault", "Could not compare: " + err.message));
  }
}

async function runFlowDiff(aID, bID) {
  dom.diffBody.hidden = false;
  dom.diffBody.replaceChildren(el("div", "insp-sec__body insp-sec__body--loading", "Comparing the two runs"));

  try {
    const res = await fetch("api/flowdiff?a=" + aID + "&b=" + bID);
    const out = await res.json();
    if (!res.ok) throw new Error(out.error || ("flowdiff " + res.status));
    renderFlowDiff(out, aID, bID);
  } catch (err) {
    dom.diffBody.replaceChildren(
      el("div", "insp-sec__body note note--fault", "Could not compare the runs: " + err.message));
  }
}

function renderFlowDiff(d, aID, bID) {
  const out = document.createDocumentFragment();

  const head = section("FLOW DIFF  #" + aID + " to #" + bID,
    d.matched + " matched" + (d.unmatched ? " · " + d.unmatched + " unpaired" : ""));

  // The three ways this comparison can be misleading, said before the tree
  // rather than left to be worked out from it.
  if (!d.same_entry) {
    head.body.appendChild(el("p", "note note--fault",
      "These two runs do not start from the same call, so the comparison below is probably meaningless."));
  }
  if (!d.certain) {
    head.body.appendChild(el("p", "note",
      "at least one run was grouped by timing, not by a trace id — the shapes are inferred"));
  }
  if (d.unmatched > d.matched && d.matched > 0) {
    head.body.appendChild(el("p", "note",
      "More calls went unpaired than paired. That is usually the path matching, not the code: the ids in these paths are not being recognised."));
  }

  if (d.divergence && d.divergence.length) {
    head.body.appendChild(kv([["first divergence", d.divergence.join("  →  ")]]));
  } else {
    head.body.appendChild(el("p", "diff__same", "Both runs did the same things with the same outcomes."));
  }
  out.appendChild(head.wrap);

  const flow = section("ALIGNED CALLS");
  const list = el("div", "tree");
  drawPair(list, d.root, "", true, true);
  flow.body.appendChild(list);
  out.appendChild(flow.wrap);

  for (const body of d.bodies || []) {
    const b = section("PAYLOAD  " + body.signature, "#" + body.a + " to #" + body.b);
    b.body.appendChild(renderSideDiff("REQUEST", body.request));
    b.body.appendChild(renderSideDiff("RESPONSE", body.response));
    out.appendChild(b.wrap);
  }

  dom.diffBody.replaceChildren(out);
  dom.diffBody.scrollTop = 0;
}

function drawPair(list, pair, prefix, last, root) {
  const gone = pair.only_in === "a";
  const fresh = pair.only_in === "b";
  const changed = (pair.changes || []).length > 0;

  const row = el("div", "tree__row" +
    (gone || changed ? " tree__row--fault" : "") +
    (pair.inferred ? " tree__row--guess" : ""));

  const left = el("span");
  left.appendChild(el("span", "tree__pipe", prefix + (root ? "" : last ? "└─ " : "├─ ")));
  left.appendChild(el("span", "tree__name", pair.signature));
  row.appendChild(left);

  let verdict = "same";
  if (gone) verdict = "only in a — no longer called";
  else if (fresh) verdict = "only in b — new call";
  else if (changed) verdict = pair.changes.map((c) => c.field + ": " + c.a + " → " + c.b).join(", ");
  row.appendChild(el("span", "tree__verdict", verdict));

  list.appendChild(row);

  const indent = prefix + (root ? "" : last ? "   " : "│  ");
  const kids = pair.children || [];
  kids.forEach((child, i) => drawPair(list, child, indent, i === kids.length - 1, false));
}

function renderDiff(d) {
  const out = document.createDocumentFragment();

  const head = section("DIFF  #" + d.a.id + " to #" + d.b.id, "a is red, b is green");
  head.body.appendChild(kv([
    ["a", "#" + d.a.id + "  " + d.a.method + " " + d.a.path],
    ["b", "#" + d.b.id + "  " + d.b.method + " " + d.b.path],
  ]));
  out.appendChild(head.wrap);

  const outcome = section("OUTCOME");
  if (!d.metadata.length) {
    outcome.body.appendChild(el("p", "diff__same", "Same outcome."));
  } else {
    outcome.body.appendChild(renderChanges(d.metadata));
  }
  out.appendChild(outcome.wrap);

  out.appendChild(renderSideDiff("REQUEST", d.request));
  out.appendChild(renderSideDiff("RESPONSE", d.response));

  dom.diffBody.replaceChildren(out);
  dom.diffBody.scrollTop = 0;
}

function renderSideDiff(title, side) {
  const s = section(title);

  if (!side.comparable) {
    s.body.appendChild(el("p", "note", side.reason || "Not comparable."));
    s.body.appendChild(el("p", "diff__same",
      side.identical ? "The bytes are identical." : "The bytes differ."));
    return s.wrap;
  }

  if (side.messages) {
    for (const m of side.messages) {
      const block = el("div", "msg");
      block.appendChild(el("span", "label", "MESSAGE #" + m.index));
      if (m.only_in) {
        block.appendChild(el("p", "note", "Only present in " + m.only_in + "."));
      } else if (!m.comparable) {
        block.appendChild(el("p", "note", m.reason));
      } else if (m.identical) {
        block.appendChild(el("p", "diff__same", "Identical."));
      } else {
        block.appendChild(renderChanges(m.changes));
      }
      s.body.appendChild(block);
    }
    if (!side.messages.length) s.body.appendChild(el("p", "diff__same", "No messages."));
    return s.wrap;
  }

  if (side.identical) {
    s.body.appendChild(el("p", "diff__same", "Identical."));
  } else {
    s.body.appendChild(renderChanges(side.changes));
  }
  return s.wrap;
}

function renderChanges(changes) {
  const wrap = el("div", "chg");
  for (const c of changes) {
    const symbol = c.kind === "added" ? "+" : c.kind === "removed" ? "-" : "~";
    const mark = el("span", "chg__mark", symbol);
    mark.dataset.kind = c.kind;
    wrap.append(mark, el("span", "chg__path", c.path));

    const values = el("div", "chg__values");
    if (c.kind !== "added") {
      values.append(el("span", "chg__side", "a"), el("span", "chg__a", formatValue(c.a)));
    }
    if (c.kind !== "removed") {
      values.append(el("span", "chg__side", "b"), el("span", "chg__b", formatValue(c.b)));
    }
    wrap.appendChild(values);
  }
  return wrap;
}

function formatValue(value) {
  if (value === undefined || value === null) return "(absent)";
  if (typeof value === "object") return JSON.stringify(value);
  return String(value);
}


function renderSide(title, msg) {
  const aside = msg.truncated
    ? `${bytes(msg.size)} · stored ${bytes(msg.stored)}`
    : bytes(msg.size);
  const s = section(title, aside);

  if (msg.headers && Object.keys(msg.headers).length) {
    s.body.appendChild(kv(Object.entries(msg.headers).map(([k, v]) => [k, v.join(", ")])));
  }
  if (msg.truncated) {
    s.body.appendChild(el("p", "note", "Only the head was stored. The full body reached its destination."));
  }
  if (msg.text) {
    s.body.appendChild(el("pre", "payload", pretty(msg.text)));
  } else if (msg.base64) {
    s.body.appendChild(el("p", "note", "Not valid UTF-8; kept as raw bytes."));
    s.body.appendChild(el("pre", "payload", msg.base64));
  } else if (msg.size === 0) {
    s.body.appendChild(el("p", "note", "No body."));
  }
  return s;
}

function pretty(text) {
  const trimmed = text.trim();
  if (!trimmed.startsWith("{") && !trimmed.startsWith("[")) return text;
  try {
    return JSON.stringify(JSON.parse(trimmed), null, 2);
  } catch {
    return text;
  }
}

function renderGRPC(g) {
  const out = document.createDocumentFragment();

  const source = g.schema && g.schema.source
    ? "schema from " + g.schema.source.replace("_", " ")
    : "no schema";
  const head = section(g.service + " / " + g.method, source);
  if (g.schema && g.schema.error) {
    head.body.appendChild(el("p", "note", g.schema.error));
    head.body.appendChild(el("p", "note",
      "Fields below are read straight off the wire format, so names are absent and types are inferred."));
  }
  if (!g.schema || !g.schema.error) {
    head.body.appendChild(kv([
      ["service", g.service],
      ["method", g.method],
      ["status", g.status_text ? g.status + " " + g.status_text : undefined],
    ]));
  }
  out.appendChild(head.wrap);

  out.appendChild(renderMessages("REQUEST", g.request, g.request_incomplete));
  out.appendChild(renderMessages("RESPONSE", g.response, g.response_incomplete));
  return out;
}

/* A socket is one exchange carrying two streams of frames, and an event stream
   is one response carrying many events. Both are read out of the stored bytes
   here, the same way gRPC messages are. */

function renderFrames(title, frames, summary, incomplete) {
  const s = section(title, summary || "no frames");

  if (!frames || !frames.length) {
    s.body.appendChild(el("p", "note", "Nothing in this direction."));
    return s.wrap;
  }

  for (const f of frames) {
    const block = el("div", "msg");
    const head = el("div", "msg__head");
    const kind = el("span", "label", f.kind.toUpperCase());
    head.append(kind, el("span", "note",
      bytes(f.size) + (f.final ? "" : " · fragment, continues")));
    block.appendChild(head);

    if (f.kind === "close") {
      // Why the socket ended is usually the reason someone is reading this.
      block.appendChild(el("p", "note",
        f.close_code ? "code " + f.close_code + (f.close_reason ? " — " + f.close_reason : "")
                     : "closed with no code"));
    } else if (f.text !== undefined && f.text !== "") {
      block.appendChild(el("pre", "payload", pretty(f.text)));
    } else if (f.base64) {
      block.appendChild(el("p", "note", "Not text. " + bytes(f.size) + " of bytes."));
    } else if (f.size === 0) {
      block.appendChild(el("p", "note", "Empty."));
    }
    s.body.appendChild(block);
  }

  if (incomplete) {
    s.body.appendChild(el("p", "note",
      "Bytes remain after the last whole frame — the capture was cut short by the body cap, or the socket was still open."));
  }
  return s.wrap;
}

/* A GraphQL POST is one exchange carrying one or more operations, read out of
   the stored bodies here the same way events and frames are.

   The raw request body is one long escaped string that looks the same on every
   call to the endpoint. What is worth the space is the operation, what it asked
   for, and what came back wrong. */

function renderGraphQL(g) {
  const ops = g.operations || [];
  const s = section("GRAPHQL", g.batch ? "batch of " + ops.length : ops[0] && ops[0].type);

  for (const op of ops) {
    const block = el("div", "msg");
    const head = el("div", "msg__head");
    head.append(el("span", "label", op.label));
    if (op.errors && op.errors.length) {
      head.appendChild(el("span", "note note--fault",
        op.errors.length === 1 ? "1 error" : op.errors.length + " errors"));
    }
    block.appendChild(head);

    if (op.fields && op.fields.length) {
      block.appendChild(el("p", "note", "asks for " + op.fields.join(", ")));
    }
    if (op.variables !== undefined) {
      block.appendChild(el("pre", "payload", JSON.stringify(op.variables, null, 2)));
    }

    /* An error names where in the answer it happened, and that is most of what
       makes it actionable. */
    for (const e of op.errors || []) {
      block.appendChild(el("p", "note note--fault", e.message));
      const where = [e.path && "at " + e.path, e.code].filter(Boolean).join(" · ");
      if (where) block.appendChild(el("p", "note", where));
    }
    s.body.appendChild(block);
  }

  if (g.unreadable) {
    s.body.appendChild(el("p", "note",
      "The response is not JSON, so whether it carried errors is unknown — the capture was cut short by the body cap, or the server answered something else."));
  }
  return s.wrap;
}

/* A Postgres statement is one exchange carrying two streams of protocol
   messages, read out of the stored bytes here the same way frames are.

   A result is mostly DataRows, and drawing forty of them would bury the
   statement that produced them. Rows are counted; the messages that say
   something a reader came for — the SQL, its parameters, the command tag, the
   server's error — are drawn. */

function renderPostgres(title, messages, incomplete) {
  const msgs = messages || [];
  const s = section(title, msgs.length === 1 ? "1 message" : msgs.length + " messages");

  if (!msgs.length) {
    s.body.appendChild(el("p", "note", "Nothing in this direction."));
    return s.wrap;
  }

  let rows = 0;
  const flushRows = () => {
    if (!rows) return;
    s.body.appendChild(el("p", "note", rows === 1 ? "1 data row" : rows + " data rows"));
    rows = 0;
  };

  for (const m of msgs) {
    if (m.kind === "data_row") { rows++; continue; }
    flushRows();

    const block = el("div", "msg");
    const head = el("div", "msg__head");
    head.append(el("span", "label", m.kind.toUpperCase().replace(/_/g, " ")));

    /* Failure is carried by shape first: an error is its own block with its
       SQLSTATE spelled out, not a red version of an ordinary one. */
    if (m.kind === "error_response") {
      head.appendChild(el("span", "note note--fault", [m.severity, m.code].filter(Boolean).join(" ")));
    } else if (m.size) {
      head.appendChild(el("span", "note", bytes(m.size)));
    }
    block.appendChild(head);

    if (m.sql) block.appendChild(el("pre", "payload", m.sql));
    if (m.params && m.params.length) {
      block.appendChild(kv(m.params.map((p, i) => ["$" + (i + 1), pgValue(p)])));
    }
    if (m.tag) block.appendChild(el("p", "note", m.tag));
    if (m.message) {
      block.appendChild(el("p", m.kind === "error_response" ? "note note--fault" : "note", m.message));
    }
    for (const extra of [m.detail, m.hint]) {
      if (extra) block.appendChild(el("p", "note", extra));
    }
    if (m.parameters) {
      block.appendChild(kv(Object.keys(m.parameters).sort().map((k) => [k, m.parameters[k]])));
    }
    /* Which mechanism, never the exchange: the bytes that carried it were
       blanked before anything was stored. */
    if (m.auth) block.appendChild(el("p", "note", "mechanism: " + m.auth));
    if (m.tx_status) block.appendChild(el("p", "note", m.tx_status.replace(/_/g, " ")));
    /* A gap the tool knows about is stated, never hidden. */
    if (m.note) block.appendChild(el("p", "note", m.note));

    s.body.appendChild(block);
  }
  flushRows();

  if (incomplete) {
    s.body.appendChild(el("p", "note",
      "Bytes remain after the last whole message — the capture was cut short by the body cap, or the session was still open."));
  }
  return s.wrap;
}

function renderAMQP(title, frames, incomplete) {
  const items = frames || [];
  const s = section(title, items.length === 1 ? "1 frame" : items.length + " frames");
  if (!items.length) {
    s.body.appendChild(el("p", "note", "Nothing in this direction."));
    return s.wrap;
  }

  for (const frame of items) {
    const block = el("div", "msg");
    const head = el("div", "msg__head");
    head.appendChild(el("span", "label", (frame.kind || frame.type || "frame").toUpperCase()));
    head.appendChild(el("span", "note", "channel " + frame.channel + " · " + bytes(frame.size || 0)));
    block.appendChild(head);

    const facts = [];
    if (frame.exchange || frame.routing_key) {
      facts.push(["route", (frame.exchange || "(default)") + (frame.routing_key ? " → " + frame.routing_key : "")]);
    }
    for (const [name, value] of [
      ["queue", frame.queue], ["consumer", frame.consumer_tag],
      ["delivery tag", frame.delivery_tag], ["message count", frame.message_count],
      ["body size", frame.body_size ? bytes(frame.body_size) : ""],
      ["content type", frame.content_type], ["delivery mode", frame.delivery_mode],
      ["correlation id", frame.correlation_id], ["reply to", frame.reply_to],
      ["mechanisms", frame.mechanisms], ["mechanism", frame.mechanism],
      ["virtual host", frame.virtual_host], ["protocol", frame.protocol],
    ]) {
      if (value !== undefined && value !== null && value !== "") facts.push([name, String(value)]);
    }
    if (facts.length) block.appendChild(kv(facts));

    if (frame.reply_code) {
      block.appendChild(el("p", frame.reply_code >= 300 ? "note note--fault" : "note",
        frame.reply_code + (frame.reply_text ? " — " + frame.reply_text : "") +
        (frame.cause ? " (on " + frame.cause + ")" : "")));
    }
    if (frame.text) block.appendChild(el("pre", "payload", pretty(frame.text)));
    if (frame.note) block.appendChild(el("p", "note", frame.note));
    s.body.appendChild(block);
  }

  if (incomplete) {
    s.body.appendChild(el("p", "note",
      "Bytes remain after the last whole frame — the capture was truncated or ended mid-frame."));
  }
  return s.wrap;
}

/* A NULL and an empty string are different on the wire and different in a WHERE
   clause, so they read differently here. */
function pgValue(v) {
  if (v.null) return "NULL";
  if (v.text) return v.text;
  if (!v.size) return "''";
  return bytes(v.size) + ", not text";
}

function renderEvents(stream) {
  const events = stream.events || [];
  const s = section("EVENTS", events.length === 1 ? "1 event" : events.length + " events");

  if (!events.length) {
    s.body.appendChild(el("p", "note", "No events."));
    return s.wrap;
  }

  for (const e of events) {
    const block = el("div", "msg");
    const head = el("div", "msg__head");
    head.appendChild(el("span", "label", (e.name || "message").toUpperCase()));
    if (e.id) head.appendChild(el("span", "note", "id " + e.id));
    if (e.retry) head.appendChild(el("span", "note", "retry " + e.retry + "ms"));
    block.appendChild(head);
    if (e.data) block.appendChild(el("pre", "payload", pretty(e.data)));
    s.body.appendChild(block);
  }

  if (stream.incomplete) {
    s.body.appendChild(el("p", "note",
      "The last event has no terminator — the capture was cut short by the body cap, or the stream was still open."));
  }
  return s.wrap;
}

function renderMessages(title, messages, incomplete) {
  const count = messages ? messages.length : 0;
  const s = section(title, count === 1 ? "1 message" : count + " messages");

  if (!count) {
    s.body.appendChild(el("p", "note", "No messages."));
    return s.wrap;
  }

  for (const message of messages) {
    const block = el("div", "msg");
    const head = el("div", "msg__head");
    head.append(
      el("span", "label", "#" + message.index),
      el("span", "note", bytes(message.size) + (message.compressed ? " · compressed" : "")),
    );
    block.appendChild(head);

    if (message.error) block.appendChild(el("p", "note note--fault", message.error));
    if (message.json !== undefined) {
      block.appendChild(el("pre", "payload", JSON.stringify(message.json, null, 2)));
    } else if (message.fields) {
      block.appendChild(renderWire(message.fields));
    }
    s.body.appendChild(block);
  }

  if (incomplete) {
    s.body.appendChild(el("p", "note",
      "Bytes remain after the last whole message — the capture was cut short by the body cap, or the stream was still open."));
  }
  return s.wrap;
}

/* The schema-free view. Inferences carry their note so the reader knows which
 * parts to distrust. */
function renderWire(fields) {
  const wrap = el("div", "wire");
  for (const field of fields) {
    const row = el("div", "wire__row");
    row.append(
      el("span", "wire__num", field.number),
      el("span", "wire__type", field.type),
    );

    if (Array.isArray(field.value)) {
      row.appendChild(el("span", "wire__val", field.note ? "{ " + field.note + " }" : "{"));
      wrap.appendChild(row);
      const nest = el("div", "wire__nest");
      nest.appendChild(renderWire(field.value));
      wrap.appendChild(nest);
      continue;
    }

    row.appendChild(el("span", "wire__val", String(field.value)));
    if (field.note) row.appendChild(el("span", "wire__note", field.note));
    wrap.appendChild(row);
  }
  return wrap;
}

function closeInspector() {
  state.selected = null;
  state.detail = null;
  dom.diffBody.hidden = true;
  // The cursors stay where they are — they measure the field, not the
  // inspector. Only the controls go back to having nothing to place.
  renderCaliperReading();
  for (const node of state.nodes.values()) node.setAttribute("aria-pressed", "false");
  dom.inspectorBody.hidden = true;
  dom.inspectorIdle.hidden = false;
  if (window.matchMedia("(max-width: 1100px)").matches) dom.inspector.hidden = true;
}

/* ------------------------------------------------------------ controls -- */

function wireControls() {
  for (const key of document.querySelectorAll("[data-filter]")) {
    key.addEventListener("click", () => {
      state.filter = key.dataset.filter;
      for (const other of document.querySelectorAll("[data-filter]")) {
        other.setAttribute("aria-pressed", String(other === key));
      }
      reload();
    });
  }

  for (const key of document.querySelectorAll("[data-window]")) {
    key.addEventListener("click", () => {
      state.windowMs = Number(key.dataset.window);
      for (const other of document.querySelectorAll("[data-window]")) {
        other.setAttribute("aria-pressed", String(other === key));
      }
      measure();
      repositionAll();
      reload();
    });
  }

  dom.search.addEventListener("input", () => {
    clearTimeout(searchTimer);
    searchTimer = setTimeout(() => {
      state.search = dom.search.value.trim();
      syncPolling();
      reload();
    }, 220);
  });

  dom.openProjects.addEventListener("click", openAdmin);
  dom.closeAdmin.addEventListener("click", closeAdmin);

  /* The only path in the page that makes Sonda touch a user's service. It is a
   * press, never a timer and never a page load. */
  dom.diagProbe.addEventListener("click", async () => {
    if (state.probing) return;
    state.probing = true;
    dom.diagProbe.textContent = "PROBING…";
    await loadDiagnosis(true);
    state.probing = false;
    dom.diagProbe.textContent = "PROBE AGAIN";
  });

  dom.hold.addEventListener("click", () => {
    state.held = !state.held;
    dom.hold.setAttribute("aria-pressed", String(state.held));
    setAcquisition(state.held ? "held" : "live", state.held ? "HELD" : "LIVE");
    syncPolling();
    if (!state.held) reload();
  });

  document.addEventListener("keydown", (event) => {
    if (event.key === "Escape" && admin.open) {
      closeAdmin();
      return;
    }
    if (event.key === "Escape") closeInspector();
    if (event.key === "/" && document.activeElement !== dom.search) {
      event.preventDefault();
      dom.search.focus();
    }
    // Not while typing in the search box, where a and b are just letters.
    if (document.activeElement === dom.search) return;
    if (event.key === "a" || event.key === "b") {
      event.preventDefault();
      setCursor(event.key);
    }
  });

  dom.cursorA.addEventListener("click", () => setCursor("a"));
  dom.cursorB.addEventListener("click", () => setCursor("b"));

  // The rail and the lanes are one grid read across two columns; letting them
  // scroll apart would misalign every channel with its own traffic.
  let syncing = false;
  const link = (from, to) => from.addEventListener("scroll", () => {
    if (syncing) return;
    syncing = true;
    to.scrollTop = from.scrollTop;
    syncing = false;
  });
  link(dom.lanes, dom.rail);
  link(dom.rail, dom.lanes);

  dom.fieldBody.addEventListener("pointerenter", () => freeze(true));
  dom.fieldBody.addEventListener("pointerleave", () => freeze(false));

  window.addEventListener("resize", () => {
    measure();
    repositionAll();
  });
}

/* ---------------------------------------------------------------- boot -- */

async function boot() {
  wireControls();
  if (window.matchMedia("(max-width: 1100px)").matches) dom.inspector.hidden = true;

  try {
    const res = await fetch("api/targets");
    const { targets } = await res.json();
    state.targets = targets;
    state.byName = new Map(targets.map((t) => [t.name, t]));
  } catch (err) {
    setAcquisition("lost", "NO API");
    dom.emptyHeadline.textContent = "NO CONNECTION";
    dom.emptyNote.textContent = "Sonda's API did not answer. Is the process still running?";
    dom.empty.hidden = false;
    console.error("sonda: could not read targets", err);
    return;
  }

  renderRail();
  renderLanes();
  dom.railFoot.textContent = state.targets.length === 1
    ? "1 channel armed"
    : state.targets.length + " channels armed";

  await reload();
  connect();
  requestAnimationFrame(advance);
}


/* --------------------------------------------------------------- admin -- */

/* The configuration screen. Everything here goes through the same API the rest
 * of the page uses, and every mutation answers with the new state of every
 * project — so the screen never has to guess what happened, it redraws what the
 * server says is true. */

const admin = {
  open: false,
  projects: [],
  found: null,
  target: null,   // project the import panel belongs to
  ca: null,       // the certificate authority, once one exists
};

async function loadProjects() {
  const res = await fetch("api/projects");
  if (!res.ok) throw new Error("could not read projects");
  admin.projects = (await res.json()).projects;

  /* Read alongside the projects because it changes with them: the authority is
   * created the moment a service is set to terminate TLS, and the panel that
   * says how to trust it has to appear in the same redraw. */
  const authority = await fetch("api/tls");
  admin.ca = authority.ok ? await authority.json() : null;
}

function note(text, kind) {
  const el = dom.adminNote;
  el.textContent = text || "";
  el.className = "admin__note" + (kind ? " admin__note--" + kind : "");
}

async function call(method, url, body, options) {
  const init = { method, headers: {} };
  if (body !== undefined) {
    init.headers["Content-Type"] = (options && options.contentType) || "application/json";
    init.body = typeof body === "string" ? body : JSON.stringify(body);
  }
  const res = await fetch(url, init);
  const text = await res.text();
  let parsed = null;
  try { parsed = text ? JSON.parse(text) : null; } catch { /* not JSON */ }
  if (!res.ok) throw new Error((parsed && parsed.error) || text || ("the API answered " + res.status));
  return parsed;
}

// Answers whether it went through, so a caller with fields on screen can leave
// them alone after a refusal. The API refuses a rule that would do nothing, and
// redrawing the row over that refusal would clear what was typed and read as
// though something had been armed.
async function mutate(fn, success) {
  try {
    note("working…");
    const result = await fn();
    if (result && result.projects) admin.projects = result.projects;
    else await loadProjects();
    note(success || "", "ok");
    renderAdmin();
    // Channels and captures belong to the project that is now listening.
    await boot_reloadAfterProjectChange();
    return true;
  } catch (err) {
    note(err.message, "fault");
    return false;
  }
}

function openAdmin() {
  admin.open = true;
  dom.admin.hidden = false;
  loadProjects().then(renderAdmin).catch((err) => note(err.message, "fault"));
}

function closeAdmin() {
  admin.open = false;
  admin.found = null;
  dom.admin.hidden = true;
}

function renderAdmin() {
  const out = document.createDocumentFragment();

  for (const project of admin.projects) {
    out.appendChild(renderProject(project));
  }
  if (!admin.projects.length) {
    out.appendChild(el("p", "note", "No projects yet. Create one, then add the services it talks to — or import them from a file the project already has."));
  }
  out.appendChild(renderNewProject());
  const authority = renderAuthority();
  if (authority) out.appendChild(authority);

  dom.adminBody.replaceChildren(out);
}

/* What to run to trust Sonda's certificate authority, and what to run to get rid
 * of it again.
 *
 * The commands are the panel. Sonda deliberately does not touch the trust store
 * — that is the user's decision to make deliberately — and "install the CA" with
 * no exact line is how a deliberate decision turns into a browser warning
 * clicked through. The removal half is here for the same reason: a root nobody
 * can find later is worse than no root at all.
 *
 * The private key is not shown, not linked and not downloadable. */
function renderAuthority() {
  if (!admin.ca || !admin.ca.exists) return null;
  const i = admin.ca.instructions;

  const box = el("section", "prj");
  const head = el("div", "prj__head");
  head.append(el("span", "prj__name", "CERTIFICATE AUTHORITY"));
  const download = el("a", "switch__key switch__key--lone", "DOWNLOAD");
  download.href = "api/tls/ca.pem";
  download.download = "sonda-ca.pem";
  download.title = "The public certificate only. Useful when Sonda runs in a container and the file is not on this machine.";
  head.appendChild(download);
  box.appendChild(head);

  for (const [label, value] of [["file", i.path], ["subject", i.subject], ["sha256", i.fingerprint_sha256], ["expires", i.expires]]) {
    const row = el("div", "svc__point");
    row.append(el("span", null, label), el("code", "svc__command", value));
    box.appendChild(row);
  }

  const group = (title, steps) => {
    box.appendChild(el("p", "label", title));
    for (const step of steps) {
      const row = el("div", "svc__point");
      row.append(
        el("span", null, step.where),
        el("code", "svc__command", step.command),
        button("COPY", () => copy(step.command)),
      );
      box.appendChild(row);
    }
  };

  group("TRUST IT IN ONE PROGRAM — no administrator, nothing left behind", i.per_tool);
  group("OR FOR THE WHOLE MACHINE — run it yourself, Sonda will not", i.trust_system_wide);
  group("REMOVE IT — withdraw the trust first, then delete the files", i.remove);

  box.appendChild(el("p", "note",
    "Sonda never adds this to your trust store. It generated the authority, wrote it beside the database with the private key readable only by you, and stops there."));
  return box;
}

function renderProject(project) {
  const box = el("section", "prj" + (project.active ? " prj--active" : ""));

  const head = el("div", "prj__head");
  head.appendChild(el("span", "prj__name", project.name));

  const listening = project.services.filter((s) => s.running).length;
  const summary = project.active
    ? `${project.services.length} services · ${listening} listening`
    : `${project.services.length} services`;
  head.appendChild(el("span", "prj__count", summary));

  if (project.active) {
    head.appendChild(el("span", "prj__lamp prj__lamp--on", "■ ACTIVE"));
  } else {
    head.appendChild(button("ACTIVATE", () =>
      mutate(() => call("POST", `api/projects/${project.id}/activate`), `${project.name} is listening`)));
  }

  head.appendChild(button("RENAME", () => {
    const name = prompt("New name for " + project.name, project.name);
    if (name && name !== project.name) {
      mutate(() => call("PATCH", `api/projects/${project.id}`, { name }), "renamed");
    }
  }));
  head.appendChild(button("DELETE", () => {
    if (confirm(`Delete ${project.name} and its ${project.services.length} services? Captures already taken are kept.`)) {
      mutate(() => call("DELETE", `api/projects/${project.id}`), "deleted");
    }
  }));
  box.appendChild(head);

  for (const svc of project.services) {
    box.appendChild(renderService(project, svc));
  }

  box.appendChild(renderServiceForm(project));
  box.appendChild(renderProjectTools(project));
  if (admin.target === project.id && admin.found) {
    box.appendChild(renderFound(project));
  }
  return box;
}

function upstreamCell(svc) {
  if (!svc.insecure_skip_verify) {
    return el("span", "svc__cell svc__cell--faint", svc.upstream);
  }
  const cell = el("span", "svc__cell svc__state--off", svc.upstream + " · NO VERIFY");
  cell.title = "Sonda does not check this upstream's certificate for this service. Every capture taken through it is recorded as unverified.";
  return cell;
}

function renderService(project, svc) {
  const row = el("div", "svc");
  row.append(
    el("span", "svc__name", svc.name),
    el("span", "svc__cell svc__cell--faint", svc.protocol),
    /* The scheme is part of the address once Sonda terminates TLS: a caller
     * pointed at http:// on this port gets nothing. */
    el("span", "svc__cell", (svc.tls ? "https://" : "") + svc.listen),
    /* Said in the upstream's own cell, in the colour failure owns, because that
     * is what is not being checked — and because the grid has a column for the
     * upstream and none for a badge. Someone scanning the list for "which of
     * these am I not verifying" must not have to open anything. */
    upstreamCell(svc),
  );

  // What is really happening on the port, not what was configured. Named for
  // the port and not `state`, which is the module's own and is read below.
  const portState = project.active
    ? el("span", "svc__state " + (svc.running ? "svc__state--on" : "svc__state--off"),
        svc.running ? "LISTENING" : "FAILED")
    : el("span", "svc__state svc__cell--faint", "IDLE");
  if (svc.error) portState.title = svc.error;
  row.appendChild(portState);

  // A latched position, not a verb: the label says where the switch sits, and
  // pressing it moves it. Only offered on the project that is actually
  // listening, since stubbing a project whose ports are closed does nothing.
  if (project.active) {
    const stubbed = state.stubbed.has(svc.name);
    const toggle = button(stubbed ? "FROM RECORDING" : "LIVE", async () => {
      await mutate(() => call("POST", "api/stub",
        { service: svc.name, enabled: !stubbed }),
        stubbed ? `${svc.name} is being called again` : `${svc.name} answers from recordings`);
      await reload();
      renderAdmin();
    });
    toggle.title = stubbed
      ? `${svc.name} is not being called. Press to forward again.`
      : `${svc.name} is being called for real. Press to answer from recordings instead.`;
    if (stubbed) toggle.setAttribute("aria-pressed", "true");
    row.appendChild(toggle);
  }

  row.appendChild(button("REMOVE", () => {
    if (confirm(`Remove ${svc.name}?`)) {
      mutate(() => call("DELETE", `api/services/${svc.id}`), "removed");
    }
  }));

  const point = el("div", "svc__point");
  point.append(
    el("span", null, "point the caller here:"),
    el("code", "svc__command", svc.point_at),
    button("COPY", () => copy(svc.point_at)),
  );
  if (svc.error) {
    point.appendChild(el("span", "found__taken", svc.error));
  }
  row.appendChild(point);
  // Same condition as the stub toggle, and for the same reason: a rule only
  // reaches services whose ports are open, and the API refuses it otherwise.
  if (project.active) row.appendChild(renderBreak(svc));
  return row;
}

/* Breaking a service on purpose, in the row that already says what it is doing.
 *
 * Not a latched toggle like stubbing: a rule is what to do and how often, so the
 * fields are the control and ARM is the latch. They are the three the API takes
 * and the three the agent tool takes, in that order — a fourth way to say "break
 * this" is how two surfaces start disagreeing about what a rule is.
 *
 * A rule that would do nothing is refused by the API. The refusal is left on
 * screen as the API wrote it, with the fields exactly as they were typed:
 * redrawing the row would clear them and read as though something was armed. */
function renderBreak(svc) {
  const row = el("div", "svc__break");
  const rule = state.broken.get(svc.name) || "";

  /* The reading before the controls. Whoever opens this panel while everything
   * is failing has to meet Sonda's own doing before the fields that caused it,
   * and a rule is a failure — just Sonda's, not the service's. */
  row.appendChild(el("span", "svc__break-state" + (rule ? " svc__break-state--armed" : ""),
    rule ? "BROKEN ON PURPOSE · " + rule : "BREAK ON PURPOSE"));

  const number = (text, title) => {
    const wrap = el("label", "svc__break-field");
    wrap.title = title;
    wrap.appendChild(el("span", "label", text));
    const input = document.createElement("input");
    input.type = "number";
    input.min = "0";
    wrap.appendChild(input);
    row.appendChild(wrap);
    return input;
  };

  const latency = number("LATENCY MS",
    "Added before the call is forwarded. The service still answers — this is the case a timeout is meant to catch.");
  const status = number("HTTP STATUS",
    "Answered with this status instead of forwarding. The service is never reached.");
  /* A count, and labelled as one. "3" is every third call in that order on
   * every run; a percentage would be a different sequence each time and turn a
   * failing test into a coin toss. */
  const oneIn = number("ONE CALL IN",
    "A count, not a percentage: 3 means every third call, in that order, every run. Leave it empty to break every call.");

  const cutWrap = el("label", "svc__break-cut");
  cutWrap.title = "Drop the connection without answering at all — the failure a client handles differently from a 500, and the one almost nobody tests.";
  const cut = document.createElement("input");
  cut.type = "checkbox";
  cutWrap.append(cut, el("span", "label", "CUT"));
  row.appendChild(cutWrap);

  const arm = button(rule ? "REPLACE" : "ARM", async () => {
    const armed = await mutate(() => call("POST", "api/faults", {
      service: svc.name,
      latency_ms: Number(latency.value) || 0,
      status: Number(status.value) || 0,
      cut: cut.checked,
      one_in: Number(oneIn.value) || 0,
    }), `${svc.name} is being broken on purpose — until you restore it, or Sonda restarts`);
    if (!armed) return;
    await reload();
    renderAdmin();
  });
  arm.title = "Sonda applies this to every call it forwards to " + svc.name +
    ". Nothing is written down: rules are forgotten when Sonda restarts.";
  row.appendChild(arm);

  if (rule) {
    const restore = button("RESTORE", async () => {
      await mutate(() => call("POST", "api/faults", { service: svc.name, clear: true }),
        `${svc.name} is answering for itself again`);
      await reload();
      renderAdmin();
    });
    restore.title = `Take the rule off ${svc.name} and forward its calls untouched again.`;
    row.appendChild(restore);
  }
  return row;
}

function renderServiceForm(project) {
  const form = el("form", "form");

  const name = field(form, "NAME", "text", "ms-auth");
  const listen = field(form, "SONDA LISTENS ON", "text", "127.0.0.1:9152");
  const upstream = field(form, "THE REAL SERVICE", "text", "http://127.0.0.1:50052");

  const protoWrap = el("div", "form__field");
  protoWrap.appendChild(el("span", "label", "PROTOCOL"));
  const protocol = document.createElement("select");
  for (const value of ["grpc", "http", "postgres", "amqp"]) {
    const option = document.createElement("option");
    option.value = option.textContent = value;
    protocol.appendChild(option);
  }
  protoWrap.appendChild(protocol);
  form.appendChild(protoWrap);

  /* A database is declared as postgres://host:port, and never with the user and
   * password from a DATABASE_URL: Sonda forwards the client's own handshake, so
   * a credential here would only be a password written into the capture file. */
  const upstreamHint = el("p", "note", "");
  form.appendChild(upstreamHint);

  const checkbox = (id, text) => {
    const row = el("div", "form__row");
    const input = document.createElement("input");
    input.type = "checkbox";
    input.id = id;
    const label = el("label", "note", " " + text);
    label.htmlFor = input.id;
    row.append(input, label);
    form.appendChild(row);
    return input;
  };

  const reflection = checkbox(`refl-${project.id}`,
    "ask this service for its schema (reflection)");

  /* Two separate switches because they are two separate decisions, and folding
   * them into one "use TLS" would make turning on encryption also turn off the
   * check that gives it any value. */
  const terminate = checkbox(`tls-${project.id}`,
    "answer this port with TLS, so the caller can use https:// or amqps://");
  const skipVerify = checkbox(`noverify-${project.id}`,
    "do not check the upstream's certificate (only for https:// or amqps:// upstreams)");

  const skipHint = el("p", "note note--fault", "");
  skipVerify.addEventListener("change", () => {
    /* Said at the moment of the decision rather than buried in documentation:
     * whoever ticks this is about to stop identifying the far end, and the
     * consequences follow the service everywhere afterwards. */
    skipHint.textContent = skipVerify.checked
      ? "Anything that answers on that address will be trusted. Every capture taken through this service is recorded as unverified, and the service is shown as unverified everywhere."
      : "";
  });
  form.appendChild(skipHint);

  /* A database is declared as postgres://host:port, and never with the user and
   * password from a DATABASE_URL: Sonda forwards the client's own handshake, so
   * a credential here would only be a password written into the capture file. */
  protocol.addEventListener("change", () => {
    const pg = protocol.value === "postgres";
    const amqp = protocol.value === "amqp";
    upstream.placeholder = pg ? "postgres://127.0.0.1:5432" :
      (amqp ? "amqp://127.0.0.1:5672" : "http://127.0.0.1:50052");
    upstreamHint.textContent = pg
      ? "No user and no password: the client's own handshake is forwarded untouched. A database negotiates encryption inside its own protocol, so Sonda cannot terminate TLS in front of it."
      : (amqp ? "No user and no password in the upstream URL: the client's AMQP handshake is forwarded untouched, and SASL responses are blanked before capture." : "");
    /* Offering a switch Sonda will refuse to save is worse than not offering
     * it: the refusal arrives after the form has been filled in. */
    terminate.disabled = pg;
    if (pg) terminate.checked = false;
  });

  const actions = el("div", "form__row");
  actions.appendChild(submit("ADD SERVICE"));
  form.appendChild(actions);

  form.addEventListener("submit", (event) => {
    event.preventDefault();
    mutate(() => call("POST", `api/projects/${project.id}/services`, {
      name: name.value.trim(),
      listen: listen.value.trim(),
      upstream: upstream.value.trim(),
      protocol: protocol.value,
      reflection: reflection.checked,
      tls: terminate.checked,
      insecure_skip_verify: skipVerify.checked,
    }), "added");
  });
  return form;
}

function renderProjectTools(project) {
  const row = el("div", "form");
  const tools = el("div", "form__row");

  // Reading the addresses out of a file the project already has beats asking
  // for fifteen of them by hand, which is how a tool like this gets abandoned.
  tools.appendChild(filePicker("IMPORT FROM A FILE", async (filename, text) => {
    const result = await call("POST", `api/discover?filename=${encodeURIComponent(filename)}`,
      text, { contentType: "text/plain" });
    admin.target = project.id;
    admin.found = result.found;
    note(`${result.found.length} services found in ${filename}`, "ok");
    renderAdmin();
  }));

  const descriptor = project.has_descriptor
    ? `schemas: ${project.descriptor_name}`
    : "no schemas uploaded";
  tools.appendChild(el("span", "note", descriptor));

  tools.appendChild(filePicker("UPLOAD SCHEMAS", async (filename, _text, bytes) => {
    const result = await call("POST",
      `api/projects/${project.id}/descriptor?name=${encodeURIComponent(filename)}`,
      bytes, { contentType: "application/octet-stream" });
    await loadProjects();
    note(`${result.services} services in ${filename}`, "ok");
    renderAdmin();
  }, true));

  row.appendChild(tools);
  return row;
}

function renderFound(project) {
  const box = el("div", "found-panel");
  const head = el("div", "found-panel__head");
  head.appendChild(el("span", "label", "FOUND — review before adding"));
  head.appendChild(button("ADD ALL", () => addFound(project, admin.found.filter((f) => !f.port_taken))));
  head.appendChild(button("DISCARD", () => { admin.found = null; renderAdmin(); }));
  box.appendChild(head);

  for (const f of admin.found) {
    const row = el("div", "found");
    row.append(
      button("+", () => addFound(project, [f])),
      el("span", "svc__name", f.name),
      el("span", "svc__cell svc__cell--faint", f.protocol),
      el("span", "svc__cell svc__cell--faint", f.upstream),
      el("span", f.port_taken ? "found__taken" : "svc__cell", f.listen),
      // Where it was read from, so a wrong reading is visible before saving.
      el("span", "found__source", f.port_taken ? "port already in use" : f.source),
    );
    box.appendChild(row);
  }
  return box;
}

async function addFound(project, entries) {
  if (!entries.length) {
    note("nothing to add: every suggested port is already in use", "fault");
    return;
  }
  await mutate(async () => {
    let last = null;
    for (const f of entries) {
      last = await call("POST", `api/projects/${project.id}/services`, {
        name: f.name, listen: f.listen, upstream: f.upstream,
        protocol: f.protocol, reflection: false,
        // The variable the address was read from, passed on rather than shown:
        // it is the evidence that lets the pointing be undone later, and a file
        // imported here is disconnected by an agent just as often as one
        // connected by an agent. A compose file has none, and sends none.
        env_key: f.key || "",
      });
    }
    admin.found = null;
    return last;
  }, `${entries.length} services added`);
}

function renderNewProject() {
  const form = el("form", "form");
  const name = field(form, "NEW PROJECT", "text", "core-delpagroup");
  const row = el("div", "form__row");
  row.appendChild(submit("CREATE"));
  form.appendChild(row);

  form.addEventListener("submit", (event) => {
    event.preventDefault();
    if (!name.value.trim()) return;
    mutate(() => call("POST", "api/projects", { name: name.value.trim() }), "created");
  });
  return form;
}

/* ------------------------------------------------------- admin pieces -- */

function field(form, label, type, placeholder) {
  const wrap = el("div", "form__field");
  wrap.appendChild(el("span", "label", label));
  const input = document.createElement("input");
  input.type = type;
  input.placeholder = placeholder;
  wrap.appendChild(input);
  form.appendChild(wrap);
  return input;
}

function button(label, onClick) {
  const b = el("button", "switch__key switch__key--lone", label);
  b.type = "button";
  b.addEventListener("click", onClick);
  return b;
}

function submit(label) {
  const b = el("button", "switch__key switch__key--lone", label);
  b.type = "submit";
  return b;
}

function filePicker(label, handler, asBytes) {
  const wrap = el("label", "switch__key switch__key--lone", label);
  wrap.style.cursor = "pointer";
  const input = document.createElement("input");
  input.type = "file";
  input.hidden = true;
  input.addEventListener("change", async () => {
    const file = input.files && input.files[0];
    if (!file) return;
    try {
      note("reading " + file.name + "…");
      if (asBytes) {
        await handler(file.name, null, await file.arrayBuffer());
      } else {
        await handler(file.name, await file.text());
      }
    } catch (err) {
      note(err.message, "fault");
    } finally {
      input.value = "";
    }
  });
  wrap.appendChild(input);
  return wrap;
}

function copy(text) {
  if (navigator.clipboard) {
    navigator.clipboard.writeText(text).then(
      () => note("copied", "ok"),
      () => note("could not copy; select the line instead", "fault"));
    return;
  }
  note("copying is not available here; select the line instead", "fault");
}

// After a project change the channels, the captures and the counts all belong
// to a different system, so the field is rebuilt rather than filtered.
async function boot_reloadAfterProjectChange() {
  try {
    const res = await fetch("api/targets");
    const body = await res.json();
    state.targets = body.targets;
    state.byName = new Map(state.targets.map((t) => [t.name, t]));
    state.selected = 0;
    state.detail = null;
    renderRail();
    renderLanes();
    dom.railFoot.textContent = state.targets.length === 1
      ? "1 channel armed"
      : state.targets.length + " channels armed";
    await reload();
  } catch (err) {
    console.error("sonda: could not refresh after the project changed", err);
  }
}

boot();
