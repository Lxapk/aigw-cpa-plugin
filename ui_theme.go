package main

// This file holds the shared visual language for the plugin's management pages.
//
// The look follows the credit-manager plug-in the operator asked to match:
// a soft green accent, white cards on a very light background, 12-14px radii,
// low-contrast borders and a restrained shadow. Rather than repeat the palette
// in each page's <style> block, it lives here and every page embeds it, so a
// change lands everywhere at once.
//
// Two constraints shape the markup:
//
//   - the page is rendered inside CPA's own panel, so it must not assume a
//     fixed viewport or try to take over the whole screen
//   - no <form> elements: CPA serves resource routes with GET only, so all
//     actions go through fetch() with the management key from localStorage
const uiCSS = `
:root {
  color-scheme: light dark;
  --accent: #1eb787;
  --accent-dark: #14926b;
  --accent-soft: #e4f7ef;
  --accent-2: #7968ee;
  --purple-soft: #efedff;
  --warn: #e5a12d;
  --warn-soft: #fdf3e2;
  --danger: #d95c51;
  --danger-soft: #fdf0ee;
  --bg: #fafbf8;
  --card-bg: #ffffff;
  --panel-2: #f7f9f6;
  --line: #e7ebe5;
  --text: #1a1f1c;
  --muted: #5f6a61;
  --shadow: 0 8px 28px rgba(41, 57, 46, .07);
  --shadow-sm: 0 4px 15px rgba(41, 57, 46, .04);
  font-family: -apple-system, BlinkMacSystemFont, "Segoe UI", "Noto Sans SC", system-ui, sans-serif;
}
@media (prefers-color-scheme: dark) {
  :root {
    --bg: #14171a;
    --card-bg: #1b1f23;
    --panel-2: #202428;
    --line: #2c3237;
    --text: #e6eae6;
    --muted: #9aa39c;
    --accent-soft: #14322a;
    --purple-soft: #232034;
    --warn-soft: #33291a;
    --danger-soft: #33211f;
    --shadow: 0 8px 28px rgba(0, 0, 0, .35);
    --shadow-sm: 0 4px 15px rgba(0, 0, 0, .25);
  }
}
* { box-sizing: border-box; }
body {
  margin: 0;
  padding: 0;
  background: var(--bg);
  color: var(--text);
  font-size: 14px;
  line-height: 1.6;
}
.root {
  max-width: 1180px;
  margin: 0 auto;
  padding: 22px 20px 44px;
  background:
    radial-gradient(680px 260px at 40% -10%, rgba(90, 219, 176, .14), transparent 72%),
    radial-gradient(560px 260px at 100% 6%, rgba(121, 104, 238, .06), transparent 76%);
}
/* ---------- header ---------- */
.hero { display: flex; flex-wrap: wrap; gap: 14px; justify-content: space-between; align-items: flex-end; margin: 4px 0 18px; }
.hero h1 { margin: 0; font-size: 1.55rem; line-height: 1.15; letter-spacing: -.035em; font-weight: 750; }
.hero h1::before { content: ""; display: inline-block; width: 8px; height: 8px; margin: 0 9px 4px 0; background: var(--accent); border-radius: 50%; }
.hero .sub { color: var(--muted); margin-top: 6px; font-size: .84rem; }
.hero .badge {
  display: inline-flex; align-items: center; gap: 6px;
  padding: 5px 11px; border-radius: 999px;
  background: var(--accent-soft); color: var(--accent-dark);
  font-size: .74rem; font-weight: 700;
}
/* ---------- tabs ---------- */
.tabs { display: flex; flex-wrap: wrap; gap: 6px; margin: 0 0 18px; border-bottom: 1px solid var(--line); padding-bottom: 0; }
.tabs button {
  border: 0; background: transparent; color: var(--muted);
  padding: 9px 14px; border-radius: 9px 9px 0 0;
  font: inherit; font-size: .86rem; font-weight: 650; cursor: pointer;
  border-bottom: 2px solid transparent; margin-bottom: -1px;
}
.tabs button:hover { color: var(--text); background: var(--panel-2); }
.tabs button.active { color: var(--accent-dark); border-bottom-color: var(--accent); background: var(--accent-soft); }
.panel { display: none; }
.panel.active { display: block; }
/* ---------- cards ---------- */
.card {
  background: var(--card-bg);
  border: 1px solid var(--line);
  border-radius: 14px;
  padding: 16px 18px;
  box-shadow: var(--shadow-sm);
  margin-bottom: 14px;
}
.card > h2 {
  margin: 0 0 12px; font-size: .95rem; font-weight: 720;
  display: flex; align-items: center; gap: 8px;
}
.card > h2 .hint { color: var(--muted); font-weight: 500; font-size: .78rem; }
.grid { display: grid; gap: 12px; }
.grid.stats { grid-template-columns: repeat(auto-fit, minmax(148px, 1fr)); }
.stat {
  position: relative; overflow: hidden;
  background: linear-gradient(135deg, var(--card-bg), var(--panel-2));
  border: 1px solid var(--line); border-radius: 12px;
  padding: 13px 15px; box-shadow: var(--shadow-sm);
}
.stat::before { content: ""; position: absolute; top: 12px; left: 15px; width: 16px; height: 2px; border-radius: 2px; background: var(--accent); }
.stat:nth-child(2n)::before { background: var(--accent-2); }
.stat:nth-child(3n)::before { background: var(--warn); }
.stat .k { color: var(--muted); font-size: .75rem; margin-top: 8px; }
.stat .v { font-size: 1.32rem; font-weight: 730; letter-spacing: -.025em; margin-top: 3px; font-variant-numeric: tabular-nums; word-break: break-all; }
/* ---------- controls ---------- */
.row { display: flex; flex-wrap: wrap; gap: 10px; align-items: center; margin: 8px 0; }
.row.tight { margin: 5px 0; }
label.field { display: inline-flex; align-items: center; gap: 7px; color: var(--muted); font-size: .82rem; }
input[type=password], input[type=text], input[type=number] {
  border: 1px solid var(--line); border-radius: 9px;
  background: var(--panel-2); color: var(--text);
  padding: 8px 11px; font: inherit; font-size: .86rem; outline: 0;
}
input[type=password], input[type=text] { font-family: ui-monospace, SFMono-Regular, Menlo, Consolas, monospace; }
input:focus { border-color: #76d7b8; box-shadow: 0 0 0 3px rgba(30, 183, 135, .13); background: var(--card-bg); }
input[type=number] { width: 5em; }
input[type=checkbox], input[type=radio] { accent-color: var(--accent); width: 15px; height: 15px; }
button {
  border: 0; border-radius: 9px; padding: 9px 15px;
  background: var(--accent); color: #fff;
  font: inherit; font-size: .85rem; font-weight: 700; cursor: pointer;
}
button:hover { background: var(--accent-dark); }
button:disabled { cursor: wait; opacity: .55; }
button.ghost { background: var(--panel-2); color: var(--text); border: 1px solid var(--line); }
button.ghost:hover { background: var(--accent-soft); color: var(--accent-dark); }
.opt { display: flex; align-items: flex-start; gap: 9px; padding: 10px 12px; border: 1px solid var(--line); border-radius: 10px; margin: 7px 0; cursor: pointer; background: var(--panel-2); }
.opt:hover { border-color: var(--accent); background: var(--accent-soft); }
.opt input { margin-top: 3px; }
.opt .name { font-weight: 680; }
.opt .desc { color: var(--muted); font-size: .79rem; }
/* ---------- table ---------- */
table { width: 100%; border-collapse: collapse; font-size: .84rem; }
th, td { text-align: left; padding: 8px 10px; border-bottom: 1px solid var(--line); vertical-align: middle; }
th { color: var(--muted); font-weight: 640; font-size: .77rem; white-space: nowrap; }
tbody tr:hover { background: var(--panel-2); }
td.num, th.num { text-align: right; font-variant-numeric: tabular-nums; }
code { font-family: ui-monospace, SFMono-Regular, Menlo, Consolas, monospace; font-size: .79rem; }
.mono { font-family: ui-monospace, SFMono-Regular, Menlo, Consolas, monospace; font-size: .78rem; }
/* ---------- status ---------- */
.pill { display: inline-flex; align-items: center; gap: 5px; padding: 2px 9px; border-radius: 999px; font-size: .75rem; font-weight: 660; }
.pill.ok { background: var(--accent-soft); color: var(--accent-dark); }
.pill.warn { background: var(--warn-soft); color: #a9721a; }
.pill.bad { background: var(--danger-soft); color: var(--danger); }
.pill.idle { background: var(--panel-2); color: var(--muted); }
.dot { width: 7px; height: 7px; border-radius: 50%; display: inline-block; background: currentColor; }
.muted { color: var(--muted); }
.small { font-size: .78rem; }
.ok { color: var(--accent-dark); }
.warn { color: #b8801f; }
.bad { color: var(--danger); }
.note { margin: 8px 0 0; color: var(--muted); font-size: .78rem; line-height: 1.55; }
.empty { padding: 18px 4px; color: var(--muted); font-size: .84rem; }
/* ---------- run log ---------- */
pre.log {
  margin: 8px 0 0; padding: 11px 13px; max-height: 340px; overflow: auto;
  background: var(--panel-2); border: 1px solid var(--line); border-radius: 9px;
  font-family: ui-monospace, SFMono-Regular, Menlo, Consolas, monospace;
  font-size: .76rem; line-height: 1.6; white-space: pre-wrap; word-break: break-word;
}
details > summary { cursor: pointer; margin-top: 8px; }
/* ---------- panels/radio groups ---------- */
.seg { display: inline-flex; border: 1px solid var(--line); border-radius: 9px; overflow: hidden; background: var(--panel-2); }
.seg button { border-radius: 0; background: transparent; color: var(--text); font-weight: 620; padding: 7px 13px; }
.seg button + button { border-left: 1px solid var(--line); }
.seg button.active { background: var(--accent); color: #fff; }
`

// uiTabsScript wires the tab bar. It is plain top-level script source (no
// <script> wrapper) so the page can concatenate it with the rest of its
// JavaScript into a single block — two separate blocks would still work, but a
// single one keeps function order obvious and avoids a redundant tag.
const uiTabsScript = `
function showTab(id, btn) {
  var root = document;
  root.querySelectorAll('.panel').forEach(function (p) { p.classList.remove('active'); });
  root.querySelectorAll('.tabs button').forEach(function (b) { b.classList.remove('active'); });
  var panel = document.getElementById(id);
  if (panel) panel.classList.add('active');
  if (btn) btn.classList.add('active');
  try { localStorage.setItem('workbuddy-panel-tab', id); } catch (e) {}
}
function restoreTab() {
  var saved = '';
  try { saved = localStorage.getItem('workbuddy-panel-tab') || ''; } catch (e) {}
  if (saved) {
    var panel = document.getElementById(saved);
    var btn = document.querySelector('.tabs button[data-tab="' + saved + '"]');
    if (panel && btn) { showTab(saved, btn); return; }
  }
  var first = document.querySelector('.tabs button');
  if (first) first.click();
}
`
