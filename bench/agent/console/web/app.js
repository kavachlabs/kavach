// The web console of kavach bench: a client of the API in ../API.md, served
// by the same server.

import { checksOf, renderInvariantBand, renderInvariants } from "./invariants.js";
import { closeReplay, renderReplay, replayKey } from "./replay.js";
import { renderHeadline, renderProbe, renderScrubber, renderTrail, showStep, trailRows } from "./trail.js";
import { LABS } from "./logos.js";
import { retheme } from "./monaco.js";
import { TRIAL_VIEWS, renderTrialNew, renderTrialView } from "./trial.js";
import { NARROW, UI_PREFS_KEY, initialPanes, prefsJSON, readPrefs, sideOpen as sideOpenOn, statusRedraw, toggleFold, toggleRight, toggleSide } from "./uistate.js";

// Where the theme a person picked is kept; index.html reads it too.
const THEME_KEY = "kavach-bench-theme";

const COMMANDS = [
  ["/run", "start the benchmark run"],
  ["/model <id>", "add a model by its OpenRouter id"],
  ["/models [search]", "browse and pick models from OpenRouter"],
  ["/arm kavach|control|workflow|kavach-implement|both", "choose the experiment arms"],
  ["/runs <n>", "attempts per task and model"],
  ["/timeout <minutes>|default", "override the tasks' agent timeout"],
  ["/agent opencode|oracle|nop", "choose the Harbor agent"],
  ["/task <name>", "choose the benchmark task"],
  ["/settings", "open API keys & environment settings"],
  ["/matrix", "open the latest run's matrix"],
  ["/compare", "open the latest run's comparison results"],
];

const AGENTS = {
  opencode: { label: "OpenCode", desc: "Autonomous coding agent via OpenRouter proxy" },
  oracle: { label: "Oracle", desc: "Applies reference solution to verify grading" },
  nop: { label: "Nop", desc: "Makes no changes to verify baseline failure" },
};

const ARMS = ["control", "workflow", "kavach", "kavach-implement"];
const ARM_SUFFIX = /-(kavach-implement|workflow|control|kavach)$/;
const SETTINGS_KEY = "kavach-bench.new-run";

// Authoritative descriptions for each experiment arm shown on tiles, headers, and task cards.
const ARM_INFO = {
  control: {
    label: "control",
    tag: "Baseline",
    tone: "blue",
    short: "Base repo + bug report or spec only (no Kavach tools)",
    tileHint: "Baseline arm: agent receives only the repository and prompt. Kavach checks the patch after the run.",
    detail: "The agent works with the base repository and issue/feature prompt alone, without Kavach CLI or MCP tools. After the attempt finishes, the grader runs kavach diff or kavach contract check post-hoc.",
  },
  workflow: {
    label: "workflow",
    tag: "Test workflow",
    tone: "blue",
    short: "Contract-first workflow using the repo's own test suite",
    tileHint: "Workflow arm: agent follows a contract-first test workflow using repo tests only (no Kavach tools).",
    detail: "The agent is instructed to follow a contract-first workflow using only the project's existing unit test suite, isolating the effect of workflow discipline from Kavach's verifier.",
  },
  kavach: {
    label: "kavach",
    tag: "Kavach-assisted",
    tone: "peach",
    short: "Kavach CLI & MCP + recorded incident fixture or locked contract",
    tileHint: "Kavach arm: agent uses kavach diff / contract check with the incident fixture or locked contract during the run.",
    detail: "The agent is equipped with the kavach CLI and MCP server plus the recorded production fixture (incidents: kavach diff with variants) or locked behavior contract (features: kavach contract check).",
  },
  "kavach-implement": {
    label: "kavach-implement",
    tag: "Self-contract",
    tone: "peach",
    short: "Kavach tools · agent records traffic & builds its own contract",
    tileHint: "Kavach-implement arm: agent records baseline traffic, writes invariants, and checks its own contract.",
    detail: "The agent has the Kavach SDK integration and tools, and autonomously records baseline traffic, declares invariants from the specification, and initializes and checks its own contract.",
  },
};

// What an arm gets differs by track: an incident's bug report and fixture, or
// a feature's spec and contract.
const ARM_TRACK = {
  incident: {
    control: { short: "Base repo + bug report only (no Kavach tools)" },
    kavach: {
      short: "Kavach CLI & MCP + the recorded incident fixture",
      tileHint: "Kavach arm: agent checks its fix with kavach diff on the recorded incident during the run.",
    },
  },
  feature: {
    control: { short: "Base repo + spec only (no Kavach tools)" },
    kavach: {
      short: "Kavach CLI & MCP + the locked behaviour contract",
      tileHint: "Kavach arm: agent checks its build with kavach contract check against the locked contract during the run.",
    },
  },
};

function armInfo(arm, track) {
  const info = ARM_INFO[arm];
  if (info && track && ARM_TRACK[track] && ARM_TRACK[track][arm]) return { ...info, ...ARM_TRACK[track][arm] };
  return info || {
    label: arm || "task",
    tag: "Standard",
    tone: "peach",
    short: "Standard benchmark execution arm",
    tileHint: "Standard benchmark trial execution.",
    detail: "Standard benchmark task execution.",
  };
}

// The panes survive a reload: uistate.js says what is saved and restored.
function loadUIPrefs() {
  let raw = null;
  try {
    raw = localStorage.getItem(UI_PREFS_KEY);
  } catch {}
  return readPrefs(raw);
}

function saveUIPrefs() {
  try {
    localStorage.setItem(UI_PREFS_KEY, prefsJSON(state));
  } catch {}
}

const narrowScreen = () => window.matchMedia(NARROW).matches;
const initialPrefs = loadUIPrefs();

// The editors code links can open, by their URL schemes. The source tree is
// only in a task's image, so links open the patch that has the code.
export const CODE_EDITORS = [
  { id: "vscode", label: "VS Code" },
  { id: "cursor", label: "Cursor" },
  { id: "antigravity", label: "Antigravity" },
  { id: "windsurf", label: "Windsurf" },
  { id: "zed", label: "Zed" },
];
const CODE_EDITOR_KEY = "kavach-bench.code-editor";

function loadCodeEditor() {
  try {
    const v = localStorage.getItem(CODE_EDITOR_KEY);
    if (CODE_EDITORS.some((e) => e.id === v)) return v;
  } catch {}
  return "vscode";
}

// setCodeEditor picks the editor code links open in.
export function setCodeEditor(id) {
  if (!CODE_EDITORS.some((e) => e.id === id)) return;
  state.codeEditor = id;
  try { localStorage.setItem(CODE_EDITOR_KEY, id); } catch {}
  renderView();
  if (state.settings.open) renderSettingsModal();
}

