// The replay scene: one saved variant of a contract check, stepped through
// input by input as the new build replayed it (GET /api/trials/…/variants/
// {round}/{id}), or the recorded incident itself as the old or the new build
// replayed it (GET /api/trials/…/incident), which for a feature task is the
// contract's recorded traffic. It takes the job view's left pane
// while open. What it shows is only what the journal and the replay record:
// the inputs, the env reads, the outputs (recorded and the build's), and
// where the replay stopped. There is no handler state in either, so the
// scene draws none.

import { h, state, api, renderView } from "./app.js";
import { checksOf } from "./invariants.js";

const listCache = new Map(); // "job/trial#checks" -> { list } | { list: [], error, status } | "loading"
const sceneCache = new Map(); // "job/trial/round/id" -> response | { error, status } | "loading"
const derived = new WeakMap(); // response -> what the scene draws from it
let ctrl = null; // aborts the scene fetch a newer selection replaced
let timer = 0;
let expanded = false;
let opener = ""; // data-replay of the button that opened the scene
let ctx = null; // the last scene built: { t, d, m }

const STEP_MS = 140; // about 7 steps a second
const enc = encodeURIComponent;
const tkey = (t) => `${t.job}/${t.id}`;
const vkey = (r) => (r.incident ? `${r.job}/${r.trial}/incident/${r.build}` : `${r.job}/${r.trial}/${r.round}/${r.id}`);
// lkey is what a scene loads: the incident's one response serves both builds.
const lkey = (r) => (r.incident ? `${r.job}/${r.trial}/incident` : vkey(r));
const views = new Map(); // vkey of an incident scene -> its response, seen as one build's replay
const vpath = (job, trial) => `/trials/${enc(job)}/${enc(trial)}/variants`;

// savedVariants returns the variants the trial's contract checks saved,
// loading them the first time. gen is the number of checks, so a new round
// fetches the list again.
export function savedVariants(t, gen) {
  const key = `${tkey(t)}#${gen}`;
  const got = listCache.get(key);
  if (got && got !== "loading") return got;
  if (!got) {
    listCache.set(key, "loading");
    api(vpath(t.job, t.id))
      .then((r) => listCache.set(key, { list: r.variants }))
      .catch((e) => listCache.set(key, { list: [], error: e.message, status: e.status }))
      .finally(() => { if (state.stepsFor === tkey(t)) renderView(); });
  }
  return { loading: true, list: [] };
}

export const isSaved = (sv, round, id) => sv.list.some((v) => v.round === round && v.id === id);

// openReplay opens the scene on variant id of round.
export function openReplay(t, round, id) {
  stop();
  state.replay = { job: t.job, trial: t.id, round, id, i: null };
  opener = `${round}/${id}`;
  expanded = false;
  load(state.replay);
  renderView();
  const el = document.getElementById("rp");
  if (el) el.focus({ preventScroll: true });
}

// openIncident opens the scene on the recorded incident, or with traffic
// set the contract's recorded traffic, as build ("old" or "new") replayed it.
export function openIncident(t, build = "new", traffic = false) {
  stop();
  const i = state.replay && state.replay.incident && state.replay.trial === t.id ? state.replay.i : null;
  state.replay = { job: t.job, trial: t.id, incident: true, traffic, build, i };
  if (!opener.startsWith("incident")) expanded = false;
  opener = "incident";
  load(state.replay);
  renderView();
  const el = document.getElementById("rp");
  if (el) el.focus({ preventScroll: true });
}

export function closeReplay() {
  stop();
  state.replay = null;
  ctx = null;
  renderView();
  const back = document.querySelector(`[data-replay="${CSS.escape(opener)}"]`);
  if (back) back.focus();
}

function load(r) {
  const key = lkey(r);
  if (sceneCache.has(key)) return;
  if (ctrl) ctrl.abort();
  const mine = new AbortController();
  ctrl = mine;
  sceneCache.set(key, "loading");
  const path = r.incident ? `/trials/${enc(r.job)}/${enc(r.trial)}/incident` : `${vpath(r.job, r.trial)}/${enc(r.round)}/${enc(r.id)}`;
  api(path, { signal: mine.signal })
    .then((d) => sceneCache.set(key, d))
    .catch((e) => {
      if (mine.signal.aborted) sceneCache.delete(key);
      else sceneCache.set(key, { error: e.message, status: e.status });
    })
    .finally(() => {
      if (ctrl === mine) ctrl = null;
      if (state.replay && lkey(state.replay) === key) renderView();
    });
}

// sceneData is what the scene draws for r: the response, or, for the
// incident, the response seen as one build's replay of the recording.
function sceneData(r) {
  const got = sceneCache.get(lkey(r));
  if (!r.incident || !got || got === "loading" || got.error) return got;
  const key = vkey(r);
  const seen = views.get(key);
  if (seen && seen.src === got) return seen.d;
  const f = got.failure;
  const d = {
    ...got, incident: true, build: r.build, id: "incident", round: null, new: got[r.build] || null,
    variant: { id: "incident", mutation: "", incident: f ? f.seq : null, failure: (got.diff && got.diff.old) || (f ? `${f.kind}@${f.seq}` : "") },
  };
  if (got.kind === "traffic") {
    // The feature steps, as the replays name them: those the new build
    // changed, and those either build left as recorded.
    const changed = (got.new && got.new.changed) || [];
    const feats = new Set([...changed, ...["old", "new"].flatMap((b) => (got[b] && [...(got[b].changed || []), ...(got[b].unreached || [])]) || [])]);
    const nw = got[r.build];
    d.traffic = { changed: new Set(changed), report: { feature_steps: [...feats], declared: (nw && nw.invariants) || [] } };
  }
  views.set(key, { src: got, d });
  return d;
}

