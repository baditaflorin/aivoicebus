// @ts-check
// The aivoicebus viewer page. Everything shown comes from agents and is untrusted, so the page builds
// its DOM with createElement and textContent only: no markup is ever made from data (a test bans the
// HTML-writing APIs in this file). It reads the viewer's JSON API and never writes. What is open lives in
// the address hash (see lib.js), and the hash is the source of truth: a click writes it, then the page
// reads it back, the same path as a pasted link or the back button.
"use strict";

const lib = /** @type {any} */ (globalThis).AVLib;

/**
 * @typedef {{me: string, title?: string, tool: string, model?: string, machine: string, repo?: string, branch?: string,
 *   last_seen: string, sent: number, received: number, threads: number, hidden?: string}} Session
 * @typedef {{me: string, title?: string}} Participant
 * @typedef {{thread: string, subject?: string, participants: Participant[], audience?: string[],
 *   messages: number, first: string, last: string, last_from: string, withdrawn?: number,
 *   threads?: string[], hidden?: boolean}} Conversation
 * @typedef {{id: string, from: string, from_title?: string, from_model?: string, to: string, re?: string, body: string,
 *   at: string, prio?: string, kind?: string, retracted?: object, read_by: number, readers?: string[],
 *   taken_by?: string, recipients: string[]}} ThreadMessage
 * @typedef {{title: string, rev: number, demo: boolean, sessions: number, messages: number,
 *   last_pull: string|null, error: string}} Status
 */

const ONLINE_MS = 10 * 60 * 1000;
const POLL_MS = 4000;

/** @type {{rev: number, session: string, thread: string, sessions: Session[], showHidden: boolean, missing: {thread: boolean, session: boolean}}} */
const state = { rev: -1, session: "", thread: "", sessions: [], showHidden: false, missing: { thread: false, session: false } };

/** The address the state asks for, and the hash as the page found it. */
function currentHash() {
  return lib.formatHash({ session: state.session, thread: state.thread, hidden: state.showHidden });
}

/**
 * Makes the page match the hash: parses it (a whitelist, so a hostile or stale hash selects nothing),
 * rewrites it in canonical form, and redraws what changed.
 * @param {boolean} first
 */
function sync(first) {
  const r = lib.parseHash(location.hash);
  const reload = first || r.session !== state.session || r.hidden !== state.showHidden;
  state.session = r.session;
  state.thread = r.thread;
  state.showHidden = r.hidden;
  const canon = currentHash();
  if (canon !== location.hash) history.replaceState(null, "", location.pathname + location.search + canon);
  return reload ? load(true) : loadThread();
}

/**
 * Selections add a history entry (back undoes them); a toggle only rewrites the current one.
 * @param {{session?: string, thread?: string, hidden?: boolean}} patch
 * @param {boolean} push
 */
function navigate(patch, push) {
  const next = lib.formatHash({ session: state.session, thread: state.thread, hidden: state.showHidden, ...patch });
  if (next !== lib.formatHash(lib.parseHash(location.hash))) {
    history[push ? "pushState" : "replaceState"](null, "", location.pathname + location.search + next);
  }
  return sync(false).catch(showError);
}

/** @param {unknown} e */
function showError(e) {
  const status = byId("status");
  status.classList.add("stale");
  status.textContent = "viewer not answering: " + String(e && /** @type {any} */ (e).message);
}

/**
 * Copies text. Where the clipboard API is missing or refused (an insecure page, no permission), the text is
 * put on the page, selected, for the person to copy by hand. Either way the button says what happened.
 * @param {HTMLElement} button
 * @param {string} text
 */
async function copyText(button, text) {
  const label = button.dataset.label || (button.dataset.label = button.textContent || "");
  const done = (/** @type {string} */ msg) => {
    button.textContent = msg;
    byId("announce").textContent = msg;
    setTimeout(() => {
      button.textContent = label;
    }, 1800);
  };
  try {
    await navigator.clipboard.writeText(text);
    done("Copied");
    return;
  } catch (e) {
    /* fall through to the manual way */
  }
  const box = byId("copy-fallback");
  box.hidden = false;
  box.textContent = text;
  const sel = window.getSelection();
  if (sel) {
    sel.removeAllRanges();
    const range = document.createRange();
    range.selectNodeContents(box);
    sel.addRange(range);
  }
  done("Press Ctrl/Cmd+C");
  setTimeout(() => {
    box.hidden = true;
  }, 10000);
}

