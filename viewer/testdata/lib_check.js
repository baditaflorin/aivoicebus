// Table checks for ui/lib.js (the address grammar and the identity reading). Run by TestLibJS in
// ../ui_test.go with `node testdata/lib_check.js`; prints "ok N" or throws.
"use strict";
const assert = require("node:assert/strict");
const lib = require("../ui/lib.js");

let n = 0;
function eq(got, want, what) {
  n++;
  assert.deepEqual(got, want, what);
}
const none = { session: "", thread: "", hidden: false };

// parse: the grammar, then everything outside it.
const parse = [
  ["", none],
  ["#", none],
  ["#thread/a1b2c3d4e5", { ...none, thread: "a1b2c3d4e5" }],
  ["thread/a1b2c3d4e5", { ...none, thread: "a1b2c3d4e5" }],
  ["#session/host%2Fclaude.3b89", { ...none, session: "host/claude.3b89" }],
  ["#session/host%2fclaude.3b89", { ...none, session: "host/claude.3b89" }],
  ["#session/host%2Fclaude.3b89/thread/a1b2c3d4e5", { session: "host/claude.3b89", thread: "a1b2c3d4e5", hidden: false }],
  ["#thread/a1b2c3d4e5/session/host%2Fclaude.3b89", { session: "host/claude.3b89", thread: "a1b2c3d4e5", hidden: false }],
  ["#?hidden=1", { ...none, hidden: true }],
  ["#thread/abc123?hidden=1", { ...none, thread: "abc123", hidden: true }],
  ["#session/x%2Fy.z?hidden=1&junk=2", { ...none, session: "x/y.z", hidden: true }],
  ["#thread/abc123?hidden=0", { ...none, thread: "abc123" }],
  ["#thread/abc123?hidden=11", { ...none, thread: "abc123" }],
  ["#session/laptop-a%2Fclaude.a1b2", { ...none, session: "laptop-a/claude.a1b2" }],
  ["#session/imac", { ...none, session: "imac" }],
  ["#session/%C3%A9t%C3%A9%2Fclaude.1", { ...none, session: "été/claude.1" }],
  // hostile and malformed
  ["#thread/<script>alert(1)</script>", none],
  ["#thread/%3Cscript%3Ealert(1)%3C%2Fscript%3E", none],
  ["#session/%3Cimg%20src%3Dx%20onerror%3Dalert(1)%3E", none],
  ["#session/<script>/thread/abc123", { ...none, thread: "abc123" }],
  ["#thread/abc123%00", none],
  ["#thread/abc%20123", none],
  ["#thread/../../etc/passwd", none],
  ["#thread/%2e%2e%2f%2e%2e", none],
  ["#thread/%E0%A4%A", none], // malformed percent-encoding
  ["#thread/%25252e", none],
  ["#session/host%252Fclaude", none], // double-encoded
  ["#session/a%2Fb%2Fc", none], // two slashes
  ["#session/%2Fclaude", none],
  ["#session/host%2F", none],
  ["#session/javascript:alert(1)//x", none],
  ["#session/a%0Ab", none],
  ["#session/a%E2%80%AEb", none], // bidi override
  ["#thread/" + "a".repeat(65), none],
  ["#thread/" + "a".repeat(64), { ...none, thread: "a".repeat(64) }],
  ["#session/" + "a".repeat(201), none],
  ["#session/" + "a".repeat(200), { ...none, session: "a".repeat(200) }],
  ["#frob/1/thread/abc123", none], // an unknown segment ends what is understood
  ["#thread/abc123/frob/1", { ...none, thread: "abc123" }],
  ["#thread/abc123/thread/other", { ...none, thread: "abc123" }], // first wins
  ["#thread", none],
  ["#thread/", none],
  ["#/thread/abc123", { ...none, thread: "abc123" }],
  ["#" + "x".repeat(10 * 1024), none],
  ["#thread/abc123?" + "q".repeat(10 * 1024), none],
  ["#thread/" + "%41".repeat(3000), none],
  ["#thread/abc123?hidden=1" + "&a=b".repeat(300), { ...none, thread: "abc123", hidden: true }],
  [null, none],
  [undefined, none],
  [42, none],
  [{}, none],
];
for (const [raw, want] of parse) eq(lib.parseHash(raw), want, "parse " + String(raw).slice(0, 60));