const DUR = { d: 86400e9, h: 3600e9, m: 60e9, s: 1e9, ms: 1e6 };
// shiftOf reads a clock-shift variant's offset, in nanoseconds, from its
// mutation ("clock shifted by +1h30m", "… -5h30m", "… +366 days").
function shiftOf(mutation) {
  const m = /^clock shifted by\s*([+-]?)(.+)$/.exec(mutation);
  if (!m) return null;
  let ns = 0;
  const re = /(\d+(?:\.\d+)?)\s*(days?|ms|h|m|s|d)/g;
  let p;
  while ((p = re.exec(m[2]))) ns += parseFloat(p[1]) * DUR[p[2].startsWith("day") ? "d" : p[2]];
  return { label: `${m[1] || "+"}${m[2].trim()}`, ns: m[1] === "-" ? -ns : ns };
}

// faultOf finds the step the mutation changed: the input it names, the
// second delivery of a redelivered one, the step after a dropped one, else
// the variant's incident.
// The seqs a mutation names are the traffic's (steps[].traffic_seq); the
// incident is a seq of the variant.
function faultOf(steps, firstIdx, v) {
  const mut = v.mutation;
  let m;
  if ((m = /^input seq (\d+) delivered twice/.exec(mut))) {
    const q = +m[1];
    for (let i = steps.length - 1; i >= 0; i--) if (steps[i].tseq === q) return { i, why: `traffic input @${q} delivered twice: this is the second delivery` };
  }
  if ((m = /^input seq (\d+) dropped/.exec(mut))) {
    const q = +m[1];
    const i = steps.findIndex((s) => s.tseq > q);
    if (i >= 0) return { i, why: `traffic input @${q} was dropped before this one` };
  }
  if ((m = /^failing input \(seq (\d+)\)/.exec(mut))) {
    const q = +m[1];
    const i = steps.findIndex((s) => s.tseq === q && s.input_changed !== false);
    if (i >= 0) return { i, why: mut.replace(/^failing input \(seq \d+\)[:\s]*/, "") || mut };
  }
  const shift = /^clock shifted by\s*(.+)$/.exec(mut);
  const why = shift ? `the old build fails here; every clock read is shifted ${shift[1]}` : mut;
  if (firstIdx.has(v.incident)) return { i: firstIdx.get(v.incident), why };
  return { i: -1, why: mut };
}

const invOf = (s) => (/^invariant_violated\(([^)]+)\)/.exec(s) || [])[1] || (/^invariant:\s*(.+)$/.exec(s) || [])[1] || null;
const seqOf = (verdict) => { const m = /@(\d+)$/.exec(verdict); return m ? +m[1] : null; };

function derive(d, report) {
  const memo = derived.get(d);
  if (memo && memo.report === report) return memo;
  const steps = d.steps.map((s) => ({ ...s, tseq: s.traffic_seq != null ? s.traffic_seq : s.seq }));
  const n = steps.length;
  const v = d.variant || { id: d.id, mutation: "", incident: null, failure: "" };
  const nw = d.new;
  const nsteps = nw ? nw.steps : [];
  const firstIdx = new Map();
  steps.forEach((s, i) => { if (!firstIdx.has(s.seq)) firstIdx.set(s.seq, i); });
  const bySeq = new Map();
  nsteps.forEach((s) => { if (!bySeq.has(s.seq)) bySeq.set(s.seq, s); });
  const newAt = (i) => {
    if (!nw || i >= nsteps.length) return null;
    const a = nsteps[i];
    return a.seq === steps[i].seq ? a : bySeq.get(steps[i].seq) || null;
  };

  // Where the new build's replay ended, and whether it stopped on a failure.
  const failing = !!nw && !/^(ok|pass|fixed|passed)$/.test(nw.status);
  let end = n - 1;
  if (nw && nsteps.length) end = Math.min(n - 1, nsteps.length - 1);
  let fail = -1;
  if (failing) {
    const q = nw.seq != null ? nw.seq : seqOf(nw.verdict);
    if (q == null || (steps[end] && steps[end].seq === q)) fail = end;
    else fail = firstIdx.has(q) ? firstIdx.get(q) : end;
    end = Math.max(0, fail);
  }
  if (!n) end = -1;

  // The last clock read at or before each step.
  const lastRead = new Array(n);
  let prev = null;
  for (let i = 0; i < n; i++) {
    const c = steps[i].clock;
    if (c.length) prev = { ns: c[c.length - 1], i };
    lastRead[i] = prev;
  }

  const broke = (nw && (nw.invariant || invOf(nw.verdict))) || null;
  const names = [];
  const add = (x) => { if (x && !names.includes(x)) names.push(x); };
  if (report) {
    (report.declared && report.declared.length ? report.declared : []).forEach(add);
    (report.invariants || []).forEach(add);
  }
  add(invOf(v.failure));
  add(broke);

  const feats = new Set((report && report.feature_steps) || []);
  const featIdx = [];
  // feature_steps are traffic seqs.
  steps.forEach((s, i) => { if (feats.has(s.tseq)) featIdx.push(i); });

  const fault = faultOf(steps, firstIdx, v);
  if (d.traffic) {
    // No failure to point at: the first feature step the new build changed.
    const i = steps.findIndex((s) => d.traffic.changed.has(s.seq));
    Object.assign(fault, i >= 0 ? { i, why: "the first feature step the new build changed" } : { i: -1, why: "" });
  } else if (d.incident) {
    const f = d.failure;
    fault.why = f ? `production failed here: ${[f.kind, f.message].filter(Boolean).join(": ")}` : "";
  }
  const out = {
    report, steps, n, v, nw, newAt, end, fail, failing, lastRead, names, broke, feats, featIdx,
    fault, shift: shiftOf(v.mutation),
    incident: !!d.incident,
    traffic: !!d.traffic,
    changed: d.traffic ? d.traffic.changed : new Set(),
    // What the scene calls the build replaying, and the outputs it is held to.
    bname: d.incident && d.build === "old" ? "old build" : "new build",
    rname: d.traffic ? "recorded traffic" : d.incident ? "production" : null,
  };
  derived.set(d, out);
  return out;
}

