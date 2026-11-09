// Trainyard dashboard: polls /api/state and renders Home, Claims, Merge queue and Activity.
// Plain JS, no build step.
"use strict";

const ICONS = {
  inbox: '<path d="M22 12h-6l-2 3h-4l-2-3H2"/><path d="M5.45 5.11 2 12v6a2 2 0 0 0 2 2h16a2 2 0 0 0 2-2v-6l-3.45-6.89A2 2 0 0 0 16.76 4H7.24a2 2 0 0 0-1.79 1.11z"/>',
  flag: '<path d="M4 15s1-1 4-1 5 2 8 2 4-1 4-1V3s-1 1-4 1-5-2-8-2-4 1-4 1z"/><path d="M4 22v-7"/>',
  layers: '<path d="m12 2 10 5-10 5L2 7z"/><path d="m2 17 10 5 10-5"/><path d="m2 12 10 5 10-5"/>',
  list: '<path d="M8 6h13M8 12h13M8 18h13M3 6h.01M3 12h.01M3 18h.01"/>',
  eye: '<path d="M2 12s3-7 10-7 10 7 10 7-3 7-10 7S2 12 2 12z"/><circle cx="12" cy="12" r="3"/>',
  alert: '<path d="m21.73 18-8-14a2 2 0 0 0-3.48 0l-8 14A2 2 0 0 0 4 21h16a2 2 0 0 0 1.73-3"/><path d="M12 9v4M12 17h.01"/>',
  x: '<circle cx="12" cy="12" r="10"/><path d="m15 9-6 6M9 9l6 6"/>',
  check: '<path d="M20 6 9 17l-5-5"/>',
  ext: '<path d="M15 3h6v6M10 14 21 3M18 13v6a2 2 0 0 1-2 2H5a2 2 0 0 1-2-2V8a2 2 0 0 1 2-2h6"/>',
  git: '<circle cx="6" cy="6" r="3"/><circle cx="6" cy="18" r="3"/><circle cx="18" cy="12" r="3"/><path d="M6 9v6M9 6h3a6 6 0 0 1 6 6"/>',
  play: '<path d="m6 3 14 9-14 9z"/>', search: '<circle cx="11" cy="11" r="8"/><path d="m21 21-4.3-4.3"/>',
  bot: '<rect x="3" y="8" width="18" height="12" rx="2"/><path d="M12 8V4M8 14h.01M16 14h.01"/>',
  clock: '<circle cx="12" cy="12" r="10"/><path d="M12 6v6l4 2"/>',
};
const icon = (n, s = 16) => `<svg width="${s}" height="${s}" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true">${ICONS[n] || ""}</svg>`;
document.querySelectorAll("[data-i]").forEach(el => el.outerHTML = icon(el.dataset.i));

const esc = s => String(s ?? "").replace(/[&<>"']/g, c => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" }[c]));
const safeURL = u => /^https?:\/\//.test(u || "") ? u : "";
const ago = t => {
  const s = Math.max(0, (Date.now() - new Date(t)) / 1000);
  if (s < 60) return "just now";
  if (s < 3600) return Math.floor(s / 60) + " min ago";
  if (s < 86400) return Math.floor(s / 3600) + " h ago";
  return Math.floor(s / 86400) + " d ago";
};
const hhmm = t => new Date(t).toLocaleTimeString([], { hour: "2-digit", minute: "2-digit" });

const LABEL = { working: "Working", queued: "In merge queue", testing: "Testing on latest main", sent_back: "Sent back to agent",
  ready: "Waiting for review", landed: "Landed", needs_person: "Needs a person", abandoned: "Stopped" };
const PILL_ICON = { working: "bot", queued: "layers", testing: "play", sent_back: "alert", ready: "eye", landed: "check", needs_person: "x", abandoned: "x", overlap: "flag" };
const pill = (state, text) => `<span class="pill p-${esc(state)}">${icon(PILL_ICON[state] || "clock", 13)}${esc(text || LABEL[state] || state)}</span>`;

let S = null, tab = (location.hash.match(/tab=(\w+)/) || [])[1] || "home";

function setTab(t) {
  tab = t;
  history.replaceState(null, "", "#tab=" + t);
  document.querySelectorAll(".nav").forEach(b => b.classList.toggle("on", b.dataset.tab === t));
  render();
}
document.querySelectorAll(".nav").forEach(b => b.addEventListener("click", () => setTab(b.dataset.tab)));