// editorURL opens path (absolute) at line in the chosen editor.
export function editorURL(path, line, editor = state.codeEditor) {
  const p = encodeURI(path).replace(/#/g, "%23").replace(/\?/g, "%3F");
  return `${editor}://file${p.startsWith("/") ? "" : "/"}${p}:${line}:1`;
}

export const state = {
  status: null, statusErr: "",
  tasks: null, tasksErr: "",
  jobs: null, jobsErr: "",
  job: null, jobErr: "", compare: null,
  sel: "", steps: null, stepsFor: "",
  grades: new Map(),
  // side (the left sidebar: null follows the screen), rightSide (the right
  // auxiliary pane), folded (the sidebar sections folded shut: "runs",
  // "tasks", "incident", "feature"), leftW and rightW.
  ...initialPanes(initialPrefs, narrowScreen()),
  auxTab: "task", // "task" | "arms" | "models"
  codeEditor: loadCodeEditor(), // CODE_EDITORS id
  live: false,
  form: loadForm(),
  picker: { open: false, q: "", list: null, err: "", previewId: "" },
  modelCatalog: new Map(), // id -> Model object for Model Cards
  modelsLoading: false,
  openSelect: "", // id of currently open custom select menu
  selectFilter: "",
  // tab: "general" | "keys" | "environment" | "shortcuts"
  settings: { open: false, tab: "general", data: null, loading: false, saving: false, err: "", msg: "", drafts: {}, reveal: {} },
  confirm: "", actionErr: "", note: "", noteErr: false, starting: false,
  // The attempt inspector: its tab, and what the Invariants tab shows (round
  // is an index into the attempt's checks, null for the latest).
  inspTab: "steps", // "steps" | "trail" | "invariants"
  iv: { page: "results", inv: "", view: "radar", kind: null, variant: null, round: null },
  // The replay scene (replay.js) in the left pane, while a saved variant is
  // open: { job, trial, round, id, i } with i the step shown.
  replay: null,
};

function applySidebarWidths() {
  document.documentElement.style.setProperty("--left-w", `${state.leftW}px`);
  document.documentElement.style.setProperty("--right-w", `${state.rightW}px`);
}
applySidebarWidths();

function svgPanelIcon(side = "right") {
  const ns = "http://www.w3.org/2000/svg";
  const svg = document.createElementNS(ns, "svg");
  svg.setAttribute("class", "pane-ic");
  svg.setAttribute("width", "16");
  svg.setAttribute("height", "16");
  svg.setAttribute("viewBox", "0 0 16 16");
  svg.setAttribute("fill", "none");
  svg.setAttribute("aria-hidden", "true");

  const frame = document.createElementNS(ns, "rect");
  frame.setAttribute("x", "1.5");
  frame.setAttribute("y", "2.5");
  frame.setAttribute("width", "13");
  frame.setAttribute("height", "11");
  frame.setAttribute("rx", "2");
  frame.setAttribute("stroke", "currentColor");
  frame.setAttribute("stroke-width", "1.35");

  const divider = document.createElementNS(ns, "line");
  divider.setAttribute("x1", side === "left" ? "6" : "10");
  divider.setAttribute("y1", "2.5");
  divider.setAttribute("x2", side === "left" ? "6" : "10");
  divider.setAttribute("y2", "13.5");
  divider.setAttribute("stroke", "currentColor");
  divider.setAttribute("stroke-width", "1.35");

  const fill = document.createElementNS(ns, "rect");
  fill.setAttribute("class", "pane-fill");
  fill.setAttribute("x", side === "left" ? "2.5" : "10.5");
  fill.setAttribute("y", "3.5");
  fill.setAttribute("width", "3");
  fill.setAttribute("height", "9");
  fill.setAttribute("rx", "0.8");
  fill.setAttribute("fill", "currentColor");

  svg.append(frame, divider, fill);
  return svg;
}

export function h(tag, attrs, ...kids) {
  const el = document.createElement(tag);
  for (const [k, v] of Object.entries(attrs || {})) {
    if (v == null || v === false) continue;
    if (k.startsWith("on")) el.addEventListener(k.slice(2), v);
    else if (k === "class") el.className = v;
    else if (k === "dataset") Object.assign(el.dataset, v);
    else el.setAttribute(k, v === true ? "" : v);
  }
  for (const kid of kids.flat(Infinity)) {
    if (kid == null || kid === false) continue;
    el.append(kid instanceof Node ? kid : String(kid));
  }
  return el;
}

// STATIC is set in a copy written by bench export: some runs, read-only, on
// a static host. Its meta tag names the run it opens on.
const staticMeta = document.querySelector('meta[name="bench-static"]');
export const STATIC = staticMeta !== null;
const LANDING = staticMeta ? staticMeta.content : "";

// TRIAL is the public trial server: people bring their own keys and start a
// few runs. It tells the page by a meta tag, or by answering the session.
export let TRIAL = !STATIC && document.querySelector('meta[name="bench-trial"]') !== null;
export const trial = { session: null };

export async function loadTrialSession() {
  try {
    const resp = await fetch("/api/trial/session");
    if (resp.ok) {
      trial.session = await resp.json();
      TRIAL = true;
    }
  } catch {}
}
if (!STATIC) await loadTrialSession();

// trialReady is whether this session may start a run.
export const trialReady = () => TRIAL && !!trial.session && (trial.session.has_keys || trial.session.invite);

// apiURL is where an API request goes: to bench api, or in a copy to the
// file bench export wrote for it (staticPath in cmd/bench/export.go).
export function apiURL(path) {
  if (!STATIC) return "/api" + path;
  const [p, q = ""] = path.split("?");
  const params = new URLSearchParams(q);
  const base = "api" + decodeURI(p);
  if (params.has("i")) return `${base}/${params.get("i")}.txt`;
  if (params.has("path")) return `${base}/${fnv1a(params.get("path"))}.txt`;
  return `${base}/index.json`;
}

// fnv1a is the 32-bit FNV-1a hash of s's UTF-8 bytes, in hex.
function fnv1a(s) {
  let h = 0x811c9dc5;
  for (const b of new TextEncoder().encode(s)) h = Math.imul(h ^ b, 0x01000193) >>> 0;
  return h.toString(16).padStart(8, "0");
}

export async function api(path, opts) {
  const resp = await fetch(apiURL(path), opts);
  const text = await resp.text();
  let body = null;
  try { body = text ? JSON.parse(text) : null; } catch { body = text; }
  if (!resp.ok) {
    const err = new Error((body && body.error) || `${resp.status} ${resp.statusText}`);
    err.status = resp.status;
    throw err;
  }
  return body;
}

function loadForm() {
  const form = { task: "", arms: ["kavach", "control"], models: [], runs: 1, agent: "opencode", concurrent: 1, retries: 1, timeout: 0, modelSort: "", environment: "docker" };
  try {
    Object.assign(form, JSON.parse(localStorage.getItem(SETTINGS_KEY) || "{}"));
  } catch {}
  return form;
}

function saveForm() {
  try { localStorage.setItem(SETTINGS_KEY, JSON.stringify(state.form)); } catch {}
}

const base = (task) => task.replace(ARM_SUFFIX, "");
const orId = (model) => model.replace(/^openrouter\//, "");
const shortModel = (model) => orId(model).split("/").pop();
// OpenRouter prefixes a model that tracks a lab's latest release with "~".
const labOf = (model) => orId(model).split("/")[0].replace(/^~/, "");
const needsModel = (agent) => agent !== "oracle" && agent !== "nop";
const trialKey = (t) => `${t.task}|${t.model}|${t.attempt}`;
const fresh = (tok) => tok ? (tok.fresh ?? tok.input + tok.output) : 0;

function fmtDur(sec) {
  if (sec == null) return "–";
  sec = Math.round(sec);
  if (sec < 60) return `${sec}s`;
  if (sec < 3600) return `${Math.floor(sec / 60)}m ${String(sec % 60).padStart(2, "0")}s`;
  return `${Math.floor(sec / 3600)}h ${String(Math.floor(sec / 60) % 60).padStart(2, "0")}m`;
}

function clock(sec) {
  sec = Math.max(0, Math.floor(sec));
  const s = String(sec % 60).padStart(2, "0");
  if (sec < 3600) return `${Math.floor(sec / 60)}:${s}`;
  return `${Math.floor(sec / 3600)}:${String(Math.floor(sec / 60) % 60).padStart(2, "0")}:${s}`;
}

function fmtTok(n) {
  if (n == null) return "–";
  if (n < 1000) return String(n);
  if (n < 10000) return `${(n / 1000).toFixed(1)}k`;
  if (n < 1e6) return `${Math.round(n / 1000)}k`;
  return `${(n / 1e6).toFixed(1)}M`;
}

function fmtCost(c) {
  if (c == null) return "–";
  if (c === 0) return "$0";
  return c < 0.01 ? `$${c.toFixed(3)}` : `$${c.toFixed(2)}`;
}

function fmtPricePerM(v) {
  if (v == null) return "varies";
  if (v === 0) return "$0";
  return `$${v < 1 ? v.toFixed(2) : v.toFixed(v < 10 ? 2 : 0)}`;
}

const since = (iso) => (Date.now() - Date.parse(iso)) / 1000;

// A live elapsed time; the ticker below rewrites it every second.
function elapsed(iso, fmt = clock, prefix = "") {
  return h("span", { dataset: { since: iso, fmt: fmt === clock ? "clock" : "dur", prefix } }, prefix + fmt(since(iso)));
}

// What a running attempt is up to, shown the way Claude shows it's thinking.
const VERBS = [
  "Apparating", "Brewing", "Conjuring", "Transfiguring", "Summoning", "Levitating",
  "Hexing", "Jinxing", "Unjinxing", "Charming", "Divining", "Scrying", "Sorting",
  "Wand-waving", "Mischief-managing", "Owl-posting", "Floo-travelling", "Marauding",
  "Pensieving", "Potion-stirring", "Spell-checking", "Lumos-ing",
  "Forging", "Questing", "Wandering", "Second-breakfasting", "Riddling", "Ent-mooting",
  "Beacon-lighting", "Palantír-gazing", "Delving", "Mithril-mining", "Lembas-baking",
  "Fellowshipping", "Ring-bearing", "Shire-strolling", "Ranger-tracking", "Wizarding",
];
const VERB_EVERY = 9000;

// verbFor picks the verb a seed shows now: the same on every redraw, and a
// new one every few seconds.
function verbFor(seed) {
  let n = 0;
  for (const c of seed) n = (n * 31 + c.charCodeAt(0)) >>> 0;
  return VERBS[(n + Math.floor(Date.now() / VERB_EVERY)) % VERBS.length] + "…";
}

// spinner is Claude's glyph spinner, drawn by CSS.
const spinner = () => h("span", { class: "st running", "aria-hidden": "true" });

setInterval(() => {
  for (const el of document.querySelectorAll("[data-since]")) {
    const s = since(el.dataset.since);
    el.textContent = el.dataset.prefix + (el.dataset.fmt === "clock" ? clock(s) : fmtDur(s));
  }
  for (const el of document.querySelectorAll("[data-verb]")) {
    const v = verbFor(el.dataset.verb);
    if (el.textContent !== v) el.textContent = v;
  }
}, 1000);

function jobShape(job, compact) {
  const bases = [...new Set(job.tasks.map(base))];
  const arms = new Set(job.tasks.map((t) => (t.match(ARM_SUFFIX) || [])[1] || "")).size;
  const models = job.models.length;
  const per = job.tasks.length * Math.max(models, 1);
  const runs = per ? Math.round(job.trials_total / per) : 0;
  const name = (compact && bases.length > 2 ? `${bases[0]} +${bases.length - 1}` : bases.join(", ")) || job.id;
  if (!job.tasks.length) return { name, shape: "" };
  const who = models ? (compact ? `${models}` : `${models} model${models > 1 ? "s" : ""}`) : job.agent;
  const shape = compact ? `${who}×${arms}×${runs}` : `${who} × ${arms} arm${arms > 1 ? "s" : ""} × ${runs}`;
  return { name, shape };
}

const jobTitle = (job) => {
  const { name, shape } = jobShape(job, false);
  return shape ? `${name} · ${shape}` : name;
};

function route() {
  const parts = location.hash.replace(/^#\/?/, "").split("/").map(decodeURIComponent);
  if (parts[0] === "jobs" && parts[1]) return { view: parts[2] === "compare" ? "compare" : "runs", job: parts[1] };
  if (TRIAL) {
    if (parts[0] === "trial" && TRIAL_VIEWS.includes(parts[1])) return { view: parts[1] };
    if (parts[0] === "new") return { view: "new", task: parts[1] || "" };
    return { view: "landing" };
  }
  // A copy has no new-run form: it opens on its landing run's results.
  if (STATIC) return { view: "compare", job: LANDING };
  return { view: "new", task: parts[0] === "new" ? parts[1] || "" : "" };
}

let current = route();
if (current.view === "new" && current.task) {
  state.form.task = current.task;
}

async function onRoute() {
  const prev = current;
  current = route();
  state.confirm = "";
  state.actionErr = "";
  state.openSelect = "";
  closeNav();
  if (current.view === "new" && current.task) {
    state.form.task = current.task;
    saveForm();
  }
  if (current.job !== prev.job) {
    state.job = null;
    state.jobErr = "";
    state.compare = null;
    state.sel = "";
    state.steps = null;
    state.stepsFor = "";
    state.replay = null;
  }
  if (narrowScreen()) state.side = false;
  await enterView(() => {
    renderAll();
    if (current.job) return refreshJob();
  });
}

// enterView fades in what render draws, including a redraw once the job has
// loaded; later redraws appear without it.
async function enterView(render) {
  const view = document.getElementById("view");
  view.classList.add("entering");
  await render();
  setTimeout(() => view.classList.remove("entering"), 300);
}

// loadStatus runs every 15s; uistate.js says whether a poll redraws.
let drawnStatus;
async function loadStatus() {
  try {
    state.status = await api("/status");
    state.statusErr = "";
  } catch (e) {
    state.statusErr = e.message;
  }
  const draw = statusRedraw(drawnStatus, state.status, state.statusErr);
  if (!draw.redraw) return;
  drawnStatus = draw.key;
  renderTop();
  renderSide();
  if (current.view === "new") renderView();
}

async function loadSettings() {
  try {
    state.settings.data = await api("/settings");
    state.settings.err = "";
  } catch (e) {
    state.settings.err = e.message;
  }
  renderSide();
  if (state.settings.open) renderSettingsModal();
}

async function loadTasks() {
  try {
    state.tasks = await api("/tasks");
    state.tasksErr = "";
  } catch (e) {
    state.tasksErr = e.message;
  }
  renderSide();
  if (current.view === "new") renderView();
}

async function loadJobs() {
  try {
    state.jobs = await api("/jobs");
    state.jobsErr = "";
  } catch (e) {
    state.jobsErr = e.message;
  }
  renderSide();
  renderTop();
  if (TRIAL && TRIAL_VIEWS.includes(current.view)) renderView();
}

async function ensureModelsLoaded() {
  if (state.modelCatalog.size > 0 || state.modelsLoading) return;
  state.modelsLoading = true;
  try {
    const list = await api("/models");
    for (const m of list || []) {
      state.modelCatalog.set(m.id, m);
    }
    if (!state.picker.list) state.picker.list = list;
    if (current.view === "new") renderView();
  } catch {} finally {
    state.modelsLoading = false;
  }
}

let refreshTimer = 0;
function scheduleRefresh() {
  clearTimeout(refreshTimer);
  refreshTimer = setTimeout(refreshJob, 250);
}

async function refreshJob() {
  const id = current.job;
  if (!id) return;
  try {
    const job = await api(`/jobs/${encodeURIComponent(id)}`);
    if (current.job !== id) return;
    job.trials = job.trials || [];
    state.job = job;
    state.jobErr = "";
    if (current.view === "compare") state.compare = await api(`/jobs/${encodeURIComponent(id)}/compare`);
  } catch (e) {
    if (current.job !== id) return;
    state.jobErr = e.status === 404 ? "notfound" : e.message;
  }
  const sel = selected();
  if (sel && sel.id && current.view === "runs") await loadSteps(sel);
  if (current.view === "compare" && state.job) await loadGrades(state.job.trials.filter(finished));
  else if (sel && finished(sel)) await loadGrades([sel]);
  renderAll();
}

async function loadSteps(t) {
  const key = `${t.job}/${t.id}`;
  try {
    const steps = await api(`/trials/${encodeURIComponent(t.job)}/${encodeURIComponent(t.id)}/steps`);
    if (selected() && selected().id === t.id) {
      state.steps = steps;
      state.stepsFor = key;
    }
  } catch (e) {
    state.steps = { error: e.message };
    state.stepsFor = key;
  }
}

const finished = (t) => t.id && (t.status === "pass" || t.status === "fail" || t.status === "error");

async function loadGrades(trials) {
  await Promise.all(trials.filter((t) => !state.grades.has(`${t.job}/${t.id}`)).map(async (t) => {
    let g = null;
    try {
      g = await api(`/trials/${encodeURIComponent(t.job)}/${encodeURIComponent(t.id)}/grade`);
    } catch {}
    state.grades.set(`${t.job}/${t.id}`, g);
  }));
}

// Events carry the job or trial as it is now. A model call changes only a
// trial's steps, tokens and cost, and the job's cost; those are merged in and
// patched where they are shown, so the page around them stays as it is.
// Anything else (a trial starting or finishing, a job's status) refetches the
// job and redraws it.
function connectEvents() {
  const es = new EventSource("/api/events");
  let dropped = false;
  es.onopen = () => {
    state.live = true;
    renderTop();
    // Events sent while the stream was down are lost.
    if (dropped) {
      dropped = false;
      loadJobs();
      scheduleRefresh();
    }
  };
  es.onerror = () => { state.live = false; dropped = true; renderTop(); };
  es.addEventListener("job", (e) => onJob(JSON.parse(e.data)));
  es.addEventListener("trial", (e) => onTrial(JSON.parse(e.data)));
}

const jobShapeOf = ({ trials, cost, ...rest }) => JSON.stringify(rest);

function onJob(job) {
  if (!state.jobs) state.jobs = [];
  const i = state.jobs.findIndex((j) => j.id === job.id);
  const costOnly = i >= 0 && jobShapeOf(state.jobs[i]) === jobShapeOf(job);
  if (i >= 0) state.jobs[i] = job;
  else state.jobs.push(job);
  if (costOnly) {
    if (job.id !== current.job || !state.job) return;
    state.job.cost = job.cost;
    const pill = document.querySelector("#right .cost");
    if (pill) pill.textContent = fmtCost(job.cost);
    else renderTop();
    return;
  }
  state.jobs.sort((a, b) => (Date.parse(b.started) || 0) - (Date.parse(a.started) || 0));
  renderSide();
  renderTop();
  if (job.id === current.job) scheduleRefresh();
}

// Fields that move a trial between states; a change in any redraws the job.
const TRIAL_SHAPE = ["status", "reward", "started", "finished", "seconds", "error", "kavach", "opencode"];

function onTrial(t) {
  if (t.job !== current.job) return;
  const old = state.job && state.job.trials.find((x) => x.id && x.id === t.id);
  if (!old || TRIAL_SHAPE.some((k) => JSON.stringify(old[k]) !== JSON.stringify(t[k]))) {
    if (finished(t)) state.grades.delete(`${t.job}/${t.id}`);
    scheduleRefresh();
    return;
  }
  const more = old.steps !== t.steps;
  Object.assign(old, t);
  patchTrial(old, more);
}

let stepsReq = 0;
async function patchTrial(t, moreSteps) {
  if (current.view === "compare") {
    const meta = document.querySelector(`.kcard[data-key="${CSS.escape(trialKey(t))}"] .card-meta`);
    if (meta) meta.replaceWith(cardMeta(t));
    else renderView();
    return;
  }
  if (current.view !== "runs" || selected() !== t) return;
  const stats = document.querySelector(".right-pane > .stats");
  if (stats) stats.textContent = tokenLine(t);
  const key = `${t.job}/${t.id}`;
  if (!moreSteps || state.stepsFor !== key) return;
  const n = ++stepsReq;
  let steps;
  try {
    steps = await api(`/trials/${encodeURIComponent(t.job)}/${encodeURIComponent(t.id)}/steps`);
  } catch {
    return;
  }
  if (n === stepsReq && selected() === t && state.stepsFor === key) patchSteps(t, steps);
}

// patchSteps renders the steps that are new or changed since the last load
// and leaves the others, with their scroll, selection and expanded state.
function patchSteps(t, steps) {
  const old = Array.isArray(state.steps) ? state.steps : [];
  const same = (a, b) => a && JSON.stringify(a) === JSON.stringify(b);
  // Unchanged steps keep their objects: the invariants view finds a check's
  // step by identity.
  const merged = steps.map((s, i) => same(old[i], s) ? old[i] : s);
  const checks = checksOf(merged).map((c) => c.step);
  const before = checksOf(old).map((c) => c.step);
  state.steps = merged;
  if (checks.length !== before.length || checks.some((s, i) => s !== before[i])) return renderView();

  const shown = merged.filter((s) => s.kind !== "usage");
  const oldShown = old.filter((s) => s.kind !== "usage");
  const badge = document.querySelector(".insp-tabs .aux-tab:first-child .aux-tab-badge");
  if (badge) badge.textContent = shown.length;
  if (document.querySelector(".insp-tabs") && state.inspTab === "invariants") return;
  if (state.inspTab === "trail") {
    if (shown.filter((s) => s.kavach).length !== oldShown.filter((s) => s.kavach).length) renderView();
    return;
  }
  const scrub = document.querySelector(".scrub");
  if (scrub) { const next = renderScrubber(t, shown, clock); if (next) scrub.replaceWith(next); }

  const key = CSS.escape(`${t.job}/${t.id}`);
  const box = document.querySelector(`.steps[data-key="${key}"]`);
  const placeholder = !box && document.querySelector(`[data-steps="${key}"]`);
  if (placeholder) {
    const el = renderSteps(t, state.job);
    placeholder.replaceWith(el);
    el.scrollTop = el.scrollHeight;
    return;
  }
  if (!box || box.children.length !== oldShown.length || shown.length < oldShown.length) return renderView();
  const atBottom = box.scrollHeight - box.scrollTop - box.clientHeight < 24;
  let lastText = -1;
  shown.forEach((s, i) => { if (s.kind === "text") lastText = i; });
  shown.forEach((s, i) => {
    if (s === oldShown[i]) return;
    const el = stepEl(s, i, i === lastText, t.model);
    const prev = box.children[i];
    if (!prev) return box.append(el);
    if (prev.matches("details[open]") || prev.querySelector("details[open]")) {
      const d = el.matches("details") ? el : el.querySelector("details");
      if (d) d.open = true;
    }
    prev.replaceWith(el);
  });
  if (atBottom && t.status === "running") box.scrollTop = box.scrollHeight;
}

function statusDot(name, check) {
  if (!check) return null;
  const word = check.ok ? `${name} ${check.version}` : `${name} down`;
  return h("span", { class: "status-pill", title: check.ok ? `${name} ${check.version} ready` : check.error },
    h("span", { class: `dot ${check.ok ? "" : "bad"}` }), word);
}

// syncPill is a run's backup to R2. A copy on a static host has none.
function syncPill(sync) {
  if (STATIC || TRIAL || !sync) return null;
  const held = sync.skipped ? `${sync.skipped} file${sync.skipped === 1 ? "" : "s"} held back by the secret guard` : "";
  const [label, dot, tip] = {
    off: ["R2 off", "off", "Not backed up: set the R2 keys in Settings"],
    pending: ["R2 pending", "off", "Not backed up yet"],
    syncing: ["R2 syncing", "run", "Uploading to R2"],
    synced: ["R2 ✓", "", `Backed up to R2 ${sync.at ? new Date(sync.at).toLocaleString() : ""} (${sync.files} files)`],
    failed: ["R2 failed", "bad", sync.error || "The last sync failed"],
  }[sync.state] || [sync.state, "off", ""];
  const tipHeld = held ? `${tip}. ${held}: ${(sync.skipped_files || []).map((f) => f.path).join(", ")}` : tip;
  return h("span", { class: "status-pill", title: tipHeld },
    h("span", { class: `dot ${dot}` }), held ? `${label} · ${sync.skipped} held back` : label);
}

function renderTop() {
  const right = document.getElementById("right");
  const title = document.getElementById("title-text");
  let text = "New run";
  const kids = [];
  document.querySelector(".app").classList.toggle("trial-bare", TRIAL && (current.view === "new" || TRIAL_VIEWS.includes(current.view)));
  if (TRIAL && TRIAL_VIEWS.includes(current.view)) {
    title.textContent = current.view === "landing" ? "kavach bench · trial" : "Trial setup";
    document.title = "Try Kavach bench";
    return right.replaceChildren(h("a", { class: "btn wide", href: "#/" }, "Home"), themeToggle());
  }
  if (!state.live && state.statusErr) {
    kids.push(h("span", { class: "status-pill bad" }, h("span", { class: "dot bad" }), "server unreachable"));
  }
  const job = current.job && (state.job || (state.jobs || []).find((j) => j.id === current.job));
  if (current.view !== "new" && job) {
    text = jobTitle(job);
    const dot = job.status === "running" || job.status === "pausing" ? "run" : job.status === "done" ? "" : "off";
    const doneOf = `${job.trials_done} of ${job.trials_total}`;
    kids.push(h("span", { class: "status-pill" },
      h("span", { class: `dot ${dot}` }),
      job.status === "running" || job.status === "done" ? `${doneOf} done` : `${job.status} · ${doneOf}`));
    if (job.environment && ENVIRONMENTS[job.environment]) kids.push(h("span", { class: "status-pill", title: "Where this run's attempts execute" }, ENVIRONMENTS[job.environment]));
    kids.push(syncPill(job.sync));
    if (TRIAL) kids.push(h("a", { class: "btn wide", href: "#/" }, "Home"), shareButton());
    if (job.cost) kids.push(h("span", { class: "status-pill cost" }, fmtCost(job.cost)));
    if (job.started) {
      kids.push(h("span", { class: "wide" },
        job.finished ? fmtDur((Date.parse(job.finished) - Date.parse(job.started)) / 1000) : job.status === "running" ? elapsed(job.started, fmtDur) : "",
        job.status === "running" ? " elapsed" : ""));
    }
    const enc = encodeURIComponent(job.id);
    if (current.view === "runs") {
      kids.push(h("a", { class: "btn peach", href: `#/jobs/${enc}/compare` }, "Compare Results →"));
    } else {
      kids.push(h("a", { class: "btn wide", href: `#/jobs/${enc}` }, "← Matrix & Steps"));
      kids.push(h("button", { class: "btn", type: "button", onclick: exportCSV, disabled: !state.compare }, "Export CSV"));
    }
  } else if (current.view !== "new") {
    text = current.job;
  } else if (!TRIAL) {
    if (state.status) {
      kids.push(statusDot("docker", state.status.docker), statusDot("harbor", state.status.harbor));
      if (state.jobs) kids.push(h("span", { class: "wide" }, `${state.jobs.length} run${state.jobs.length === 1 ? "" : "s"} saved`));
    }
  }

  kids.push(themeToggle());

  // Antigravity symmetrical right-hand Auxiliary Pane toggle icon on #/new and #/jobs/<id>
  if ((current.view === "new" && !TRIAL) || current.view === "runs") {
    kids.push(h("button", {
      id: "aux-toggle",
      type: "button",
      class: `aux-toggle ${state.rightSide ? "open" : ""}`,
      "aria-expanded": String(state.rightSide),
      "aria-label": "Toggle right auxiliary pane",
      title: `${state.rightSide ? "Hide" : "Show"} right auxiliary pane (⌘J)`,
      onclick: toggleRightSide,
    }, svgPanelIcon("right")));
  }

  title.textContent = text;
  document.title = current.view === "new" ? "kavach bench" : `${text} · kavach bench`;
  right.replaceChildren(...kids.filter(Boolean));
}

// The theme in effect: the one a person picked, else the system's.
function currentTheme() {
  return document.documentElement.dataset.theme || (matchMedia("(prefers-color-scheme: dark)").matches ? "dark" : "light");
}

function themeToggle() {
  const dark = currentTheme() === "dark";
  const icon = dark
    ? '<circle cx="8" cy="8" r="3" stroke="currentColor" stroke-width="1.35"/><path d="M8 1.5v1.6M8 12.9v1.6M1.5 8h1.6M12.9 8h1.6M3.4 3.4l1.1 1.1M11.5 11.5l1.1 1.1M3.4 12.6l1.1-1.1M11.5 4.5l1.1-1.1" stroke="currentColor" stroke-width="1.35" stroke-linecap="round"/>'
    : '<path d="M13.5 9.6A5.6 5.6 0 0 1 6.4 2.5a5.6 5.6 0 1 0 7.1 7.1Z" stroke="currentColor" stroke-width="1.35" stroke-linejoin="round"/>';
  const btn = h("button", {
    type: "button",
    class: "theme-toggle",
    title: `Switch to ${dark ? "light" : "dark"} theme`,
    "aria-label": `Switch to ${dark ? "light" : "dark"} theme`,
    onclick: (e) => setTheme(dark ? "light" : "dark", e.currentTarget),
  });
  btn.innerHTML = `<svg width="16" height="16" viewBox="0 0 16 16" fill="none" aria-hidden="true">${icon}</svg>`;
  return btn;
}

// setTheme switches the theme, revealing the new one in a circle that grows
// from the button that asked for it.
function setTheme(theme, from) {
  const root = document.documentElement;
  const apply = () => {
    root.dataset.theme = theme;
    try { localStorage.setItem(THEME_KEY, theme); } catch {}
    retheme();
    renderTop();
  };
  if (!document.startViewTransition || document.hidden || matchMedia("(prefers-reduced-motion: reduce)").matches) return apply();
  const r = from.getBoundingClientRect();
  const x = r.left + r.width / 2;
  const y = r.top + r.height / 2;
  const radius = Math.hypot(Math.max(x, innerWidth - x), Math.max(y, innerHeight - y));
  // Without transitions, the new snapshot has the new colours, not the
  // first frame of a fade to them.
  root.classList.add("theme-switching");
  const t = document.startViewTransition(apply);
  t.ready.then(() => root.animate(
    { clipPath: [`circle(0px at ${x}px ${y}px)`, `circle(${radius}px at ${x}px ${y}px)`] },
    { duration: 560, easing: "cubic-bezier(0.16, 1, 0.3, 1)", pseudoElement: "::view-transition-new(root)" },
  ));
  t.finished.finally(() => root.classList.remove("theme-switching"));
}

// With no theme picked, follow the system's as it changes.
matchMedia("(prefers-color-scheme: dark)").addEventListener("change", () => {
  if (document.documentElement.dataset.theme) return;
  retheme();
  renderTop();
});

// The right pane's state as last drawn. Each render builds the layout anew, so
// a new layout starts from this state and transitions to the current one.
let drawnRightSide = null;

function slideRightSide(layout) {
  const was = drawnRightSide;
  drawnRightSide = state.rightSide;
  if (was === null || was === state.rightSide) return;
  layout.classList.toggle("noright", !was);
  void layout.offsetWidth;
  layout.classList.toggle("noright", !state.rightSide);
}

function toggleRightSide() {
  toggleRight(state);
  saveUIPrefs();
  renderTop();
  renderView();
}

function switchAuxTab(tab) {
  if (state.rightSide && state.auxTab === tab) {
    state.rightSide = false;
  } else {
    state.auxTab = tab;
    state.rightSide = true;
  }
  saveUIPrefs();
  renderTop();
  renderView();
}

function makeResizeSash(side) {
  const isLeft = side === "left";
  return h("div", {
    class: `sash ${isLeft ? "sash-left" : "sash-right"}`,
    role: "separator",
    "aria-orientation": "vertical",
    "aria-label": isLeft ? "Resize left sidebar" : "Resize right auxiliary pane",
    title: "Drag to resize · Double-click to reset width",
    ondblclick: (e) => {
      e.preventDefault();
      if (isLeft) state.leftW = 256;
      else state.rightW = 400;
      applySidebarWidths();
      saveUIPrefs();
    },
    onmousedown: (e) => {
      e.preventDefault();
      const startX = e.clientX;
      const startW = isLeft ? state.leftW : state.rightW;
      document.body.classList.add("resizing");
      const onMove = (ev) => {
        const dx = ev.clientX - startX;
        if (isLeft) {
          state.leftW = Math.max(200, Math.min(440, Math.round(startW + dx)));
        } else {
          state.rightW = Math.max(300, Math.min(680, Math.round(startW - dx)));
        }
        applySidebarWidths();
      };
      const onUp = () => {
        document.body.classList.remove("resizing");
        window.removeEventListener("mousemove", onMove);
        window.removeEventListener("mouseup", onUp);
        saveUIPrefs();
      };
      window.addEventListener("mousemove", onMove);
      window.addEventListener("mouseup", onUp);
    },
  });
}

function renderNav() {
  const nav = document.getElementById("nav");
  const kids = STATIC ? [] : [h("a", { href: "#/new" }, h("span", {}, "+ New benchmark run"), h("span", { class: "dim m" }, "#/new"))];
  if (TRIAL) kids.unshift(h("a", { href: "#/" }, h("span", {}, "Home")));
  if (current.job) {
    const enc = encodeURIComponent(current.job);
    kids.push(h("a", { href: `#/jobs/${enc}` }, "Matrix & Step Inspector", h("span", { class: "dim m" }, current.job)));
    kids.push(h("a", { href: `#/jobs/${enc}/compare` }, "Pass Rate & Arm Comparison", h("span", { class: "dim m" }, current.job)));
  }
  const jobs = (state.jobs || []).slice(0, 8);
  if (jobs.length) kids.push(h("div", { class: "lbl" }, "Recent runs"));
  for (const j of jobs) {
    kids.push(h("a", { href: `#/jobs/${encodeURIComponent(j.id)}` },
      h("span", {}, jobShape(j, true).name),
      h("span", { class: "dim m" }, j.id)));
  }
  nav.replaceChildren(...kids);
}

function toggleNav() {
  const nav = document.getElementById("nav");
  const open = nav.hidden;
  if (open) renderNav();
  nav.hidden = !open;
  document.getElementById("title").setAttribute("aria-expanded", String(open));
  if (open) {
    const first = nav.querySelector("a");
    if (first) first.focus();
  }
}

function closeNav() {
  const nav = document.getElementById("nav");
  if (nav) nav.hidden = true;
  const t = document.getElementById("title");
  if (t) t.setAttribute("aria-expanded", "false");
}

// The left sidebar shows by default on a wide screen, so navigation and
// Settings are always at hand.
function sideOpen() {
  return sideOpenOn(state.side, narrowScreen());
}

function hasConfiguredKey() {
  if (state.settings.data && state.settings.data.keys) {
    return state.settings.data.keys.some((k) => k.set && !k.name.startsWith("R2_"));
  }
  return Boolean(state.status && state.status.env_file);
}

function defaultKeySpecs() {
  const data = state.settings.data;
  return (data && data.keys) || [
    { name: "OPENROUTER_API_KEY", label: "OpenRouter API Key", hint: "Used by the OpenCode agent for frontier & open-weight models", set: false, masked: "" },
    { name: "ANTHROPIC_API_KEY", label: "Anthropic API Key", hint: "Direct Anthropic API access for Claude models", set: false, masked: "" },
    { name: "OPENAI_API_KEY", label: "OpenAI API Key", hint: "Direct OpenAI API access for GPT & o-series models", set: false, masked: "" },
    { name: "NVIDIA_API_KEY", label: "NVIDIA NIM API Key", hint: "Hosted Nemotron endpoints on build.nvidia.com", set: false, masked: "" },
    { name: "ZAI_API_KEY", label: "Z.ai API Key", hint: "Direct Z.ai API access for GLM models", set: false, masked: "" },
  ];
}

// sideSection is a sidebar heading that folds its section open and shut.
function sideSection(key, label, count, cls = "") {
  const shut = state.folded.has(key);
  return h("button", {
    type: "button",
    class: `lbl fold ${cls} ${shut ? "shut" : ""}`,
    "aria-expanded": String(!shut),
    title: `${shut ? "Show" : "Hide"} ${label.toLowerCase()}`,
    onclick: () => {
      toggleFold(state, key);
      saveUIPrefs();
      renderSide();
    },
  },
    h("span", {}, h("span", { class: "fold-ic", "aria-hidden": "true" }, "▾"), label),
    count == null ? null : h("span", { class: "faint" }, String(count)));
}

function renderSide() {
  const side = document.getElementById("side");
  const open = sideOpen();
  document.getElementById("main").classList.toggle("noside", !open);
  document.documentElement.classList.remove("boot-noside"); // index.html's guess until now
  const menuBtn = document.getElementById("menu");
  if (menuBtn) {
    menuBtn.setAttribute("aria-expanded", String(open));
    menuBtn.classList.toggle("open", open);
    menuBtn.title = `${open ? "Hide" : "Show"} left sidebar (⌘B)`;
  }

  const keyOk = hasConfiguredKey();
  const st = state.status;
  const foot = STATIC ? staticFoot() : TRIAL ? trialFoot() : h("div", { class: "side-foot" },
    st ? h("div", { class: "side-env-row" },
      h("span", { class: "side-env-items" },
        h("span", { title: st.docker.ok ? `Docker ${st.docker.version}` : st.docker.error },
          h("span", { class: `dot ${st.docker.ok ? "" : "bad"}` }), "docker"),
        h("span", { title: st.harbor.ok ? `Harbor ${st.harbor.version}` : st.harbor.error },
          h("span", { class: `dot ${st.harbor.ok ? "" : "bad"}` }), "harbor")),
      h("span", { class: "m" }, location.host)) : null,
    h("button", {
      type: "button",
      class: `side-settings-btn ${state.settings.open ? "active" : ""}`,
      title: "Open settings (⌘,)",
      "aria-haspopup": "dialog",
      "aria-expanded": String(state.settings.open),
      // With no key saved, land on the tab that fixes that.
      onclick: () => openSettingsModal(keyOk ? undefined : "keys"),
    },
      h("span", { class: "side-settings-left" },
        h("span", { class: "side-settings-ic", "aria-hidden": "true" }, "⚙"),
        h("span", {}, "Settings")),
      h("span", { class: `key-badge ${keyOk ? "ok" : "warn"}` }, keyOk ? "Keys ready" : "Add API key")));

  const scrollKids = [
    STATIC ? null : h("a", { href: "#/new", class: `new-run-link ${current.view === "new" ? "on" : ""}` },
      h("span", {}, "+ New benchmark run"),
      h("span", { class: "faint m" }, "/new")),
    sideSection("runs", "Runs", state.jobs ? state.jobs.length : null),
  ].filter(Boolean);

  const runsShut = state.folded.has("runs");
  if (runsShut) {
    // nothing under a folded heading
  } else if (state.jobsErr) scrollKids.push(h("div", { class: "none bad" }, "Couldn't list runs: " + state.jobsErr));
  else if (state.jobs && !state.jobs.length) scrollKids.push(h("div", { class: "none" }, "No runs yet."));

  for (const j of runsShut ? [] : state.jobs || []) {
    const { name, shape } = jobShape(j, true);
    const mark = j.status === "running"
      ? h("span", { class: "side-run", title: "running" }, spinner())
      : h("span", { class: j.status === "done" ? "dim" : "faint", title: j.status }, `${j.status === "paused" ? "paused " : ""}${j.trials_done}/${j.trials_total}`);
    scrollKids.push(h("a", {
      href: `#/jobs/${encodeURIComponent(j.id)}`,
      class: j.id === current.job ? "on" : null,
      title: `${name} (${j.id})`,
    },
      h("span", {}, shape ? `${name} · ${shape}` : name, h("span", { class: "sub" }, j.environment && j.environment !== "docker" ? `${j.id} · ${j.environment}` : j.id)),
      mark));
  }

  // A task's link opens the new-run form, which a copy doesn't have.
  if (!STATIC && !TRIAL) scrollKids.push(sideSection("tasks", "Tasks", state.tasks ? state.tasks.length : null));

  const tasksShut = STATIC || TRIAL || state.folded.has("tasks");
  if (!tasksShut && state.tasks && !state.tasks.length) scrollKids.push(h("div", { class: "none" }, "None found."));
  // Incidents first, then features; each task names the benchmark it came from.
  const groups = tasksShut ? [] : [["incident", "Incidents"], ["feature", "Features"]]
    .map(([track, label]) => [track, label, (state.tasks || []).filter((t) => trackOf(t) === track)])
    .filter(([, , ts]) => ts.length);
  for (const [track, label, ts] of groups) {
    scrollKids.push(sideSection(track, label, ts.length, "grp"));
    for (const t of state.folded.has(track) ? [] : ts) {
      const isSelectedTask = current.view === "new" && state.form.task === t.name;
      scrollKids.push(h("a", {
        href: `#/new/${encodeURIComponent(t.name)}`,
        class: isSelectedTask ? "on" : null,
        title: `${t.name} — ${t.title}`,
        onclick: (e) => {
          if (current.view === "new") {
            e.preventDefault();
            state.form.task = t.name;
            state.auxTab = "task";
            saveForm();
            history.replaceState(null, "", `#/new/${encodeURIComponent(t.name)}`);
            renderSide();
            renderView();
          }
        },
      },
        h("span", {}, t.name,
          h("span", { class: `sub task-src src-${sourceOf(t).toLowerCase().replace(/[^a-z]+/g, "-")}`, title: t.source || null }, sourceOf(t)))));
    }
  }

  const prevScroll = side.querySelector(".side-scroll");
  const top = prevScroll ? prevScroll.scrollTop : 0;
  // Status polling re-renders the sidebar; keep focus on the Settings button.
  const hadFocus = document.activeElement && document.activeElement.classList.contains("side-settings-btn");
  const scrollEl = h("div", { class: "side-scroll" }, scrollKids);
  side.replaceChildren(scrollEl, foot, makeResizeSash("left"));
  scrollEl.scrollTop = top;
  if (hadFocus) side.querySelector(".side-settings-btn").focus({ preventScroll: true });
}

// staticFoot is the sidebar's foot in a copy: what it is, in place of the
// machine's status and Settings.
function staticFoot() {
  return h("div", { class: "side-foot side-static" },
    h("span", {}, "A read-only copy of these runs, made with ",
      h("code", {}, "bench export"), "."),
    h("span", {},
      h("a", { href: "https://github.com/kavachlabs/kavach/tree/main/bench/agent" }, "The benchmark on GitHub"),
      " · ",
      h("a", { href: "https://www.thekavachlabs.com" }, "Kavach Labs")));
}

// Settings modal: a rail of tabs on the left and one panel on the right.
// Every way into settings (the sidebar button, /settings, the missing-key
// banner) opens it.
const SETTINGS_TABS = [
  { id: "general", label: "General", desc: "Defaults for new benchmark runs, and where code opens. Changes apply right away." },
  { id: "keys", label: "API keys", desc: "Provider keys the agents use to call models." },
  { id: "environment", label: "Environment", desc: "The tools and folders this console found on your machine." },
  { id: "shortcuts", label: "Shortcuts", desc: "Keyboard shortcuts, and the commands you can type in the run composer.", foot: true },
];
const MOD_KEY = /Mac|iPhone|iPad/.test(navigator.platform) ? "⌘" : "Ctrl";
let settingsOpener = null;

function openSettingsModal(tab) {
  if (!state.settings.open) settingsOpener = document.activeElement;
  // tab is ignored when this runs as an event handler.
  if (typeof tab === "string" && SETTINGS_TABS.some((t) => t.id === tab)) state.settings.tab = tab;
  state.settings.open = true;
  state.settings.msg = "";
  state.settings.err = "";
  state.settings.drafts = {};
  renderSide();
  renderSettingsModal();
  focusSettingsTab();
  loadSettings();
}

function closeSettingsModal() {
  state.settings.open = false;
  const existing = document.getElementById("settings-backdrop");
  if (existing) existing.remove();
  renderSide();
  // The sidebar re-renders its button, so fall back to the new one.
  const back = settingsOpener && settingsOpener.isConnected && settingsOpener !== document.body ? settingsOpener : document.querySelector(".side-settings-btn");
  if (back) back.focus();
  settingsOpener = null;
}

function selectSettingsTab(id) {
  if (state.settings.tab === id) return;
  state.settings.tab = id;
  state.settings.msg = "";
  renderSettingsModal();
  focusSettingsTab();
}

function focusSettingsTab() {
  const el = document.getElementById(`settings-tab-${state.settings.tab}`);
  if (el) el.focus();
}

function onSettingsTabKey(e) {
  const ids = SETTINGS_TABS.map((t) => t.id);
  let i = ids.indexOf(state.settings.tab);
  // The rail is vertical on desktop and a horizontal strip on phones.
  if (e.key === "ArrowDown" || e.key === "ArrowRight") i = (i + 1) % ids.length;
  else if (e.key === "ArrowUp" || e.key === "ArrowLeft") i = (i - 1 + ids.length) % ids.length;
  else if (e.key === "Home") i = 0;
  else if (e.key === "End") i = ids.length - 1;
  else return;
  e.preventDefault();
  selectSettingsTab(ids[i]);
}

// Keep Tab and Shift+Tab inside the dialog while it is open.
function trapSettingsFocus(e) {
  if (e.key !== "Tab") return;
  const modal = e.currentTarget.querySelector(".settings-modal");
  if (!modal) return;
  const els = [...modal.querySelectorAll("button, input, a[href], [tabindex]")]
    .filter((el) => !el.disabled && el.tabIndex >= 0 && el.offsetParent !== null);
  if (!els.length) return;
  const first = els[0];
  const last = els[els.length - 1];
  if (e.shiftKey && document.activeElement === first) {
    e.preventDefault();
    last.focus();
  } else if (!e.shiftKey && document.activeElement === last) {
    e.preventDefault();
    first.focus();
  }
}

async function saveSettingsKeys(e) {
  if (e) e.preventDefault();
  const payload = { ...state.settings.drafts };
  if (!Object.keys(payload).length) {
    state.settings.msg = "Nothing to save. Paste a key first.";
    renderSettingsModal();
    return;
  }
  state.settings.saving = true;
  state.settings.err = "";
  state.settings.msg = "";
  renderSettingsModal();
  try {
    state.settings.data = await api("/settings", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ keys: payload }),
    });
    state.settings.drafts = {};
    state.settings.reveal = {};
    state.settings.msg = `Saved to ${state.settings.data.env_file || "bench/agent/.env"}.`;
    await loadStatus();
  } catch (err) {
    state.settings.err = `Couldn't save keys: ${err.message}`;
  } finally {
    state.settings.saving = false;
    renderSettingsModal();
    renderSide();
  }
}