function decodeInput(s) {
  if (s._in) return s._in;
  const raw = s.input;
  let text = raw, note = "";
  if (s.input_base64) {
    try {
      const bytes = Uint8Array.from(atob(raw), (c) => c.charCodeAt(0));
      try {
        text = new TextDecoder("utf-8", { fatal: true }).decode(bytes);
        note = "base64 in the journal, shown decoded";
      } catch {
        text = [...bytes.slice(0, 96)].map((b) => b.toString(16).padStart(2, "0")).join(" ") + (bytes.length > 96 ? " …" : "");
        note = `${bytes.length} bytes of binary, shown as hex`;
      }
    } catch { note = "base64"; }
  }
  let pretty = text, line = text.replace(/\s+/g, " ").trim();
  try {
    const j = JSON.parse(text);
    if (j && typeof j === "object") { pretty = JSON.stringify(j, null, 2); line = JSON.stringify(j); }
  } catch { /* not JSON: shown as it is */ }
  s._in = { line, pretty, note, long: line.length > 110 || pretty.includes("\n") };
  return s._in;
}

const clip = (str, n) => (str.length > n ? `${str.slice(0, n - 1)}…` : str);

function fmtClock(ns) {
  const d = new Date(ns / 1e6);
  if (isNaN(d)) return { time: "—", date: "" };
  const iso = d.toISOString();
  return { time: iso.slice(11, 23), date: `${iso.slice(0, 10)} UTC` };
}

function fmtDelta(ns) {
  const a = Math.abs(ns), sign = ns < 0 ? "−" : "+";
  if (a < 1e3) return "under 1 µs";
  if (a < 1e6) return `${sign}${Math.round(a / 1e3)} µs`;
  if (a < 1e9) return `${sign}${Math.round(a / 1e6)} ms`;
  if (a < 60e9) return `${sign}${(a / 1e9).toFixed(a < 10e9 ? 2 : 1)} s`;
  if (a < 3600e9) return `${sign}${Math.floor(a / 60e9)}m ${Math.round((a % 60e9) / 1e9)}s`;
  return `${sign}${Math.floor(a / 3600e9)}h ${Math.round((a % 3600e9) / 60e9)}m`;
}

const outKey = (o) => `${o.sink}\u0000${o.data}`;

function cur() {
  const m = ctx && ctx.m;
  if (!m || !state.replay) return null;
  const r = state.replay;
  if (r.i == null) r.i = Math.max(0, (m.fault.i >= 0 ? m.fault.i : Math.max(0, m.fail)) - 3);
  r.i = Math.max(0, Math.min(Math.max(0, m.end), r.i));
  return r;
}

function go(i) {
  const r = cur();
  if (!r) return;
  r.i = Math.max(0, Math.min(Math.max(0, ctx.m.end), i));
  update();
}

const stopAt = (m) => (m.fail >= 0 ? m.fail : m.end);

function stop() {
  if (timer) clearInterval(timer);
  timer = 0;
}

function toggle() {
  const r = cur();
  if (!r) return;
  if (timer) { stop(); update(); return; }
  const key = vkey(r);
  if (r.i >= stopAt(ctx.m)) r.i = 0;
  timer = setInterval(() => {
    const now = state.replay;
    if (!now || vkey(now) !== key || !ctx || !document.getElementById("rp")) { stop(); return; }
    now.i++;
    if (now.i >= stopAt(ctx.m)) { now.i = stopAt(ctx.m); stop(); }
    update();
  }, STEP_MS);
  update();
}

const jump = () => { if (ctx && ctx.m.fault.i >= 0) { stop(); go(ctx.m.fault.i); } };