async function load() {
  try {
    const r = await fetch("/api/state", { credentials: "same-origin" });
    if (r.status === 401) { location.href = "/"; return; }
    S = await r.json();
    document.getElementById("updated").textContent = "Updated " + hhmm(S.now);
    document.getElementById("me").textContent = "Signed in as " + S.me;
    render();
  } catch (e) {
    document.getElementById("updated").textContent = "Can't reach the server — retrying";
  }
}

function badge(id, n) { const el = document.getElementById(id); el.hidden = !n; el.textContent = n; }

function render() {
  if (!S) return;
  document.querySelectorAll(".nav").forEach(b => b.classList.toggle("on", b.dataset.tab === tab));
  badge("n-home", S.counts.needs_you);
  badge("n-claims", S.overlaps.length);
  const m = document.getElementById("main");
  const open = m.querySelectorAll("details[open]");
  const keep = [...open].map(d => d.dataset.k);
  m.innerHTML = ({ home, claims, queue, activity }[tab] || home)();
  keep.forEach(k => { const d = m.querySelector(`details[data-k="${k}"]`); if (d) d.open = true; });
}

const jobById = id => S.jobs.find(j => j.id === id);
const prBtn = (j, label, cls = "") => safeURL(j.pr_url) ? `<a class="btn ${cls}" href="${esc(j.pr_url)}" target="_blank" rel="noopener">${esc(label)} ${icon("ext", 14)}</a>` : "";
const repoCrumb = () => esc(S.repos.length === 1 ? S.repos[0] : S.repos.length + " repos");

function greeting() {
  const h = new Date().getHours();
  return h < 12 ? "Good morning" : h < 18 ? "Good afternoon" : "Good evening";
}

function needsYou() {
  const rows = [];
  for (const j of S.jobs.filter(j => j.state === "ready"))
    rows.push(`<div class="row"><span class="ic" style="background:#0b2a12;color:#5fe08f">${icon("eye", 18)}</span>
      <div><div class="t">Review “${esc(j.title)}”</div><div class="d">${icon("clock", 12)} Waiting ${ago(j.state_since)} · ${j.changed_files.length} ${j.changed_files.length === 1 ? "file" : "files"} · ${`<span class="pill p-ready">${icon("check", 13)}All checks passed</span>`}</div></div>
      <div class="act">${prBtn(j, "Review on GitHub", "blue")}</div></div>`);
  for (const o of S.overlaps)
    rows.push(`<div class="row"><span class="ic" style="background:#2a2200;color:var(--yellow)">${icon("flag", 18)}</span>
      <div><div class="t">${esc(o.agent_a)} and ${esc(o.agent_b)} both claimed ${esc(o.pattern)}</div><div class="d">Both agents were told · nobody is blocked · ${ago(o.since)}</div></div>
      <div class="act"><button class="btn" onclick="setTab('claims')">See claims</button></div></div>`);
  for (const j of S.jobs.filter(j => j.state === "needs_person"))
    rows.push(`<div class="row"><span class="ic" style="background:#2a0b0a;color:var(--red)">${icon("x", 18)}</span>
      <div><div class="t">${esc(j.agent)} is stuck after ${j.attempts} tries</div><div class="d">“${esc(j.title)}” · ${esc((j.last_feedback || {}).message || "").split("\n")[0]}</div></div>
      <div class="act">${prBtn(j, "Take a look")}</div></div>`);
  return rows.length ? `<div class="list">${rows.join("")}</div>` : `<div class="list"><div class="empty">Nothing needs you right now.</div></div>`;
}

function agentsTable(jobs) {
  if (!jobs.length) return `<div class="list"><div class="empty">No agents running. Start one with <code>trainyard start "what to do"</code>.</div></div>`;
  return `<table><thead><tr><th>Agent</th><th>Job</th><th>Started by</th><th>Claims</th><th>Status</th><th>Updated</th></tr></thead><tbody>${jobs.map(j => {
    const st = j.overlapping.length && (j.state === "working" || j.state === "sent_back") ? pill("overlap", "Claim overlaps " + j.overlapping.map(o => (jobById(o) || {}).agent || o).join(", ")) : pill(j.state);
    return `<tr><td><b>${esc(j.agent)}</b><div class="sub">${esc([j.agent_tool, j.agent_model].filter(Boolean).join(" · ") || j.id)}</div></td>
      <td>${esc(j.title)}<div class="sub">${esc(j.id)}${j.pr ? " · PR #" + j.pr : ""}</div></td><td>${esc(j.started_by)}</td>
      <td class="mono">${j.claims.map(esc).join("<br>") || '<span class="sub">none</span>'}</td><td>${st}${j.attempts ? `<div class="sub">try ${j.attempts}</div>` : ""}</td>
      <td class="sub">${ago(j.updated_at)}</td></tr>`;
  }).join("")}</tbody></table>`;
}

