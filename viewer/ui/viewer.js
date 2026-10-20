// @ts-check
// The aivoicebus viewer page. Everything shown comes from agents and is untrusted, so the page builds
// its DOM with createElement and textContent only: no markup is ever made from data (a test bans the
// HTML-writing APIs in this file). It reads the viewer's JSON API and never writes.
"use strict";

/**
 * @typedef {{me: string, title?: string, tool: string, model?: string, machine: string, repo?: string, branch?: string,
 *   caps?: string[], last_seen: string, sent: number, received: number, threads: number, hidden?: string}} Session
 * @typedef {{me: string, title?: string}} Participant
 * @typedef {{thread: string, subject?: string, participants: Participant[], audience?: string[],
 *   messages: number, first: string, last: string, last_from: string, withdrawn?: number,
 *   threads?: string[], hidden?: boolean, state?: string, state_by?: string}} Conversation
 * @typedef {{id: string, from: string, from_title?: string, from_model?: string, to: string, re?: string, body: string,
 *   at: string, prio?: string, kind?: string, retracted?: object, read_by: number, readers?: string[],
 *   taken_by?: string, state?: string, state_by?: string, recipients: string[]}} ThreadMessage
 * @typedef {{title: string, rev: number, demo: boolean, sessions: number, messages: number,
 *   last_pull: string|null, error: string}} Status
 */

const ONLINE_MS = 10 * 60 * 1000;
const POLL_MS = 4000;

/** @type {{rev: number, session: string, thread: string, sessions: Session[], showHidden: boolean}} */
const state = { rev: -1, session: "", thread: "", sessions: [], showHidden: false };

/**
 * @param {string} tag
 * @param {string} [cls]
 * @param {string} [text]
 * @returns {HTMLElement}
 */
function el(tag, cls, text) {
  const e = document.createElement(tag);
  if (cls) e.className = cls;
  if (text !== undefined) e.textContent = text;
  return e;
}

/** @param {string} id */
function byId(id) {
  const e = document.getElementById(id);
  if (!e) throw new Error("missing #" + id);
  return e;
}

/** @param {HTMLElement} e */
function clear(e) {
  while (e.firstChild) e.removeChild(e.firstChild);
}

/** @param {string} path */
async function get(path) {
  const r = await fetch(path, { credentials: "same-origin", cache: "no-store" });
  if (!r.ok) throw new Error(path + ": " + r.status);
  return r.json();
}

/** @param {string|null|undefined} iso */
function ago(iso) {
  if (!iso) return "never";
  const s = Math.max(0, (Date.now() - Date.parse(iso)) / 1000);
  if (s < 60) return "just now";
  if (s < 3600) return Math.floor(s / 60) + "m ago";
  if (s < 86400) return Math.floor(s / 3600) + "h ago";
  return Math.floor(s / 86400) + "d ago";
}

/** Local time: the clock alone for today, else the date and the zone too (as the CLI shows it).
 * @param {string} iso */
function clock(iso) {
  const d = new Date(iso);
  if (d.toDateString() === new Date().toDateString()) {
    return d.toLocaleTimeString(undefined, { hour: "2-digit", minute: "2-digit" });
  }
  return d.toLocaleString(undefined, { month: "short", day: "numeric", hour: "2-digit", minute: "2-digit", timeZoneName: "short" });
}

/** @param {Participant} p */
function person(p) {
  const box = el("span", "person");
  box.title = p.me;
  box.appendChild(el("b", "", p.title || p.me));
  if (p.title) box.appendChild(el("span", "", p.me));
  return box;
}