// replayKey handles the scene's keys: ←/→ step, Home/End, space play or
// pause, F jump to the fault. Escape is app.js's, which calls closeReplay.
export function replayKey(e) {
  if (!state.replay || !ctx || e.metaKey || e.ctrlKey || e.altKey) return false;
  const tg = e.target;
  const inScene = tg === document.body || tg === document.documentElement || tg.closest("#rp");
  if (!inScene || tg.matches("input:not([type=range]), textarea, select, [contenteditable]")) return false;
  const range = tg.matches("input[type=range]");
  const button = tg.tagName === "BUTTON";
  const r = cur();
  if (!r) return false;
  switch (e.key) {
    case "ArrowLeft": if (range) return false; stop(); go(r.i - 1); break;
    case "ArrowRight": if (range) return false; stop(); go(r.i + 1); break;
    case "Home": if (range) return false; stop(); go(0); break;
    case "End": if (range) return false; stop(); go(ctx.m.end); break;
    case " ": if (button) return false; toggle(); break;
    case "f": case "F": jump(); break;
    default: return false;
  }
  e.preventDefault();
  return true;
}

// update redraws what a step changes, leaving the scrubber in place so a
// drag on it carries on.
function update() {
  const el = document.getElementById("rp");
  if (!el || !ctx || !state.replay || el.dataset.key !== vkey(state.replay) || !el.querySelector(".rp-side")) { renderView(); return; }
  const r = cur();
  const fid = document.activeElement && el.contains(document.activeElement) ? document.activeElement.dataset.fid : "";
  const { t, d, m } = ctx;
  el.querySelector(".rp-head").replaceWith(head(t, d, m, r));
  el.querySelector(".rp-strip").replaceWith(strip(m, r));
  el.querySelector(".rp-ctl-row").replaceWith(ctlRow(m, r));
  el.querySelector(".rp-stage").replaceWith(stage(d, m, r));
  el.querySelector(".rp-side").replaceWith(side(m, r));
  const range = el.querySelector(".rp-range");
  if (range && +range.value !== r.i) range.value = String(r.i);
  if (range) range.setAttribute("aria-valuetext", valueText(m, r.i));
  if (fid) {
    const f = el.querySelector(`[data-fid="${CSS.escape(fid)}"]`);
    if (f && f !== document.activeElement) f.focus({ preventScroll: true });
  }
}

const valueText = (m, i) => (m.steps[i] ? `input @${m.steps[i].seq}, step ${i + 1} of ${m.n}` : "");
const sceneName = (r) => (r.traffic ? "the recorded traffic" : r.incident ? "the recorded incident" : `variant ${r.id}, round ${r.round}`);
const cap = (s) => s.charAt(0).toUpperCase() + s.slice(1);

function reportFor(t, round) {
  if (state.stepsFor !== tkey(t)) return null;
  return checksOf(state.steps).find((c) => (c.round || c.n) === round) || null;
}

// renderReplay is the job view's left pane while a variant is open: the
// site hero's window, with the step strip and the controls on top, the step
// and its verdict below, and the clock and the inputs to come beside them.
export function renderReplay(t) {
  const r = state.replay;
  load(r);
  const got = sceneData(r);
  const fid = document.activeElement && document.activeElement.closest("#rp") ? document.activeElement.dataset.fid : "";
  const pane = h("div", { class: "pane rp-pane", id: "rp", tabindex: "-1", role: "region", "aria-label": `Replay of ${sceneName(r)}`, dataset: { key: vkey(r) } });
  const win = (...kids) => h("div", { class: "rp-win" }, kids);
  if (!got || got === "loading" || got.error) {
    ctx = null;
    const what = r.traffic ? "the traffic" : r.incident ? "the incident" : "the variant";
    pane.append(win(
      h("header", { class: "rp-head" }, backBtn(), h("div", { class: "rp-title" },
        h("span", { class: "rp-name" }, h("span", { class: "rp-dot none", "aria-hidden": "true" }),
          r.incident ? (r.traffic ? "Recorded traffic" : "Recorded incident") : [`Variant ${r.id}`, h("span", { class: "m faint" }, ` · round ${r.round}`)]))),
      h("div", { class: "rp-body rp-empty" }, !got || got === "loading" ? h("span", { class: "dim" }, `Loading ${what}…`)
        : got.status === 404 ? h("span", { class: "dim" }, r.incident ? "This trial's task has no recorded incident, or the server predates incident replays."
          : "The console's API has no replay of this variant: it was not saved, or the server predates saved variants.")
          : h("span", { class: "bad" }, `Couldn't load ${what}: ${got.error}`))));
    return pane;
  }
  const m = derive(got, got.traffic ? got.traffic.report : r.incident ? null : reportFor(t, r.round));
  ctx = { t, d: got, m };
  const rr = cur();
  if (!m.n) {
    pane.append(win(head(t, got, m, rr), h("div", { class: "rp-body rp-empty" }, h("span", { class: "dim" }, `This ${r.incident ? "recording" : "variant's journal"} has no steps.`))));
    return pane;
  }
  pane.append(win(
    head(t, got, m, rr),
    h("div", { class: "rp-body" },
      h("div", { class: "rp-main" }, controls(m, rr), stage(got, m, rr), legend(m)),
      side(m, rr))));
  if (fid) queueMicrotask(() => { const f = document.querySelector(`#rp [data-fid="${CSS.escape(fid)}"]`); if (f) f.focus({ preventScroll: true }); });
  return pane;
}

function backBtn() {
  return h("button", { type: "button", class: "rp-back", "data-fid": "back", onclick: closeReplay, title: "Back to the run (Esc)" }, "← Run");
}

