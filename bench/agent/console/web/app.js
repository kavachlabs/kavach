// The web console of kavach bench: a client of the API in ../API.md, served
// by the same server.

// [kavach, bench] per line, from the approved mock-up.
const LOGO = [["█  ▄ ▀▀▀█ █  █ ▀▀▀█ █▀▀▀ █   ", "█    ▄▀▀▀ █▀▀▄ █▀▀▀ █   "], ["█▄▀  █▀▀█ █  █ █▀▀█ █    █▀▀▄", "█▀▀█ █▀▀▀ █  █ █    █▀▀▄"], ["█ ▀▄ ▀▀▀▀  ▀▀  ▀▀▀▀ ▀▀▀▀ ▀  ▀", "▀▀▀▀ ▀▀▀▀ ▀  ▀ ▀▀▀▀ ▀  ▀"]];

const COMMANDS = [
  ["/run", "start the run"],
  ["/model <id>", "add a model by its OpenRouter id"],
  ["/models [search]", "pick models from OpenRouter"],
  ["/arm kavach|control|both", "choose the arms"],
  ["/runs <n>", "attempts per task and model"],
  ["/timeout <minutes>|default", "override the tasks' agent timeout"],
  ["/agent opencode|oracle|nop", "choose the Harbor agent"],
  ["/task <name>", "choose the task"],
  ["/matrix", "open the latest run"],
  ["/compare", "open the latest run's results"],
];
const AGENTS = { opencode: "OpenCode", oracle: "Oracle", nop: "Nop" };
const ARMS = ["control", "kavach"];
const SETTINGS_KEY = "kavach-bench.new-run";

const state = {
  status: null, statusErr: "",
  tasks: null, tasksErr: "",
  jobs: null, jobsErr: "",
  job: null, jobErr: "", compare: null,
  sel: "", steps: null, stepsFor: "",
  grades: new Map(),
  side: null, // null follows the screen; true or false once toggled
  live: false,
  form: loadForm(),
  picker: { open: false, q: "", list: null, err: "" },
  confirmStop: false, stopErr: "", note: "", noteErr: false, starting: false,
};

// --- small helpers

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

const base = (task) => task.replace(/-(control|kavach)$/, "");
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

function arrow(a, b, fmt) {
  if (a == null && b == null) return "–";
  return `${fmt(a)} → ${fmt(b)}`;
}

// --- job shape

function jobShape(job, compact) {
  const bases = [...new Set(job.tasks.map(base))];
  const arms = new Set(job.tasks.map((t) => (t.match(/-(control|kavach)$/) || [])[1] || "")).size;
  const models = job.models.length;
  const per = job.tasks.length * Math.max(models, 1);
  const runs = per ? Math.round(job.trials_total / per) : 0;
  const name = bases.join(", ") || job.id;
  if (!job.tasks.length) return { name, shape: "" };
  const who = models ? (compact ? `${models}` : `${models} model${models > 1 ? "s" : ""}`) : job.agent;
  const shape = compact ? `${who}×${arms}×${runs}` : `${who} × ${arms} arm${arms > 1 ? "s" : ""} × ${runs}`;
  return { name, shape };
}

const jobTitle = (job) => {
  const { name, shape } = jobShape(job, false);
  return shape ? `${name} · ${shape}` : name;
};

// --- routing