/** The link to one thing: this page's own address plus its hash, never the query (which may hold the key). */
function linkTo(hash) {
  return location.origin + location.pathname + hash;
}

/**
 * @param {string} copyLinkId
 * @param {string} copyRefId
 * @param {() => {hash: string, ref: string}|null} target
 */
function wireCopy(copyLinkId, copyRefId, target) {
  byId(copyLinkId).addEventListener("click", (ev) => {
    const t = target();
    if (t) copyText(/** @type {HTMLElement} */ (ev.currentTarget), linkTo(t.hash));
  });
  byId(copyRefId).addEventListener("click", (ev) => {
    const t = target();
    if (t && t.ref) copyText(/** @type {HTMLElement} */ (ev.currentTarget), t.ref);
  });
}

/**
 * Who a session is, at a glance: a product badge (a letter, a colour and the name, never colour alone),
 * the title in bold (or "untitled session"), then the short id and host, muted. Text only.
 * @param {string} me
 * @param {string} [title]
 * @param {string} [tool] the roster's tool label, which beats the one in the identity
 * @returns {HTMLElement}
 */
function identityChip(me, title, tool) {
  const p = lib.identityParts(me, tool);
  const box = el("span", "idchip");
  box.title = me;
  const badge = el("span", "prod p-" + p.tone);
  badge.appendChild(el("b", "", p.letter));
  badge.appendChild(document.createTextNode(p.product ? p.product.slice(0, 16) : "unknown"));
  box.appendChild(badge);
  box.appendChild(el("b", "idname" + (title ? "" : " untitled"), title || "untitled session"));
  const meta = [p.short, p.host].filter(Boolean).join(" · ");
  box.appendChild(el("span", "idmeta", meta || me));
  return box;
}

/** @param {HTMLElement} banner @param {string} what */
function missingBanner(banner, what) {
  clear(banner);
  banner.appendChild(el("b", "", "Not found; the archive may still be syncing."));
  banner.appendChild(el("span", "", " " + what + " is not in this archive yet. This page keeps looking."));
  banner.hidden = false;
}

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

/** @param {string} iso */
function clock(iso) {
  const d = new Date(iso);
  return d.toLocaleString(undefined, { month: "short", day: "numeric", hour: "2-digit", minute: "2-digit" });
}

/** @param {Participant} p */
function person(p) {
  const box = el("span", "person");
  box.appendChild(identityChip(p.me, p.title));
  return box;
}