function settingsGroup(title, rows, note) {
  return h("section", { class: "settings-group" },
    title ? h("h3", { class: "settings-group-title" }, title) : null,
    note ? h("p", { class: "settings-group-note" }, note) : null,
    h("div", { class: "settings-rows" }, rows));
}

function settingsRow({ label, hint, control, below, labelId }) {
  return h("div", { class: "settings-row" },
    h("div", { class: "settings-row-main" },
      h("div", { class: "settings-row-text" },
        h("div", { class: "settings-row-label", id: labelId }, label),
        hint ? h("div", { class: "settings-row-hint" }, hint) : null),
      control ? h("div", { class: "settings-row-control" }, control) : null),
    below || null);
}

// A row of buttons where exactly one is pressed.
function segmented({ id, labelledBy, value, options, onChange }) {
  return h("div", { class: "seg", role: "group", "aria-labelledby": labelledBy },
    options.map((o) => {
      const on = String(o.value) === String(value);
      return h("button", {
        type: "button",
        id: `${id}-${o.value}`,
        class: on ? "on" : null,
        "aria-pressed": String(on),
        title: o.title,
        onclick: () => { if (!on) onChange(o.value); },
      }, o.label);
    }));
}

function updateRunDefaults(patch) {
  Object.assign(state.form, patch);
  saveForm();
  renderView();
  renderSettingsModal();
}

const ENVIRONMENTS = { docker: "Local Docker", daytona: "Daytona", modal: "Modal", vercel: "Vercel" };

// Why a run on a remote provider cannot start, from the server's status.
function envProblem(env) {
  const e = ((state.status && state.status.environments) || []).find((x) => x.name === env);
  return e && !e.ready ? e.problem : "";
}

function environmentHint(env) {
  if (env === "docker") return "Each attempt runs in a container on this machine.";
  const problem = envProblem(env);
  return problem
    ? `${problem[0].toUpperCase()}${problem.slice(1)}`
    : `Each attempt runs in a ${ENVIRONMENTS[env]} sandbox, with the keys from your API keys file.`;
}

function settingsGeneralTab() {
  const f = state.form;
  const timeouts = [...new Set([0, 15, 30, 60, 120, f.timeout])].sort((a, b) => a - b);
  return [
    settingsGroup("Run defaults", [
      settingsRow({
        label: "Agent",
        labelId: "set-agent-lbl",
        hint: (AGENTS[f.agent] || {}).desc,
        control: segmented({
          id: "set-agent", labelledBy: "set-agent-lbl", value: f.agent,
          options: Object.entries(AGENTS).map(([v, info]) => ({ value: v, label: info.label, title: info.desc })),
          onChange: (v) => updateRunDefaults({ agent: v }),
        }),
      }),
      settingsRow({
        label: "Where attempts run",
        labelId: "set-env-lbl",
        hint: environmentHint(f.environment),
        control: segmented({
          id: "set-env", labelledBy: "set-env-lbl", value: f.environment,
          options: Object.entries(ENVIRONMENTS).map(([v, label]) => ({ value: v, label })),
          onChange: (v) => updateRunDefaults({ environment: v }),
        }),
      }),
      settingsRow({
        label: "Attempts per task and model",
        labelId: "set-runs-lbl",
        hint: "Repeat each task this many times to measure how often a model passes.",
        control: h("input", {
          id: "set-runs", class: "settings-num", type: "number", min: "1", max: "100",
          value: String(f.runs), "aria-labelledby": "set-runs-lbl",
          onchange: (e) => updateRunDefaults({ runs: clampInt(e.target.value, 1, 100, 1) }),
        }),
      }),
      settingsRow({
        label: "Attempts at once",
        labelId: "set-conc-lbl",
        hint: "Each attempt runs in its own container. More at once finishes sooner and uses more memory.",
        control: segmented({
          id: "set-conc", labelledBy: "set-conc-lbl", value: f.concurrent,
          options: [1, 2, 4, 8].map((n) => ({ value: n, label: String(n) })),
          onChange: (v) => updateRunDefaults({ concurrent: +v }),
        }),
      }),
      settingsRow({
        label: "Retries after an error",
        labelId: "set-retries-lbl",
        hint: f.retries
          ? `An attempt that ends with an error, such as a provider or container failure, runs again up to ${f.retries} time${f.retries === 1 ? "" : "s"}. Timeouts and failed tests are results and are not retried.`
          : "An attempt that ends with an error is not run again.",
        control: segmented({
          id: "set-retries", labelledBy: "set-retries-lbl", value: f.retries,
          options: [0, 1, 2, 3].map((n) => ({ value: n, label: String(n) })),
          onChange: (v) => updateRunDefaults({ retries: +v }),
        }),
      }),
      settingsRow({
        label: "Agent timeout",
        labelId: "set-timeout-lbl",
        hint: f.timeout ? `The agent is stopped after ${f.timeout} minutes on every task.` : "Each task uses the timeout set in its task.toml.",
        control: segmented({
          id: "set-timeout", labelledBy: "set-timeout-lbl", value: f.timeout,
          options: timeouts.map((m) => ({ value: m, label: m ? `${m}m` : "Task's", title: m ? `${m} minutes` : "Use each task's own timeout" })),
          onChange: (v) => updateRunDefaults({ timeout: +v }),
        }),
      }),
    ]),
    settingsGroup("Code", [
      settingsRow({
        label: "Open code in",
        labelId: "set-editor-lbl",
        hint: "Invariant definitions link to the patch that declares them, at the line, in this editor. The patched source is only in the task's image, so the link opens the patch.",
        below: h("div", { class: "settings-editor" }, segmented({
          id: "set-editor", labelledBy: "set-editor-lbl", value: state.codeEditor,
          options: CODE_EDITORS.map((e) => ({ value: e.id, label: e.label, title: `Open code in ${e.label}` })),
          onChange: (v) => {
            setCodeEditor(v);
            const el = document.getElementById(`set-editor-${v}`);
            if (el) el.focus();
          },
        })),
      }),
    ]),
  ];
}