function home() {
  const active = S.jobs.filter(j => !["landed", "abandoned"].includes(j.state));
  const landed = S.jobs.filter(j => j.state === "landed");
  const n = S.counts.needs_you;
  return `<div class="crumb">${repoCrumb()} › Home</div>
  <h1>${greeting()}, ${esc(S.me)}</h1>
  <p class="lede">${n ? `${n} ${n === 1 ? "thing needs" : "things need"} you.` : "Nothing needs you."} Everything else is moving on its own.</p>
  <h2><span class="bullet">1</span>Needs you</h2>${needsYou()}
  <div class="tiles" style="margin-top:14px">
    <div class="tile"><div class="k">${icon("bot", 14)} Agents working</div><div class="v">${S.counts.working}</div><div class="s">editing in their own workspaces</div></div>
    <div class="tile"><div class="k">${icon("layers", 14)} In merge queue</div><div class="v">${S.counts.in_queue}</div><div class="s">being tested on latest main</div></div>
    <div class="tile"><div class="k">${icon("eye", 14)} Waiting for review</div><div class="v">${S.jobs.filter(j => j.state === "ready").length}</div><div class="s">all checks passed</div></div>
    <div class="tile"><div class="k">${icon("git", 14)} Landed today</div><div class="v">${S.counts.landed_today}</div><div class="s">merged into main</div></div>
  </div>
  <h2><span class="bullet">2</span>Agents</h2>${agentsTable(active)}
  <h2><span class="bullet">3</span>Landed recently</h2>
  ${landed.length ? `<div class="list">${landed.map(j => `<div class="row"><span style="color:#5fe08f">${icon("check", 20)}</span><div><div class="t">${esc(j.title)}</div><div class="d">${esc(j.agent)} · ${esc(j.started_by)} · ${hhmm(j.state_since)}</div></div><div class="act">${prBtn(j, "PR #" + j.pr)}</div></div>`).join("")}</div>`
    : `<div class="list"><div class="empty">Nothing has landed yet today.</div></div>`}`;
}

function claims() {
  const overlaps = S.overlaps.map(o => {
    const a = jobById(o.a) || {}, b = jobById(o.b) || {};
    return `<div class="callout"><span class="pill p-overlap">${icon("alert", 13)}Overlap</span> <span class="sub" style="color:var(--fg3)">${ago(o.since)} · nobody is blocked</span>
      <div class="t">${esc(o.agent_a)} and ${esc(o.agent_b)} both claimed <code>${esc(o.pattern)}</code></div>
      <div class="d" style="color:var(--fg2);margin-top:6px">${esc(o.agent_a)}: “${esc(a.title)}” (${esc(a.started_by)}) · ${esc(o.agent_b)}: “${esc(b.title)}” (${esc(b.started_by)})</div>
      <div class="note">Both agents were told. Whichever lands second is re-tested on top of the first automatically.</div></div>`;
  }).join("");
  const rows = S.claims.map(c => `<tr><td class="mono">${esc(c.pattern)}</td><td><b>${esc(c.agent)}</b><div class="sub">${esc(c.started_by)}</div></td>
    <td>${esc(c.job_title)}<div class="sub">${esc(c.job_id)}${c.reason ? " · “" + esc(c.reason) + "”" : ""}</div></td><td class="sub">${ago(c.created_at)}</td>
    <td>${S.overlaps.some(o => o.a === c.job_id || o.b === c.job_id) ? pill("overlap", "Overlap") : `<span class="pill p-ready">${icon("check", 13)}Clear</span>`}</td></tr>`).join("");
  return `<div class="crumb">${repoCrumb()} › Claims</div><h1>Claims</h1>
  <p class="lede">Before editing, each agent says which files it plans to change. Claims are a heads-up, not a lock. They’re released when work lands, or after ${S.claim_ttl_minutes} quiet minutes.</p>
  ${overlaps}
  <h2><span class="bullet">1</span>Active claims</h2>
  ${rows ? `<table><thead><tr><th>Files</th><th>Agent</th><th>Job</th><th>Claimed</th><th>Status</th></tr></thead><tbody>${rows}</tbody></table>` : `<div class="list"><div class="empty">No active claims.</div></div>`}`;
}