/** @param {Session} s */
function card(s) {
  const b = /** @type {HTMLButtonElement} */ (el("button", "card"));
  b.type = "button";
  if (s.me === state.session) b.classList.add("selected");
  const head = el("div", "card-title" + (s.title ? "" : " untitled"));
  const dot = el("span", "presence");
  if (Date.now() - Date.parse(s.last_seen) < ONLINE_MS) dot.classList.add("online");
  dot.title = "last seen " + ago(s.last_seen);
  head.appendChild(dot);
  head.appendChild(el("span", "", s.title || "untitled session"));
  b.appendChild(head);
  b.appendChild(el("div", "ident", s.me));
  const chips = el("div", "chips");
  if (s.tool) chips.appendChild(el("span", "chip", s.tool));
  if (s.model) chips.appendChild(el("span", "chip model", s.model));
  if (s.machine) chips.appendChild(el("span", "chip", s.machine));
  if (s.repo) chips.appendChild(el("span", "chip repo", s.branch ? s.repo + " @ " + s.branch : s.repo));
  // What task --needs matches: the declared words and roles get a chip (repo, branch, tool and model
  // have theirs above); the whole list is the chips' tooltip.
  const caps = s.caps || [];
  for (const c of caps) {
    if (!c.includes(":") || c.startsWith("role:")) chips.appendChild(el("span", "chip cap", c));
  }
  if (caps.length) chips.title = "can: " + caps.join(" ");
  b.appendChild(chips);
  b.appendChild(el("div", "meta", "seen " + ago(s.last_seen) + " · sent " + s.sent + " · received " + s.received + " · " + s.threads + " conversation(s)"));
  b.addEventListener("click", () => {
    state.session = state.session === s.me ? "" : s.me;
    load(true);
  });
  return b;
}

/** @param {Conversation} c */
function conversation(c) {
  const li = el("li");
  const b = /** @type {HTMLButtonElement} */ (el("button", "conv"));
  b.type = "button";
  b.dataset.thread = c.thread;
  if (c.thread === state.thread) b.classList.add("selected");
  const allWithdrawn = !!c.withdrawn && c.withdrawn === c.messages;
  b.appendChild(el("div", "conv-subject" + (c.subject ? "" : " muted"), c.subject || (allWithdrawn ? "(withdrawn by its sender)" : "(no subject)")));
  const people = el("div", "people");
  c.participants.forEach((p, i) => {
    if (i > 0) people.appendChild(el("span", "swap", "⇄"));
    people.appendChild(person(p));
  });
  (c.audience || []).forEach((a) => people.appendChild(el("span", "audience", "to " + a)));
  b.appendChild(people);
  let meta = c.messages + " message(s) · last " + ago(c.last) + " by " + c.last_from;
  if (c.threads && c.threads.length > 1) meta += " · one message sent to " + c.threads.length + " sessions";
  if (c.withdrawn) meta += " · " + c.withdrawn + " withdrawn";
  if (c.state) meta += " · " + c.state + (c.state_by ? " (" + c.state_by + ")" : "");
  b.appendChild(el("div", "meta", meta));
  b.addEventListener("click", () => {
    state.thread = c.thread;
    loadThread();
  });
  li.appendChild(b);
  return li;
}

/** @param {ThreadMessage} m */
function message(m) {
  const li = el("li", "msg" + (m.retracted ? " withdrawn" : ""));
  const head = el("div", "msg-head");
  head.appendChild(el("span", "who", m.from_title || m.from));
  if (m.from_title) head.appendChild(el("span", "ident", m.from));
  head.appendChild(el("span", "", "→ " + (m.recipients && m.recipients.length ? m.recipients.join(", ") : m.to)));
  head.appendChild(el("span", "", clock(m.at)));
  if (m.from_model) head.appendChild(el("span", "tag model", m.from_model));
  if (m.retracted) head.appendChild(el("span", "tag withdrawn", "withdrawn"));
  if (m.kind) head.appendChild(el("span", "tag", m.kind));
  if (m.prio && m.prio !== "ask") head.appendChild(el("span", "tag", m.prio));
  if (m.kind === "task" && !m.retracted) {
    head.appendChild(m.taken_by ? el("span", "tag taken", "taken by " + m.taken_by) : el("span", "tag open", "open: nobody took it yet"));
  }
  if (m.state && !m.retracted) {
    head.appendChild(el("span", "tag state state-" + m.state, "state: " + m.state + (m.state_by ? " by " + m.state_by : "")));
  }
  if (m.read_by > 0) {
    const r = el("span", "readers", "read by " + m.read_by);
    if (m.readers && m.readers.length) r.title = m.readers.join(", ");
    head.appendChild(r);
  }
  li.appendChild(head);
  li.appendChild(el("p", "body", m.body));
  return li;
}

