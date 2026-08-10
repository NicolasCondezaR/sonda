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
  stubbed: new Set(),        // services answering from recordings, not the wire
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

    const stubbed = state.stubbed.has(target.name);
    const swatch = el("span", "channel__swatch" +
      (target.hollow ? " channel__swatch--hollow" : "") +
      (stubbed ? " channel__swatch--stub" : ""));
    row.append(swatch, el("span", "channel__name", target.name));
    if (target.protocol === "grpc") row.appendChild(el("span", "channel__proto", "gRPC"));
    // Stubbing is a mode the service is in, not a property of one call, so it
    // is engraved on the channel rather than repeated on every event.
    if (stubbed) {
      row.appendChild(el("span", "channel__stub", "STUB"));
      row.title += " — answering from recordings, not being called";
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
  measure();
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
    `${call.method} ${call.path} on ${call.target}, ${outcome(call)}${served}, ${duration(call.duration_ms)}`);
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
    const [callsRes, statsRes, stubRes] = await Promise.all([
      fetch("api/calls?" + query()),
      fetch("api/stats"),
      fetch("api/stub"),
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

  // Provenance before anything else. Everything below is a reading of something
  // recorded earlier, and a reader who scrolls straight to the payload would
  // otherwise take it for what just happened.
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

  out.appendChild(head);
  out.appendChild(renderActions(call));
  out.appendChild(renderTracePlaceholder(call));

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

  dom.openProjects.addEventListener("click", openAdmin);
  dom.closeAdmin.addEventListener("click", closeAdmin);

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
};

async function loadProjects() {
  const res = await fetch("api/projects");
  if (!res.ok) throw new Error("could not read projects");
  admin.projects = (await res.json()).projects;
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
  } catch (err) {
    note(err.message, "fault");
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

  dom.adminBody.replaceChildren(out);
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

function renderService(project, svc) {
  const row = el("div", "svc");
  row.append(
    el("span", "svc__name", svc.name),
    el("span", "svc__cell svc__cell--faint", svc.protocol),
    el("span", "svc__cell", svc.listen),
    el("span", "svc__cell svc__cell--faint", svc.upstream),
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
  for (const value of ["grpc", "http"]) {
    const option = document.createElement("option");
    option.value = option.textContent = value;
    protocol.appendChild(option);
  }
  protoWrap.appendChild(protocol);
  form.appendChild(protoWrap);

  const row = el("div", "form__row");
  const reflection = document.createElement("input");
  reflection.type = "checkbox";
  reflection.id = `refl-${project.id}`;
  const reflLabel = el("label", "note", " ask this service for its schema (reflection)");
  reflLabel.htmlFor = reflection.id;
  row.append(reflection, reflLabel);
  row.appendChild(submit("ADD SERVICE"));
  form.appendChild(row);

  form.addEventListener("submit", (event) => {
    event.preventDefault();
    mutate(() => call("POST", `api/projects/${project.id}/services`, {
      name: name.value.trim(),
      listen: listen.value.trim(),
      upstream: upstream.value.trim(),
      protocol: protocol.value,
      reflection: reflection.checked,
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