function settingsKeysTab() {
  const data = state.settings.data;
  const file = data ? data.env_file : "bench/agent/.env";
  const syncSave = () => {
    const btn = document.getElementById("settings-save");
    if (btn) btn.disabled = state.settings.saving || !Object.keys(state.settings.drafts).length;
  };
  const rows = defaultKeySpecs().map((k) => {
    const draft = state.settings.drafts[k.name];
    const clearing = draft === "";
    const shown = Boolean(state.settings.reveal[k.name]);
    const inputId = `set-key-${k.name}`;
    return settingsRow({
      label: h("label", { for: inputId }, k.label),
      hint: [h("code", { class: "settings-env" }, k.name), " ", k.hint],
      control: h("span", { class: "key-status" },
        h("span", { class: `key-badge ${clearing ? "warn" : k.set ? "ok" : ""}` },
          clearing ? "Clears on save" : k.set ? k.masked : "Not set"),
        k.set && !clearing ? h("button", {
          type: "button", id: `${inputId}-clear`, class: "link-btn danger",
          onclick: () => { state.settings.drafts[k.name] = ""; renderSettingsModal(); },
        }, "Remove") : null),
      below: h("div", { class: "key-input-row" },
        h("input", {
          id: inputId,
          type: shown ? "text" : "password",
          autocomplete: "off",
          spellcheck: "false",
          placeholder: clearing ? "This key is removed when you save" : k.set ? "Paste a new key to replace it" : `Paste your ${k.label.replace(/ API Key$/, "")} key`,
          value: draft || "",
          oninput: (e) => {
            const v = e.target.value;
            if (v === "") delete state.settings.drafts[k.name];
            else state.settings.drafts[k.name] = v;
            syncSave();
          },
        }),
        h("button", {
          type: "button", id: `${inputId}-show`, class: "btn",
          "aria-pressed": String(shown),
          onclick: () => { state.settings.reveal[k.name] = !shown; renderSettingsModal(); },
        }, shown ? "Hide" : "Show")),
    });
  });
  const dirty = Object.keys(state.settings.drafts).length > 0;
  return [
    settingsGroup(null, rows, ["Saved to ", h("code", {}, file), " with permissions 0600. Harbor passes them to the agent; they are never copied into task containers."]),
    h("form", { class: "settings-actions", onsubmit: saveSettingsKeys },
      h("span", { class: state.settings.err ? "bad" : "ok", role: "status" }, state.settings.err || state.settings.msg),
      h("button", { type: "submit", id: "settings-save", class: "btn peach", disabled: state.settings.saving || !dirty },
        state.settings.saving ? "Saving…" : "Save keys")),
  ];
}

function pathRow(label, path, hint) {
  return settingsRow({
    label,
    hint,
    below: h("code", { class: "settings-path" }, path || "Unknown"),
  });
}

function settingsEnvironmentTab() {
  const st = state.status;
  if (!st) {
    return [h("p", { class: state.statusErr ? "settings-empty bad" : "settings-empty" },
      state.statusErr ? `Couldn't reach the console server: ${state.statusErr}` : "Checking your machine…")];
  }
  const tool = (name, c, fix) => settingsRow({
    label: name,
    hint: c.ok ? `Version ${c.version}` : `${c.error || "Not found."} ${fix}`,
    control: h("span", { class: `key-badge ${c.ok ? "ok" : "bad"}` }, c.ok ? "Working" : "Not working"),
  });
  const data = state.settings.data;
  return [
    settingsGroup("Tools", [
      tool("Docker", st.docker, "Start Docker Desktop, then reopen this tab."),
      tool("Harbor", st.harbor, "Install it with uv tool install harbor."),
    ]),
    settingsGroup("Cloud providers", (st.environments || []).map((e) => settingsRow({
      label: ENVIRONMENTS[e.name] || e.name,
      hint: e.ready ? (e.via ? `Set via ${e.via}` : e.keys.filter((k) => k.set).map((k) => k.name).join(", ")) : `${e.problem[0].toUpperCase()}${e.problem.slice(1)}`,
      control: h("span", { class: `key-badge ${e.ready ? "ok" : "bad"}` }, e.ready ? "Ready" : "Not ready"),
    }))),
    st.sync ? settingsGroup("Run backup", [settingsRow({
      label: "Cloudflare R2",
      hint: st.sync.enabled
        ? `Finished runs are copied to the bucket ${st.sync.bucket}. Files that hold an API key are left out.`
        : `Off: ${st.sync.reason}. Add the R2 keys in API keys to back finished runs up.`,
      control: h("span", { class: `key-badge ${st.sync.enabled ? "ok" : "warn"}` }, st.sync.enabled ? "On" : "Off"),
    })]) : null,
    settingsGroup("Folders", [
      st.root ? pathRow("Repository", st.root) : null,
      pathRow("Benchmark tasks", st.tasks_dir, `${st.tasks} task${st.tasks === 1 ? "" : "s"} found.`),
      pathRow("Run results", st.jobs_dir, st.jobs_warning ? h("span", { class: "bad" }, st.jobs_warning) : null),
      pathRow("API keys file", data ? data.env_file : "bench/agent/.env",
        st.env_file ? null : "Not created yet. Saving a key in API keys creates it."),
    ]),
  ];
}

function settingsShortcutsTab() {
  const kbd = (...keys) => h("span", { class: "kbds" }, keys.map((k) => h("kbd", {}, k)));
  return [
    settingsGroup("Keyboard", [
      settingsRow({ label: "Show or hide the left sidebar", control: kbd(MOD_KEY, "B") }),
      settingsRow({ label: "Show or hide the right pane", hint: "On the new run and run pages.", control: kbd(MOD_KEY, "J") }),
      settingsRow({ label: "Close a dialog or menu", control: kbd("Esc") }),
    ]),
    settingsGroup("Composer commands", COMMANDS.map(([cmd, desc]) =>
      settingsRow({ label: h("code", { class: "settings-cmd" }, cmd), hint: desc[0].toUpperCase() + desc.slice(1) + "." }))),
  ];
}

const SETTINGS_RENDER = {
  general: settingsGeneralTab,
  keys: settingsKeysTab,
  environment: settingsEnvironmentTab,
  shortcuts: settingsShortcutsTab,
};

function renderSettingsModal() {
  let backdrop = document.getElementById("settings-backdrop");
  if (!state.settings.open) {
    if (backdrop) backdrop.remove();
    return;
  }
  if (!backdrop) {
    backdrop = h("div", {
      id: "settings-backdrop",
      class: "modal-backdrop",
      onmousedown: (e) => { if (e.target === backdrop) closeSettingsModal(); },
      onkeydown: (e) => { backdrop.dataset.kbd = "1"; trapSettingsFocus(e); },
      onpointerdown: () => { delete backdrop.dataset.kbd; },
    });
    document.body.append(backdrop);
  }

  // Re-rendering replaces the DOM: keep the panel's scroll and the focused control.
  const tab = SETTINGS_TABS.find((t) => t.id === state.settings.tab);
  const prevPane = backdrop.querySelector(".settings-pane");
  const scroll = prevPane && backdrop.dataset.tab === tab.id ? prevPane.scrollTop : 0;
  const active = document.activeElement;
  const focusId = active && backdrop.contains(active) ? active.id : "";

  const tabBtn = (t) => {
    const on = t.id === tab.id;
    return h("button", {
      type: "button",
      role: "tab",
      id: `settings-tab-${t.id}`,
      class: `settings-tab ${on ? "on" : ""}`,
      "aria-selected": String(on),
      "aria-controls": "settings-panel",
      tabindex: on ? "0" : "-1",
      onclick: () => selectSettingsTab(t.id),
    },
      h("span", {}, t.label),
      t.id === "keys" && !hasConfiguredKey() ? h("span", { class: "settings-tab-dot", title: "No API key saved" }) : null);
  };

  const rail = h("nav", { class: "settings-rail", "aria-label": "Settings" },
    h("h2", { class: "settings-rail-title", id: "settings-title" }, "Settings"),
    h("div", { class: "settings-tabs", role: "tablist", "aria-orientation": window.matchMedia("(max-width: 680px)").matches ? "horizontal" : "vertical", onkeydown: onSettingsTabKey },
      SETTINGS_TABS.filter((t) => !t.foot).map(tabBtn),
      h("div", { class: "settings-tabs-gap", role: "presentation" }),
      SETTINGS_TABS.filter((t) => t.foot).map(tabBtn)));

  const pane = h("section", {
    class: "settings-pane",
    id: "settings-panel",
    role: "tabpanel",
    "aria-labelledby": `settings-tab-${tab.id}`,
  },
    h("header", { class: "settings-pane-head" },
      h("h2", {}, tab.label),
      h("p", {}, tab.desc)),
    SETTINGS_RENDER[tab.id]());

  const modal = h("div", { class: "settings-modal", role: "dialog", "aria-modal": "true", "aria-labelledby": "settings-title" },
    rail,
    pane,
    h("button", { type: "button", id: "settings-close", class: "settings-close", "aria-label": "Close settings", title: "Close (Esc)", onclick: closeSettingsModal }, "✕"));

  backdrop.dataset.tab = tab.id;
  backdrop.replaceChildren(modal);
  pane.scrollTop = scroll;
  if (focusId) {
    const el = document.getElementById(focusId);
    if (el) el.focus({ preventScroll: true });
  }
}

// Custom Select Component (.cselect) to replace broken native <select> menus
function renderSelect({ id, label, value, options, onChange, compact = false, up = false, searchable = false }) {
  const isOpen = state.openSelect === id;
  const currentOpt = options.find((o) => String(o.value) === String(value)) || options[0] || { label: String(value), value };

  const toggle = (e) => {
    e.stopPropagation();
    state.openSelect = isOpen ? "" : id;
    state.selectFilter = "";
    renderView();
    if (state.openSelect === id) {
      const searchEl = document.getElementById(`cselect-q-${id}`);
      if (searchEl) searchEl.focus();
      else {
        const activeBtn = document.querySelector(`#cselect-${id} .cselect-opt.sel`);
        if (activeBtn) activeBtn.focus();
      }
    }
  };

  let menu = null;
  if (isOpen) {
    const q = state.selectFilter.trim().toLowerCase();
    const filtered = q
      ? options.filter((o) =>
          String(o.label).toLowerCase().includes(q) ||
          (o.desc && o.desc.toLowerCase().includes(q)) ||
          (o.badge && o.badge.toLowerCase().includes(q)))
      : options;

    const listItems = filtered.length
      ? filtered.map((o) => {
          const isSel = String(o.value) === String(value);
          return h("button", {
            type: "button",
            role: "option",
            "aria-selected": String(isSel),
            class: `cselect-opt ${isSel ? "sel" : ""}`,
            onclick: (e) => {
              e.stopPropagation();
              state.openSelect = "";
              state.selectFilter = "";
              onChange(o.value);
            },
          },
            h("span", { class: "cselect-check", "aria-hidden": "true" }, isSel ? "✓" : ""),
            h("span", { class: "cselect-main" },
              h("span", { class: "cselect-lbl" }, o.label),
              o.desc ? h("span", { class: "cselect-desc" }, o.desc) : null),
            o.badge ? h("span", { class: "cselect-badge" }, o.badge) : null);
        })
      : [h("div", { class: "none" }, `No match for “${state.selectFilter}”`)];

    menu = h("div", {
      class: "cselect-menu",
      role: "listbox",
      "aria-label": label,
      onclick: (e) => e.stopPropagation(),
    },
      searchable && options.length > 5 ? h("input", {
        id: `cselect-q-${id}`,
        class: "cselect-search",
        type: "search",
        placeholder: `Filter ${label.toLowerCase()}…`,
        value: state.selectFilter,
        oninput: (e) => {
          state.selectFilter = e.target.value;
          renderView();
          const input = document.getElementById(`cselect-q-${id}`);
          if (input) {
            input.focus();
            input.setSelectionRange(input.value.length, input.value.length);
          }
        },
      }) : null,
      listItems);
  }

  return h("div", {
    id: `cselect-${id}`,
    class: `cselect ${isOpen ? "open" : ""} ${compact ? "compact" : ""} ${up ? "up" : ""}`,
  },
    h("button", {
      type: "button",
      class: "cselect-btn",
      "aria-label": label,
      "aria-haspopup": "listbox",
      "aria-expanded": String(isOpen),
      onclick: toggle,
    },
      h("span", { class: "cselect-text" }, currentOpt.label),
      currentOpt.buttonSub ? h("span", { class: "cselect-sub" }, currentOpt.buttonSub) : null,
      h("span", { class: "cselect-arrow", "aria-hidden": "true" }, "▾")),
    menu);
}

function renderAll() {
  renderTop();
  renderSide();
  renderView();
}

export function renderView() {
  const view = document.getElementById("view");
  if (TRIAL && TRIAL_VIEWS.includes(current.view)) return renderTrialView(view, current.view);
  if (current.view === "new") return TRIAL ? renderTrialNew(view) : renderNew(view);
  if (state.jobErr) {
    const msg = state.jobErr === "notfound"
      ? STATIC
        ? [h("b", {}, `No run named ${current.job}.`), " This copy doesn't include it."]
        : [h("b", {}, `No run named ${current.job}.`), " It isn't under ", h("code", {}, state.status ? state.status.jobs_dir : "the jobs directory"), ". ", h("a", { href: "#/new" }, "Start a new run"), "."]
      : [h("b", {}, "Couldn't load this run: "), state.jobErr];
    return view.replaceChildren(h("div", { class: "empty" }, msg));
  }
  if (!state.job) return view.replaceChildren(h("div", { class: "empty" }, "Loading…"));
  if (current.view === "runs") return renderRuns(view);
  renderCompare(view);
}

function shortSource(src) {
  if (!src) return "";
  if (src.startsWith("DeepSWE")) return "DeepSWE";
  if (src.startsWith("SWE-bench Multilingual")) return "SWE-bench";
  return src.split(/\s+/)[0] || src;
}

// sourceOf names the benchmark a task was taken from, from task.toml's
// metadata.source ("DeepSWE deep-swe-1-1 ...", "SWE-bench Multilingual ...").
function sourceOf(task) {
  const src = task.source || "";
  if (/deep-?swe/i.test(src)) return "DeepSWE";
  if (/swe-bench multilingual/i.test(src)) return "SWE-bench Multilingual";
  if (/swe-bench/i.test(src)) return "SWE-bench";
  return src.split(/\s+/)[0] || "local";
}

// trackOf says whether a task is an incident to fix or a feature to build.
function trackOf(task) {
  return task.track === "feature" || task.arms.some((a) => a.arm === "workflow" || a.arm === "kavach-implement") ? "feature" : "incident";
}

// Task Card component rendered in the Right Auxiliary Pane ("Task" tab)
function renderTaskCard(task) {
  if (!task) return null;
  const isFeature = trackOf(task) === "feature";
  const trackBadge = isFeature ? "Feature" : "Incident";
  const checkCmd = isFeature ? "kavach contract check" : "kavach diff";
  const timeoutMin = task.timeout_sec ? `${Math.round(task.timeout_sec / 60)}m` : null;
  const srcShort = shortSource(task.source);

  return h("div", { class: "aux-task-pane", "aria-label": `Task details for ${task.name}` },
    h("div", { class: "task-card" },
      h("div", { class: "task-card-head" },
        h("span", { class: `track-badge ${isFeature ? "feature" : ""}` }, trackBadge),
        h("div", { class: "task-card-badges" },
          srcShort ? h("span", { class: "task-meta-pill", title: task.source }, srcShort) : null,
          timeoutMin ? h("span", { class: "task-meta-pill", title: "Default agent timeout" }, timeoutMin) : null)),
      h("div", { class: "task-card-title-block" },
        h("h3", { class: "task-card-title" }, task.title.replace(/^Task:\s*/, "")),
        h("div", { class: "task-card-slug" }, `${task.name} · ${checkCmd}`)),
      task.summary
        ? h("div", { class: "task-card-desc md" }, renderMarkdown(task.summary))
        : null),
    task.instruction
      ? h("div", { class: "aux-spec-section" },
          h("div", { class: "aux-section-hdr" },
            h("span", { class: "rside-title" }, "Task Specification"),
            h("span", { class: "rside-sub m" }, "INSTRUCTION.md")),
          h("div", { class: "aux-spec-card md" }, renderMarkdown(task.instruction)))
      : null);
}

// Interactive vertical list of experiment arms in the Right Auxiliary Pane ("Arms" tab)
function renderRightArms(task, selectedArms) {
  if (!task || !task.arms || !task.arms.length) return null;
  const activeCount = task.arms.filter((a) => selectedArms.includes(a.arm)).length;
  return h("div", { class: "aux-arms-pane" },
    h("div", { class: "aux-pane-intro" },
      h("span", {}, `${activeCount} of ${task.arms.length} experiment arms enabled for `, h("code", {}, task.name), "."),
      h("span", { class: "faint" }, "Click any arm card below to toggle it in the run matrix.")),
    h("div", { class: "rside-arm-list" },
      task.arms.map((a) => {
        const info = armInfo(a.arm, trackOf(task));
        const active = selectedArms.includes(a.arm);
        const toneCls = info.tone === "blue" ? "blue" : "peach";
        return h("button", {
          type: "button",
          class: `rside-arm-row ${active ? "on" : "off"} ${toneCls}`,
          "aria-pressed": String(active),
          title: info.detail,
          onclick: () => toggleArm(a.arm),
        },
          h("div", { class: "rside-arm-top" },
            h("span", { class: "rside-arm-left" },
              h("span", { class: `rside-arm-dot ${toneCls}` }),
              h("span", { class: `rside-arm-name ${toneCls}` }, a.arm || "default")),
            h("span", { class: "rside-arm-tag" }, info.tag)),
          h("div", { class: "rside-arm-desc" }, info.short),
          h("div", { class: "rside-arm-detail" }, info.detail));
      })));
}

// Model Card component for selected OpenRouter models and live picker preview
function renderModelCard(modelId, { onRemove = null, isPreview = false } = {}) {
  const cleanId = orId(modelId);
  const meta = state.modelCatalog.get(cleanId) || (state.picker.list || []).find((m) => m.id === cleanId);
  const parts = cleanId.split("/");
  const provider = parts.length > 1 ? parts[0] : "openrouter";
  const displayName = meta && meta.name ? meta.name : cleanId;
  const ctxText = meta && meta.context_length ? fmtTok(meta.context_length) : "–";
  const inPrice = meta ? fmtPricePerM(meta.price_in) : "–";
  const outPrice = meta ? fmtPricePerM(meta.price_out) : "–";
  const toolsOk = meta ? meta.tools : null;

  return h("div", { class: `model-card ${isPreview ? "preview" : ""}` },
    h("div", { class: "model-card-top" },
      h("div", { class: "model-card-idwrap" },
        h("div", { class: "model-provider-row" },
          isPreview ? h("span", { class: "model-preview-pill" }, "Preview") : null,
          h("span", { class: "model-provider" }, LABS[labOf(cleanId)] ? labMark(cleanId) : null, provider),
          toolsOk != null
            ? h("span", {
                class: `model-tool-badge ${toolsOk ? "ok" : "bad"}`,
                title: toolsOk ? "Supports tool calls required by the coding agent" : "Does not advertise tool call support on OpenRouter",
              }, toolsOk ? "Tools ✓" : "No tools ✕")
            : null,
          meta && meta.modality ? h("span", { class: "faint m", style: "font-size:10.5px" }, meta.modality) : null),
        h("div", { class: "model-card-name", title: displayName }, displayName),
        h("div", { class: "model-card-slug", title: `openrouter/${cleanId}` }, cleanId)),
      onRemove ? h("button", {
        type: "button",
        class: "model-card-rm",
        "aria-label": `Remove ${cleanId}`,
        title: `Remove ${cleanId}`,
        onclick: onRemove,
      }, "×") : null),
    h("div", { class: "model-card-specs" },
      h("div", { class: "model-spec" },
        h("span", { class: "model-spec-lbl" }, "Context"),
        h("span", { class: "model-spec-val" }, ctxText)),
      h("div", { class: "model-spec" },
        h("span", { class: "model-spec-lbl" }, "Input / 1M"),
        h("span", { class: "model-spec-val" }, inPrice)),
      h("div", { class: "model-spec" },
        h("span", { class: "model-spec-lbl" }, "Output / 1M"),
        h("span", { class: "model-spec-val" }, outPrice))),
    meta && meta.description ? h("div", { class: "model-card-desc", title: meta.description }, meta.description) : null);
}

