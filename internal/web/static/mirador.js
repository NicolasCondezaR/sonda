/* Mirador — the instrument face.
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
  detail: null,             // the call currently in the inspector
  totals: { calls: 0, dropped: 0, byTarget: new Map() },
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
  readout: document.getElementById("readout"),
  acquisition: document.getElementById("acquisition"),
  acquisitionText: document.getElementById("acquisition-text"),
  search: document.getElementById("search"),
  hold: document.getElementById("hold"),
  inspector: document.getElementById("inspector"),
  inspectorIdle: document.getElementById("inspector-idle"),
  inspectorBody: document.getElementById("inspector-body"),
  diffBody: document.getElementById("diff-body"),
};

/* ------------------------------------------------------------- helpers -- */

function el(tag, className, text) {
  const node = document.createElement(tag);
  if (className) node.className = className;
  if (text !== undefined && text !== null) node.textContent = String(text);
  return node;
}

function isFault(call) {
  if (call.error) return true;
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
  if (call.error) return "TRANSPORT";
  if (call.grpc_status_text) return call.grpc_status_text.toUpperCase();
  return String(call.status);
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

    const swatch = el("span", "channel__swatch" + (target.hollow ? " channel__swatch--hollow" : ""));
    row.append(swatch, el("span", "channel__name", target.name));
    if (target.protocol === "grpc") row.appendChild(el("span", "channel__proto", "gRPC"));
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
  measure();
}

function addEvent(call, isNew) {
  const target = state.byName.get(call.target);
  if (!target || !target.track) return;

  const fault = isFault(call);
  const node = el("button", "ev" + (fault ? " ev--fault" : "") + (isNew ? " ev--new" : ""));
  node.type = "button";
  node.style.left = xFor(call.started_at) + "px";
  if (!fault) {
    node.style.width = Math.max(3, call.duration_ms * state.pxPerMs) + "px";
  }
  node.setAttribute("aria-pressed", "false");
  node.setAttribute("aria-label",
    `${call.method} ${call.path} on ${call.target}, ${outcome(call)}, ${duration(call.duration_ms)}`);
  node.title = `${outcome(call)}  ${call.method} ${call.path}\n${duration(call.duration_ms)} · ${clockTime(call.started_at)}`;

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
  for (const [id, node] of state.nodes) {
    if (parseFloat(node.style.left) + offset < -80) {
      node.remove();
      state.nodes.delete(id);
      state.calls.delete(id);
    }
  }
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

  const noneShown = shown === 0;
  dom.empty.hidden = !noneShown;
  if (noneShown) {
    if (state.totals.calls === 0) {
      dom.emptyHeadline.textContent = "ARMED";
      dom.emptyNote.textContent = "No traffic captured yet. Point a client at a channel's listen port.";
    } else if (state.filter === "failed") {
      dom.emptyHeadline.textContent = "NO FAULTS";
      dom.emptyNote.textContent =
        `${state.totals.calls} calls captured, none flagged. Switch to ALL to see the whole field.`;
    } else {
      dom.emptyHeadline.textContent = "NO MATCH";
      dom.emptyNote.textContent = "Nothing in this window matches the current filter.";
    }
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
    const [callsRes, statsRes] = await Promise.all([
      fetch("api/calls?" + query()),
      fetch("api/stats"),
    ]);
    if (!callsRes.ok) throw new Error("calls " + callsRes.status);

    const { calls } = await callsRes.json();
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
    console.error("mirador: could not load calls", err);
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
      console.error("mirador: bad event", err);
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
  head.appendChild(el("div", "insp-head__path", call.method + " " + call.path));
  const meta = el("div", "insp-head__meta");
  meta.append(
    el("span", null, call.target),
    el("span", null, (call.protocol || "http").toUpperCase()),
    el("span", null, "HTTP " + call.status),
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
  if (call.error) {
    head.appendChild(el("div", "insp-head__fault", call.error));
  }
  out.appendChild(head);
  out.appendChild(renderActions(call));

  if (call.grpc) {
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

/* ------------------------------------------------------------- actions -- */

function renderActions(call) {
  const bar = el("div", "actions");

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
  return bar;
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

  dom.hold.addEventListener("click", () => {
    state.held = !state.held;
    dom.hold.setAttribute("aria-pressed", String(state.held));
    setAcquisition(state.held ? "held" : "live", state.held ? "HELD" : "LIVE");
    syncPolling();
    if (!state.held) reload();
  });

  document.addEventListener("keydown", (event) => {
    if (event.key === "Escape") closeInspector();
    if (event.key === "/" && document.activeElement !== dom.search) {
      event.preventDefault();
      dom.search.focus();
    }
  });

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
    dom.emptyNote.textContent = "Mirador's API did not answer. Is the process still running?";
    dom.empty.hidden = false;
    console.error("mirador: could not read targets", err);
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

boot();
