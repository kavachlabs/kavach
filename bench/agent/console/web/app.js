// The web console of kavach bench: a client of the API in ../API.md, served
// by the same server.

// The wordmark, as [kavach, bench] pairs, one per text line.
const LOGO = [
  ["█  ▄ ▀▀▀█ █  █ ▀▀▀█ █▀▀▀ █   ", "█    ▄▀▀▀ █▀▀▄ █▀▀▀ █   "],
  ["█▄▀  █▀▀█ █  █ █▀▀█ █    █▀▀▄", "█▀▀█ █▀▀▀ █  █ █    █▀▀▄"],
  ["█ ▀▄ ▀▀▀▀  ▀▀  ▀▀▀▀ ▀▀▀▀ ▀  ▀", "▀▀▀▀ ▀▀▀▀ ▀  ▀ ▀▀▀▀ ▀  ▀"],
];

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
    detail: "The agent is equipped with the kavach CLI and MCP server plus the recorded production fixture (Track A: kavach diff with variants) or locked behavior contract (Track B: kavach contract check).",
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

function armInfo(arm) {
  return ARM_INFO[arm] || {
    label: arm || "task",
    tag: "Standard",
    tone: "peach",
    short: "Standard benchmark execution arm",
    tileHint: "Standard benchmark trial execution.",
    detail: "Standard benchmark task execution.",
  };
}

const UI_PREFS_KEY = "kavach-bench.ui-prefs";

function loadUIPrefs() {
  const prefs = { leftW: 256, rightW: 400 };
  try {
    Object.assign(prefs, JSON.parse(localStorage.getItem(UI_PREFS_KEY) || "{}"));
  } catch {}
  return prefs;
}

function saveUIPrefs() {
  try {
    localStorage.setItem(UI_PREFS_KEY, JSON.stringify({ leftW: state.leftW, rightW: state.rightW }));
  } catch {}
}

const initialPrefs = loadUIPrefs();