function renderRightSidebarModels() {
  const f = state.form;
  if (!needsModel(f.agent)) {
    return h("div", { class: "rside-empty" }, `Agent “${f.agent}” runs deterministically without an LLM.`);
  }
  const cards = [];
  const previewId = state.picker.open && state.picker.previewId ? orId(state.picker.previewId) : "";
  if (previewId && !f.models.map(orId).includes(previewId)) {
    cards.push(renderModelCard(previewId, { isPreview: true }));
  }
  for (const m of f.models) {
    cards.push(renderModelCard(m, {
      isPreview: state.picker.open && orId(m) === previewId,
      onRemove: () => {
        f.models = f.models.filter((x) => x !== m);
        saveForm();
        renderView();
      },
    }));
  }
  if (!cards.length) {
    return h("div", { class: "rside-empty" },
      h("p", { style: "margin:0 0 10px" }, "No OpenRouter models selected yet."),
      h("button", {
        type: "button",
        class: "btn peach",
        onclick: () => openPicker(""),
      }, "+ Browse OpenRouter Models"));
  }
  return h("div", { class: "rside-model-stack" }, cards);
}

function renderRightSidebar(task) {
  const f = state.form;
  const activeCount = task ? task.arms.filter((a) => f.arms.includes(a.arm)).length : 0;
  const totalArms = task ? task.arms.length : 0;
  const tab = state.auxTab || "task";

  const tabs = [
    { id: "task", label: "Task", badge: task ? (trackOf(task) === "feature" ? "Feature" : "Incident") : null },
    { id: "arms", label: "Arms", badge: totalArms ? `${activeCount}/${totalArms}` : null },
    { id: "models", label: "Models", badge: needsModel(f.agent) ? String(f.models.length) : "–" },
  ];

  let bodyContent = null;
  if (tab === "task") {
    bodyContent = task ? renderTaskCard(task) : h("div", { class: "rside-empty" }, "Select a task to view its specification.");
  } else if (tab === "arms") {
    bodyContent = task ? renderRightArms(task, f.arms) : h("div", { class: "rside-empty" }, "Select a task to configure its experiment arms.");
  } else {
    bodyContent = h("div", { id: "rside-models" }, renderRightSidebarModels());
  }

  return h("aside", {
    class: "right-side",
    "aria-label": "Auxiliary inspector pane",
    "aria-hidden": String(!state.rightSide),
  },
    makeResizeSash("right"),
    h("div", { class: "aux-bar" },
      h("div", { class: "aux-tabs", role: "tablist", "aria-label": "Inspector tabs" },
        tabs.map((t) => h("button", {
          type: "button",
          role: "tab",
          class: `aux-tab ${tab === t.id ? "on" : ""}`,
          "aria-selected": String(tab === t.id),
          title: tab === t.id && state.rightSide ? `Click to collapse pane (⌘J)` : `Show ${t.label} tab`,
          onclick: () => switchAuxTab(t.id),
        },
          h("span", {}, t.label),
          t.badge != null ? h("span", { class: "aux-tab-badge" }, t.badge) : null))),
      h("button", {
        type: "button",
        class: "aux-close",
        "aria-label": "Hide auxiliary pane",
        title: "Hide auxiliary pane (⌘J)",
        onclick: toggleRightSide,
      }, "⇥")),
    h("div", { class: "aux-body", role: "tabpanel" }, bodyContent));
}

// Line icons for the new-run form's rows, drawn like the top bar's.
const ROW_ICONS = {
  task: '<path d="M4 1.75h5.25l3 3v9.5H4z"/><path d="M9.25 1.75v3h3"/><path d="M6.25 8.25h3.5M6.25 10.75h2.5"/>',
  arms: '<circle cx="4.25" cy="3.5" r="1.6"/><circle cx="11.75" cy="3.5" r="1.6"/><circle cx="8" cy="12.5" r="1.6"/><path d="M4.25 5.1v.9c0 2 3.75 2.4 3.75 4.9M11.75 5.1v.9c0 2-3.75 2.4-3.75 4.9"/>',
  models: '<rect x="4.25" y="4.25" width="7.5" height="7.5" rx="1.5"/><path d="M6.5 1.75v2.5M9.5 1.75v2.5M6.5 11.75v2.5M9.5 11.75v2.5M1.75 6.5h2.5M1.75 9.5h2.5M11.75 6.5h2.5M11.75 9.5h2.5"/>',
  runs: '<path d="M2.75 7.5a5.25 5.25 0 0 1 9.4-3.2"/><path d="M12.5 1.75v2.9H9.6"/><path d="M13.25 8.5a5.25 5.25 0 0 1-9.4 3.2"/><path d="M3.5 14.25v-2.9h2.9"/>',
};

function rowIcon(name) {
  const ic = h("span", { class: "ic", "aria-hidden": "true" });
  ic.innerHTML = `<svg width="16" height="16" viewBox="0 0 16 16" fill="none" stroke="currentColor" stroke-width="1.35" stroke-linecap="round" stroke-linejoin="round">${ROW_ICONS[name]}</svg>`;
  return ic;
}

function renderNew(view) {
  const f = state.form;
  const st = state.status;
  if (needsModel(f.agent) && f.models.length && state.modelCatalog.size === 0) {
    ensureModelsLoaded();
  }

  const kids = [
    h("div", { class: "hero-head" },
      h("h1", { class: "logo", "aria-label": "kavach bench" },
        h("span", { class: "logo-mark", "aria-hidden": "true" }),
        h("span", { class: "logo-sub", "aria-hidden": "true" }, "bench")),
      h("p", { class: "hero-sub" }, "Coding agents, with and without Kavach.")),
  ];

  if (state.statusErr && !st) {
    kids.push(banner("Can't reach the bench server.", `${state.statusErr}\nStart it with: bench web`));
  }
  if (st && !st.docker.ok) kids.push(banner(["Docker isn't running: start Colima with ", h("code", {}, "colima start"), "."], st.docker.error));
  if (st && st.jobs_warning) kids.push(banner(st.jobs_warning));
  if (st && !st.harbor.ok) kids.push(banner(["Harbor didn't answer ", h("code", {}, "harbor --version"), ". Is it installed and on PATH?"], st.harbor.error));
  if (st && !hasConfiguredKey() && needsModel(f.agent)) {
    kids.push(h("div", { class: "banner warn", role: "alert" },
      h("span", {}, "No API key configured in ", h("code", {}, "bench/agent/.env"), ": OpenRouter runs need ", h("code", {}, "OPENROUTER_API_KEY"), "."),
      h("button", { type: "button", class: "btn peach", onclick: openSettingsModal }, "⚙ Configure API Keys")));
  }

  const tasks = state.tasks || [];
  if (state.tasks && !f.task && tasks.length) f.task = tasks[0].name;
  const task = tasks.find((t) => t.name === f.task);
  let taskRow;
  if (state.tasksErr) taskRow = h("span", { class: "bad" }, "Couldn't list tasks: " + state.tasksErr);
  else if (!state.tasks) taskRow = h("span", {}, "Loading…");
  else if (!tasks.length) taskRow = h("span", {}, "No tasks found under ", h("code", {}, st ? st.tasks_dir : "bench/agent/harbor/tasks"), ".");
  else {
    const taskOpts = tasks.map((t) => ({
      value: t.name,
      label: t.name,
      desc: t.title.replace(/^Task:\s*/, ""),
      badge: `${t.arms.length} arms · ${trackOf(t)}`,
    }));
    taskRow = [
      renderSelect({
        id: "task",
        label: "Task",
        value: f.task,
        options: taskOpts,
        searchable: true,
        onChange: (val) => {
          f.task = val;
          state.auxTab = "task";
          saveForm();
          renderSide();
          renderView();
        },
      }),
      task
        ? h("span", { class: "faint m" }, `${trackOf(task) === "feature" ? "Feature" : "Incident"} · ${task.arms.length} arms`)
        : h("span", { class: "bad" }, `${f.task} isn't a task any more`),
    ];
  }

  const taskArms = task ? task.arms.map((a) => a.arm).filter(Boolean) : ["control", "kavach"];
  const chipOrder = ["kavach", "kavach-implement", "workflow", "control"];
  const chipArms = [...chipOrder.filter((a) => taskArms.includes(a)), ...taskArms.filter((a) => !chipOrder.includes(a))];
  const armChips = chipArms.map((a) => {
    const on = f.arms.includes(a);
    const info = armInfo(a);
    const tone = info.tone === "blue" ? "blue" : "";
    return h("button", {
      type: "button",
      class: `chip ${on ? "on" : ""} ${tone}`,
      "aria-pressed": String(on),
      title: `${a} (${info.tag}): ${info.short}`,
      onclick: () => toggleArm(a),
    },
      h("span", { class: "arm-chip-dot", "aria-hidden": "true" }),
      a,
      h("span", { class: "faint", style: "font-size:11px;font-weight:400" }, info.tag));
  });

  let modelVal;
  if (!needsModel(f.agent)) {
    modelVal = h("span", { class: "faint" }, `The ${f.agent} agent needs no model.`);
  } else {
    modelVal = h("div", { class: "chips" },
      f.models.map((m) => h("span", { class: "chip on", title: m },
        labMark(m),
        h("span", { class: "m" }, m),
        h("button", {
          type: "button",
          class: "x",
          "aria-label": `Remove ${m}`,
          onclick: () => {
            f.models = f.models.filter((x) => x !== m);
            saveForm();
            renderView();
          },
        }, "×"))),
      h("button", {
        type: "button",
        class: "chip dim",
        "aria-expanded": String(state.picker.open),
        onclick: () => openPicker(state.picker.open ? null : ""),
      }, state.picker.open ? "Close model picker" : "+ Add OpenRouter model"));
  }

  const n = attemptsCount();
  const conc = f.concurrent > 1 ? `${f.concurrent} at once` : "one at a time";
  kids.push(h("div", { class: "config-card" },
    h("div", { class: "meta" },
      h("div", {}, rowIcon("task"), h("span", { class: "row-lbl" }, "Task"), h("span", { class: "val" }, taskRow)),
      h("div", {}, rowIcon("arms"), h("span", { class: "row-lbl" }, "Arms"), h("span", { class: "val chips" }, armChips)),
      h("div", {}, rowIcon("models"), h("span", { class: "row-lbl" }, "Models"), h("div", { class: "val" }, modelVal)),
      h("div", {}, rowIcon("runs"), h("span", { class: "row-lbl" }, "Runs"), h("span", { class: "val" },
        h("input", {
          class: "num v", type: "number", min: "1", max: "100", value: String(f.runs), "aria-label": "Runs per task and model",
          onchange: (e) => { f.runs = clampInt(e.target.value, 1, 100, 1); saveForm(); renderView(); },
        }),
        h("span", { class: "v" }, "each"),
        h("span", { class: "faint" }, `${n} attempt${n === 1 ? "" : "s"} · ${conc}${f.retries ? ` · ${f.retries} retr${f.retries === 1 ? "y" : "ies"} on error` : ""}${f.timeout ? ` · ${f.timeout} min timeout` : ""}${f.environment !== "docker" ? ` · on ${ENVIRONMENTS[f.environment]}` : ""}`))))));

  if (state.picker.open && needsModel(f.agent)) kids.push(renderPicker());

  kids.push(renderPrompt());
  if (state.note) kids.push(h("div", { class: `note ${state.noteErr ? "err" : ""}`, role: "status" }, state.note));

  const prevAuxBody = view.querySelector(".aux-body");
  const prevAuxScroll = prevAuxBody ? prevAuxBody.scrollTop : 0;
  const rightPane = renderRightSidebar(task);

  // Keep rightPane in the DOM when closed so CSS grid-template-columns transitions smoothly!
  const layout = h("div", { class: `new-layout ${state.rightSide ? "" : "noright"}` },
    h("div", { class: "new-main" }, h("div", { class: "center" }, kids)),
    rightPane);

  const focused = document.activeElement && document.activeElement.id;
  const caret = focused === "ask" ? document.activeElement.selectionStart : null;
  view.replaceChildren(layout);
  slideRightSide(layout);
  const newAuxBody = view.querySelector(".aux-body");
  if (newAuxBody) newAuxBody.scrollTop = prevAuxScroll;
  if (focused) {
    const el = document.getElementById(focused);
    if (el) {
      el.focus();
      if (caret != null && el.setSelectionRange) el.setSelectionRange(caret, caret);
    }
  }
}

function banner(msg, why, warn) {
  return h("div", { class: `banner ${warn ? "warn" : ""}`, role: "alert" }, h("span", {}, msg), why ? h("span", { class: "why" }, why) : null);
}

const clampInt = (v, lo, hi, dflt) => {
  const n = parseInt(v, 10);
  return Number.isFinite(n) ? Math.min(hi, Math.max(lo, n)) : dflt;
};

function attemptsCount() {
  const f = state.form;
  const models = needsModel(f.agent) ? f.models.length : 1;
  return chosenTasks().length * models * f.runs;
}

function chosenTasks() {
  const task = (state.tasks || []).find((t) => t.name === state.form.task);
  return task ? task.arms.filter((a) => state.form.arms.includes(a.arm)).map((a) => a.task) : [];
}

function toggleArm(a) {
  const f = state.form;
  f.arms = f.arms.includes(a) ? f.arms.filter((x) => x !== a) : [...f.arms, a];
  saveForm();
  renderView();
}

function renderPrompt() {
  const f = state.form;
  const ready = canRun() === "";
  const input = h("input", {
    id: "ask", class: "ask", type: "text", autocomplete: "off", spellcheck: "false",
    placeholder: 'Type / for commands… e.g. "/models glm", "/arm both", "/settings"', "aria-label": "Command",
    oninput: () => renderCmds(), onkeydown: onAskKey,
  });

  const agentSel = renderSelect({
    id: "agent",
    label: "Agent",
    value: f.agent,
    compact: true,
    up: true,
    options: Object.entries(AGENTS).map(([v, info]) => ({
      value: v,
      label: info.label,
      desc: info.desc,
    })),
    onChange: (val) => {
      f.agent = val;
      saveForm();
      renderView();
    },
  });

  const concSel = renderSelect({
    id: "conc",
    label: "Attempts at once",
    value: f.concurrent,
    compact: true,
    up: true,
    options: [
      { value: 1, label: "one at a time", desc: "Sequential trial execution (1 container)" },
      { value: 2, label: "2 at once", desc: "Run 2 trials concurrently" },
      { value: 4, label: "4 at once", desc: "Run 4 trials concurrently" },
      { value: 8, label: "8 at once", desc: "Run 8 trials concurrently" },
    ],
    onChange: (val) => {
      f.concurrent = +val;
      saveForm();
      renderView();
    },
  });

  const timeouts = [...new Set([0, 15, 30, 60, 120, f.timeout])].sort((a, b) => a - b);
  const timeSel = renderSelect({
    id: "timeout",
    label: "Agent timeout",
    value: f.timeout,
    compact: true,
    up: true,
    options: timeouts.map((m) => ({
      value: m,
      label: m ? `${m} min timeout` : "task's timeout",
      desc: m ? `Override agent timeout to ${m} minutes` : "Use each task's configured timeout_sec",
    })),
    onChange: (val) => {
      f.timeout = +val;
      saveForm();
      renderView();
    },
  });

  const bar = h("div", { class: "bar" },
    agentSel,
    needsModel(f.agent)
      ? h("button", {
          type: "button",
          class: "sel",
          onclick: () => openPicker(state.picker.open ? null : ""),
        }, `${f.models.length} model${f.models.length === 1 ? "" : "s"}`, h("span", { class: "faint", style: "font-size:10px" }, "▾"))
      : null,
    concSel,
    timeSel,
    needsModel(f.agent) ? h("span", { class: "faint m" }, "OpenRouter") : null,
    h("button", {
      type: "button",
      class: `go ${ready ? "on" : ""}`,
      "aria-label": "Start the run",
      title: ready ? "Start the run" : canRun(),
      disabled: state.starting,
      onclick: startRun,
    }, state.starting ? "Starting…" : ["Run ", h("span", { "aria-hidden": "true" }, "▶")]));

  return h("div", { class: "prompt" }, h("div", { class: "cmds", id: "cmds", hidden: true, role: "listbox" }), input, bar);
}

let cmdIndex = 0;
function matchingCmds() {
  const askEl = document.getElementById("ask");
  const v = askEl ? askEl.value : "";
  if (!v.startsWith("/") || v.includes(" ")) return [];
  return COMMANDS.filter(([c]) => c.startsWith(v));
}

function renderCmds() {
  const box = document.getElementById("cmds");
  if (!box) return;
  const cmds = matchingCmds();
  cmdIndex = Math.min(cmdIndex, Math.max(cmds.length - 1, 0));
  box.hidden = !cmds.length;
  box.replaceChildren(...cmds.map(([c, what], i) => h("button", {
    type: "button", class: i === cmdIndex ? "on" : "", role: "option", "aria-selected": String(i === cmdIndex), tabindex: "-1",
    onmousedown: (e) => { e.preventDefault(); completeCmd(c); },
  }, h("span", { class: "m" }, c), h("span", {}, what))));
}

function completeCmd(c) {
  const ask = document.getElementById("ask");
  const word = c.split(" ")[0];
  ask.value = c.includes(" ") ? word + " " : word;
  ask.focus();
  renderCmds();
}

function onAskKey(e) {
  const cmds = matchingCmds();
  if (cmds.length && (e.key === "ArrowDown" || e.key === "ArrowUp")) {
    e.preventDefault();
    cmdIndex = (cmdIndex + (e.key === "ArrowDown" ? 1 : cmds.length - 1)) % cmds.length;
    renderCmds();
  } else if (cmds.length && e.key === "Tab") {
    e.preventDefault();
    completeCmd(cmds[cmdIndex][0]);
  } else if (e.key === "Escape") {
    e.target.value = "";
    renderCmds();
  } else if (e.key === "Enter") {
    e.preventDefault();
    const v = e.target.value.trim();
    const exact = cmds.find(([c]) => c.split(" ")[0] === v);
    if (cmds.length && !exact) return completeCmd(cmds[cmdIndex][0]);
    e.target.value = "";
    renderCmds();
    runCommand(v);
  }
}

function say(msg, err) {
  state.note = msg;
  state.noteErr = !!err;
  renderView();
}

function latestJob() {
  return (state.jobs || [])[0];
}

function runCommand(line) {
  const f = state.form;
  if (!line) return startRun();
  if (!line.startsWith("/")) {
    return say("This box takes commands, not prompts: the task's instruction is the agent's prompt. Type / to see the commands.", true);
  }
  const [cmd, ...rest] = line.split(/\s+/);
  const arg = rest.join(" ");
  switch (cmd) {
    case "/run":
      return startRun();
    case "/settings":
      return openSettingsModal();
    case "/model": {
      if (!arg) return say("Usage: /model <OpenRouter id>, e.g. /model z-ai/glm-5.3", true);
      addModel(orId(arg));
      return say(`Added ${orId(arg)}.`);
    }
    case "/models":
      return openPicker(arg);
    case "/arm": {
      const task = (state.tasks || []).find((t) => t.name === f.task);
      const avail = task ? task.arms.map((a) => a.arm).filter(Boolean) : ["kavach", "control"];
      const arms = (arg === "both" || arg === "all") ? avail : ARMS.includes(arg) ? [arg] : null;
      if (!arms || !arms.length) return say("Usage: /arm kavach, /arm control, /arm workflow, /arm kavach-implement or /arm both", true);
      f.arms = arms;
      saveForm();
      return say(`Arms: ${arms.join(arms.length === 2 ? " and " : ", ")}.`);
    }
    case "/runs": {
      if (!arg) {
        state.side = true;
        saveUIPrefs();
        renderSide();
        return say("Runs are in the sidebar. /runs <n> sets the attempts per task and model.");
      }
      f.runs = clampInt(arg, 1, 100, f.runs);
      saveForm();
      return say(`${f.runs} attempt${f.runs === 1 ? "" : "s"} per task and model.`);
    }
    case "/timeout": {
      if (arg === "default" || arg === "0") f.timeout = 0;
      else {
        const m = arg.match(/^(\d+(?:\.\d+)?)\s*(s|m|min|h)?$/);
        if (!m) return say("Usage: /timeout <minutes> (or 90s, 2h), or /timeout default", true);
        const unit = { s: 1 / 60, h: 60 }[m[2]] || 1;
        f.timeout = Math.max(1, Math.round(+m[1] * unit));
      }
      saveForm();
      return say(f.timeout ? `Agent timeout: ${f.timeout} minutes.` : "Agent timeout: each task's own.");
    }
    case "/agent": {
      if (!AGENTS[arg]) return say("Usage: /agent opencode, /agent oracle or /agent nop", true);
      f.agent = arg;
      saveForm();
      return say(`Agent: ${AGENTS[arg].label}.`);
    }
    case "/task": {
      const t = (state.tasks || []).find((x) => x.name === arg || x.arms.some((a) => a.task === arg));
      if (!t) return say(`No task ${arg || "given"}. Tasks: ${(state.tasks || []).map((x) => x.name).join(", ") || "none"}.`, true);
      f.task = t.name;
      const arm = t.arms.find((a) => a.task === arg);
      if (arm) f.arms = [arm.arm];
      saveForm();
      return say(`Task: ${t.name}.`);
    }
    case "/matrix":
    case "/compare": {
      const j = latestJob();
      if (!j) return say("No runs yet.", true);
      location.hash = `#/jobs/${encodeURIComponent(j.id)}${cmd === "/compare" ? "/compare" : ""}`;
      return;
    }
  }
  say(`Unknown command ${cmd}. Type / to see the commands.`, true);
}