function stageCells(j) {
  const done = `<span class="pill p-ready">${icon("check", 13)}Passed</span>`, wait = `<span class="pill">${icon("clock", 13)}Waiting</span>`;
  const fb = j.last_feedback || {};
  if (j.state === "queued") return [`<span class="pill p-queued">${icon("clock", 13)}Next in line</span>`, wait, wait, wait];
  if (j.state === "testing") return [done, done, `<span class="pill p-testing">${icon("play", 13)}Running</span><div class="sub">since ${hhmm(j.state_since)}</div>`, `PR #${j.pr}`];
  if (j.state === "ready") return [done, done, done, `${pill("ready", "PR #" + j.pr + " open")}<div class="sub">waiting for review</div>`];
  const failedAt = fb.kind === "conflict" || fb.kind === "not_pushed" || fb.kind === "no_changes" ? 0 : fb.kind === "check_failed" ? 1 : 2;
  const bad = pill(j.state, j.state === "needs_person" ? `Failed ${j.attempts} times` : "Sent back");
  return [0, 1, 2, 3].map(i => i < failedAt ? done : i === failedAt ? bad : wait);
}

function queue() {
  const jobs = S.jobs.filter(j => ["queued", "testing", "sent_back", "ready", "needs_person"].includes(j.state));
  const rows = jobs.map(j => {
    const c = stageCells(j), fb = j.last_feedback;
    const detail = fb && (j.state === "sent_back" || j.state === "needs_person") ? `<tr><td colspan="5" style="border-top:0;padding-top:0">
      <div style="border-left:3px solid ${j.state === "needs_person" ? "var(--red)" : "var(--yellow)"};padding-left:12px;color:var(--fg2);white-space:pre-wrap">${esc(fb.message)}</div>
      ${fb.log ? `<details data-k="${esc(j.id)}"><summary>Log sent to the agent</summary><pre>${esc(fb.log)}</pre></details>` : ""}</td></tr>` : "";
    return `<tr><td><b>${esc(j.title)}</b><div class="sub">${esc(j.agent)} · joined ${hhmm(j.state_since)}</div></td>${c.map(x => `<td>${x}</td>`).join("")}</tr>${detail}`;
  }).join("");
  return `<div class="crumb">${repoCrumb()} › Merge queue</div><h1>Merge queue</h1>
  <p class="lede">Finished work lines up here and is tested on top of the latest main. Only work that passes reaches your reviewers.</p>
  <div class="route">
    <div class="stop">${icon("git", 18)}<div><b>1 · Update</b><span>Put the change on top of the latest main</span></div></div>
    <div class="stop">${icon("search", 18)}<div><b>2 · Quick checks</b><span>Optional checks on the Trainyard server</span></div></div>
    <div class="stop">${icon("play", 18)}<div><b>3 · Tests</b><span>Your GitHub Actions run on the result</span></div></div>
    <div class="stop">${icon("eye", 18)}<div><b>4 · Pull request</b><span>Ready for review once tests pass</span></div></div>
  </div>
  <p class="note">If something fails, the agent gets the log and tries again (up to 3 times). Trainyard never edits an agent’s code itself.</p>
  <h2><span class="bullet">1</span>In the queue</h2>
  ${rows ? `<table><thead><tr><th>Change</th><th>Update</th><th>Quick checks</th><th>Tests</th><th>Result</th></tr></thead><tbody>${rows}</tbody></table>` : `<div class="list"><div class="empty">The queue is empty.</div></div>`}`;
}

const KIND_COLOR = { landed: "var(--green)", ready: "var(--green)", overlap: "var(--yellow)", sent_back: "var(--yellow)", needs_person: "var(--red)", error: "var(--red)", testing: "var(--blue)", queued: "var(--blue)" };
function activity() {
  return `<div class="crumb">${repoCrumb()} › Activity</div><h1>Activity</h1><p class="lede">Everything that happened, newest first.</p>
  <div class="list feed">${S.events.map(e => `<div class="row"><time>${hhmm(e.at)}</time><span class="dot" style="background:${KIND_COLOR[e.kind] || "var(--grey)"}"></span>
    <div><div>${esc(e.message)}</div><div class="d">${esc(e.repo)}${e.job_id ? " · " + esc(e.job_id) : ""}</div></div></div>`).join("") || '<div class="empty">No activity yet.</div>'}</div>`;
}

setTab(tab);
load();
setInterval(load, 3000);
