// Pure helpers of the viewer page: the address (URL hash) grammar and how an identity is read. No DOM
// here, so a test can run this file under Node; viewer.js loads it first and reads AVLib.
//
// The address is a hash, so it never reaches a server log or a Referer header, and it carries only
// ids and an identity, never a message body or a key:
//
//   #thread/<id>                      one conversation open
//   #session/<me>                     conversations of one session (<me> percent-encoded: host%2Fclaude.3b89)
//   #session/<me>/thread/<id>         both
//   <any of the above>?hidden=1       the probe and test sessions shown
//
// This is the same grammar as the hub's route.ts (#messages/thread/<id>, #messages/session/<me>), minus
// its "messages/" prefix and plus the combined form, because this page can hold both at once. Anything
// outside the whitelist is dropped, never repaired, and the writer only ever emits what the parser takes.
"use strict";

(function (root) {
  const MAX_HASH = 2048; // a longer address is not ours: it is ignored whole
  const MAX_THREAD = 64; // the server's own limit for a message or thread id
  const MAX_ME = 200; // msg.MaxIdentity
  const REF_MIN = 6; // shortest id prefix a ref may carry
  const REF_LEN = 8; // what the page writes

  const THREAD_RE = /^[A-Za-z0-9_-]{1,64}$/;
  // host/agent.suffix, or a bare name: letters, digits and . _ ~ @ : + - only, at most one slash.
  const ME_RE = /^[\p{L}\p{N}._~@:+-]+(?:\/[\p{L}\p{N}._~@:+-]+)?$/u;

  /** @param {unknown} s */
  function validThread(s) {
    return typeof s === "string" && s.length <= MAX_THREAD && THREAD_RE.test(s);
  }

  /** @param {unknown} s */
  function validMe(s) {
    return typeof s === "string" && s.length <= MAX_ME && ME_RE.test(s);
  }

  /** @param {string} s @returns {string|null} */
  function decode(s) {
    try {
      return decodeURIComponent(s);
    } catch (e) {
      return null;
    }
  }

  /**
   * @param {unknown} raw location.hash, with or without the leading #
   * @returns {{session: string, thread: string, hidden: boolean}}
   */
  function parseHash(raw) {
    const out = { session: "", thread: "", hidden: false };
    if (typeof raw !== "string" || raw.length > MAX_HASH) return out;
    const h = raw.charAt(0) === "#" ? raw.slice(1) : raw;
    const q = h.indexOf("?");
    const path = q < 0 ? h : h.slice(0, q);
    const query = q < 0 ? "" : h.slice(q + 1);
    for (const kv of query.split("&")) {
      if (kv === "hidden=1") out.hidden = true;
    }
    const segs = path.replace(/^\/+/, "").split("/");
    for (let i = 0; i + 1 < segs.length; i += 2) {
      const key = segs[i];
      const val = decode(segs[i + 1]);
      if (key === "thread" && !out.thread && validThread(val)) out.thread = /** @type {string} */ (val);
      else if (key === "session" && !out.session && validMe(val)) out.session = /** @type {string} */ (val);
      else if (key !== "thread" && key !== "session") break; // an unknown segment ends what we understand
    }
    return out;
  }

  /**
   * The canonical hash for a state: "" for the empty one, else "#..." Values that fail the whitelist are left out.
   * @param {{session?: string, thread?: string, hidden?: boolean}} s
   */
  function formatHash(s) {
    const parts = [];
    if (validMe(s.session)) parts.push("session", encodeURIComponent(/** @type {string} */ (s.session)));
    if (validThread(s.thread)) parts.push("thread", /** @type {string} */ (s.thread));
    const query = s.hidden ? "?hidden=1" : "";
    const path = parts.join("/");
    return path || query ? "#" + path + query : "";
  }

  /**
   * A short ref to paste in a chat or the hub's command palette: thr:<id prefix>, ses:<identity>.
   * @param {"thread"|"session"} kind
   * @param {string} value
   */
  function formatRef(kind, value) {
    if (kind === "thread" && validThread(value)) return "thr:" + value.slice(0, REF_LEN);
    if (kind === "session" && validMe(value)) return "ses:" + value;
    return "";
  }

  /**
   * @param {unknown} text
   * @returns {{kind: "thread"|"session", value: string}|null}
   */
  function parseRef(text) {
    if (typeof text !== "string" || text.length > MAX_ME + 8) return null;
    const t = text.trim();
    if (t.startsWith("thr:")) {
      const v = t.slice(4);
      return v.length >= REF_MIN && validThread(v) ? { kind: "thread", value: v } : null;
    }
    if (t.startsWith("ses:")) {
      const v = t.slice(4);
      return validMe(v) ? { kind: "session", value: v } : null;
    }
    return null;
  }

  // The products the council named; each gets a letter and one of four colours (CSS p-*). Never hue alone:
  // the badge also spells the name. Anything else is shown as "other", an absent one as "?".
  const PRODUCTS = {
    claude: "claude", codex: "codex", gemini: "gemini",
    grok: "other", "cursor-agent": "other", opencode: "other", aider: "other", goose: "other",
  };

  /**
   * Reads host/claude.3b89: host "host", product "claude", short id "3b89". Self-declared, so only a
   * name that looks like a product is believed; the rest stays unknown rather than guessed.
   * @param {string} me
   * @param {string} [tool] the roster's tool label, when there is one
   * @returns {{host: string, product: string, tone: string, letter: string, short: string}}
   */
  function identityParts(me, tool) {
    const s = typeof me === "string" ? me : "";
    const slash = s.indexOf("/");
    const host = slash < 0 ? "" : s.slice(0, slash);
    const agent = slash < 0 ? s : s.slice(slash + 1);
    const dot = agent.indexOf(".");
    const named = slash < 0 ? "" : dot < 0 ? agent : agent.slice(0, dot);
    let product = String(tool || named).toLowerCase();
    if (!/^[a-z][a-z0-9-]{0,31}$/.test(product)) product = "";
    const short = slash < 0 ? agent : dot < 0 ? "" : agent.slice(dot + 1);
    const tone = product ? (Object.prototype.hasOwnProperty.call(PRODUCTS, product) ? PRODUCTS[product] : "other") : "unknown";
    const letter = product ? product.charAt(0).toUpperCase() : "?";
    return { host, product, tone, letter, short: short.slice(0, 16) };
  }

  const api = { parseHash, formatHash, formatRef, parseRef, identityParts, validMe, validThread, MAX_HASH };
  if (typeof module === "object" && module && module.exports) module.exports = api;
  else root.AVLib = api;
})(typeof globalThis !== "undefined" ? globalThis : this);