// head is the window's bar: what is replayed, with a dot in the replay's
// colour, and for the recording a switch between the builds replaying it.
function head(t, d, m, r) {
  const tone = !m.nw ? "none" : m.failing ? "bad" : "ok";
  let name, sub;
  if (m.incident) {
    const f = d.failure;
    name = [m.traffic ? "Recorded traffic" : "Recorded incident", d.service ? h("span", { class: "m faint" }, ` · ${d.service}`) : null];
    sub = m.traffic
      ? [h("span", {}, `${m.n} input${m.n === 1 ? "" : "s"} recorded on the base build`),
        h("span", { class: "violet-t" }, ` · ${m.feats.size} feature step${m.feats.size === 1 ? "" : "s"}, ${m.changed.size} changed by the new build`)]
      : [h("span", {}, `${m.n} input${m.n === 1 ? "" : "s"} from production`),
        f ? h("span", { class: "violet-t" }, ` · failed at @${f.seq}: ${f.kind}`) : h("span", { class: "faint" }, " · no failure recorded"),
        d.diff && d.diff.verdict ? h("span", { class: "faint" }, ` · kavach diff ${d.diff.verdict}`) : null];
  } else {
    name = [`Variant ${m.v.id}`, h("span", { class: "m faint" }, ` · round ${r.round}`)];
    sub = [d.service ? h("span", { class: "m" }, `${d.service} · `) : null,
      h("span", { class: "violet-t" }, m.v.mutation || "mutation not recorded"),
      m.v.failure ? h("span", { class: "faint" }, ` · old build: ${m.v.failure}`) : null];
  }
  const pick = (b, label) => h("button", {
    type: "button", class: "rp-build", "data-fid": `build-${b}`, "aria-pressed": String(d.build === b),
    title: `Replay the recording on the ${label.toLowerCase()}`, onclick: () => { if (d.build !== b) openIncident(t0(), b, m.traffic); },
  }, label);
  return h("header", { class: "rp-head" },
    backBtn(),
    h("div", { class: "rp-title" },
      h("span", { class: "rp-name" }, h("span", { class: `rp-dot ${tone}`, "aria-hidden": "true" }), name),
      h("span", { class: "rp-sub" }, sub),
      d.new_error ? h("span", { class: "bad rp-sub" }, `The new build's replay couldn't be read: ${d.new_error}`) : null),
    m.incident ? h("span", { class: "rp-builds", role: "group", "aria-label": "Replay on" }, pick("old", "Old build"), pick("new", "New build")) : null);
}

const t0 = () => (ctx ? ctx.t : { job: state.replay.job, id: state.replay.trial });

// verdict is what the replay says at step i: running toward where it stops,
// stopped there, or through; or that the build's replay wasn't saved. Why
// the step matters is in its input row.
function verdict(d, m, i, ns) {
  const nw = m.nw;
  const s = m.steps[i];
  if (!nw) {
    const logged = m.incident && d.diff && d.diff[d.build];
    return { tone: "none", title: logged || "not replayed", detail: [logged ? "this run saved the verdict, not the replay's steps" : `the ${m.bname}'s replay wasn't saved`] };
  }
  if (m.failing && i < m.fail) return { tone: "run", title: "running", detail: [`the ${m.bname} stops at @${m.steps[m.fail].seq}`] };
  if (m.failing) {
    const why = (ns && (ns.panic || ns.error || ns.crash)) || nw.detail || "";
    const how = !ns ? "" : ns.panic ? "panic" : ns.error ? "error" : ns.crash ? "crash" : ns.timeout ? "timed out" : "";
    return { tone: "bad", title: nw.verdict, detail: [`replay stopped at @${s.seq}`, how], why };
  }
  const left = m.end - i;
  return { tone: "ok", title: nw.verdict, detail: [left ? `${left} more input${left === 1 ? "" : "s"} to replay` : `the ${m.bname} replayed all ${m.end + 1} inputs`] };
}

const MARKS = { ok: "M3.5 8.5l3 3 6-7", bad: "M4.5 4.5l7 7m0-7l-7 7", run: "M6 4.5v7l5.5-3.5z", none: "M4.5 8h7" };

// stage is the step: its input, env reads and outputs, then the verdict at
// it and the invariants after it.
function stage(d, m, r) {
  const i = r.i;
  const s = m.steps[i];
  const ns = m.newAt(i);
  const v = verdict(d, m, i, ns);
  const detail = v.detail.filter(Boolean).join(" · ");
  const held = m.names.filter((name) => lampState(m, name, i)[0] === "held").length;
  return h("div", { class: `rp-stage st-${v.tone}` },
    io(m, i, s, ns),
    h("div", { class: "rp-verdict" },
      h("span", { class: "rp-mark", "aria-hidden": "true" }, icon(MARKS[v.tone], v.tone === "run", 16)),
      h("div", { class: "rp-verdict-txt" },
        h("strong", { class: "m" }, v.title),
        detail ? h("span", {}, detail) : null,
        v.why ? h("code", { class: `rp-why ${expanded ? "full" : ""}` }, v.why) : null)),
    h("div", { class: "rp-invs" },
      h("div", { class: "rp-invs-head" }, h("span", {}, "Invariants"),
        m.names.length && m.nw ? h("span", { class: "m" }, `${held} of ${m.names.length} held at @${s.seq}`) : null),
      m.names.length ? h("div", { class: "rp-lamps" }, m.names.map((name, k) => lampEl(m, name, i, k)))
        : h("span", { class: "faint rp-nolamps" }, m.traffic ? `The ${m.bname} declares no invariants.` : m.incident ? "The recorded failure names no invariant." : "No invariants named for this round.")));
}