function route() {
  const parts = location.hash.replace(/^#\/?/, "").split("/").map(decodeURIComponent);
  if (parts[0] === "jobs" && parts[1]) return { view: parts[2] === "compare" ? "compare" : "runs", job: parts[1] };
  return { view: "new", task: parts[0] === "new" ? parts[1] || "" : "" };
}

let current = route();

async function onRoute() {
  const prev = current;
  current = route();
  state.confirmStop = false;
  state.stopErr = "";
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

// --- data

async function loadStatus() {
  try {
    state.status = await api("/status");
    state.statusErr = "";
  } catch (e) {
    state.statusErr = e.message;
  }
  renderTop();
  if (current.view === "new") renderView();
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
    job.trials = job.trials || []; // omitted while Harbor has written no trial
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

// --- top bar, nav and sidebar

function statusDot(name, check) {
  if (!check) return null;
  const word = check.ok ? `${name} ${check.version}` : `${name} down`;
  return h("span", { title: check.ok ? null : check.error }, h("span", { class: `dot ${check.ok ? "" : "bad"}` }), word);
}

function renderTop() {
  const right = document.getElementById("right");
  const title = document.getElementById("title-text");
  let text = "New run";
  const kids = [];
  if (!state.live && state.statusErr) {
    kids.push(h("span", { class: "bad" }, h("span", { class: "dot bad" }), "server unreachable"));
  }
  const job = current.job && (state.job || (state.jobs || []).find((j) => j.id === current.job));
  if (current.view !== "new" && job) {
    text = jobTitle(job);
    const dot = job.status === "running" ? "run" : job.status === "stopped" ? "off" : "";
    kids.push(h("span", {}, h("span", { class: `dot ${dot}` }), job.status === "stopped" ? `stopped · ${job.trials_done} of ${job.trials_total}` : `${job.trials_done} of ${job.trials_total} done`));
    if (job.cost) kids.push(h("span", {}, fmtCost(job.cost)));
    if (job.started) {
      kids.push(h("span", { class: "wide" }, job.finished ? fmtDur((Date.parse(job.finished) - Date.parse(job.started)) / 1000) : job.status === "running" ? elapsed(job.started, fmtDur) : "", job.status === "running" ? " elapsed" : ""));
    }
    const enc = encodeURIComponent(job.id);
    if (current.view === "runs") kids.push(h("a", { class: "btn", href: `#/jobs/${enc}/compare` }, "Results"));
    else {
      kids.push(h("a", { class: "btn wide", href: `#/jobs/${enc}` }, "Matrix"));
      kids.push(h("button", { class: "btn", type: "button", onclick: exportCSV, disabled: !state.compare }, "Export CSV"));
    }
  } else if (current.view !== "new") {
    text = current.job;
  } else if (state.status) {
    kids.push(statusDot("docker", state.status.docker), statusDot("harbor", state.status.harbor));
    if (state.jobs) kids.push(h("span", { class: "wide" }, `${state.jobs.length} run${state.jobs.length === 1 ? "" : "s"} saved`));
  }
  title.textContent = text;
  document.title = current.view === "new" ? "kavach bench" : `${text} · kavach bench`;
  right.replaceChildren(...kids.filter(Boolean));
}

function renderNav() {
  const nav = document.getElementById("nav");
  const kids = [h("a", { href: "#/new" }, "New run")];
  if (current.job) {
    const enc = encodeURIComponent(current.job);
    kids.push(h("a", { href: `#/jobs/${enc}` }, "Matrix", h("span", { class: "dim" }, current.job)));
    kids.push(h("a", { href: `#/jobs/${enc}/compare` }, "Results", h("span", { class: "dim" }, current.job)));
  }
  const jobs = (state.jobs || []).slice(0, 8);
  if (jobs.length) kids.push(h("div", { class: "lbl" }, "Recent runs"));
  for (const j of jobs) kids.push(h("a", { href: `#/jobs/${encodeURIComponent(j.id)}` }, jobShape(j, true).name, h("span", { class: "dim" }, j.id)));
  nav.replaceChildren(...kids);
}

function toggleNav() {
  const nav = document.getElementById("nav");
  const open = nav.hidden;
  if (open) renderNav();
  nav.hidden = !open;
  document.getElementById("title").setAttribute("aria-expanded", String(open));
  if (open) nav.querySelector("a").focus();
}

function closeNav() {
  document.getElementById("nav").hidden = true;
  document.getElementById("title").setAttribute("aria-expanded", "false");
}

function sideOpen() {
  if (state.side != null) return state.side;
  return current.view === "runs" && !window.matchMedia("(max-width: 900px)").matches;
}

function renderSide() {
  const side = document.getElementById("side");
  document.getElementById("main").classList.toggle("noside", !sideOpen());
  const kids = [h("div", { class: "lbl" }, "Runs"), h("a", { href: "#/new", class: current.view === "new" ? "on" : null }, h("span", {}, "+ New run"))];
  if (state.jobsErr) kids.push(h("div", { class: "none bad" }, "Couldn't list runs: " + state.jobsErr));
  else if (state.jobs && !state.jobs.length) kids.push(h("div", { class: "none" }, "No runs yet."));
  for (const j of state.jobs || []) {
    const { name, shape } = jobShape(j, true);
    const mark = j.status === "running" ? h("span", { class: "peach", title: "running" }, "●")
      : h("span", { class: j.status === "stopped" ? "faint" : "", title: j.status }, `${j.trials_done}/${j.trials_total}`);
    kids.push(h("a", { href: `#/jobs/${encodeURIComponent(j.id)}`, class: j.id === current.job ? "on" : null },
      h("span", {}, shape ? `${name} · ${shape}` : name, h("span", { class: "sub" }, j.id)), mark));
  }
  kids.push(h("div", { class: "lbl" }, "Tasks"));
  if (state.tasks && !state.tasks.length) kids.push(h("div", { class: "none" }, "None found."));
  for (const t of state.tasks || []) {
    kids.push(h("a", { href: `#/new/${encodeURIComponent(t.name)}`, title: t.title }, h("span", {}, t.name), h("span", { class: "faint" }, t.arms.length > 1 ? `${t.arms.length} arms` : t.arms[0].arm)));
  }
  side.replaceChildren(...kids);
}

// --- views

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

function renderNew(view) {
  const f = state.form;
  const st = state.status;
  const kids = [h("pre", { class: "logo", "aria-label": "kavach bench" }, LOGO.map(([k, b], i) => [i ? "\n" : "", h("span", { class: "peach" }, k), " ", h("span", { class: "blue" }, b)]))];

  if (state.statusErr && !st) {
    kids.push(banner("Can't reach the bench server.", `${state.statusErr}\nStart it with: bench web`));
  }
  if (st && !st.docker.ok) kids.push(banner(["Docker isn't running: start Colima with ", h("code", {}, "colima start"), "."], st.docker.error));
  if (st && !st.harbor.ok) kids.push(banner(["Harbor didn't answer ", h("code", {}, "harbor --version"), ". Is it installed and on PATH?"], st.harbor.error));
  if (st && !st.env_file && needsModel(f.agent)) {
    kids.push(banner(["No ", h("code", {}, "bench/agent/.env"), ": OpenRouter runs need ", h("code", {}, "OPENROUTER_API_KEY"), " there. Oracle and nop runs don't."], "", true));
  }

  const tasks = state.tasks || [];
  if (state.tasks && !f.task && tasks.length) f.task = tasks[0].name;
  const task = tasks.find((t) => t.name === f.task);
  let taskRow;
  if (state.tasksErr) taskRow = h("span", { class: "bad" }, "Couldn't list tasks: " + state.tasksErr);
  else if (!state.tasks) taskRow = h("span", {}, "Loading…");
  else if (!tasks.length) taskRow = h("span", {}, "No tasks found under ", h("code", {}, st ? st.tasks_dir : "bench/agent/harbor/tasks"), ".");
  else {
    taskRow = [
      h("select", { class: "plain v", "aria-label": "Task", onchange: (e) => { f.task = e.target.value; saveForm(); renderView(); } },
        tasks.map((t) => h("option", { value: t.name, selected: t.name === f.task }, t.name))),
      task ? h("span", { class: "faint" }, task.title.replace(/^Task:\s*/, "")) : h("span", { class: "bad" }, `${f.task} isn't a task any more`),
    ];
  }

  const taskArms = task ? task.arms.map((a) => a.arm) : ARMS;
  const armChips = ["kavach", "control"].filter((a) => taskArms.includes(a)).map((a) => {
    const on = f.arms.includes(a);
    return h("button", { type: "button", class: `chip ${on ? "on" : ""} ${a === "control" ? "blue" : ""}`, "aria-pressed": String(on), onclick: () => toggleArm(a) }, a);
  });

  let modelVal;
  if (!needsModel(f.agent)) {
    modelVal = h("span", { class: "faint" }, `The ${f.agent} agent needs no model.`);
  } else {
    modelVal = h("span", { class: "chips" },
      f.models.map((m) => h("span", { class: "chip" }, m, h("button", { type: "button", class: "x", "aria-label": `Remove ${m}`, onclick: () => { f.models = f.models.filter((x) => x !== m); saveForm(); renderView(); } }, "×"))),
      h("button", { type: "button", class: "chip dim", "aria-expanded": String(state.picker.open), onclick: () => openPicker(state.picker.open ? null : "") }, "+ add"));
  }

  const n = attemptsCount();
  const conc = f.concurrent > 1 ? `${f.concurrent} at once` : "one at a time";
  kids.push(h("div", { class: "meta" },
    h("div", {}, h("span", { class: "ic", "aria-hidden": "true" }, "▢"), h("span", {}, "Task"), h("span", { class: "val" }, taskRow)),
    h("div", {}, h("span", { class: "ic", "aria-hidden": "true" }, "◑"), h("span", {}, "Arms"), h("span", { class: "val chips" }, armChips)),
    h("div", {}, h("span", { class: "ic", "aria-hidden": "true" }, "◇"), h("span", {}, "Models"), h("span", { class: "val" }, modelVal)),
    h("div", {}, h("span", { class: "ic", "aria-hidden": "true" }, "↻"), h("span", {}, "Runs"), h("span", { class: "val" },
      h("input", { class: "num v", type: "number", min: "1", max: "100", value: String(f.runs), "aria-label": "Runs per task and model",
        onchange: (e) => { f.runs = clampInt(e.target.value, 1, 100, 1); saveForm(); renderView(); } }),
      h("span", { class: "v" }, "each"),
      h("span", { class: "faint" }, `${n} attempt${n === 1 ? "" : "s"} · ${conc}${f.timeout ? ` · ${f.timeout} min timeout` : ""}`))),
  ));
  if (state.picker.open && needsModel(f.agent)) kids.push(renderPicker());

  kids.push(renderPrompt());
  if (state.note) kids.push(h("div", { class: `note ${state.noteErr ? "err" : ""}`, role: "status" }, state.note));

  const center = h("div", { class: "center" }, kids);
  const focused = document.activeElement && document.activeElement.id;
  const caret = focused === "ask" ? document.activeElement.selectionStart : null;
  view.replaceChildren(center);
  if (focused) {
    const el = document.getElementById(focused);
    if (el) {
      el.focus();
      if (caret != null) el.setSelectionRange(caret, caret);
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
    placeholder: 'Type / for commands… "/models nemotron"', "aria-label": "Command",
    oninput: () => renderCmds(), onkeydown: onAskKey,
  });
  const agentSel = h("select", { "aria-label": "Agent", onchange: (e) => { f.agent = e.target.value; saveForm(); renderView(); } },
    Object.entries(AGENTS).map(([v, label]) => h("option", { value: v, selected: v === f.agent }, label)));
  const concSel = h("select", { "aria-label": "Attempts at once", onchange: (e) => { f.concurrent = +e.target.value; saveForm(); renderView(); } },
    [1, 2, 4, 8].map((c) => h("option", { value: c, selected: c === f.concurrent }, c === 1 ? "one at a time" : `${c} at once`)));
  const timeouts = [...new Set([0, 15, 30, 60, 120, f.timeout])].sort((a, b) => a - b);
  const timeSel = h("select", { "aria-label": "Agent timeout", onchange: (e) => { f.timeout = +e.target.value; saveForm(); renderView(); } },
    timeouts.map((m) => h("option", { value: m, selected: m === f.timeout }, m ? `${m} min timeout` : "task's timeout")));
  const bar = h("div", { class: "bar" }, agentSel,
    needsModel(f.agent) ? h("button", { type: "button", class: "sel", onclick: () => openPicker(state.picker.open ? null : "") }, `${f.models.length} model${f.models.length === 1 ? "" : "s"}`) : null,
    concSel, timeSel,
    needsModel(f.agent) ? h("span", { class: "dim" }, "OpenRouter") : null,
    h("button", { type: "button", class: `go ${ready ? "on" : ""}`, "aria-label": "Start the run", title: ready ? "Start the run" : canRun(), disabled: state.starting, onclick: startRun }, "▶"));
  return h("div", { class: "prompt" }, h("div", { class: "cmds", id: "cmds", hidden: true, role: "listbox" }), input, bar);
}

let cmdIndex = 0;
function matchingCmds() {
  const v = document.getElementById("ask").value;
  if (!v.startsWith("/") || v.includes(" ")) return [];
  return COMMANDS.filter(([c]) => c.startsWith(v));
}

function renderCmds() {
  const box = document.getElementById("cmds");
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
    case "/model": {
      if (!arg) return say("Usage: /model <OpenRouter id>, e.g. /model z-ai/glm-5.3", true);
      addModel(orId(arg));
      return say(`Added ${orId(arg)}.`);
    }
    case "/models":
      return openPicker(arg);
    case "/arm": {
      const arms = arg === "both" ? ["kavach", "control"] : ARMS.includes(arg) ? [arg] : null;
      if (!arms) return say("Usage: /arm kavach, /arm control or /arm both", true);
      f.arms = arms;
      saveForm();
      return say(`Arms: ${arms.join(" and ")}.`);
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
      return say(`Agent: ${AGENTS[arg]}.`);
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
  saveForm();
  renderView();
}

// --- model picker

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
    if (p.q !== q) return;
    p.list = list;
    p.err = "";
  } catch (e) {
    p.err = e.message;
  }
  if (p.open) renderPickerList();
}

function renderPicker() {
  const p = state.picker;
  return h("div", { class: "picker", id: "picker", onkeydown: (e) => { if (e.key === "Escape") { openPicker(null); } } },
    h("input", {
      id: "pick-q", type: "search", value: p.q, placeholder: "Search OpenRouter models: glm, nemotron, sonnet…", "aria-label": "Search models",
      oninput: (e) => { p.q = e.target.value; clearTimeout(pickerTimer); pickerTimer = setTimeout(searchModels, 200); },
    }),
    h("div", { class: "list", id: "pick-list" }, pickerRows()),
    h("div", { class: "foot", id: "pick-foot" }, pickerFoot()));
}

function renderPickerList() {
  const list = document.getElementById("pick-list");
  if (!list) return;
  list.replaceChildren(pickerRows());
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
    h("thead", {}, h("tr", {}, h("th", {}, "model"), h("th", { class: "num" }, "context"), h("th", { class: "num" }, "in/M"), h("th", { class: "num" }, "out/M"), h("th", {}, "tools"))),
    h("tbody", {}, p.list.slice(0, SHOWN).map((m) => {
      const have = state.form.models.includes(m.id);
      const add = () => { addModel(m.id); openPicker(p.q); };
      const cell = (text, cls) => h("td", { class: cls }, h("button", { type: "button", tabindex: cls ? "-1" : null, title: m.name, onclick: add }, text));
      return h("tr", {},
        cell([have ? h("span", { class: "ok" }, "✓ ") : null, m.id]),
        cell(m.context_length ? fmtTok(m.context_length) : "–", "num"),
        cell(price(m.price_in), "num"), cell(price(m.price_out), "num"),
        cell(m.tools ? "yes" : h("span", { class: "bad" }, "no"), "tl"));
    })));
}

function pickerFoot() {
  const p = state.picker;
  if (!p.list) return "";
  const more = p.list.length > SHOWN ? ` · showing ${SHOWN} of ${p.list.length}; search to narrow` : "";
  return `Prices in USD per million tokens. The agent needs tool calls.${more} · Esc closes`;
}

// --- start

function canRun() {
  const f = state.form;
  if (!state.tasks || !state.tasks.length) return "No task to run.";
  if (!chosenTasks().length) return "Choose at least one arm.";
  if (needsModel(f.agent) && !f.models.length) return "Add a model: + add, or /model <id>.";
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

// --- runs: matrix and attempt

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
  state.confirmStop = false;
  renderView();
  const focus = document.querySelector(`.dots button[data-key="${CSS.escape(state.sel)}"]`);
  if (focus) focus.focus();
  if (t.id) await loadSteps(t);
  if (finished(t)) await loadGrades([t]);
  if (state.sel === trialKey(t)) renderView();
}

function trialLabel(t) {
  return `${t.model ? shortModel(t.model) : t.task.replace(/-(control|kavach)$/, "")} · ${t.arm || "task"} · #${t.attempt}`;
}

function pill(t) {
  if (t.status === "running") return h("span", { class: "pill running" }, t.started ? elapsed(t.started, clock, "running ") : "running");
  const word = { pass: "PASS", fail: "FAIL", error: "ERROR", queued: "queued" }[t.status] || t.status;
  return h("span", { class: `pill ${t.status}` }, word);
}

function renderRuns(view) {
  const job = state.job;
  const kids = [];
  if (job.error) kids.push(banner("Harbor exited before it wrote the run's folder.", job.error));
  if (!job.trials.length) {
    kids.push(h("div", { class: "empty" }, job.status === "running"
      ? [h("b", {}, "Harbor is starting the run."), " Building a task's image the first time takes minutes; attempts appear here as they start."]
      : "This run has no attempts."));
    kids.push(h("div", { class: "actions" }, stopControl(job)));
    return view.replaceChildren(h("div", { class: "pane" }, kids));
  }
  const arms = armsOf(job.trials);
  const sel = selected();
  const rows = matrixRows(job);
  const table = h("table", { class: "mx" },
    h("thead", {}, h("tr", {}, h("th", {}, job.models.length ? "model" : "agent"), arms.map((a) => h("th", {}, a || "attempts")), h("th", {}, "pass rate"))),
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
          "aria-label": `${trialLabel(t)}: ${t.status}`, title: `${trialLabel(t)}: ${t.status}`, "aria-pressed": String(t === sel),
          onclick: () => select(t),
        }))))),
        h("td", { class: allQueued ? "dim" : null }, allQueued ? "queued" : rates.join(" · ")));
    })));
  const counts = { pass: 0, fail: 0 };
  for (const t of job.trials) if (t.status in counts) counts[t.status]++;
  const armPasses = arms.map((a) => {
    const ts = job.trials.filter((t) => t.arm === a);
    const graded = ts.filter((t) => t.status === "pass" || t.status === "fail").length;
    return graded ? `${a || "all"} ${ts.filter((t) => t.status === "pass").length} of ${graded} passed` : `${a || "all"} none graded yet`;
  });
  const left = h("div", { class: "pane" }, kids, h("div", { class: "tablewrap" }, table),
    h("div", { class: "legend" },
      h("span", {}, h("i", { class: "pass" }), "pass"), h("span", {}, h("i", { class: "fail" }), "fail"),
      h("span", {}, h("i", { class: "running" }), "running"), h("span", {}, h("i", {}), "queued"),
      h("span", {}, h("i", { class: "error" }), "error"),
      h("span", { class: "faint" }, "the grade is the task's hidden test")),
    h("div", { class: "stats" }, armPasses.join(" · ")));

  const steps = document.querySelector(".steps");
  const keep = steps && { top: steps.scrollTop, bottom: steps.scrollHeight - steps.scrollTop - steps.clientHeight < 24, key: steps.dataset.key, open: [...steps.querySelectorAll("details[open]")].map((d) => d.dataset.i) };
  view.replaceChildren(h("div", { class: "body" }, left, sel ? renderAttempt(sel) : h("div", { class: "pane" })));
  const now = document.querySelector(".steps");
  if (now) {
    if (keep && keep.key === now.dataset.key) {
      for (const i of keep.open) {
        const d = now.querySelector(`details[data-i="${i}"]`);
        if (d) d.open = true;
      }
      now.scrollTop = keep.bottom ? now.scrollHeight : keep.top;
    } else {
      now.scrollTop = now.scrollHeight;
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

function kavachLine(t) {
  const g = state.grades.get(`${t.job}/${t.id}`);
  const verdict = t.kavach ? t.kavach.verdict : g && g.grade && g.grade.kavach_diff;
  if (!verdict) return "";
  return `kavach diff ${verdict}${t.arm === "control" ? " · checked after the run" : ""}`;
}

function renderAttempt(t) {
  const job = state.job;
  const kids = [h("div", { class: "row" }, h("span", { class: "h" }, trialLabel(t)), pill(t))];
  const tl = tokenLine(t);
  if (tl) kids.push(h("div", { class: "stats" }, tl));
  if (t.error) kids.push(h("div", { class: "err-line" }, t.error));
  const gl = gradeLine(t);
  if (gl) kids.push(h("div", { class: t.status === "fail" ? "bad" : "dim" }, gl));
  if (t.kavach) {
    kids.push(h("div", { class: "kcard" },
      h("span", {}, h("span", { class: "peach" }, "kavach diff "), h("b", {}, t.kavach.verdict)),
      t.kavach.variants ? h("span", { class: "dim" }, t.kavach.variants) : null,
      t.kavach.old ? h("span", { class: "faint" }, `old build: ${t.kavach.old}`) : null));
  }
  kids.push(renderSteps(t, job));
  if (finished(t)) kids.push(patchView(t));
  kids.push(h("div", { class: "actions" }, openInOpenCode(t, job), stopControl(job)));
  return h("div", { class: "pane" }, kids);
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
  shown.forEach((s, i) => box.append(renderStep(s, i)));
  return box;
}

function renderStep(s, i) {
  const ts = h("span", { class: "ts" }, clock(s.t_ms / 1000));
  if (s.kind === "text") {
    return h("div", { class: "step text" }, h("p", { class: "say", title: "Click to expand", onclick: (e) => e.currentTarget.classList.toggle("open") }, s.title));
  }
  let label = KIND[s.kind] || s.kind;
  let title = [s.title];
  if (s.kind === "read") {
    const [path, range] = s.title.split(" lines ");
    title = [path, range ? h("span", { class: "faint" }, ` ${range}`) : null];
  }
  if (s.kind === "shell") title = [h("span", { class: "m" }, s.title), s.exit ? h("span", { class: "x" }, `exit ${s.exit}`) : null];
  if (s.kavach) label = "Kavach";
  const cls = `step ${s.kind}${s.kavach ? " kavach" : ""}`;
  const head = [h("span", { class: "k" }, label), h("span", { class: "t" }, title), ts];
  const verdict = s.kavach && (s.verdict || s.variants) ? h("div", { class: "verdict" },
    s.verdict ? h("span", { class: `pill ${s.passed ? "pass" : s.passed === false ? "fail" : "queued"}` }, s.verdict) : null,
    s.variants ? h("span", { class: "dim" }, s.variants) : null) : null;
  if (!s.detail) return h("div", { class: cls }, h("div", { class: "line" }, head), verdict);
  const body = s.kind === "edit" ? diffView(s.detail) : h("pre", {}, s.detail);
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
    return h("a", { class: "btn blue", href: oc.url, target: "_blank", rel: "noopener" }, "Open in OpenCode ↗");
  }
  if (oc && oc.export) {
    const cmd = `opencode import ${shq(oc.export)}\nopencode --session ${shq(oc.session)}`;
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
      h("div", { class: "cmd" }, h("span", {}, "This attempt's OpenCode server has stopped. Import the saved session, then open it:"), h("pre", {}, cmd), h("span", {}, copy)));
  }
  let why;
  if (t.status === "queued") why = "Not started yet.";
  else if (!oc && job.agent !== "opencode") why = `The ${job.agent} agent has no OpenCode session.`;
  else if (!oc && t.status === "running") why = "OpenCode hasn't started its server yet.";
  else if (!oc) why = "No OpenCode session was recorded for this attempt.";
  else why = "OpenCode's server has stopped and no session export was saved.";
  return h("span", { class: "note" }, h("button", { type: "button", class: "btn", disabled: true }, "Open in OpenCode ↗"), " ", why);
}