// format: canonical, and only what the parser takes.
const fmt = [
  [none, ""],
  [{}, ""],
  [{ hidden: true }, "#?hidden=1"],
  [{ thread: "a1b2c3d4e5" }, "#thread/a1b2c3d4e5"],
  [{ session: "host/claude.3b89" }, "#session/host%2Fclaude.3b89"],
  [{ session: "host/claude.3b89", thread: "a1b2c3d4e5", hidden: true }, "#session/host%2Fclaude.3b89/thread/a1b2c3d4e5?hidden=1"],
  [{ session: "<script>", thread: "x y" }, ""],
  [{ session: "<script>", thread: "abc123" }, "#thread/abc123"],
  [{ session: "a/b/c" }, ""],
  [{ thread: "a".repeat(65) }, ""],
  [{ session: "a".repeat(201) }, ""],
  [{ session: 5, thread: {}, hidden: 1 }, "#?hidden=1"],
  [{ session: "été/claude.1" }, "#session/%C3%A9t%C3%A9%2Fclaude.1"],
];
for (const [s, want] of fmt) eq(lib.formatHash(s), want, "format " + JSON.stringify(s).slice(0, 60));

// round trip: format(parse(h)) is a fixed point, and parse(format(s)) gives s back for every valid s.
for (const [raw] of parse) {
  const once = lib.formatHash(lib.parseHash(raw));
  eq(lib.formatHash(lib.parseHash(once)), once, "fixed point " + String(raw).slice(0, 40));
  eq(/[<>"'`\s]/.test(once), false, "no markup characters in " + once);
}
for (const s of [none, { ...none, thread: "abc123" }, { session: "h/claude.1", thread: "abc123", hidden: true }, { ...none, hidden: true }]) {
  eq(lib.parseHash(lib.formatHash(s)), s, "round trip " + JSON.stringify(s));
}

// refs
eq(lib.formatRef("thread", "a1b2c3d4e5"), "thr:a1b2c3d4", "thread ref");
eq(lib.formatRef("thread", "abc123"), "thr:abc123", "short thread id");
eq(lib.formatRef("session", "host/claude.3b89"), "ses:host/claude.3b89", "session ref");
eq(lib.formatRef("thread", "<b>"), "", "bad thread ref");
eq(lib.formatRef("session", "a b"), "", "bad session ref");
eq(lib.formatRef("other", "abc123"), "", "bad kind");
eq(lib.parseRef(" thr:a1b2c3 "), { kind: "thread", value: "a1b2c3" }, "parse thr");
eq(lib.parseRef("ses:host/claude.3b89"), { kind: "session", value: "host/claude.3b89" }, "parse ses");
for (const bad of ["thr:abc", "thr:", "thr:<script>", "ses:", "ses:<b>", "msg:ab12cd", "", "thr:" + "a".repeat(10 * 1024), null, 7]) eq(lib.parseRef(bad), null, "bad ref " + String(bad).slice(0, 20));

// identity: product, colour tone, short id, host; never guessed, never trusted blindly.
const id = [
  ["host/claude.3b89", undefined, { host: "host", product: "claude", tone: "claude", letter: "C", short: "3b89" }],
  ["build-1/codex.q1w2", undefined, { host: "build-1", product: "codex", tone: "codex", letter: "C", short: "q1w2" }],
  ["imac/gemini.z7y6", undefined, { host: "imac", product: "gemini", tone: "gemini", letter: "G", short: "z7y6" }],
  ["imac/grok.1234", undefined, { host: "imac", product: "grok", tone: "other", letter: "G", short: "1234" }],
  ["imac/cursor-agent.1", undefined, { host: "imac", product: "cursor-agent", tone: "other", letter: "C", short: "1" }],
  ["imac/mystery.1", undefined, { host: "imac", product: "mystery", tone: "other", letter: "M", short: "1" }],
  ["imac/claude", undefined, { host: "imac", product: "claude", tone: "claude", letter: "C", short: "" }],
  ["imac", undefined, { host: "", product: "", tone: "unknown", letter: "?", short: "imac" }],
  ["host/claude.3b89", "codex", { host: "host", product: "codex", tone: "codex", letter: "C", short: "3b89" }],
  ["host/claude.3b89", "<script>", { host: "host", product: "", tone: "unknown", letter: "?", short: "3b89" }],
  ["host/CLAUDE.1", undefined, { host: "host", product: "claude", tone: "claude", letter: "C", short: "1" }],
  ["host/constructor.1", undefined, { host: "host", product: "constructor", tone: "other", letter: "C", short: "1" }],
  ["host/__proto__.1", undefined, { host: "host", product: "", tone: "unknown", letter: "?", short: "1" }],
  ["host/" + "x".repeat(40) + ".1", undefined, { host: "host", product: "", tone: "unknown", letter: "?", short: "1" }],
  ["host/claude." + "z".repeat(40), undefined, { host: "host", product: "claude", tone: "claude", letter: "C", short: "z".repeat(16) }],
  [null, undefined, { host: "", product: "", tone: "unknown", letter: "?", short: "" }],
];
for (const [me, tool, want] of id) eq(lib.identityParts(me, tool), want, "identity " + me + " " + tool);

console.log("ok " + n);