// io lists what the step read through env and what it emitted, the
// journal's record against the build's when they differ.
function io(m, i, s, ns) {
  const inp = decodeInput(s);
  const isFault = i === m.fault.i;
  const isFeat = m.feats.has(s.tseq);
  const row = (k, v, cls = "") => h("div", { class: `rp-row ${cls}` }, h("dt", {}, k), h("dd", {}, v));
  const note = (text, cls = "faint") => h("span", { class: `rp-row-note ${cls}` }, text);
  const tag = isFault ? (m.incident ? m.fault.why : `fault: ${m.fault.why}`) : isFeat ? (m.changed.has(s.seq) ? "feature step · changed by the new build" : "feature step") : m.incident ? "recorded step" : "baseline step";

  const fromTraffic = Array.isArray(s.traffic);
  const rec = fromTraffic ? s.traffic : s.recorded;
  const recName = m.rname || (fromTraffic ? "old build" : "recorded");
  const bn = m.bname;
  const got = ns ? ns.outputs : null;
  const same = got && got.length === rec.length && got.every((o, k) => outKey(o) === outKey(rec[k]));
  const long = inp.long || [...rec, ...(got || [])].some((o) => o.data.length > 80)
    || s.gateways.some((g) => g.request.length + g.response.length > 80);

  const rows = [row(
    ["Input ", h("span", { class: "m" }, `@${s.seq}`), s.tseq !== s.seq ? h("span", { class: "faint" }, ` · traffic @${s.tseq}`) : null, s.source && s.source !== "traffic" ? h("span", { class: "faint" }, ` · ${s.source}`) : null],
    [expanded && inp.long ? h("pre", { class: "m rp-input-full" }, inp.pretty) : h("code", { class: "rp-input-line" }, inp.line || "(empty input)"),
      h("span", { class: "rp-tags" },
        h("span", { class: `rp-tag ${isFault ? "fault" : isFeat ? "feat" : ""}` }, tag),
        s.input_changed && !isFault ? h("span", { class: "rp-tag fault" }, "the mutation changed this input") : null,
        inp.note ? h("span", { class: "rp-tag" }, inp.note) : null,
        long ? h("button", { type: "button", class: "link-btn rp-expand", "data-fid": "expand", "aria-pressed": String(expanded), onclick: () => { expanded = !expanded; update(); } }, expanded ? "Clip" : "Show in full") : null),
      s.marker && !m.incident ? h("span", { class: "rp-marker" }, `the variant records the old build failing here: ${[s.marker.kind, s.marker.message].filter(Boolean).join(": ")}`) : null],
    isFault ? "fault" : "")];

  const clockN = s.clock.length;
  if (clockN) rows.push(row(h("span", { class: "m" }, "clock"), h("span", { class: "m" }, `${clockN} read${clockN === 1 ? "" : "s"}`, h("span", { class: "faint" }, ` · ${s.clock.slice(0, 4).map((x) => fmtClock(x).time).join(", ")}${clockN > 4 ? " …" : ""}`))));
  if (s.random > 0) rows.push(row(h("span", { class: "m" }, "random"), h("span", { class: "m" }, String(s.random))));
  for (const g of s.gateways) {
    rows.push(row(h("span", { class: "m" }, g.gateway || "gateway"), h("span", { class: "m rp-gw" },
      line(g.request, "rp-req"), h("span", { class: "faint" }, " → "), g.error ? line(`error: ${g.error}`, "bad") : line(g.response, ""))));
  }
  for (const c of s.config) {
    rows.push(row(h("span", { class: "m" }, `config ${c.key}`), h("span", { class: "m" }, !c.present ? h("span", { class: "faint" }, "not set") : line(c.value, ""))));
  }

  const outList = (list, other, cls) => {
    const keys = new Set(other ? other.map(outKey) : []);
    return list.length ? list.map((o) => h("div", { class: `rp-out ${other && !keys.has(outKey(o)) ? cls : ""}` },
      h("span", { class: "m faint rp-sink" }, o.sink || "out"), line(o.data, "rp-data"), o.local ? h("span", { class: "faint rp-local", title: "a local output: not compared" }, "local") : null))
      : [h("div", { class: "faint rp-out" }, "nothing")];
  };
  if (i > m.end || (!ns && m.nw)) {
    rows.push(row(cap(recName), [outList(rec, null, ""), note(`not replayed by the ${bn}`)]));
  } else if (!m.nw) {
    rows.push(row(cap(recName), [outList(rec, null, ""), note(`the ${bn}'s replay wasn't saved`)]));
  } else if (same) {
    rows.push(row("Emitted", [outList(got, null, ""), rec.length ? note(`the same as ${m.rname ? "" : "the "}${recName}'s`) : null]));
  } else {
    rows.push(row(cap(recName), outList(rec, got, "gone")),
      row(cap(bn), [outList(got, rec, "new"), note(`differs from ${m.rname ? "" : "the "}${recName}'s`, "peach")]));
  }
  return h("dl", { class: `rp-io ${i === m.fail ? "stopped" : ""}` }, rows);
}