async function loadThread() {
  const pane = byId("thread-pane");
  const list = byId("thread");
  if (!state.thread) {
    pane.hidden = true;
    byId("main-layout").classList.add("no-thread");
    document.querySelectorAll(".conv.selected").forEach((b) => b.classList.remove("selected"));
    return;
  }
  /** @type {ThreadMessage[]} */
  const ms = await get("api/thread?id=" + encodeURIComponent(state.thread));
  pane.hidden = false;
  byId("main-layout").classList.remove("no-thread");
  clear(list);
  const first = ms.find((m) => m.id === state.thread);
  byId("thread-subject").textContent = first && !first.retracted ? first.body.split("\n")[0].slice(0, 120) : "";
  ms.forEach((m) => list.appendChild(message(m)));
  document.querySelectorAll(".conv").forEach((b) => {
    const e = /** @type {HTMLElement} */ (b);
    e.classList.toggle("selected", e.dataset.thread === state.thread);
  });
}

/** @param {boolean} force */
async function load(force) {
  /** @type {Status} */
  const st = await get("api/status");
  const status = byId("status");
  clear(status);
  status.appendChild(el("span", "dot"));
  status.classList.toggle("stale", !!st.error);
  status.appendChild(document.createTextNode(
    st.demo ? "demo data, nothing real" : st.error ? "nodes not answering: " + st.error : "live · updated " + ago(st.last_pull)));
  byId("title").textContent = st.title;
  document.title = st.title;
  if (!force && st.rev === state.rev) return;
  state.rev = st.rev;

  /** @type {Session[]} */
  const sessions = await get("api/sessions");
  state.sessions = sessions;
  const cards = byId("sessions");
  clear(cards);
  const shown = sessions.filter((s) => state.showHidden || !s.hidden || s.me === state.session);
  shown.forEach((s) => cards.appendChild(card(s)));
  if (!sessions.length) cards.appendChild(el("div", "empty", "No session has been seen yet."));
  byId("n-sessions").textContent = String(shown.length);
  const hiddenN = sessions.length - sessions.filter((s) => !s.hidden).length;
  const toggle = byId("toggle-hidden");
  clear(toggle);
  if (hiddenN > 0) {
    const robots = sessions.filter((s) => s.hidden === "automation").length;
    const t = /** @type {HTMLButtonElement} */ (el("button", "link",
      state.showHidden ? "hide " + hiddenN : "show " + hiddenN + " hidden (" + robots + " probes and tests, " + (hiddenN - robots) + " with no messages)"));
    t.type = "button";
    t.addEventListener("click", () => {
      state.showHidden = !state.showHidden;
      load(true);
    });
    toggle.appendChild(t);
  }

  /** @type {Conversation[]} */
  const convs = await get("api/conversations" + (state.session ? "?session=" + encodeURIComponent(state.session) : ""));
  const list = byId("convs");
  clear(list);
  const shownConvs = convs.filter((c) => state.showHidden || !c.hidden);
  shownConvs.forEach((c) => list.appendChild(conversation(c)));
  if (!convs.length) list.appendChild(el("li", "empty", state.session ? "This session has no conversation yet." : "No conversation yet."));
  byId("n-convs").textContent = String(shownConvs.length);

  const filter = byId("filter");
  clear(filter);
  if (state.session) {
    const f = el("span", "filter");
    const s = sessions.find((x) => x.me === state.session);
    f.appendChild(document.createTextNode("with " + (s && s.title ? s.title : state.session)));
    const x = /** @type {HTMLButtonElement} */ (el("button", "", "×"));
    x.type = "button";
    x.title = "show every conversation";
    x.addEventListener("click", () => {
      state.session = "";
      load(true);
    });
    f.appendChild(x);
    filter.appendChild(f);
  }
  if (state.thread) await loadThread();
}

function start() {
  byId("close-thread").addEventListener("click", () => {
    state.thread = "";
    loadThread();
  });
  const tick = () => load(false).catch((e) => {
    const status = byId("status");
    status.classList.add("stale");
    status.textContent = "viewer not answering: " + String(e && e.message);
  });
  load(true).catch(() => tick());
  setInterval(tick, POLL_MS);
}

document.addEventListener("DOMContentLoaded", start);