const state = {
  status: null, statusErr: "",
  tasks: null, tasksErr: "",
  jobs: null, jobsErr: "",
  job: null, jobErr: "", compare: null,
  sel: "", steps: null, stepsFor: "",
  grades: new Map(),
  side: null, // null follows the screen; true or false once toggled
  sideView: "nav", // "nav" | "settings" (Antigravity in-sidebar settings view)
  rightSide: true, // right auxiliary pane open/closed
  auxTab: "task", // "task" | "arms" | "models"
  leftW: initialPrefs.leftW || 256,
  rightW: initialPrefs.rightW || 400,
  live: false,
  form: loadForm(),
  picker: { open: false, q: "", list: null, err: "", previewId: "" },
  modelCatalog: new Map(), // id -> Model object for Model Cards
  modelsLoading: false,
  openSelect: "", // id of currently open custom select menu
  selectFilter: "",
  settings: { open: false, data: null, loading: false, saving: false, err: "", msg: "", drafts: {}, reveal: {} },
  confirm: "", actionErr: "", note: "", noteErr: false, starting: false,
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

function h(tag, attrs, ...kids) {
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

async function api(path, opts) {
  const resp = await fetch("/api" + path, opts);
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
  const form = { task: "", arms: ["kavach", "control"], models: [], runs: 1, agent: "opencode", concurrent: 1, timeout: 0 };
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

setInterval(() => {
  for (const el of document.querySelectorAll("[data-since]")) {
    const s = since(el.dataset.since);
    el.textContent = el.dataset.prefix + (el.dataset.fmt === "clock" ? clock(s) : fmtDur(s));
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
  }
  if (window.matchMedia("(max-width: 900px)").matches) state.side = false;
  renderAll();
  if (current.job) await refreshJob();
}

async function loadStatus() {
  try {
    state.status = await api("/status");
    state.statusErr = "";
  } catch (e) {
    state.statusErr = e.message;
  }
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

function connectEvents() {
  const es = new EventSource("/api/events");
  es.onopen = () => { state.live = true; renderTop(); };
  es.onerror = () => { state.live = false; renderTop(); };
  es.addEventListener("job", (e) => {
    const job = JSON.parse(e.data);
    if (!state.jobs) state.jobs = [];
    const i = state.jobs.findIndex((j) => j.id === job.id);
    if (i >= 0) state.jobs[i] = job;
    else state.jobs.push(job);
    state.jobs.sort((a, b) => (Date.parse(b.started) || 0) - (Date.parse(a.started) || 0));
    renderSide();
    renderTop();
    if (job.id === current.job) scheduleRefresh();
  });
  es.addEventListener("trial", (e) => {
    const t = JSON.parse(e.data);
    if (t.job !== current.job) return;
    if (finished(t)) state.grades.delete(`${t.job}/${t.id}`);
    scheduleRefresh();
  });
}

function statusDot(name, check) {
  if (!check) return null;
  const word = check.ok ? `${name} ${check.version}` : `${name} down`;
  return h("span", { class: "status-pill", title: check.ok ? `${name} ${check.version} ready` : check.error },
    h("span", { class: `dot ${check.ok ? "" : "bad"}` }), word);
}

function renderTop() {
  const right = document.getElementById("right");
  const title = document.getElementById("title-text");
  let text = "New run";
  const kids = [];
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
    if (job.cost) kids.push(h("span", { class: "status-pill" }, fmtCost(job.cost)));
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
  } else {
    if (state.status) {
      kids.push(statusDot("docker", state.status.docker), statusDot("harbor", state.status.harbor));
      if (state.jobs) kids.push(h("span", { class: "wide" }, `${state.jobs.length} run${state.jobs.length === 1 ? "" : "s"} saved`));
    }
  }

  // Antigravity symmetrical right-hand Auxiliary Pane toggle icon on #/new and #/jobs/<id>
  if (current.view === "new" || current.view === "runs") {
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

function toggleRightSide() {
  state.rightSide = !state.rightSide;
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
  const kids = [h("a", { href: "#/new" }, h("span", {}, "+ New benchmark run"), h("span", { class: "dim m" }, "#/new"))];
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

// Keep sidebar open on desktop by default so navigation is rock-solid and Settings is always accessible.
function sideOpen() {
  if (state.side != null) return state.side;
  return !window.matchMedia("(max-width: 900px)").matches;
}

function hasConfiguredKey() {
  if (state.settings.data && state.settings.data.keys) {
    return state.settings.data.keys.some((k) => k.set);
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

function renderSideSettingsView() {
  const keys = defaultKeySpecs();
  const data = state.settings.data;
  const rows = keys.map((k) => {
    const draft = state.settings.drafts[k.name];
    const isCleared = draft === "";
    const shown = Boolean(state.settings.reveal[k.name]);
    return h("div", { class: "side-key-card" },
      h("div", { class: "side-key-top" },
        h("span", { class: "side-key-lbl" }, k.label),
        h("span", { class: `key-badge ${k.set && !isCleared ? "ok" : "warn"}` },
          isCleared ? "Clear" : k.set ? k.masked : "Unset")),
      h("div", { class: "side-key-env m" }, k.name),
      h("div", { class: "side-key-input" },
        h("input", {
          type: shown ? "text" : "password",
          autocomplete: "off",
          spellcheck: "false",
          placeholder: isCleared ? "Clearing on save…" : k.set ? "Paste new key to replace…" : `Paste ${k.name}…`,
          value: draft || "",
          "aria-label": k.label,
          oninput: (e) => {
            const v = e.target.value;
            if (v === "") delete state.settings.drafts[k.name];
            else state.settings.drafts[k.name] = v;
          },
        }),
        h("button", {
          type: "button",
          class: "side-key-mini",
          title: shown ? "Hide key" : "Show key",
          onclick: () => {
            state.settings.reveal[k.name] = !shown;
            renderSide();
          },
        }, shown ? "Hide" : "Show"),
        k.set ? h("button", {
          type: "button",
          class: "side-key-mini danger",
          title: `Clear ${k.name}`,
          onclick: () => {
            state.settings.drafts[k.name] = "";
            renderSide();
          },
        }, "×") : null));
  });

  return [
    h("div", { class: "side-view-bar" },
      h("button", {
        type: "button",
        class: "side-back-btn",
        onclick: () => {
          state.sideView = "nav";
          renderSide();
        },
      }, "← Back"),
      h("span", { class: "side-view-title" }, "Settings & Keys"),
      h("button", {
        type: "button",
        class: "side-expand-btn",
        title: "Open full Settings modal",
        onclick: () => openSettingsModal(),
      }, "↗")),
    h("div", { class: "side-scroll side-settings-scroll" },
      h("div", { class: "side-settings-note" },
        "Saved to ", h("code", {}, data ? data.env_file : "bench/agent/.env"), " (mode 0600)."),
      rows,
      state.settings.err ? h("div", { class: "side-settings-msg bad" }, state.settings.err) : null,
      state.settings.msg ? h("div", { class: "side-settings-msg ok" }, state.settings.msg) : null),
  ];
}

function renderSide() {
  const side = document.getElementById("side");
  const open = sideOpen();
  document.getElementById("main").classList.toggle("noside", !open);
  const menuBtn = document.getElementById("menu");
  if (menuBtn) {
    menuBtn.setAttribute("aria-expanded", String(open));
    menuBtn.classList.toggle("open", open);
    menuBtn.title = `${open ? "Hide" : "Show"} left sidebar (⌘B)`;
  }

  const keyOk = hasConfiguredKey();
  const st = state.status;
  const foot = h("div", { class: "side-foot" },
    state.sideView === "settings"
      ? h("button", {
          type: "button",
          class: "btn peach side-save-btn",
          disabled: state.settings.saving,
          onclick: saveSettingsKeys,
        }, state.settings.saving ? "Saving…" : "Save API keys")
      : null,
    st ? h("div", { class: "side-env-row" },
      h("span", { class: "side-env-items" },
        h("span", { title: st.docker.ok ? `Docker ${st.docker.version}` : st.docker.error },
          h("span", { class: `dot ${st.docker.ok ? "" : "bad"}` }), "docker"),
        h("span", { title: st.harbor.ok ? `Harbor ${st.harbor.version}` : st.harbor.error },
          h("span", { class: `dot ${st.harbor.ok ? "" : "bad"}` }), "harbor")),
      h("span", { class: "m" }, "127.0.0.1:4810")) : null,
    h("button", {
      type: "button",
      class: `side-settings-btn ${state.sideView === "settings" || state.settings.open ? "active" : ""}`,
      title: "Toggle Settings & API Keys sidebar (bench/agent/.env)",
      onclick: () => {
        state.sideView = state.sideView === "settings" ? "nav" : "settings";
        state.settings.msg = "";
        state.settings.err = "";
        if (state.sideView === "settings") loadSettings();
        renderSide();
      },
      "aria-expanded": String(state.sideView === "settings"),
    },
      h("span", { class: "side-settings-left" },
        h("span", { class: "side-settings-ic", "aria-hidden": "true" }, "⚙"),
        h("span", {}, state.sideView === "settings" ? "Back to Runs & Tasks" : "Settings & Keys")),
      h("span", { class: `key-badge ${keyOk ? "ok" : "warn"}` }, keyOk ? "Keys ready" : "Set key")));

  if (state.sideView === "settings") {
    side.replaceChildren(...renderSideSettingsView(), foot, makeResizeSash("left"));
    return;
  }

  const scrollKids = [
    h("a", { href: "#/new", class: `new-run-link ${current.view === "new" ? "on" : ""}` },
      h("span", {}, "+ New benchmark run"),
      h("span", { class: "faint m" }, "/new")),
    h("div", { class: "lbl" }, h("span", {}, "Runs"), state.jobs ? h("span", { class: "faint" }, String(state.jobs.length)) : null),
  ];

  if (state.jobsErr) scrollKids.push(h("div", { class: "none bad" }, "Couldn't list runs: " + state.jobsErr));
  else if (state.jobs && !state.jobs.length) scrollKids.push(h("div", { class: "none" }, "No runs yet."));

  for (const j of state.jobs || []) {
    const { name, shape } = jobShape(j, true);
    const mark = j.status === "running"
      ? h("span", { class: "peach", title: "running" }, "●")
      : h("span", { class: j.status === "done" ? "dim" : "faint", title: j.status }, `${j.status === "paused" ? "paused " : ""}${j.trials_done}/${j.trials_total}`);
    scrollKids.push(h("a", {
      href: `#/jobs/${encodeURIComponent(j.id)}`,
      class: j.id === current.job ? "on" : null,
      title: `${name} (${j.id})`,
    },
      h("span", {}, shape ? `${name} · ${shape}` : name, h("span", { class: "sub" }, j.id)),
      mark));
  }

  scrollKids.push(h("div", { class: "lbl" },
    h("span", {}, "Tasks"),
    state.tasks ? h("span", { class: "faint" }, String(state.tasks.length)) : null));

  if (state.tasks && !state.tasks.length) scrollKids.push(h("div", { class: "none" }, "None found."));
  for (const t of state.tasks || []) {
    const isSelectedTask = current.view === "new" && state.form.task === t.name;
    const armLabel = t.arms.length > 1 ? `${t.arms.length} arms` : t.arms[0].arm;
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
      h("span", {}, t.name),
      h("span", { class: "side-arm-badge" }, armLabel)));
  }

  const prevScroll = side.querySelector(".side-scroll");
  const top = prevScroll ? prevScroll.scrollTop : 0;
  const scrollEl = h("div", { class: "side-scroll" }, scrollKids);
  side.replaceChildren(scrollEl, foot, makeResizeSash("left"));
  scrollEl.scrollTop = top;
}

// Antigravity-style Settings Modal for API Keys (bench/agent/.env)
function openSettingsModal() {
  state.settings.open = true;
  state.settings.msg = "";
  state.settings.err = "";
  state.settings.drafts = {};
  renderSide();
  renderSettingsModal();
  loadSettings();
}

function closeSettingsModal() {
  state.settings.open = false;
  renderSide();
  const existing = document.getElementById("settings-backdrop");
  if (existing) existing.remove();
}

async function saveSettingsKeys(e) {
  if (e) e.preventDefault();
  const drafts = state.settings.drafts;
  const payload = {};
  for (const [k, v] of Object.entries(drafts)) {
    if (v !== undefined) payload[k] = v;
  }
  if (!Object.keys(payload).length) {
    state.settings.msg = "No changes to save.";
    renderSettingsModal();
    renderSide();
    return;
  }
  state.settings.saving = true;
  state.settings.err = "";
  state.settings.msg = "";
  renderSettingsModal();
  renderSide();
  try {
    state.settings.data = await api("/settings", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ keys: payload }),
    });
    state.settings.drafts = {};
    state.settings.msg = "Saved API keys to bench/agent/.env.";
    await loadStatus();
  } catch (err) {
    state.settings.err = err.message;
  } finally {
    state.settings.saving = false;
    renderSettingsModal();
    renderSide();
  }
}

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
      onclick: (e) => { if (e.target === backdrop) closeSettingsModal(); },
    });
    document.body.append(backdrop);
  }

  const data = state.settings.data;
  const keys = (data && data.keys) || [
    { name: "OPENROUTER_API_KEY", label: "OpenRouter API Key", hint: "Used by the OpenCode agent for frontier & open-weight models", set: false, masked: "" },
    { name: "ANTHROPIC_API_KEY", label: "Anthropic API Key", hint: "Direct Anthropic API access for Claude models", set: false, masked: "" },
    { name: "OPENAI_API_KEY", label: "OpenAI API Key", hint: "Direct OpenAI API access for GPT & o-series models", set: false, masked: "" },
    { name: "NVIDIA_API_KEY", label: "NVIDIA NIM API Key", hint: "Hosted Nemotron endpoints on build.nvidia.com", set: false, masked: "" },
    { name: "ZAI_API_KEY", label: "Z.ai API Key", hint: "Direct Z.ai API access for GLM models", set: false, masked: "" },
  ];

  const fields = keys.map((k) => {
    const draft = state.settings.drafts[k.name];
    const isCleared = draft === "";
    const shown = Boolean(state.settings.reveal[k.name]);
    const placeholder = isCleared
      ? "Will be cleared on save…"
      : k.set
        ? `Configured (${k.masked}) — paste new key to replace`
        : `Paste ${k.name}…`;

    return h("div", { class: "key-field" },
      h("div", { class: "key-field-top" },
        h("div", { class: "key-field-title" },
          h("span", { class: "key-field-name" }, k.label),
          h("span", { class: "key-field-env" }, k.name)),
        h("span", { class: `key-badge ${k.set && !isCleared ? "ok" : "warn"}` },
          isCleared ? "Pending clear" : k.set ? `Active · ${k.masked}` : "Not set")),
      h("p", { class: "key-field-hint" }, k.hint),
      h("div", { class: "key-input-row" },
        h("input", {
          type: shown ? "text" : "password",
          autocomplete: "off",
          spellcheck: "false",
          placeholder,
          value: draft || "",
          "aria-label": k.label,
          oninput: (e) => {
            const v = e.target.value;
            if (v === "") delete state.settings.drafts[k.name];
            else state.settings.drafts[k.name] = v;
          },
        }),
        h("button", {
          type: "button",
          class: "btn",
          onclick: () => {
            state.settings.reveal[k.name] = !shown;
            renderSettingsModal();
          },
        }, shown ? "Hide" : "Show"),
        k.set ? h("button", {
          type: "button",
          class: "btn danger",
          title: `Clear ${k.name}`,
          onclick: () => {
            state.settings.drafts[k.name] = "";
            renderSettingsModal();
          },
        }, "Clear") : null));
  });

  const modal = h("form", {
    class: "settings-modal",
    role: "dialog",
    "aria-modal": "true",
    "aria-label": "Settings & API Keys",
    onsubmit: saveSettingsKeys,
  },
    h("div", { class: "settings-head" },
      h("div", {},
        h("h3", {}, "Settings & API Keys"),
        h("p", {}, "Keys are written to ", h("code", {}, data ? data.env_file : "bench/agent/.env"), " (mode 0600, gitignored) and passed to Harbor runs.")),
      h("button", { type: "button", class: "btn", onclick: closeSettingsModal }, "✕")),
    h("div", { class: "settings-body" }, fields),
    h("div", { class: "settings-foot" },
      h("div", {},
        state.settings.err ? h("span", { class: "bad" }, state.settings.err)
          : state.settings.msg ? h("span", { class: "ok" }, state.settings.msg)
          : h("span", { class: "faint" }, "Keys stay on your machine and never enter task containers directly.")),
      h("div", { class: "row" },
        h("button", { type: "button", class: "btn", onclick: closeSettingsModal }, "Close"),
        h("button", { type: "submit", class: "btn peach", disabled: state.settings.saving },
          state.settings.saving ? "Saving…" : "Save API keys"))));

  backdrop.replaceChildren(modal);
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

function renderView() {
  const view = document.getElementById("view");
  if (current.view === "new") return renderNew(view);
  if (state.jobErr) {
    const msg = state.jobErr === "notfound"
      ? [h("b", {}, `No run named ${current.job}.`), " It isn't under ", h("code", {}, state.status ? state.status.jobs_dir : "the jobs directory"), ". ", h("a", { href: "#/new" }, "Start a new run"), "."]
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

// Task Card component rendered in the Right Auxiliary Pane ("Task" tab)
function renderTaskCard(task) {
  if (!task) return null;
  const isFeature = task.track === "feature" || task.arms.some((a) => a.arm === "workflow" || a.arm === "kavach-implement");
  const trackBadge = isFeature ? "Track B · Feature" : "Track A · Incident";
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
        const info = armInfo(a.arm);
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
          h("span", { class: "model-provider" }, provider),
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
    { id: "task", label: "Task", badge: task ? (task.track === "feature" ? "B" : "A") : null },
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

function renderNew(view) {
  const f = state.form;
  const st = state.status;
  if (needsModel(f.agent) && f.models.length && state.modelCatalog.size === 0) {
    ensureModelsLoaded();
  }

  const kids = [
    h("div", { class: "hero-head" },
      h("pre", { class: "logo", "aria-label": "kavach bench" },
        LOGO.map(([k, b], i) => [i ? "\n" : "", h("span", { class: "peach" }, k), " ", h("span", { class: "blue" }, b)])),
      h("p", { class: "hero-sub" }, "Agent benchmark console for incident reproduction & contract-verified feature engineering")),
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
      badge: `${t.arms.length} arms · ${t.track || (t.arms.length > 2 ? "feature" : "incident")}`,
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
        ? h("span", { class: "faint m" }, `${task.track === "feature" ? "Feature" : "Incident"} · ${task.arms.length} arms`)
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
      f.models.map((m) => h("span", { class: "chip on" },
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
      h("div", {}, h("span", { class: "ic", "aria-hidden": "true" }, "▢"), h("span", { class: "row-lbl" }, "Task"), h("span", { class: "val" }, taskRow)),
      h("div", {}, h("span", { class: "ic", "aria-hidden": "true" }, "◑"), h("span", { class: "row-lbl" }, "Arms"), h("span", { class: "val chips" }, armChips)),
      h("div", {}, h("span", { class: "ic", "aria-hidden": "true" }, "◇"), h("span", { class: "row-lbl" }, "Models"), h("div", { class: "val" }, modelVal)),
      h("div", {}, h("span", { class: "ic", "aria-hidden": "true" }, "↻"), h("span", { class: "row-lbl" }, "Runs"), h("span", { class: "val" },
        h("input", {
          class: "num v", type: "number", min: "1", max: "100", value: String(f.runs), "aria-label": "Runs per task and model",
          onchange: (e) => { f.runs = clampInt(e.target.value, 1, 100, 1); saveForm(); renderView(); },
        }),
        h("span", { class: "v" }, "each"),
        h("span", { class: "faint" }, `${n} attempt${n === 1 ? "" : "s"} · ${conc}${f.timeout ? ` · ${f.timeout} min timeout` : ""}`))))));

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
      p.previewId = list[0].id;
    }
  } catch (e) {
    p.err = e.message;
  }
  if (p.open) renderPickerList();
}

function renderPicker() {
  const p = state.picker;
  return h("div", { class: "picker", id: "picker", onkeydown: (e) => { if (e.key === "Escape") { openPicker(null); } } },
    h("div", { class: "picker-top" },
      h("input", {
        id: "pick-q", type: "search", value: p.q, placeholder: "Search OpenRouter models: glm, nemotron, claude, gemini, deepseek…", "aria-label": "Search models",
        oninput: (e) => { p.q = e.target.value; clearTimeout(pickerTimer); pickerTimer = setTimeout(searchModels, 180); },
      }),
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
  const previewId = state.picker.previewId || (state.picker.list && state.picker.list[0] && state.picker.list[0].id);
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
  return h("table", {},
    h("thead", {}, h("tr", {}, h("th", {}, "model"), h("th", {}, "name"), h("th", { class: "num" }, "context"), h("th", { class: "num" }, "in/M"), h("th", { class: "num" }, "out/M"), h("th", {}, "tools"))),
    h("tbody", {}, p.list.slice(0, SHOWN).map((m) => {
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
        cell([have ? h("span", { class: "ok" }, "✓ ") : null, m.id]),
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
  if (state.status && state.status.jobs_warning) return state.status.jobs_warning;
  if (!state.tasks || !state.tasks.length) return "No task to run.";
  if (!chosenTasks().length) return "Choose at least one arm.";
  if (needsModel(f.agent) && !f.models.length) return "Add a model: + Add OpenRouter model, or /model <id>.";
  return "";
}

async function startRun() {
  const f = state.form;
  const why = canRun();
  if (why) return say(why, true);
  const body = { tasks: chosenTasks(), attempts: f.runs, agent: f.agent, concurrent: f.concurrent };
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
      rows.set(key, { label: multi ? `${base(t.task)} · ${who}` : who, trials: [] });
    }
    rows.get(key).trials.push(t);
  }
  return [...rows.values()];
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
  state.sel = trialKey(t);
  state.steps = null;
  state.stepsFor = "";
  state.confirm = "";
  if (!state.rightSide) {
    state.rightSide = true;
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
  if (t.status === "running") return h("span", { class: "pill running" }, t.started ? elapsed(t.started, clock, "running ") : "running");
  const word = { pass: "PASS", fail: "FAIL", error: "ERROR", queued: "queued" }[t.status] || t.status;
  return h("span", { class: `pill ${t.status}` }, word);
}

// Arm Guide / Legend banner explaining what control, workflow, kavach, and kavach-implement mean
function renderArmGuide(arms) {
  const known = arms.filter(Boolean);
  if (!known.length) return null;
  return h("div", { class: "arm-guide", "aria-label": "Experiment arm definitions" },
    known.map((a) => {
      const info = armInfo(a);
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
  const info = armInfo(t.arm);
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
        h("td", {}, r.label),
        arms.map((a) => h("td", {}, h("span", { class: "dots" }, r.trials.filter((t) => t.arm === a).map((t) => h("button", {
          type: "button", class: `${t.status} ${t === sel ? "on" : ""}`, dataset: { key: trialKey(t) },
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

  const left = h("div", { class: "pane" },
    kids,
    renderArmGuide(arms),
    h("div", { class: "tablewrap" }, table),
    h("div", { class: "legend" },
      h("span", {}, h("i", { class: "pass" }), "pass"),
      h("span", {}, h("i", { class: "fail" }), "fail"),
      h("span", {}, h("i", { class: "running" }), "running"),
      h("span", {}, h("i", {}), "queued"),
      h("span", {}, h("i", { class: "error" }), "error"),
      h("span", { class: "faint" }, "grade = task's hidden verifier test suite")),
    h("div", { class: "stats" }, armPasses.join(" · ")));

  const steps = document.querySelector(".steps");
  const keep = steps && {
    top: steps.scrollTop,
    bottom: steps.scrollHeight - steps.scrollTop - steps.clientHeight < 24,
    key: steps.dataset.key,
    open: [...steps.querySelectorAll("details[open]")].map((d) => d.dataset.i),
  };
  view.replaceChildren(h("div", { class: `body ${state.rightSide ? "" : "noright"}` },
    left,
    sel ? renderAttempt(sel) : h("div", { class: "pane right-pane" }, makeResizeSash("right"))));
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

function renderAttempt(t) {
  const job = state.job;
  const multi = isMultiTaskJob(job);
  const info = armInfo(t.arm);
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
  const tl = tokenLine(t);
  if (tl) kids.push(h("div", { class: "stats" }, tl));
  if (t.error) kids.push(h("div", { class: "err-line" }, t.error));
  const gl = gradeLine(t);
  if (gl) kids.push(h("div", { class: t.status === "fail" ? "bad" : "dim" }, gl));

  const cmd = t.kavach ? (isContractTrial(t, t.kavach.verdict) ? "kavach contract check " : "kavach diff ") : "";
  const meta = t.kavach ? [t.kavach.features, t.kavach.variants].filter(Boolean).join(" · ") : "";
  kids.push(h("div", { class: `kcard arm-${t.arm || "kavach"}` },
    renderArmHintBar(t),
    t.kavach ? h("span", {}, h("span", { class: "peach m" }, cmd), h("b", { class: "m" }, t.kavach.verdict)) : h("span", { class: "dim" }, info.short),
    meta ? h("span", { class: "dim" }, meta) : null,
    t.kavach && t.kavach.old ? h("span", { class: "faint m" }, `old build: ${t.kavach.old}`) : null));

  kids.push(renderSteps(t, job));
  if (finished(t)) kids.push(patchView(t));
  kids.push(h("div", { class: "actions" }, openInOpenCode(t, job), runControl(job)));
  return h("div", { class: "pane right-pane", "aria-hidden": String(!state.rightSide) }, kids);
}

const KIND = { shell: "Shell", read: "Read", search: "Grep", edit: "Edit", mcp: "Tool", error: "Error" };

function renderSteps(t, job) {
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
  shown.forEach((s, i) => box.append(renderStep(s, i, i === lastTextIdx)));
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

function renderStep(s, i, isLastText) {
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
      h("div", { class: "step-head" }, h("span", { class: "k" }, "Agent"), toggleBtn, ts),
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
  const verdict = s.kavach && (s.verdict || s.variants) ? h("div", { class: "verdict" },
    s.verdict ? h("span", { class: `pill ${s.passed ? "pass" : s.passed === false ? "fail" : "queued"}` }, s.verdict) : null,
    s.variants ? h("span", { class: "dim" }, s.variants) : null) : null;
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

function patchView(t) {
  const d = h("details", { class: "patch" }, h("summary", {}, "Patch"));
  d.addEventListener("toggle", async () => {
    if (!d.open || d.dataset.loaded) return;
    d.dataset.loaded = "1";
    let body;
    try {
      const resp = await fetch(`/api/trials/${encodeURIComponent(t.job)}/${encodeURIComponent(t.id)}/patch`);
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

function openInOpenCode(t, job) {
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
      h("div", { class: "cmd" }, h("span", {}, "This attempt has finished. Open its saved session in OpenCode's terminal UI:"), h("pre", {}, cmd), h("span", {}, copy)));
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
  if (job.resumable) {
    const warning = state.status && state.status.jobs_warning;
    return h("span", { class: "note" },
      h("button", { type: "button", class: "btn blue", disabled: !!warning, onclick: () => jobAction("resume") }, "Resume run"),
      " ", job.status === "paused" ? "Paused." : "Stopped.", " Resuming keeps the finished attempts and runs the rest.",
      state.actionErr ? h("span", { class: "bad" }, " ", state.actionErr) : null);
  }
  if (job.status === "pausing") return h("span", { class: "note" }, "Pausing: Harbor is cancelling the attempts in progress.");
  if (job.status !== "running") return null;
  if (!job.stoppable) {
    return h("span", { class: "note" },
      h("button", { type: "button", class: "btn", disabled: true }, "Pause run"), " ",
      h("button", { type: "button", class: "btn", disabled: true }, "Stop run"), " Another process started this run: pause or stop it there.");
  }
  const running = job.trials.filter((t) => t.status === "running").length;
  const queued = job.trials.filter((t) => t.status === "queued").length;
  const ask = (what) => { state.confirm = what; renderView(); document.getElementById("confirm-yes").focus(); };
  if (!state.confirm) {
    return [
      h("button", { type: "button", class: "btn", onclick: () => running ? ask("pause") : jobAction("pause") }, running ? "Pause run…" : "Pause run"), " ",
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
  kids.push(h("h2", { class: "h" }, `Pass rate comparison · ${arms.join(" → ")}`));
  kids.push(renderArmGuide(arms));

  if (!groups.length) {
    kids.push(h("div", { class: "dim" }, job.status === "running" ? "No attempt has finished yet." : "This run has no attempts."));
  } else {
    const rate = (r) => r && r.attempts ? r.passes / r.attempts : null;
    const cell = (r, better) => {
      if (!r) return h("td", { class: "dim" }, "–");
      const extra = [r.errors ? `${r.errors} error${r.errors > 1 ? "s" : ""}` : "", r.pending ? `${r.pending} pending` : ""].filter(Boolean).join(" · ");
      return h("td", { class: better ? "b peach" : null }, r.attempts ? `${r.passes} / ${r.attempts}` : "–", extra ? h("span", { class: "sub" }, extra) : null);
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
          h("td", {}, multi ? `${g.task} · ${who}` : who),
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
  const attempts = job.trials.filter((t) => t.id);
  const queued = job.trials.length - attempts.length;
  kids.push(h("div", { class: "row", style: "margin-top:8px" }, h("h2", { class: "h" }, "Attempt Tiles"), queued ? h("span", { class: "dim" }, `${queued} queued`) : null));
  if (!attempts.length) kids.push(h("div", { class: "dim" }, "None has started yet."));
  else kids.push(h("div", { class: "cards" }, attempts.map((t) => attemptCard(t, job, multi))));
  view.replaceChildren(h("div", { class: "pane" }, kids));
}

function attemptCard(t, job, multi = false) {
  const lines = [];
  const kl = kavachLine(t);
  if (kl) lines.push(h("div", { class: "dim m" }, kl + (t.kavach && t.kavach.variants ? ` · ${t.kavach.variants}` : "")));
  const gl = gradeLine(t);
  if (gl) lines.push(h("div", { class: t.status === "fail" ? "bad" : "dim" }, gl));
  if (t.error) lines.push(h("div", { class: "err-line" }, t.error));
  const meta = [
    t.seconds != null ? fmtDur(t.seconds) : t.started && t.status === "running" ? elapsed(t.started, fmtDur) : null,
    t.steps ? `${t.steps} step${t.steps === 1 ? "" : "s"}` : null,
    fresh(t.tokens) ? `${fmtTok(fresh(t.tokens))} tokens` : null,
    t.cost != null ? fmtCost(t.cost) : null,
  ].filter(Boolean);
  if (meta.length) lines.push(h("div", { class: "dim" }, meta.flatMap((m, i) => i ? [" · ", m] : [m])));
  const oc = openInOpenCode(t, job);
  const links = h("div", { class: "links" },
    oc.tagName === "SPAN" ? null : oc,
    finished(t) ? patchView(t) : null,
    h("a", { href: `#/jobs/${encodeURIComponent(job.id)}`, onclick: () => { state.sel = trialKey(t); } }, "Steps →"));
  // Only strip button styling from top-level links/summaries, preserving inner buttons like Copy!
  for (const b of links.querySelectorAll(":scope > .btn, :scope > details > summary.btn")) {
    b.classList.remove("btn", "blue");
  }
  return h("div", { class: `kcard arm-${t.arm || "kavach"} ${t.status}` },
    renderArmHintBar(t),
    h("div", { class: "title" }, pill(t), h("b", { title: t.id }, trialLabel(t, multi))),
    lines,
    links);
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
  state.side = !sideOpen();
  renderSide();
});
document.getElementById("title").addEventListener("click", toggleNav);
document.addEventListener("keydown", (e) => {
  if ((e.metaKey || e.ctrlKey) && !e.shiftKey && !e.altKey) {
    const k = e.key.toLowerCase();
    if (k === "b") {
      e.preventDefault();
      state.side = !sideOpen();
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
    } else if (state.side && window.matchMedia("(max-width: 900px)").matches) {
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
  if (e.target.closest(".side-scroll a") && window.matchMedia("(max-width: 900px)").matches) {
    state.side = false;
    renderSide();
  }
});
window.addEventListener("hashchange", onRoute);

renderAll();
loadStatus();
loadSettings();
loadTasks();
loadJobs();
connectEvents();
setInterval(loadStatus, 15000);
if (current.job) refreshJob();