function line(text, cls) {
  return h("span", { class: `rp-clip ${expanded ? "full" : ""} ${cls}`, title: !expanded && text.length > 80 ? clip(text, 600) : null }, text || " ");
}

// side is the trail beside the step: the clock as the step read it, and the
// input at hand with the ones after it.
function side(m, r) {
  const i = r.i;
  const reads = m.steps[i].clock;
  const shown = reads.length ? { ns: reads[0], i } : m.lastRead[i];
  const before = (() => { const j = reads.length ? i - 1 : (shown ? shown.i - 1 : -1); return j >= 0 ? m.lastRead[j] : null; })();
  const fc = shown ? fmtClock(shown.ns) : null;
  const clock = h("div", { class: "rp-clock" },
    h("span", { class: "m rp-clock-k" }, "env.Now()"),
    h("span", { class: `m rp-time ${reads.length ? "" : "stale"}` }, fc ? fc.time : "—"),
    fc ? h("span", { class: "m faint rp-date" }, fc.date) : null,
    h("span", { class: "m rp-clock-note" },
      !shown ? "no clock read yet" : !reads.length ? `no read at this step · last at @${m.steps[shown.i].seq}`
        : reads.length > 1 ? `first of ${reads.length} reads` : before ? `${fmtDelta(shown.ns - before.ns)} since @${m.steps[before.i].seq}` : "the first read"),
    m.shift ? h("span", { class: "m rp-shift", title: shown ? `every clock read in this variant is shifted; the recording read ${fmtClock(shown.ns - m.shift.ns).time} UTC here` : "every clock read in this variant is shifted" }, `clock shifted ${m.shift.label}`)
      : h("span", { class: "m faint" }, "as recorded"));
  const items = [0, 1, 2, 3, 4].map((k) => i + k).filter((j) => j < m.n).map((j) => {
    const q = m.steps[j];
    const cls = [j === i ? "cur" : "", j === m.fault.i ? "fault" : m.feats.has(q.tseq) ? "feat" : "", j > m.end ? "off" : ""].join(" ");
    return h("li", { class: `rp-q ${cls}`, "aria-current": j === i ? "step" : null },
      h("button", {
        type: "button", class: "rp-q-btn", "data-fid": `q-${j - i}`, disabled: j > m.end || j === i,
        title: j > m.end ? "not replayed: the replay stopped before it" : j === i ? "the input at hand" : `Go to input @${q.seq}`,
        onclick: () => { stop(); go(j); },
      },
        h("span", { class: "m rp-q-at" }, `@${q.seq}`, q.tseq !== q.seq ? ` · traffic @${q.tseq}` : "", j === m.fault.i ? (m.incident ? " · failed" : " · fault") : ""),
        h("span", { class: "m rp-q-op" }, clip(decodeInput(q).line, 80) || "(empty)")));
  });
  return h("aside", { class: "rp-side", "aria-label": "The clock, and the inputs to come" },
    h("h3", {}, "Clock"), clock,
    h("h3", {}, "Inputs"),
    h("ol", { class: "rp-queue" }, items),
    i + 1 >= m.n ? h("span", { class: "faint rp-q-end" }, "No more inputs.") : null);
}

// strip is the recording's inputs, one segment each: replayed so far,
// the one at hand, feature steps, the fault, where the replay stopped.
function strip(m, r) {
  return h("div", { class: `rp-strip ${m.n > 90 ? "dense" : ""}`, style: `--n:${m.n}`, "aria-hidden": "true" },
    m.steps.map((q, j) => h("i", {
      class: [j <= r.i ? "done" : "", j === r.i ? "cur" : "", j === m.fault.i ? "fault" : m.feats.has(q.tseq) ? "feat" : "", j === m.fail ? "stop" : "", j > m.end ? "off" : ""].join(" "),
    })));
}

function ctlRow(m, r) {
  const s = m.steps[r.i];
  const playing = !!timer;
  return h("div", { class: "rp-ctl-row" },
    h("button", { type: "button", class: "rp-ctl", "data-fid": "prev", "aria-label": "Back one step", title: "Back one step (←)", disabled: r.i <= 0, onclick: () => { stop(); go(r.i - 1); } }, icon("M8 2 L3 6 L8 10", false)),
    h("button", { type: "button", class: "rp-ctl play", "data-fid": "play", "aria-label": playing ? "Pause" : "Play", title: `${playing ? "Pause" : "Play"} (space)`, onclick: toggle }, icon(playing ? "M2 1 H5 V11 H2 Z M7 1 H10 V11 H7 Z" : "M2.5 1 L11 6 L2.5 11 Z", true)),
    h("button", { type: "button", class: "rp-ctl", "data-fid": "next", "aria-label": "Forward one step", title: "Forward one step (→)", disabled: r.i >= m.end, onclick: () => { stop(); go(r.i + 1); } }, icon("M4 2 L9 6 L4 10", false)),
    m.fault.i >= 0 ? h("button", { type: "button", class: "rp-jump", "data-fid": "jump", title: `Jump to the ${m.traffic ? "first changed feature step" : m.incident ? "recorded failure" : "fault"} (F)`, onclick: jump }, `Jump to ${m.traffic ? "change" : m.incident ? "failure" : "fault"} @${m.steps[m.fault.i].seq}`) : null,
    h("span", { class: "rp-spacer" }),
    h("span", { class: "m rp-pos", "aria-live": playing ? "off" : "polite" }, h("b", {}, `@${s.seq}`),
      h("span", { class: "faint" }, ` · ${r.i + 1} of ${m.end + 1} replayed · ${m.n} in the ${m.incident ? "recording" : "variant"}${ctx.d.truncated || ctx.d.journal_truncated ? " (the API sent only the first steps)" : ""}`)));
}