function stopControl(job) {
  if (job.status !== "running") return null;
  if (!job.stoppable) {
    return h("span", { class: "note" }, h("button", { type: "button", class: "btn", disabled: true }, "Stop run"), " Another process started this run: stop it there.");
  }
  if (!state.confirmStop) {
    return h("button", { type: "button", class: "btn", onclick: () => { state.confirmStop = true; renderView(); document.getElementById("stop-yes").focus(); } }, "Stop run…");
  }
  const running = job.trials.filter((t) => t.status === "running").length;
  const queued = job.trials.filter((t) => t.status === "queued").length;
  return h("div", { class: "confirm", role: "alertdialog", "aria-label": "Stop the run", onkeydown: (e) => { if (e.key === "Escape") { state.confirmStop = false; renderView(); } } },
    job.trials.length
      ? h("span", {}, h("b", {}, "Stop the whole run, not just this attempt? "),
        `Harbor can only stop a run as a whole: it cancels ${running} running and ${queued} queued attempt${running + queued === 1 ? "" : "s"} of ${job.id}; they end as errors.`)
      : h("span", {}, h("b", {}, "Stop the run? "), `Harbor stops ${job.id} before any attempt starts.`),
    state.stopErr ? h("span", { class: "bad" }, state.stopErr) : null,
    h("div", { class: "row" },
      h("button", { type: "button", id: "stop-yes", class: "btn danger", onclick: stopJob }, "Stop run"),
      h("button", { type: "button", class: "btn", onclick: () => { state.confirmStop = false; renderView(); } }, "Cancel")));
}