/** @param {Session} s */
function card(s) {
  const b = /** @type {HTMLButtonElement} */ (el("button", "card"));
  b.type = "button";
  if (s.me === state.session) b.classList.add("selected");
  const head = el("div", "card-title");
  const dot = el("span", "presence");
  if (Date.now() - Date.parse(s.last_seen) < ONLINE_MS) dot.classList.add("online");
  dot.title = "last seen " + ago(s.last_seen);
  head.appendChild(dot);
  head.appendChild(identityChip(s.me, s.title, s.tool));
  b.appendChild(head);
  const chips = el("div", "chips");
  if (s.model) chips.appendChild(el("span", "chip model", s.model));
  if (s.machine) chips.appendChild(el("span", "chip", s.machine));
  if (s.repo) chips.appendChild(el("span", "chip repo", s.branch ? s.repo + " @ " + s.branch : s.repo));
  b.appendChild(chips);
  b.appendChild(el("div", "meta", "seen " + ago(s.last_seen) + " · sent " + s.sent + " · received " + s.received + " · " + s.threads + " conversation(s)"));
  b.addEventListener("click", () => {
    navigate({ session: state.session === s.me ? "" : s.me }, true);
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
  b.appendChild(el("div", "meta", meta));
  b.addEventListener("click", () => {
    navigate({ thread: c.thread }, true);
  });
  li.appendChild(b);
  return li;
}

/** @param {ThreadMessage} m */
function message(m) {
  const li = el("li", "msg" + (m.retracted ? " withdrawn" : ""));
  const head = el("div", "msg-head");
  head.appendChild(identityChip(m.from, m.from_title));
  head.appendChild(el("span", "", "→ " + (m.recipients && m.recipients.length ? m.recipients.join(", ") : m.to)));
  head.appendChild(el("span", "", clock(m.at)));
  if (m.from_model) head.appendChild(el("span", "tag model", m.from_model));
  if (m.retracted) head.appendChild(el("span", "tag withdrawn", "withdrawn"));
  if (m.kind) head.appendChild(el("span", "tag", m.kind));
  if (m.prio && m.prio !== "ask") head.appendChild(el("span", "tag", m.prio));
  if (m.kind === "task" && !m.retracted) {
    head.appendChild(m.taken_by ? el("span", "tag taken", "taken by " + m.taken_by) : el("span", "tag open", "open: nobody took it yet"));
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
  const banner = byId("banner-thread");
  banner.hidden = true;
  if (!state.thread) {
    state.missing.thread = false;
    pane.hidden = true;
    byId("main-layout").classList.add("no-thread");
    document.querySelectorAll(".conv.selected").forEach((b) => b.classList.remove("selected"));
    return;
  }
  /** @type {ThreadMessage[]|null} */
  const ms = await get("api/thread?id=" + encodeURIComponent(state.thread));
  if (!state.thread) return; // closed while the answer was on its way
  pane.hidden = false;
  byId("main-layout").classList.remove("no-thread");
  clear(list);
  const found = !!ms && ms.length > 0;
  state.missing.thread = !found;
  if (!found) {
    missingBanner(banner, "Thread " + state.thread);
    byId("thread-subject").textContent = "";
  } else {
    const first = ms.find((m) => m.id === state.thread);
    byId("thread-subject").textContent = first && !first.retracted ? first.body.split("\n")[0].slice(0, 120) : "";
    ms.forEach((m) => list.appendChild(message(m)));
  }
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
  // A target that was not there yet is looked for again on every poll: the archive may be syncing.
  if (!force && st.rev === state.rev && !state.missing.thread && !state.missing.session) return;
  state.rev = st.rev;

  /** @type {Session[]} */
  const sessions = (await get("api/sessions")) || [];
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
      navigate({ hidden: !state.showHidden }, false);
    });
    toggle.appendChild(t);
  }

  /** @type {Conversation[]} */
  const convs = (await get("api/conversations" + (state.session ? "?session=" + encodeURIComponent(state.session) : ""))) || []; // null when there is none
  const list = byId("convs");
  clear(list);
  const shownConvs = convs.filter((c) => state.showHidden || !c.hidden);
  shownConvs.forEach((c) => list.appendChild(conversation(c)));
  // A session nobody has heard of, with no conversation either: say so rather than show an empty list.
  const known = sessions.find((x) => x.me === state.session);
  state.missing.session = !!state.session && !known && !convs.length;
  const sessionBanner = byId("banner-session");
  sessionBanner.hidden = true;
  if (state.missing.session) missingBanner(sessionBanner, "Session " + state.session);
  else if (!convs.length) list.appendChild(el("li", "empty", state.session ? "This session has no conversation yet." : "No conversation yet."));
  byId("n-convs").textContent = String(shownConvs.length);

  const head = byId("session-head");
  head.hidden = !state.session;
  const chip = byId("session-chip");
  clear(chip);
  if (state.session) chip.appendChild(identityChip(state.session, known && known.title, known && known.tool));
  await loadThread();
}

function start() {
  byId("close-thread").addEventListener("click", () => navigate({ thread: "" }, true));
  byId("clear-session").addEventListener("click", () => navigate({ session: "" }, true));
  wireCopy("copy-thread-link", "copy-thread-ref", () =>
    state.thread ? { hash: lib.formatHash({ thread: state.thread }), ref: lib.formatRef("thread", state.thread) } : null);
  wireCopy("copy-session-link", "copy-session-ref", () =>
    state.session ? { hash: lib.formatHash({ session: state.session }), ref: lib.formatRef("session", state.session) } : null);
  // The hash is the source of truth: a pasted link, the back button and a hand edit all land here.
  window.addEventListener("hashchange", () => sync(false).catch(showError));
  const tick = () => load(false).catch(showError);
  sync(true).catch(() => tick());
  setInterval(tick, POLL_MS);
}

document.addEventListener("DOMContentLoaded", start);