function icon(d, fill, size = 12) {
  const ns = "http://www.w3.org/2000/svg";
  const svg = document.createElementNS(ns, "svg");
  svg.setAttribute("width", String(size));
  svg.setAttribute("height", String(size));
  svg.setAttribute("viewBox", `0 0 ${size} ${size}`);
  svg.setAttribute("aria-hidden", "true");
  const p = document.createElementNS(ns, "path");
  p.setAttribute("d", d);
  if (fill) p.setAttribute("fill", "currentColor");
  else { p.setAttribute("fill", "none"); p.setAttribute("stroke", "currentColor"); p.setAttribute("stroke-width", size > 12 ? "2.2" : "1.6"); p.setAttribute("stroke-linecap", "round"); p.setAttribute("stroke-linejoin", "round"); }
  svg.append(p);
  return svg;
}

// controls is the strip with the scrubber over it, its ends, and the
// buttons; the scrubber is the one part update leaves in place.
function controls(m, r) {
  const range = h("input", {
    id: "rp-scrub", class: "rp-range", type: "range", min: "0", max: String(m.n - 1), step: "1", value: String(r.i),
    "aria-valuetext": valueText(m, r.i),
    oninput: (e) => {
      stop();
      const v = Math.min(+e.target.value, m.end);
      if (+e.target.value !== v) e.target.value = String(v);
      go(v);
    },
  });
  return h("div", { class: "rp-scrub" },
    h("div", { class: "rp-track" }, strip(m, r), h("label", { class: "rp-sr", for: "rp-scrub" }, "Step"), range),
    h("div", { class: "m faint rp-ends", "aria-hidden": "true" }, h("span", {}, `@${m.steps[0].seq}`), h("span", {}, `@${m.steps[m.n - 1].seq}`)),
    ctlRow(m, r));
}

function legend(m) {
  return h("div", { class: "rp-legend" },
    h("span", {}, h("i", { class: "lg-cur" }), "the input at hand"),
    m.featIdx.length ? h("span", {}, h("i", { class: "lg-feat" }), "feature step") : null,
    m.fault.i >= 0 ? h("span", {}, h("i", { class: "lg-fault" }), m.traffic ? "the first changed feature step" : m.incident ? "the recorded failure" : "fault: the mutated input") : null,
    m.fail >= 0 ? h("span", {}, h("i", { class: "lg-stop" }), "replay stopped") : null,
    m.end < m.n - 1 ? h("span", {}, h("i", { class: "lg-off" }), "not replayed") : null,
    h("span", { class: "faint rp-keys" }, "← → step · space play · F fault · Esc back"));
}

function lampState(m, name, i) {
  const seq = m.steps[i] ? m.steps[i].seq : "?";
  const checks = `${i + 1} check${i ? "s" : ""}`;
  if (!m.nw) return ["off", "not replayed", m.incident ? `the ${m.bname}'s replay of the ${m.traffic ? "traffic" : "incident"} wasn't saved` : "the new build's replay of this variant wasn't saved"];
  if (m.fail < 0 || i < m.fail) return ["held", `✓ held, ${checks}`, `held after every step up to @${seq}`];
  if (m.nw.status === "invariant_violated" && m.broke) {
    if (name === m.broke) return ["broke", `✕ broke after @${seq}`, m.nw.detail || `broke after input @${seq}; the replay stopped`];
    return m.names.indexOf(name) < m.names.indexOf(m.broke)
      ? ["held", `✓ held, ${checks}`, `held after @${seq}; checked before ${m.broke}`]
      : ["unchecked", `○ not checked at @${seq}`, `${m.broke} broke first, so it wasn't checked after @${seq}`];
  }
  return ["unchecked", `○ not checked at @${seq}`, `the step ended ${m.nw.status} before the invariants were checked`];
}

function lampEl(m, name, i, k) {
  const [st, sub, why] = lampState(m, name, i);
  const on = state.iv.inv === name;
  return h("button", {
    type: "button", class: `rp-lamp ${st} ${on ? "on" : ""}`, "data-fid": `lamp-${k}`, title: m.incident ? why : `${why}. Select it in the Invariants tab.`,
    "aria-label": `${name}: ${sub.replace(/^[✓✕○]\s*/, "")}`, "aria-pressed": String(!!on),
    onclick: () => {
      if (state.replay.incident) return;
      const checks = checksOf(state.steps);
      const idx = checks.findIndex((c) => (c.round || c.n) === state.replay.round);
      state.iv.inv = name;
      state.iv.page = "results";
      if (idx >= 0) { state.iv.round = idx === checks.length - 1 ? null : idx; state.iv.variant = state.replay.id; }
      state.inspTab = "invariants";
      renderView();
    },
  },
    h("span", { class: "rp-bulb", "aria-hidden": "true" }),
    h("span", { class: "rp-lamp-txt" }, h("span", { class: "m b" }, name), h("span", { class: "rp-lamp-sub" }, sub)));
}