async function stopJob() {
  try {
    const job = await api(`/jobs/${encodeURIComponent(state.job.id)}/stop`, { method: "POST" });
    job.trials = job.trials || [];
    state.job = job;
    state.confirmStop = false;
    state.stopErr = "";
  } catch (e) {
    state.stopErr = `Couldn't stop it: ${e.message}`;
  }
  renderAll();
}

// --- results

function compareGroups(rows) {
  const groups = new Map();
  for (const r of rows) {
    const key = `${r.task}|${r.model}`;
    if (!groups.has(key)) groups.set(key, { task: r.task, model: r.model, arms: {} });
    groups.get(key).arms[r.arm] = r;
  }
  return [...groups.values()];
}

const freshMedian = (r) => r && (r.median_fresh_tokens ?? null);

function renderCompare(view) {
  const job = state.job;
  const rows = state.compare || [];
  const groups = compareGroups(rows);
  const multi = new Set(groups.map((g) => g.task)).size > 1;
  const kids = [];
  if (job.error) kids.push(banner("Harbor exited before it wrote the run's folder.", job.error));
  kids.push(h("h2", { class: "h" }, "Pass rate, control → kavach"));
  if (!groups.length) {
    kids.push(h("div", { class: "dim" }, job.status === "running" ? "No attempt has finished yet." : "This run has no attempts."));
  } else {
    const rate = (r) => r && r.attempts ? r.passes / r.attempts : null;
    const cell = (r, better) => {
      if (!r) return h("td", { class: "dim" }, "–");
      const extra = [r.errors ? `${r.errors} error${r.errors > 1 ? "s" : ""}` : "", r.pending ? `${r.pending} pending` : ""].filter(Boolean).join(" · ");
      return h("td", { class: better ? "b" : null }, r.attempts ? `${r.passes} / ${r.attempts}` : "–", extra ? h("span", { class: "sub" }, extra) : null);
    };
    const pick = (g, f) => [f(g.arms.control), f(g.arms.kavach)];
    kids.push(h("div", { class: "tablewrap" }, h("table", { class: "mx" },
      h("thead", {}, h("tr", {}, ["model", "control", "kavach", "median time", "median tokens", "cost / attempt"].map((c) => h("th", {}, c)))),
      h("tbody", {}, groups.map((g) => {
        const [c, k] = [g.arms.control, g.arms.kavach];
        const who = g.model ? orId(g.model) : job.agent;
        return h("tr", {},
          h("td", {}, multi ? `${g.task} · ${who}` : who),
          cell(c, false), cell(k, rate(k) != null && rate(k) > (rate(c) ?? -1)),
          h("td", {}, arrow(...pick(g, (r) => r && r.median_seconds), fmtDur)),
          h("td", { title: "fresh tokens: input not served from cache, plus output" }, arrow(...pick(g, freshMedian), fmtTok)),
          h("td", {}, arrow(...pick(g, (r) => r && r.mean_cost), fmtCost)));
      })))));
    kids.push(h("div", { class: "faint", style: "font-size:13px" }, "Rates count graded attempts; errors and pending attempts are left out of them and of the medians. Tokens are fresh tokens: input not served from cache, plus output."));
  }
  const attempts = job.trials.filter((t) => t.id);
  const queued = job.trials.length - attempts.length;
  kids.push(h("div", { class: "row", style: "margin-top:12px" }, h("h2", { class: "h" }, "Attempts"), queued ? h("span", { class: "dim" }, `${queued} queued`) : null));
  if (!attempts.length) kids.push(h("div", { class: "dim" }, "None has started yet."));
  else kids.push(h("div", { class: "cards" }, attempts.map((t) => attemptCard(t, job))));
  view.replaceChildren(h("div", { class: "pane" }, kids));
}