function addModel(id) {
  const f = state.form;
  if (!f.models.includes(id)) f.models.push(id);
  if (!needsModel(f.agent)) f.agent = "opencode";
  state.picker.previewId = id;
  state.auxTab = "models";
  state.rightSide = true;
  saveUIPrefs();
  saveForm();
  ensureModelsLoaded();
  renderTop();
  renderView();
}

let pickerTimer = 0;
function openPicker(q) {
  const p = state.picker;
  if (q == null) {
    p.open = false;
    return renderView();
  }
  if (!needsModel(state.form.agent)) {
    state.form.agent = "opencode";
    saveForm();
  }
  p.open = true;
  p.q = q;
  state.auxTab = "models";
  state.rightSide = true;
  saveUIPrefs();
  renderTop();
  renderView();
  const input = document.getElementById("pick-q");
  if (input) input.focus();
  searchModels();
}

async function searchModels() {
  const p = state.picker;
  const q = p.q;
  try {
    const list = await api(`/models?q=${encodeURIComponent(q)}`);
    for (const m of list || []) {
      state.modelCatalog.set(m.id, m);
    }
    if (p.q !== q) return;
    p.list = list;
    p.err = "";
    if (list && list.length && (!p.previewId || !list.some((m) => m.id === p.previewId))) {
      p.previewId = sortedModels(list)[0].id;
    }
  } catch (e) {
    p.err = e.message;
  }
  if (p.open) renderPickerList();
}

// The model picker's orders: a column and a direction. "" keeps
// OpenRouter's own order.
const MODEL_SORTS = [
  { value: "", label: "OpenRouter's order", desc: "As OpenRouter lists them" },
  { value: "in:asc", label: "Input price ↑", desc: "Cheapest input tokens first" },
  { value: "out:asc", label: "Output price ↑", desc: "Cheapest output tokens first" },
  { value: "in:desc", label: "Input price ↓", desc: "Most expensive input tokens first" },
  { value: "out:desc", label: "Output price ↓", desc: "Most expensive output tokens first" },
  { value: "ctx:desc", label: "Context ↓", desc: "Largest context window first" },
];

function setModelSort(v) {
  state.form.modelSort = v;
  saveForm();
  renderView();
}

// sortedModels orders the picker's list. Models whose price varies (or
// whose context is unknown) go last. A tie on one price goes by the other;
// other ties keep OpenRouter's order.
function sortedModels(list) {
  const [key, dir] = (state.form.modelSort || "").split(":");
  const fields = { in: ["price_in", "price_out"], out: ["price_out", "price_in"], ctx: ["context_length"] }[key];
  if (!fields) return list;
  const by = (f) => (a, b) => {
    const x = a[f], y = b[f];
    if (x == null || y == null) return (x == null) - (y == null);
    return dir === "desc" ? y - x : x - y;
  };
  return [...list].sort((a, b) => fields.reduce((c, f) => c || by(f)(a, b), 0));
}

function renderPicker() {
  const p = state.picker;
  return h("div", { class: "picker", id: "picker", onkeydown: (e) => { if (e.key === "Escape" && !state.openSelect) { openPicker(null); } } },
    h("div", { class: "picker-top" },
      h("input", {
        id: "pick-q", type: "search", value: p.q, placeholder: "Search OpenRouter models: glm, nemotron, claude, gemini, deepseek…", "aria-label": "Search models",
        oninput: (e) => { p.q = e.target.value; clearTimeout(pickerTimer); pickerTimer = setTimeout(searchModels, 180); },
      }),
      renderSelect({ id: "pick-sort", label: "Sort models", value: state.form.modelSort || "", compact: true, options: MODEL_SORTS, onChange: setModelSort }),
      h("button", { type: "button", class: "picker-close", onclick: () => openPicker(null) }, "Done · Esc")),
    h("div", { class: "list", id: "pick-list" }, pickerRows()),
    h("div", { class: "foot", id: "pick-foot" }, pickerFoot()));
}

function updatePickerPreview(id) {
  state.picker.previewId = id;
  if (state.auxTab !== "models") {
    state.auxTab = "models";
    renderView();
    return;
  }
  const rsideModels = document.getElementById("rside-models");
  if (rsideModels) {
    rsideModels.replaceChildren(renderRightSidebarModels());
  }
}

function renderPickerList() {
  const list = document.getElementById("pick-list");
  if (!list) return;
  list.replaceChildren(pickerRows());
  const first = state.picker.list && sortedModels(state.picker.list)[0];
  const previewId = state.picker.previewId || (first && first.id);
  if (previewId) updatePickerPreview(previewId);
  document.getElementById("pick-foot").replaceChildren(pickerFoot());
}

const SHOWN = 80;
function pickerRows() {
  const p = state.picker;
  if (p.err) return h("div", { class: "empty" }, h("b", {}, "Couldn't load OpenRouter's model list. "), p.err, h("br"), "You can still add a model by id: /model z-ai/glm-5.3");
  if (!p.list) return h("div", { class: "empty" }, "Loading OpenRouter's models…");
  if (!p.list.length) return h("div", { class: "empty" }, `No model matches “${p.q}”.`);
  const price = (v) => v == null ? "varies" : `$${v < 1 ? v.toFixed(2) : v.toFixed(v < 10 ? 2 : 0)}`;
  // A sortable column's header: a click sorts by it, a second click
  // reverses it. Prices start cheapest first, context largest first.
  const [sortKey, sortDir] = (state.form.modelSort || "").split(":");
  const sortTh = (key, text, first) => {
    const on = sortKey === key;
    const next = on ? (sortDir === "asc" ? "desc" : "asc") : first;
    return h("th", { class: "num", "aria-sort": on ? (sortDir === "asc" ? "ascending" : "descending") : "none" },
      h("button", { type: "button", class: `sort-th ${on ? "on" : ""}`, title: `Sort by ${text}`, onclick: () => setModelSort(`${key}:${next}`) },
        text, h("span", { class: "sort-arrow", "aria-hidden": "true" }, on ? (sortDir === "asc" ? "↑" : "↓") : "↕")));
  };
  return h("table", {},
    h("thead", {}, h("tr", {}, h("th", {}, "model"), h("th", {}, "name"), sortTh("ctx", "context", "desc"), sortTh("in", "in/M", "asc"), sortTh("out", "out/M", "asc"), h("th", {}, "tools"))),
    h("tbody", {}, sortedModels(p.list).slice(0, SHOWN).map((m) => {
      const have = state.form.models.includes(m.id);
      const toggleModel = () => {
        if (have) {
          state.form.models = state.form.models.filter((x) => x !== m.id);
          saveForm();
          renderView();
        } else {
          addModel(m.id);
        }
        openPicker(p.q);
      };
      const cell = (text, cls) => h("td", { class: cls }, h("button", {
        type: "button",
        tabindex: cls ? "-1" : null,
        title: `${m.name} (${m.id})`,
        onmouseenter: () => updatePickerPreview(m.id),
        onfocus: () => updatePickerPreview(m.id),
        onclick: toggleModel,
      }, text));
      return h("tr", { class: have ? "selected-row" : "" },
        cell([have ? h("span", { class: "ok" }, "✓ ") : null, labMark(m.id), m.id]),
        cell(h("span", { class: "dim" }, m.name || "–"), "nm"),
        cell(m.context_length ? fmtTok(m.context_length) : "–", "num"),
        cell(price(m.price_in), "num"),
        cell(price(m.price_out), "num"),
        cell(m.tools ? h("span", { class: "ok" }, "yes") : h("span", { class: "bad" }, "no"), "tl"));
    })));
}

function pickerFoot() {
  const p = state.picker;
  if (!p.list) return "";
  const more = p.list.length > SHOWN ? ` · showing ${SHOWN} of ${p.list.length}; search to narrow` : "";
  return `Click a row to add/remove · Hover to preview Model Card · Prices in USD / 1M tokens${more}`;
}

function canRun() {
  const f = state.form;
  if (state.status && state.status.jobs_warning && f.environment === "docker") return state.status.jobs_warning;
  if (!state.tasks || !state.tasks.length) return "No task to run.";
  if (!chosenTasks().length) return "Choose at least one arm.";
  if (envProblem(f.environment)) return environmentHint(f.environment);
  if (needsModel(f.agent) && !f.models.length) return "Add a model: + Add OpenRouter model, or /model <id>.";
  return "";
}

async function startRun() {
  const f = state.form;
  const why = canRun();
  if (why) return say(why, true);
  const body = { tasks: chosenTasks(), attempts: f.runs, agent: f.agent, concurrent: f.concurrent, retries: f.retries };
  if (f.environment !== "docker") body.environment = f.environment;
  if (needsModel(f.agent)) body.models = f.models.map((m) => `openrouter/${m}`);
  if (f.timeout) body.timeout_sec = f.timeout * 60;
  state.starting = true;
  say("Starting Harbor…");
  try {
    const job = await api("/jobs", { method: "POST", headers: { "Content-Type": "application/json" }, body: JSON.stringify(body) });
    state.note = "";
    location.hash = `#/jobs/${encodeURIComponent(job.id)}`;
  } catch (e) {
    say(`Harbor didn't start: ${e.message}`, true);
  } finally {
    state.starting = false;
  }
}

function armsOf(trials) {
  const present = new Set(trials.map((t) => t.arm));
  return [...ARMS.filter((a) => present.has(a)), ...(present.has("") ? [""] : [])];
}

function matrixRows(job) {
  const rows = new Map();
  const multi = new Set(job.trials.map((t) => base(t.task))).size > 1;
  for (const t of job.trials) {
    const key = `${base(t.task)}|${t.model}`;
    if (!rows.has(key)) {
      const who = t.model ? orId(t.model) : job.agent;
      rows.set(key, { label: multi ? `${base(t.task)} · ${who}` : who, model: t.model, trials: [] });
    }
    rows.get(key).trials.push(t);
  }
  return [...rows.values()];
}

// A table's first cell: the model's lab mark, then its name.
function modelCell(model, label) {
  if (!model) return label;
  return h("span", { class: "model-cell" }, labMark(model), h("span", {}, label));
}

function selected() {
  const job = state.job;
  if (!job || !job.trials.length) return null;
  let t = job.trials.find((x) => trialKey(x) === state.sel);
  if (!t) {
    t = job.trials.find((x) => x.status === "running") || job.trials.find((x) => x.id) || job.trials[0];
    state.sel = trialKey(t);
  }
  return t;
}

async function select(t) {
  if (state.sel !== trialKey(t)) {
    state.iv = { ...state.iv, inv: "", kind: null, variant: null, round: null };
    state.replay = null;
  }
  state.sel = trialKey(t);
  state.steps = null;
  state.stepsFor = "";
  state.confirm = "";
  if (!state.rightSide) {
    state.rightSide = true;
    saveUIPrefs();
    renderTop();
  }
  renderView();
  const focus = document.querySelector(`.dots button[data-key="${CSS.escape(state.sel)}"]`);
  if (focus) focus.focus();
  if (t.id) await loadSteps(t);
  if (finished(t)) await loadGrades([t]);
  if (state.sel === trialKey(t)) renderView();
}

function isMultiTaskJob(job) {
  if (!job || !job.trials) return false;
  return new Set(job.trials.map((t) => base(t.task))).size > 1;
}

function trialLabel(t, includeTask = false) {
  const who = t.model ? shortModel(t.model) : base(t.task);
  const prefix = includeTask && t.model ? `${base(t.task)} · ${who}` : who;
  return `${prefix} · ${t.arm || "task"} · #${t.attempt}`;
}

function pill(t) {
  if (t.status === "running") {
    return h("span", { class: "pill running musing" },
      spinner(),
      h("span", { class: "verb", dataset: { verb: t.id || trialKey(t) } }, verbFor(t.id || trialKey(t))),
      t.started ? elapsed(t.started, clock) : null);
  }
  const word = { pass: "PASS", fail: "FAIL", error: "ERROR", queued: "queued" }[t.status] || t.status;
  return h("span", { class: `pill ${t.status}` }, word);
}

// Arm Guide / Legend banner explaining what control, workflow, kavach, and kavach-implement mean
function renderArmGuide(arms, track) {
  const known = arms.filter(Boolean);
  if (!known.length) return null;
  return h("div", { class: "arm-guide", "aria-label": "Experiment arm definitions" },
    known.map((a) => {
      const info = armInfo(a, track);
      return h("div", {
        class: `arm-guide-item ${info.tone === "peach" ? "kavach-tone" : ""}`,
        title: info.detail,
      },
        h("div", { class: "arm-guide-head" },
          h("span", { class: `arm-guide-name ${info.tone}` }, a),
          h("span", { class: "arm-guide-tag" }, info.tag)),
        h("div", { class: "arm-guide-desc" }, info.short));
    }));
}

function renderArmHintBar(t) {
  const info = armInfo(t.arm, jobTrack());
  return h("div", { class: "kcard-arm-hint", title: info.detail },
    h("span", { class: `arm-pill ${info.tone === "blue" ? "blue" : ""}` }, t.arm || "task"),
    h("span", { class: "arm-hint-text" }, info.tileHint),
    h("span", { class: "hint-icon", title: info.detail, "aria-label": info.detail }, "?"));
}

function renderRuns(view) {
  const job = state.job;
  const kids = [];
  if (job.error) kids.push(banner("Harbor exited before it wrote the run's folder.", job.error));
  if (!job.trials.length) {
    kids.push(h("div", { class: "empty" }, job.status === "running"
      ? [h("b", {}, "Harbor is starting the run."), " Building a task's image the first time takes minutes; attempts appear here as they start."]
      : "This run has no attempts."));
    kids.push(h("div", { class: "actions" }, runControl(job)));
    return view.replaceChildren(h("div", { class: "pane" }, kids));
  }
  const arms = armsOf(job.trials);
  const sel = selected();
  const rows = matrixRows(job);
  const table = h("table", { class: "mx" },
    h("thead", {}, h("tr", {},
      h("th", {}, job.models.length ? "task / model" : "agent"),
      arms.map((a) => {
        const info = armInfo(a);
        return h("th", {}, h("span", { class: "th-hint", title: info.detail }, a || "attempts"));
      }),
      h("th", {}, "pass rate"))),
    h("tbody", {}, rows.map((r) => {
      const rates = arms.map((a) => {
        const ts = r.trials.filter((t) => t.arm === a);
        const graded = ts.filter((t) => t.status === "pass" || t.status === "fail").length;
        return graded ? `${ts.filter((t) => t.status === "pass").length}/${graded}` : "–";
      });
      const allQueued = r.trials.every((t) => t.status === "queued");
      return h("tr", { class: r.trials.includes(sel) ? "sel" : null },
        h("td", {}, modelCell(r.model, r.label)),
        arms.map((a) => h("td", {}, h("span", { class: "dots" }, r.trials.filter((t) => t.arm === a).map((t) => h("button", {
          type: "button", class: `st ${t.status} ${t === sel ? "on" : ""}`, dataset: { key: trialKey(t) },
          "aria-label": `${trialLabel(t, true)}: ${t.status}`, title: `${trialLabel(t, true)}: ${t.status} (${armInfo(t.arm).tag})`, "aria-pressed": String(t === sel),
          onclick: () => select(t),
        }))))),
        h("td", { class: allQueued ? "dim" : null }, allQueued ? "queued" : rates.join(" · ")));
    })));

  const armPasses = arms.map((a) => {
    const ts = job.trials.filter((t) => t.arm === a);
    const graded = ts.filter((t) => t.status === "pass" || t.status === "fail").length;
    return graded ? `${a || "all"} ${ts.filter((t) => t.status === "pass").length} of ${graded} passed` : `${a || "all"} none graded yet`;
  });

  if (state.replay && !(sel && sel.id === state.replay.trial && sel.job === state.replay.job)) state.replay = null;
  const left = state.replay ? renderReplay(sel) : h("div", { class: "pane" },
    kids,
    renderArmGuide(arms, jobTrack()),
    h("div", { class: "tablewrap" }, table),
    h("div", { class: "legend" },
      h("span", {}, h("i", { class: "st pass" }), "pass"),
      h("span", {}, h("i", { class: "st fail" }), "fail"),
      h("span", {}, h("i", { class: "st running" }), "running"),
      h("span", {}, h("i", { class: "st queued" }), "queued"),
      h("span", {}, h("i", { class: "st error" }), "error"),
      h("span", { class: "faint" }, "grade = task's hidden verifier test suite")),
    h("div", { class: "stats" }, armPasses.join(" · ")));

  const steps = document.querySelector(".steps");
  const keep = steps && {
    top: steps.scrollTop,
    bottom: steps.scrollHeight - steps.scrollTop - steps.clientHeight < 24,
    key: steps.dataset.key,
    open: [...steps.querySelectorAll("details[open]")].map((d) => d.dataset.i),
  };
  const invs = document.querySelector(".invs");
  const invKeep = invs && { key: invs.dataset.key, top: invs.scrollTop };
  const body = h("div", { class: `body ${state.rightSide ? "" : "noright"}` },
    left,
    sel ? renderAttempt(sel) : h("div", { class: "pane right-pane" }, makeResizeSash("right")));
  view.replaceChildren(body);
  slideRightSide(body);
  const invNow = document.querySelector(".invs");
  if (invNow && invKeep && invKeep.key === invNow.dataset.key) invNow.scrollTop = invKeep.top;
  const now = document.querySelector(".steps");
  if (now) {
    if (keep && keep.key === now.dataset.key) {
      for (const i of keep.open) {
        const d = now.querySelector(`details[data-i="${i}"]`);
        if (d) d.open = true;
      }
      now.scrollTop = keep.bottom && sel && sel.status === "running" ? now.scrollHeight : keep.top;
    } else if (sel && sel.status === "running") {
      now.scrollTop = now.scrollHeight;
    } else {
      now.scrollTop = 0;
    }
  }
}

function tokenLine(t) {
  const tok = t.tokens;
  const parts = [];
  if (t.steps) parts.push(`step ${t.steps}`);
  if (tok && (tok.input || tok.output)) {
    parts.push(`in ${fmtTok(tok.input)}${tok.cached ? ` (+${fmtTok(tok.cached)} cached)` : ""}`, `out ${fmtTok(tok.output)}`);
  }
  if (t.cost != null) parts.push(fmtCost(t.cost));
  if (t.seconds != null) parts.push(fmtDur(t.seconds));
  return parts.join(" · ");
}

function gradeLine(t) {
  const g = state.grades.get(`${t.job}/${t.id}`);
  if (!g) return "";
  if (g.grade && g.grade.applied === false) return "the patch didn't apply";
  if (g.grade && g.grade.built === false) return "the patched source didn't build";
  const n = g.passed + g.failed;
  if (!n) return "";
  return g.failed ? `${g.failed} of ${n} tests failed` : `${n} of ${n} tests passed`;
}