function attemptCard(t, job) {
  const lines = [];
  const kl = kavachLine(t);
  if (kl) lines.push(h("div", { class: "dim" }, kl + (t.kavach && t.kavach.variants ? ` · ${t.kavach.variants}` : "")));
  const gl = gradeLine(t);
  if (gl) lines.push(h("div", { class: t.status === "fail" ? "bad" : "dim" }, gl));
  if (t.error) lines.push(h("div", { class: "err-line" }, t.error));
  const meta = [t.seconds != null ? fmtDur(t.seconds) : t.started && t.status === "running" ? elapsed(t.started, fmtDur) : null,
    t.steps ? `${t.steps} step${t.steps === 1 ? "" : "s"}` : null, fresh(t.tokens) ? `${fmtTok(fresh(t.tokens))} tokens` : null, t.cost != null ? fmtCost(t.cost) : null].filter(Boolean);
  if (meta.length) lines.push(h("div", { class: "dim" }, meta.flatMap((m, i) => i ? [" · ", m] : [m])));
  const oc = openInOpenCode(t, job);
  const links = h("div", { class: "links" },
    oc.tagName === "SPAN" ? null : oc,
    finished(t) ? patchView(t) : null,
    h("a", { href: `#/jobs/${encodeURIComponent(job.id)}`, onclick: () => { state.sel = trialKey(t); } }, "Steps"));
  for (const b of links.querySelectorAll(".btn")) b.classList.remove("btn", "blue");
  return h("div", { class: `kcard ${t.status}` },
    h("div", { class: "title" }, pill(t), h("b", { title: t.id }, trialLabel(t))), lines, links);
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

// --- start

document.getElementById("menu").addEventListener("click", () => {
  state.side = !sideOpen();
  renderSide();
});
document.getElementById("title").addEventListener("click", toggleNav);
document.addEventListener("keydown", (e) => {
  if (e.key === "Escape") {
    if (!document.getElementById("nav").hidden) {
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
  if (e.target.closest(".side a") && window.matchMedia("(max-width: 900px)").matches) {
    state.side = false;
    renderSide();
  }
});
window.addEventListener("hashchange", onRoute);

renderAll();
loadStatus();
loadTasks();
loadJobs();
connectEvents();
setInterval(loadStatus, 15000);
if (current.job) refreshJob();