function isContractTrial(t, verdict) {
  return Boolean(
    (t.kavach && t.kavach.features) ||
    t.arm === "workflow" ||
    t.arm === "kavach-implement" ||
    /^(pass|unreached|regression|missing_invariant|baseline_mismatch|unapproved|stale_lock)/.test(verdict || "")
  );
}

function kavachLine(t) {
  const g = state.grades.get(`${t.job}/${t.id}`);
  const verdict = t.kavach ? t.kavach.verdict : g && g.grade && g.grade.kavach_diff;
  if (!verdict) return "";
  const cmd = isContractTrial(t, verdict) ? "kavach contract check" : "kavach diff";
  return `${cmd} ${verdict}${t.arm === "control" || t.arm === "workflow" ? " · checked after the run" : ""}`;
}

// disagreement says when the agent's own last kavach diff, run in its
// container, gave a different verdict from the verifier's.
function disagreement(t) {
  const g = state.grades.get(`${t.job}/${t.id}`);
  if (!g || !g.agent_diff || !t.kavach || isContractTrial(t, t.kavach.verdict)) return null;
  const status = (v) => v.replace(/[(@].*/, "");
  if (status(g.agent_diff) === status(t.kavach.verdict)) return null;
  return h("div", { class: "warn-line" },
    "The agent's last kavach diff said ", h("b", { class: "m" }, g.agent_diff), "; the verifier's said ", h("b", { class: "m" }, t.kavach.verdict),
    ". Either the code changed after that diff, or the two builds differ.");
}

function renderAttempt(t) {
  const job = state.job;
  const multi = isMultiTaskJob(job);
  const info = armInfo(t.arm, jobTrack());
  const kids = [
    makeResizeSash("right"),
    h("div", { class: "attempt-head-row" },
      h("div", { class: "row" },
        h("span", { class: "h" }, trialLabel(t, multi)),
        pill(t)),
      h("button", {
        type: "button",
        class: "aux-close",
        "aria-label": "Hide attempt inspector",
        title: "Hide attempt inspector (⌘J)",
        onclick: toggleRightSide,
      }, "⇥")),
  ];
  kids.push(h("div", { class: "stats" }, tokenLine(t)));
  if (t.error) kids.push(errorView(t));
  const gl = gradeLine(t);
  if (gl) kids.push(h("div", { class: t.status === "fail" ? "bad" : "dim" }, gl));
  const dis = disagreement(t);
  if (dis) kids.push(dis);

  const key = `${t.job}/${t.id}`;
  const checks = state.stepsFor === key ? checksOf(state.steps) : [];
  // Kavach's arms declare invariants even before, or without, a contract check.
  const hasInv = checks.length > 0 || t.arm === "kavach" || t.arm === "kavach-implement";
  // The Invariants tab shows the check itself, so the card and the band
  // above it give their room to it.
  const invTab = hasInv && state.inspTab === "invariants";
  const cmd = t.kavach ? (isContractTrial(t, t.kavach.verdict) ? "kavach contract check " : "kavach diff ") : "";
  const meta = t.kavach ? [t.kavach.features, t.kavach.variants].filter(Boolean).join(" · ") : "";
  const headline = invTab ? null : renderHeadline(t);
  if (headline) kids.push(headline);
  if (!invTab) kids.push(h("div", { class: `kcard arm-${t.arm || "kavach"}` },
    renderArmHintBar(t),
    t.kavach ? h("span", {}, h("span", { class: "peach m" }, cmd), h("b", { class: "m" }, t.kavach.verdict)) : h("span", { class: "dim" }, info.short),
    meta ? h("span", { class: "dim" }, meta) : null,
    t.kavach && t.kavach.old ? h("span", { class: "faint m" }, `old build: ${t.kavach.old}`) : null));
  const probe = invTab ? null : renderProbe(t);
  if (probe) kids.push(probe);

  const openInv = () => { state.inspTab = "invariants"; renderView(); };
  if (Array.isArray(state.steps) && state.stepsFor === key) {
    if (!invTab && checks.length) kids.push(renderInvariantBand(checks, openInv));
    const shown = state.steps.filter((s) => s.kind !== "usage");
    const rounds = trailRows(shown).filter((r) => r.round).length;
    if (!hasInv && state.inspTab === "invariants") state.inspTab = "steps";
    const tab = state.inspTab;
    const last = checks[checks.length - 1];
    const trips = last ? (last.variant_failures || []).length : 0;
    const tabs = [
      { id: "steps", label: "Steps", badge: shown.length },
      { id: "trail", label: "Kavach trail", badge: rounds ? `${rounds} round${rounds === 1 ? "" : "s"}` : null },
      hasInv ? { id: "invariants", label: "Invariants", badge: !last ? null : last.passed ? "pass" : trips ? `${trips} trip` : last.verdict.replace(/[(@].*$/, ""), tone: last && (last.passed ? "ok" : "bad") } : null,
    ].filter(Boolean);
    kids.push(h("div", { class: "aux-tabs insp-tabs", role: "tablist", "aria-label": "Attempt" },
      tabs.map((x) => h("button", {
        type: "button", role: "tab", class: `aux-tab ${tab === x.id ? "on" : ""}`, "aria-selected": String(tab === x.id),
        onclick: () => { state.inspTab = x.id; renderView(); },
      }, h("span", {}, x.label), x.badge != null ? h("span", { class: `aux-tab-badge ${x.tone || ""}` }, x.badge) : null))));
    const pick = (i) => { state.inspTab = "steps"; renderView(); requestAnimationFrame(() => showStep(i)); };
    if (tab === "invariants") kids.push(renderInvariants(t, checks));
    else if (tab === "trail") kids.push(renderTrail(shown, pick, clock));
    else kids.push(renderScrubber(t, shown, clock), renderSteps(t, job));
  } else {
    kids.push(renderSteps(t, job));
  }
  if (finished(t)) kids.push(patchView(t));
  kids.push(h("div", { class: "actions" }, openInOpenCode(t, job), retryControl(t, job), runControl(job)));
  return h("div", { class: "pane right-pane", "aria-hidden": String(!state.rightSide) }, kids);
}

const KIND = { shell: "Shell", read: "Read", search: "Grep", edit: "Edit", mcp: "Tool", error: "Error" };

// renderSteps marks what it returns, list or placeholder, so patchSteps can
// replace a placeholder alone.
function renderSteps(t, job) {
  const el = stepsBody(t, job);
  el.dataset.steps = `${t.job}/${t.id}`;
  return el;
}

function stepsBody(t, job) {
  const key = `${t.job}/${t.id}`;
  const box = h("div", { class: "steps", dataset: { key }, "aria-live": "polite" });
  if (t.status === "queued") return h("div", { class: "dim" }, "Queued: it starts when a slot frees up.");
  const steps = state.stepsFor === key ? state.steps : null;
  if (steps && steps.error) return h("div", { class: "bad" }, "Couldn't load steps: " + steps.error);
  if (!steps) return h("div", { class: "dim" }, "Loading steps…");
  const shown = steps.filter((s) => s.kind !== "usage");
  if (!shown.length) {
    let why = "No steps recorded: the trial has neither trajectory.json nor opencode.txt.";
    if (!needsModel(job.agent)) why = `The ${job.agent} agent makes no model calls, so it has no steps.`;
    else if (t.status === "running") why = "Waiting for the agent's first step…";
    return h("div", { class: "dim" }, why);
  }
  let lastTextIdx = -1;
  shown.forEach((s, idx) => { if (s.kind === "text") lastTextIdx = idx; });
  shown.forEach((s, i) => box.append(stepEl(s, i, i === lastTextIdx, t.model)));
  return box;
}

function inlineMD(str) {
  const out = [];
  const re = /(`[^`]+`|\*\*[^*]+\*\*|\*[^*\n]+\*)/g;
  let last = 0, m;
  while ((m = re.exec(str)) !== null) {
    if (m.index > last) out.push(str.slice(last, m.index));
    const tok = m[0];
    if (tok.startsWith("`")) out.push(h("code", {}, tok.slice(1, -1)));
    else if (tok.startsWith("**")) out.push(h("strong", {}, inlineMD(tok.slice(2, -2))));
    else out.push(h("em", {}, inlineMD(tok.slice(1, -1))));
    last = re.lastIndex;
  }
  if (last < str.length) out.push(str.slice(last));
  return out;
}

function splitTableCells(line) {
  return line.trim().replace(/^\||\|$/g, "").split("|").map((c) => c.trim());
}

function renderMarkdown(raw) {
  const lines = String(raw || "").replace(/\r\n/g, "\n").split("\n");
  const nodes = [];
  let i = 0;
  while (i < lines.length) {
    const line = lines[i];
    if (!line.trim()) { i++; continue; }
    if (line.trim().startsWith("```")) {
      const code = [];
      i++;
      while (i < lines.length && !lines[i].trim().startsWith("```")) {
        code.push(lines[i]);
        i++;
      }
      if (i < lines.length) i++;
      nodes.push(h("pre", { class: "md-pre" }, code.join("\n")));
      continue;
    }
    const hm = line.match(/^(#{1,4})\s+(.*)$/);
    if (hm) {
      nodes.push(h(`h${hm[1].length}`, { class: "md-h" }, inlineMD(hm[2])));
      i++;
      continue;
    }
    if (line.includes("|") && i + 1 < lines.length && /^\s*\|?[\s:-]+\|[\s|:-]*$/.test(lines[i + 1])) {
      const headers = splitTableCells(line);
      i += 2;
      const rows = [];
      while (i < lines.length && lines[i].includes("|") && lines[i].trim()) {
        rows.push(splitTableCells(lines[i]));
        i++;
      }
      nodes.push(h("table", { class: "md-table" },
        h("thead", {}, h("tr", {}, headers.map((c) => h("th", {}, inlineMD(c))))),
        h("tbody", {}, rows.map((r) => h("tr", {}, r.map((c) => h("td", {}, inlineMD(c))))))
      ));
      continue;
    }
    if (/^\s*([-*]|\d+\.)\s+/.test(line)) {
      const ordered = /^\s*\d+\.\s+/.test(line);
      const items = [];
      while (i < lines.length && /^\s*([-*]|\d+\.)\s+/.test(lines[i])) {
        items.push(h("li", {}, inlineMD(lines[i].replace(/^\s*([-*]|\d+\.)\s+/, ""))));
        i++;
      }
      nodes.push(h(ordered ? "ol" : "ul", { class: "md-list" }, items));
      continue;
    }
    if (/^\s*>\s?/.test(line)) {
      const q = [];
      while (i < lines.length && /^\s*>\s?/.test(lines[i])) {
        q.push(lines[i].replace(/^\s*>\s?/, ""));
        i++;
      }
      nodes.push(h("blockquote", {}, inlineMD(q.join(" "))));
      continue;
    }
    const para = [];
    while (
      i < lines.length &&
      lines[i].trim() &&
      !lines[i].trim().startsWith("```") &&
      !/^(#{1,4})\s+/.test(lines[i]) &&
      !/^\s*([-*]|\d+\.)\s+/.test(lines[i]) &&
      !/^\s*>\s?/.test(lines[i]) &&
      !(lines[i].includes("|") && i + 1 < lines.length && /^\s*\|?[\s:-]+\|[\s|:-]*$/.test(lines[i + 1]))
    ) {
      para.push(lines[i]);
      i++;
    }
    nodes.push(h("p", {}, inlineMD(para.join(" "))));
  }
  return nodes;
}

function compactKavachJSON(detail) {
  if (!detail) return detail;
  return detail.replace(/\[\s*\n(\s*(?:\d+|"[^"\n]{1,24}")(?:,\s*\n\s*(?:\d+|"[^"\n]{1,24}"))*)\s*\n\s*\]/g, (_, inner) => {
    const items = inner.split(",").map((s) => s.trim()).filter(Boolean);
    return `[${items.join(", ")}]`;
  });
}

// labMark is the mark of the lab behind a model, or its initial when there is
// no mark for it.
function labMark(model) {
  const slug = labOf(model);
  const lab = LABS[slug];
  if (!lab) return h("span", { class: "lab-mark mono", "aria-hidden": "true" }, slug.charAt(0).toUpperCase());
  const ns = "http://www.w3.org/2000/svg";
  const svg = document.createElementNS(ns, "svg");
  svg.setAttribute("class", "lab-mark");
  svg.setAttribute("viewBox", "0 0 24 24");
  svg.setAttribute("fill", "currentColor");
  svg.setAttribute("fill-rule", "evenodd");
  svg.setAttribute("aria-hidden", "true");
  for (const attrs of lab.paths) {
    const path = document.createElementNS(ns, "path");
    for (const [k, v] of Object.entries(attrs)) path.setAttribute(k, v);
    svg.append(path);
  }
  return svg;
}

// stepEl is a step marked with its index among the shown steps, which the
// scrubber and the trail scroll to.
function stepEl(s, i, isLastText, model) {
  const el = renderStep(s, i, isLastText, model);
  el.dataset.si = String(i);
  return el;
}

function renderStep(s, i, isLastText, model) {
  const ts = h("span", { class: "ts" }, clock(s.t_ms / 1000));
  if (s.kind === "text") {
    const text = s.title || "";
    const lineCount = text.split("\n").length;
    const isLong = text.length > 260 || lineCount > 4;
    const startCollapsed = isLong && !isLastText;
    const body = h("div", { class: `say md${startCollapsed ? " collapsed" : ""}` }, renderMarkdown(text));
    let toggleBtn = null;
    if (isLong) {
      toggleBtn = h("button", {
        type: "button",
        class: "step-toggle",
        onclick: (e) => {
          e.stopPropagation();
          const collapsed = body.classList.toggle("collapsed");
          toggleBtn.textContent = collapsed ? "Expand ▾" : "Collapse ▴";
        },
      }, startCollapsed ? "Expand ▾" : "Collapse ▴");
    }
    return h("div", { class: "step text" },
      h("div", { class: "step-head" }, h("span", { class: "k", title: model || null }, model ? labMark(model) : null, model ? shortModel(model) : "Agent"), toggleBtn, ts),
      body);
  }
  let label = KIND[s.kind] || s.kind;
  let title = [s.title];
  if (s.kind === "read") {
    const [path, range] = s.title.split(" lines ");
    title = [path, range ? h("span", { class: "faint" }, ` ${range}`) : null];
  }
  if (s.kind === "shell") {
    const cmdLines = String(s.title || "").split("\n");
    const firstLine = cmdLines[0];
    const more = cmdLines.length > 1 ? h("span", { class: "faint" }, ` (+${cmdLines.length - 1} lines)`) : null;
    title = [h("span", { class: "m" }, firstLine), more, s.exit ? h("span", { class: "x" }, `exit ${s.exit}`) : null];
  }
  if (s.kavach) label = "Kavach";
  const cls = `step ${s.kind}${s.kavach ? " kavach" : ""}`;
  const head = [h("span", { class: "k" }, label), h("span", { class: "t" }, title), ts];
  const verdict = (s.kavach || s.contract) && (s.verdict || s.variants || s.contract) ? h("div", { class: "verdict" },
    s.verdict || s.contract ? h("span", { class: `pill ${(s.contract ? s.contract.passed : s.passed) ? "pass" : (s.contract || s.passed === false) ? "fail" : "queued"}` }, s.verdict || s.contract.verdict) : null,
    s.variants ? h("span", { class: "dim" }, s.variants) : null,
    s.contract ? h("button", {
      type: "button", class: "link-btn",
      onclick: () => {
        const n = checksOf(state.steps).findIndex((c) => c.step === s);
        state.inspTab = "invariants";
        state.iv.page = "results";
        state.iv.round = n < 0 || n === checksOf(state.steps).length - 1 ? null : n;
        state.iv.variant = null;
        renderView();
      },
    }, "See invariants") : null) : null;
  if (!s.detail) return h("div", { class: cls }, h("div", { class: "line" }, head), verdict);
  const detailText = s.kavach ? compactKavachJSON(s.detail) : s.detail;
  const body = s.kind === "edit" ? diffView(detailText) : h("pre", {}, detailText);
  if (verdict) return h("div", { class: cls }, h("details", { dataset: { i } }, h("summary", {}, head), body), verdict);
  return h("details", { class: cls, dataset: { i } }, h("summary", {}, head), body);
}

function diffView(text) {
  let add = 0, del = 0;
  const lines = text.replace(/\n$/, "").split("\n").map((l) => {
    let cls = null;
    if (l.startsWith("+++") || l.startsWith("---") || l.startsWith("diff ") || l.startsWith("index ")) cls = "fh";
    else if (l.startsWith("@@")) cls = "hh";
    else if (l.startsWith("+")) { cls = "a"; add++; }
    else if (l.startsWith("-")) { cls = "d"; del++; }
    return h("div", { class: cls }, l || " ");
  });
  const el = h("div", { class: "diff" }, lines);
  el.counts = { add, del };
  return el;
}

const SIGNALS = { 1: "SIGHUP", 2: "SIGINT", 6: "SIGABRT", 9: "SIGKILL", 15: "SIGTERM" };

// errorSummary shortens Harbor's exception text, which for an agent command
// carries the whole command line (the task's prompt) and its output.
function errorSummary(text) {
  const m = /^(\w+): Command failed \(exit (\d+)\)/.exec(text);
  if (m) {
    const code = +m[2];
    const sig = code > 128 && SIGNALS[code - 128];
    return `${m[1]}: the agent's command exited with ${code}${sig ? ` (killed by ${sig})` : ""}`;
  }
  const first = text.split("\n", 1)[0];
  return first.length > 200 ? first.slice(0, 200) + "…" : first;
}

const openErrors = new Set();

function errorView(t) {
  const summary = errorSummary(t.error);
  if (summary === t.error) return h("div", { class: "err-line" }, t.error);
  const key = `${t.job}/${t.id}`;
  const d = h("details", { class: "err-line err-more" }, h("summary", {}, summary), h("pre", {}, t.error));
  d.open = openErrors.has(key);
  d.addEventListener("toggle", () => { d.open ? openErrors.add(key) : openErrors.delete(key); });
  return d;
}

function patchView(t) {
  const d = h("details", { class: "patch" }, h("summary", {}, "Patch"));
  d.addEventListener("toggle", async () => {
    if (!d.open || d.dataset.loaded) return;
    d.dataset.loaded = "1";
    let body;
    try {
      const resp = await fetch(apiURL(`/trials/${encodeURIComponent(t.job)}/${encodeURIComponent(t.id)}/patch`));
      if (resp.status === 404) body = h("div", { class: "dim" }, "No patch was saved: the task has no collect hook, or Harbor never reached it.");
      else if (!resp.ok) body = h("div", { class: "bad" }, `Couldn't load the patch: ${resp.status}`);
      else {
        const text = await resp.text();
        if (!text.trim()) body = h("div", { class: "dim" }, "The agent changed nothing.");
        else {
          body = diffView(text);
          d.querySelector("summary").append(" ", h("span", { class: "ok" }, `+${body.counts.add}`), " ", h("span", { class: "bad" }, `−${body.counts.del}`));
        }
      }
    } catch (e) {
      body = h("div", { class: "bad" }, `Couldn't load the patch: ${e.message}`);
    }
    d.append(body);
  });
  return d;
}

const shq = (s) => /^[\w@%+=:,./-]+$/.test(s) ? s : `'${s.replace(/'/g, `'\\''`)}'`;

// sessionState says why an attempt's OpenCode session has ended: the agent
// can be done while Harbor still grades it, or have been stopped part-way.
function sessionState(t) {
  if (t.status === "running" || t.status === "queued") return "The agent has finished; this attempt is being graded.";
  if (t.status === "error" && /^(interrupted|CancelledError)/.test(t.error || "")) return "This attempt was stopped before the agent finished; the session shows its work up to then.";
  return "This attempt has finished.";
}

function openInOpenCode(t, job) {
  if (STATIC) return null; // the session is on the machine that ran it
  const oc = t.opencode;
  if (t.status === "running" && oc && oc.url && !oc.finished) {
    return h("a", { class: "btn blue", href: oc.web || oc.url, target: "_blank", rel: "noopener" }, "Open in OpenCode ↗");
  }
  if (oc && oc.export) {
    const trial = oc.export.split("/").slice(0, -2).join("/");
    const root = state.status && state.status.root;
    const cmd = `${root ? shq(`${root}/bench/agent/harbor/show.sh`) : "bench/agent/harbor/show.sh"} ${shq(trial)}`;
    const copy = h("button", { type: "button", class: "btn", onclick: async (e) => {
      const b = e.currentTarget;
      try {
        await navigator.clipboard.writeText(cmd);
        b.textContent = "Copied";
      } catch {
        const range = document.createRange();
        range.selectNodeContents(b.closest(".cmd").querySelector("pre"));
        getSelection().removeAllRanges();
        getSelection().addRange(range);
        b.textContent = "Selected: press ⌘C";
      }
    } }, "Copy");
    return h("details", { class: "oc" }, h("summary", { class: "btn blue" }, "Open in OpenCode ↗"),
      h("div", { class: "cmd" }, h("span", {}, `${sessionState(t)} Open its saved session in OpenCode's terminal UI:`), h("pre", {}, cmd), h("span", {}, copy)));
  }
  let why;
  if (t.status === "queued") why = "Not started yet.";
  else if (!oc && job.agent !== "opencode") why = `The ${job.agent} agent has no OpenCode session.`;
  else if (!oc && t.status === "running") why = "OpenCode hasn't started its server yet.";
  else if (!oc) why = "No OpenCode session was recorded for this attempt.";
  else why = "OpenCode's server has stopped and no session export was saved.";
  return h("span", { class: "note" }, h("button", { type: "button", class: "btn", disabled: true }, "Open in OpenCode ↗"), " ", why);
}

function runControl(job) {
  if (job.resumable && !TRIAL) {
    const warning = state.status && state.status.jobs_warning;
    return h("span", { class: "note" },
      h("button", { type: "button", class: "btn blue", disabled: !!warning, onclick: () => jobAction("resume") }, "Resume run"),
      " ", job.status === "paused" ? "Paused." : "Stopped.", " Resuming keeps the finished attempts and runs the rest.",
      state.actionErr ? h("span", { class: "bad" }, " ", state.actionErr) : null);
  }
  if (job.status === "pausing") return h("span", { class: "note" }, "Pausing: Harbor is cancelling the attempts in progress.");
  if (job.status !== "running") return null;
  if (!job.stoppable && TRIAL) return null;
  if (!job.stoppable) {
    return h("span", { class: "note" },
      h("button", { type: "button", class: "btn", disabled: true }, "Pause run"), " ",
      h("button", { type: "button", class: "btn", disabled: true }, "Stop run"), " Another process started this run: pause or stop it there.");
  }
  const running = job.trials.filter((t) => t.status === "running").length;
  const queued = job.trials.filter((t) => t.status === "queued").length;
  const ask = (what) => { state.confirm = what; renderView(); document.getElementById("confirm-yes").focus(); };
  if (state.confirm !== "pause" && state.confirm !== "stop") {
    return [
      TRIAL ? null : h("button", { type: "button", class: "btn", onclick: () => running ? ask("pause") : jobAction("pause") }, running ? "Pause run…" : "Pause run"), " ",
      h("button", { type: "button", class: "btn", onclick: () => ask("stop") }, "Stop run…"),
      state.actionErr ? h("span", { class: "bad" }, " ", state.actionErr) : null,
    ];
  }
  const s = (n) => n === 1 ? "" : "s";
  const text = state.confirm === "pause"
    ? h("span", {}, h("b", {}, "Pause the run? "),
      `Finished attempts are kept. ${running} running attempt${s(running)} of ${job.id} start${running === 1 ? "s" : ""} over when you resume, and what was spent on ${running === 1 ? "it" : "them"} is lost.`)
    : job.trials.length
      ? h("span", {}, h("b", {}, "Stop the whole run, not just this attempt? "),
        `Harbor can only stop a run as a whole: it cancels ${running} running and ${queued} queued attempt${s(running + queued)} of ${job.id}; they end as errors.`)
      : h("span", {}, h("b", {}, "Stop the run? "), `Harbor stops ${job.id} before any attempt starts.`);
  const label = state.confirm === "pause" ? "Pause run" : "Stop run";
  return h("div", { class: "confirm", role: "alertdialog", "aria-label": label, onkeydown: (e) => { if (e.key === "Escape") { state.confirm = ""; renderView(); } } },
    text,
    state.actionErr ? h("span", { class: "bad" }, state.actionErr) : null,
    h("div", { class: "row" },
      h("button", { type: "button", id: "confirm-yes", class: state.confirm === "stop" ? "btn danger" : "btn", onclick: () => jobAction(state.confirm) }, label),
      h("button", { type: "button", class: "btn", onclick: () => { state.confirm = ""; renderView(); } }, "Cancel")));
}

// retryControl offers to run a failed attempt again once its run has ended.
function retryControl(t, job) {
  if (TRIAL || !job.retryable || (t.status !== "fail" && t.status !== "error")) return null;
  if (state.confirm !== `retry:${t.id}`) {
    return h("button", { type: "button", class: "btn", onclick: () => {
      state.confirm = `retry:${t.id}`;
      state.actionErr = "";
      renderView();
      document.getElementById("confirm-yes").focus();
    } }, "Retry attempt…");
  }
  const others = job.resumable ? " Attempts the run didn't finish run too." : "";
  return h("div", { class: "confirm", role: "alertdialog", "aria-label": "Retry attempt", onkeydown: (e) => { if (e.key === "Escape") { state.confirm = ""; renderView(); } } },
    h("span", {}, h("b", {}, "Run this attempt again? "),
      `Harbor runs a new attempt of ${t.task}${t.model ? ` with ${shortModel(t.model)}` : ""}, which costs as much as the first. This one leaves the run; its folder is kept in .bench/retried/${job.id}/.${others}`),
    state.actionErr ? h("span", { class: "bad" }, state.actionErr) : null,
    h("div", { class: "row" },
      h("button", { type: "button", id: "confirm-yes", class: "btn blue", onclick: () => retryTrial(t) }, "Retry attempt"),
      h("button", { type: "button", class: "btn", onclick: () => { state.confirm = ""; renderView(); } }, "Cancel")));
}

async function retryTrial(t) {
  try {
    const job = await api(`/trials/${encodeURIComponent(state.job.id)}/${encodeURIComponent(t.id)}/retry`, { method: "POST" });
    job.trials = job.trials || [];
    state.job = job;
    state.confirm = "";
    state.actionErr = "";
  } catch (e) {
    state.actionErr = `Couldn't retry it: ${e.message}`;
  }
  renderAll();
}

async function jobAction(action) {
  try {
    const job = await api(`/jobs/${encodeURIComponent(state.job.id)}/${action}`, { method: "POST" });
    job.trials = job.trials || [];
    state.job = job;
    state.confirm = "";
    state.actionErr = "";
  } catch (e) {
    state.actionErr = `Couldn't ${action} it: ${e.message}`;
  }
  renderAll();
}

function compareGroups(rows) {
  const groups = new Map();
  for (const r of rows) {
    const key = `${r.task}|${r.model}`;
    if (!groups.has(key)) groups.set(key, { task: r.task, model: r.model, arms: {} });
    groups.get(key).arms[r.arm || "control"] = r;
  }
  return [...groups.values()];
}

function compareArmsOf(rows) {
  const present = new Set(rows.map((r) => r.arm).filter(Boolean));
  const extra = [...present].some((a) => a !== "control" && a !== "kavach");
  if (!extra) return ["control", "kavach"];
  if (present.size === 1) present.add("control");
  return [...ARMS.filter((a) => present.has(a)), ...[...present].filter((a) => !ARMS.includes(a))];
}

const freshMedian = (r) => r && (r.median_fresh_tokens ?? null);

function renderCompare(view) {
  const job = state.job;
  const rows = state.compare || [];
  const groups = compareGroups(rows);
  const arms = compareArmsOf(rows);
  const multi = new Set(groups.map((g) => g.task)).size > 1;
  const kids = [];
  if (job.error) kids.push(banner("Harbor exited before it wrote the run's folder.", job.error));
  kids.push(h("h2", { class: "h" }, "Pass rate by arm"));
  kids.push(renderArmGuide(arms, jobTrack()));
  // Pending attempts by cell, split into running and queued: the comparison
  // only counts them together.
  const waiting = new Map();
  for (const t of job.trials) {
    if (finished(t)) continue;
    const key = `${base(t.task)}|${t.model}|${t.arm || "control"}`;
    const w = waiting.get(key) || { running: 0, queued: 0 };
    if (isQueued(t)) w.queued++;
    else w.running++;
    waiting.set(key, w);
  }

  if (!groups.length) {
    kids.push(h("div", { class: "dim" }, job.status === "running" ? "No attempt has finished yet." : "This run has no attempts."));
  } else {
    const rate = (r) => r && r.attempts ? r.passes / r.attempts : null;
    const cell = (r, better) => {
      if (!r) return h("td", { class: "dim" }, "–");
      const w = waiting.get(`${r.task}|${r.model}|${r.arm || "control"}`);
      const pending = w ? [w.running ? `${w.running} running` : "", w.queued ? `${w.queued} queued` : ""] : [r.pending ? `${r.pending} pending` : ""];
      const extra = [r.errors ? `${r.errors} error${r.errors > 1 ? "s" : ""}` : "", ...pending].filter(Boolean).join(" · ");
      const live = w && w.running ? h("span", { class: "dot run", "aria-hidden": "true" }) : null;
      if (!r.attempts) return h("td", { class: "dim" }, live, extra || "–");
      return h("td", { class: better ? "b peach" : null }, `${r.passes} / ${r.attempts}`, extra ? h("span", { class: "sub" }, live, extra) : null);
    };
    const chain = (g, f, fmt) => {
      const vals = arms.map((a) => f(g.arms[a]));
      if (vals.every((v) => v == null)) return "–";
      return vals.map(fmt).join(" → ");
    };
    kids.push(h("div", { class: "tablewrap" }, h("table", { class: "mx" },
      h("thead", {}, h("tr", {},
        h("th", {}, multi ? "task / model" : "model"),
        ...arms.map((a) => h("th", {}, h("span", { class: "th-hint", title: armInfo(a).detail }, a))),
        h("th", {}, "median time"),
        h("th", {}, "median tokens"),
        h("th", {}, "cost / attempt"))),
      h("tbody", {}, groups.map((g) => {
        const rates = arms.map((a) => rate(g.arms[a])).filter((v) => v != null);
        const best = rates.length ? Math.max(...rates) : null;
        const varied = new Set(arms.map((a) => rate(g.arms[a]) ?? -1)).size > 1;
        const who = g.model ? orId(g.model) : job.agent;
        return h("tr", {},
          h("td", {}, modelCell(g.model, multi ? `${g.task} · ${who}` : who)),
          ...arms.map((a) => {
            const r = g.arms[a];
            return cell(r, varied && best != null && rate(r) === best && a !== "control");
          }),
          h("td", {}, chain(g, (r) => r && r.median_seconds, fmtDur)),
          h("td", { title: "fresh tokens: input not served from cache, plus output" }, chain(g, freshMedian, fmtTok)),
          h("td", {}, chain(g, (r) => r && r.mean_cost, fmtCost)));
      })))));
    kids.push(h("div", { class: "faint", style: "font-size:12.5px" }, "Rates count graded attempts; errors and pending attempts are excluded from medians. Tokens are fresh tokens (uncached input + output)."));
  }
  kids.push(h("div", { class: "row", style: "margin-top:8px" }, h("h2", { class: "h" }, "Attempts"),
    h("span", { class: "dim" }, `${Math.max(job.trials_total, job.trials.length)} in this run`)));
  kids.push(renderKanban(job, multi));
  view.replaceChildren(h("div", { class: "pane" }, kids));
}

// An attempt Harbor has not started: it has no trial folder yet.
const isQueued = (t) => !t.id || t.status === "queued";

// The board's columns, left to right, by where an attempt is.
const KANBAN = [
  { id: "queued", label: "Queued", empty: "Nothing waiting.", has: isQueued },
  { id: "running", label: "Running", empty: "Nothing running.", has: (t) => !isQueued(t) && !finished(t) },
  { id: "passed", label: "Passed", empty: "None passed yet.", has: (t) => finished(t) && t.status === "pass" },
  { id: "failed", label: "Failed", empty: "None failed.", has: (t) => finished(t) && t.status !== "pass" },
];

function renderKanban(job, multi) {
  // A stopped run lists only the attempts that started; the rest are queued
  // until it resumes.
  const unlisted = Math.max(0, job.trials_total - job.trials.length);
  return h("div", { class: "kanban" }, KANBAN.map((col) => {
    const ts = job.trials.filter(col.has);
    const more = col.id === "queued" ? unlisted : 0;
    const note = more ? h("div", { class: "kcol-empty" },
      `${ts.length ? `${more} more` : more} not started${job.resumable ? ": resume the run to start them" : ""}.`) : null;
    return h("section", { class: `kcol ${col.id}`, "aria-label": `${col.label}: ${ts.length + more}` },
      h("header", { class: "kcol-head" },
        h("span", { class: "kcol-dot", "aria-hidden": "true" }),
        h("span", { class: "kcol-name" }, col.label),
        h("span", { class: "kcol-count" }, String(ts.length + more))),
      ts.map((t) => col.id === "queued" ? queuedCard(t, multi) : attemptCard(t, job, multi)),
      note,
      ts.length || note ? null : h("div", { class: "kcol-empty" }, col.empty));
  }));
}

function queuedCard(t, multi) {
  return h("div", { class: `kcard queued arm-${t.arm || "kavach"}`, dataset: { key: trialKey(t) } },
    h("div", { class: "kcard-arm-hint" },
      h("span", { class: `arm-pill ${armInfo(t.arm).tone === "blue" ? "blue" : ""}` }, t.arm || "task"),
      h("span", { class: "arm-hint-text" }, armInfo(t.arm, jobTrack()).tag)),
    h("div", { class: "title" }, labMark(t.model || ""), h("b", { title: t.model || t.task }, trialLabel(t, multi))),
    h("div", { class: "faint card-meta" }, "Waiting for a free slot"));
}

// The track of the run's tasks, when they share one.
function jobTrack() {
  const job = state.job;
  if (!job || !state.tasks) return null;
  const tracks = new Set(job.tasks.map((n) => {
    const task = state.tasks.find((t) => t.name === base(n));
    return task ? trackOf(task) : null;
  }));
  return tracks.size === 1 ? [...tracks][0] : null;
}

function attemptCard(t, job, multi = false) {
  const lines = [];
  const kl = kavachLine(t);
  if (kl) lines.push(h("div", { class: "dim m" }, kl + (t.kavach && t.kavach.variants ? ` · ${t.kavach.variants}` : "")));
  const gl = gradeLine(t);
  if (gl) lines.push(h("div", { class: t.status === "fail" ? "bad" : "dim" }, gl));
  const dis = disagreement(t);
  if (dis) lines.push(dis);
  if (t.error) lines.push(errorView(t));
  lines.push(cardMeta(t));
  const oc = openInOpenCode(t, job);
  const links = h("div", { class: "links" },
    !oc || oc.tagName === "SPAN" ? null : oc,
    finished(t) ? patchView(t) : null,
    h("a", { href: `#/jobs/${encodeURIComponent(job.id)}`, onclick: () => { state.sel = trialKey(t); } }, "Steps →"));
  // Only strip button styling from top-level links/summaries, preserving inner buttons like Copy!
  for (const b of links.querySelectorAll(":scope > .btn, :scope > details > summary.btn")) {
    b.classList.remove("btn", "blue");
  }
  return h("div", { class: `kcard arm-${t.arm || "kavach"} ${t.status}`, dataset: { key: trialKey(t) } },
    renderArmHintBar(t),
    h("div", { class: "title" }, pill(t), h("b", { title: t.id }, trialLabel(t, multi))),
    lines,
    links);
}

function cardMeta(t) {
  const meta = [
    t.seconds != null ? fmtDur(t.seconds) : t.started && t.status === "running" ? elapsed(t.started, fmtDur) : null,
    t.steps ? `${t.steps} step${t.steps === 1 ? "" : "s"}` : null,
    fresh(t.tokens) ? `${fmtTok(fresh(t.tokens))} tokens` : null,
    t.cost != null ? fmtCost(t.cost) : null,
  ].filter(Boolean);
  return h("div", { class: "dim card-meta" }, meta.flatMap((m, i) => i ? [" · ", m] : [m]));
}

function exportCSV() {
  const rows = state.compare || [];
  const cols = ["task", "model", "arm", "passes", "attempts", "errors", "pending", "median_seconds", "median_fresh_tokens", "median_cached_tokens", "mean_cost"];
  const esc = (v) => v == null ? "" : /[",\n]/.test(String(v)) ? `"${String(v).replace(/"/g, '""')}"` : String(v);
  const csv = [cols.join(","), ...rows.map((r) => cols.map((c) => esc(r[c])).join(","))].join("\n") + "\n";
  const a = h("a", { href: URL.createObjectURL(new Blob([csv], { type: "text/csv" })), download: `${state.job.id}-compare.csv` });
  document.body.append(a);
  a.click();
  a.remove();
  setTimeout(() => URL.revokeObjectURL(a.href), 1000);
}

document.getElementById("menu").addEventListener("click", () => {
  toggleSide(state, narrowScreen());
  saveUIPrefs();
  renderSide();
});
document.getElementById("title").addEventListener("click", toggleNav);
document.addEventListener("keydown", (e) => {
  if (!state.settings.open && current.view === "runs" && replayKey(e)) return;
  if ((e.metaKey || e.ctrlKey) && !e.shiftKey && !e.altKey) {
    const k = e.key.toLowerCase();
    if (k === "b") {
      e.preventDefault();
      toggleSide(state, narrowScreen());
      saveUIPrefs();
      renderSide();
      return;
    }
    if (k === "j" || e.key === "\\") {
      if (current.view === "new" || current.view === "runs") {
        e.preventDefault();
        toggleRightSide();
        return;
      }
    }
  }
  if (e.key === "Escape") {
    if (state.settings.open) {
      closeSettingsModal();
    } else if (state.openSelect) {
      state.openSelect = "";
      state.selectFilter = "";
      renderView();
    } else if (!document.getElementById("nav").hidden) {
      closeNav();
      document.getElementById("title").focus();
    } else if (state.replay && current.view === "runs") {
      closeReplay();
    } else if (state.side && narrowScreen()) {
      state.side = false;
      renderSide();
    }
  }
});
document.addEventListener("click", (e) => {
  if (!e.target.closest(".titlewrap")) closeNav();
  if (state.openSelect && !e.target.closest(".cselect")) {
    state.openSelect = "";
    state.selectFilter = "";
    renderView();
  }
  if (e.target.closest(".side-scroll a") && narrowScreen()) {
    state.side = false;
    renderSide();
  }
});
window.addEventListener("hashchange", onRoute);

// A copy has no server: no status, settings or live events, and its runs
// don't change.
if (!STATIC) {
  if (!TRIAL) {
    loadStatus();
    loadSettings();
    setInterval(loadStatus, 15000);
  }
  connectEvents();
}
loadTasks();
loadJobs();
enterView(() => {
  renderAll();
  if (current.job) return refreshJob();
});

// The trial's "Copy share link" on a run's page.
function shareButton() {
  const b = h("button", { type: "button", class: "btn", title: "Copy a link to this run", onclick: async () => {
    try { await navigator.clipboard.writeText(location.href); b.textContent = "Link copied"; } catch { b.textContent = "Copy failed"; }
    setTimeout(() => { b.textContent = "Copy share link"; }, 1800);
  } }, "Copy share link");
  return b;
}

// trialFoot is the sidebar's foot in the trial: the keys' state, in place of
// the machine's status and Settings.
function trialFoot() {
  const s = trial.session || {};
  return h("div", { class: "side-foot side-static" },
    h("span", {}, s.has_keys ? "Your keys are held for this session." : s.invite ? "Invite accepted." : "No keys yet."),
    h("a", { href: trialReady() ? "#/new" : "#/" }, trialReady() ? "Start another run" : "Set up a trial"));
}
