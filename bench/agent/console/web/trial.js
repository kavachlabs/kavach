// The public trial's own views: landing, keys, invite code and the run form.
// app.js draws them only when the server says it is a trial (TRIAL there).

import { h, state, trial, loadTrialSession, trialReady, renderView } from "./app.js";

export const TRIAL_VIEWS = ["landing", "keys", "invite", "terms"];

const SITE = "https://kavachlabs.dev";
const BENCH = "https://benchmark.kavachlabs.dev";
const trust = () => h("span", {}, "Keys you enter stay in this server's memory and are used only for your runs. Runs bill your own provider accounts, so use keys with a spending limit. ", h("a", { href: "#/trial/terms" }, "Terms"));
const ARM_SUFFIX = /-(kavach-implement|workflow|control|kavach)$/;
const base = (task) => task.replace(ARM_SUFFIX, "");
const modelName = (m) => m.replace(/^openrouter\//, "");

// send makes a request and returns the error text of a failure, or "".
async function send(method, path, body) {
  try {
    const resp = await fetch("/api" + path, {
      method,
      headers: body ? { "Content-Type": "application/json" } : {},
      body: body ? JSON.stringify(body) : undefined,
    });
    if (resp.ok) return { ok: true, body: await resp.json().catch(() => null) };
    const msg = (await resp.text()).trim();
    let err = msg;
    try { err = JSON.parse(msg).error || msg; } catch {}
    return { ok: false, err: err || `${resp.status} ${resp.statusText}` };
  } catch (e) {
    return { ok: false, err: `Can't reach the server: ${e.message}` };
  }
}

async function forgetKeys() {
  await send("DELETE", "/trial/keys");
  await loadTrialSession();
  location.hash = "#/";
  renderView();
}

export function renderTrialView(view, name) {
  const page = { landing, keys: keysForm, invite: inviteForm, terms }[name]();
  view.replaceChildren(page);
  view.scrollTop = 0;
}

const link = (href, text) => h("a", { href, target: "_blank", rel: "noopener" }, text);

// termsNote is the short version of the terms, on the forms that take keys.
function termsNote(invited) {
  return h("div", { class: "tl-terms", role: "note" },
    h("ul", {},
      invited
        ? h("li", {}, "Invite runs use our provider keys, so they cost you nothing, and are limited to the runs your code allows.")
        : [
          h("li", {}, "Your keys stay in this server's memory. They are used only for your runs, and dropped when you click Forget my keys, after 24 hours idle, or when the server restarts."),
          h("li", {}, "Runs are billed to your own model and sandbox provider accounts. Set a spend limit on your key."),
        ],
      h("li", {}, "Every run and its results are public and listed for anyone.")),
    h("a", { href: "#/trial/terms" }, "Terms and how your keys are handled →"));
}

// cmp holds each finished run's /compare rows, fetched once for the landing.
const cmp = new Map();
function loadCompares(jobs) {
  const todo = jobs.slice(0, 12).filter((j) => !cmp.has(j.id));
  let left = todo.length;
  for (const j of todo) {
    cmp.set(j.id, null);
    fetch(`/api/jobs/${encodeURIComponent(j.id)}/compare`)
      .then((r) => (r.ok ? r.json() : []))
      .catch(() => [])
      .then((rows) => {
        cmp.set(j.id, rows || []);
        if (--left === 0 && /^#?\/?$/.test(location.hash)) renderView();
      });
  }
}

// armTotals sums the graded attempts of one arm over some runs' compare rows.
function armTotals(ids, arm) {
  let passes = 0, attempts = 0;
  for (const id of ids) for (const r of cmp.get(id) || []) if ((r.arm || "control") === arm) { passes += r.passes; attempts += r.attempts; }
  return { passes, attempts };
}

// verdictCard is an example of Kavach's output, plus, once the finished runs'
// numbers are in, their real totals. The example's rows are illustrative.
function verdictCard(ids) {
  const k = armTotals(ids, "kavach"), c = armTotals(ids, "control");
  const real = k.attempts && c.attempts
    ? h("p", { class: "vc-real" }, h("b", {}, "From the finished runs below"), h("br"),
      `Kavach arm: ${k.passes}/${k.attempts} fixed · control: ${c.passes}/${c.attempts}`)
    : null;
  const row = (what, res, ok) => h("div", { class: "vc-row" }, h("span", {}, what), h("span", { class: ok ? "ok" : "bad" }, res));
  return h("figure", { class: "vc", "aria-label": "Example of a Kavach verdict" },
    h("div", { class: "vc-bar" }, h("span", { class: "vc-dots", "aria-hidden": "true" }), h("span", {}, "kavach diff incident.kavach --json"), h("span", { class: "vc-tag" }, "example")),
    h("div", { class: "vc-cols" },
      h("div", { class: "vc-col" },
        h("h4", {}, "Control arm"),
        h("p", { class: "vc-line" }, "agent: ", h("span", { class: "ok" }, "tests pass ✓")),
        h("p", { class: "vc-line faint" }, "ships the patch")),
      h("div", { class: "vc-col vc-k" },
        h("h4", {}, "Kavach arm"),
        row("Recorded incident", "fixed ✓", true),
        row("Variant 2/10 · moved earlier", "fixed ✓", true),
        row("Variant 3/10 · field removed", "still_failing ✗", false),
        h("div", { class: "vc-verdict" }, h("span", {}, "verdict"), h("code", {}, "variant_failed(3)@12")))),
    h("figcaption", {}, "The agent's tests passed; Kavach replayed variants of the incident and found the fix too narrow."),
    real);
}

// armLine is a run's pass count per arm, once its compare rows are loaded.
function armLine(j) {
  const rows = cmp.get(j.id);
  if (!rows || !rows.length) return null;
  const arms = ["kavach", "control"].filter((a) => rows.some((r) => (r.arm || "control") === a));
  return h("span", { class: "tl-arms" }, arms.map((a) => {
    const t = armTotals([j.id], a);
    return h("span", { class: `tl-arm ${a}` }, a, " ", h("b", {}, t.attempts ? `${t.passes}/${t.attempts}` : "–"));
  }));
}

function landing() {
  const ready = trialReady();
  const done = (state.jobs || []).filter((j) => j.status === "done");
  const mine = new Set((trial.session && trial.session.runs) || []);
  const yours = (state.jobs || []).filter((j) => mine.has(j.id));
  const showcase = done.filter((j) => !mine.has(j.id));
  loadCompares(showcase);

  const step = (n, title, text) => h("li", { class: "tl-step" },
    h("span", { class: "tl-n", "aria-hidden": "true" }, n),
    h("h3", {}, title),
    h("p", {}, text));

  return h("div", { class: "tl" },
    h("div", { class: "tl-top" },
    h("section", { class: "tl-hero" },
      h("span", { class: "logo-mark", role: "img", "aria-label": "Kavach" }),
      h("p", { class: "tl-eyebrow" }, "Public trial"),
      h("h1", {}, "Did the AI's fix actually fix it?"),
      h("p", { class: "tl-lede" },
        "Kavach replays a recorded production incident, and dozens of mutated variants of it, against a coding agent's patch, and tells you whether the bug is really gone. Run an agent on a real incident here, with and without Kavach, and compare."),
      h("div", { class: "tl-cta" },
        ready
          ? h("a", { class: "tl-btn primary", href: "#/new" }, "Start a run")
          : h("a", { class: "tl-btn primary", href: "#/trial/keys" }, "Try it with your keys"),
        ready ? null : h("a", { class: "tl-btn", href: "#/trial/invite" }, "Have an invite code?"),
        ready && trial.session.has_keys ? h("button", { type: "button", class: "tl-btn", onclick: forgetKeys }, "Forget my keys") : null),
      h("p", { class: "tl-trust" }, h("span", { class: "tl-lock", "aria-hidden": "true" }, "🔒"), trust())),
    verdictCard(showcase.map((j) => j.id))),

    h("section", { class: "tl-sec" },
      h("h2", {}, "How the trial works"),
      h("ol", { class: "tl-steps" },
        step("1", "Bring your keys", "An OpenRouter key for the model, and a Daytona or Modal key for the sandbox the agent works in. Or enter an invite code."),
        step("2", "Pick an incident and a model", "Real bugs from real projects such as Redis, Valkey, axum and httpx. Each runs twice: a control arm, and an arm with Kavach."),
        step("3", "Watch it live", "Follow the agent step by step, then see Kavach's verdict on its fix next to the control's."))),

    yours.length ? runsSection("Your runs", "Started from this session.", yours) : null,
    runsSection("Finished runs", "Real runs you can open now, no keys needed.", showcase,
      state.jobs ? "No finished runs to show yet." : "Loading…"),

    h("section", { class: "tl-sec tl-more" },
      h("div", {}, h("h2", {}, "The full benchmark"),
        h("p", {}, "Every task, model and arm we have run, read-only, at ", link(BENCH, "benchmark.kavachlabs.dev"), ".")),
      h("div", { class: "tl-cta" },
        h("a", { class: "tl-btn", href: BENCH, target: "_blank", rel: "noopener" }, "Open the full benchmark ↗"),
        h("a", { class: "tl-btn", href: SITE, target: "_blank", rel: "noopener" }, "kavachlabs.dev ↗"))),

    h("footer", { class: "tl-foot" },
      h("a", { href: "#/trial/terms" }, "Terms and how your keys are handled"),
      h("span", { class: "faint" }, "A free trial, provided as is.")));
}

// terms is the fuller statement. Each claim is backed by api/trial.go and
// API.md "Trial mode"; change the code and this together.
function terms() {
  const sec = (title, ...items) => h("section", { class: "tl-sec tl-terms-sec" },
    h("h2", {}, title), h("ul", {}, items.map((t) => h("li", {}, t))));
  return shell("Terms and how your keys are handled",
    "Plain version. This is a trial, so it is short.",
    sec("Your keys",
      "The console holds them in this server's memory only. It never writes them to disk or logs them, and never sends them back to your browser. The tools that run your job might write them into the run's files or its temporary home folder: we redact the files when the run ends and delete the home folder then.",
      "Passed only to the run you start, as environment variables of its process, and used only to call your model provider and your sandbox provider (Daytona or Modal).",
      "Dropped from your session when you click Forget my keys, after 24 hours without use, or when the server restarts. For a run in progress we keep its key values in memory only, to redact them from that run's output.",
      "Forgetting your keys does not stop a run that has already started: use Stop on the run. It keeps the keys it began with until it ends.",
      "Logs are redacted as they are written, and a run's files are redacted when it ends. What the server shows is also redacted while it holds the run's key values."),
    sec("What it costs you",
      "Runs bill your own accounts: model tokens at your model provider and sandbox time at Daytona or Modal. We do not charge you and cannot cap what your providers charge.",
      "Set a spend limit on every key you paste. Use a key you can revoke afterwards.",
      "With an invite code the run uses our keys instead, up to the number of runs the code allows."),
    sec("What we store",
      "Each run's folder: the task, model and settings you chose, the agent's transcript, its patch, Kavach's verdicts, grading results and logs (with keys redacted). Finished runs may also be copied to our backup storage.",
      "They are kept until we delete them. We make no promise about how long that is.",
      "A one-way hash of your session id (to let you stop your own runs), and for an invite code a hash of the code with a count of its runs. Your network address is held in memory to rate-limit requests.",
      "A session cookie. There are no accounts, and we do not ask for your name or email.",
      "The site is served through Cloudflare, which sees your traffic. If the check on the run form is on, your network address is also sent to Cloudflare's Turnstile service to verify it."),
    sec("What we don't",
      "Your keys, after they are forgotten. Any code or files of yours: you only choose from the incidents we provide."),
    sec("Runs are public",
      "Every run, running or finished, and its results is listed for every visitor, and finished runs appear on this page. Do not put anything private into a run."),
    sec("As is",
      "This is a free trial, provided as is, with no guarantee that it works, stays up, keeps your runs, or that results are correct. We may change or stop it at any time."),
    h("p", { class: "tl-alt" }, "Questions or removal requests: contact us via ", link(SITE, "kavachlabs.dev"), "."),
    h("p", { class: "tl-alt" }, h("a", { href: "#/trial/keys" }, "Bring your keys"), " · ", h("a", { href: "#/trial/invite" }, "Use an invite code")));
}

function runsSection(title, sub, jobs, empty) {
  const cards = jobs.slice(0, 12).map((j) => {
    const tasks = [...new Set(j.tasks.map(base))].join(", ") || j.id;
    const models = j.models.map((m) => modelName(m).split("/").pop()).join(", ");
    return h("a", { class: "tl-card", href: `#/jobs/${encodeURIComponent(j.id)}/compare` },
      h("b", {}, tasks),
      h("span", { class: "tl-card-sub" }, models || j.agent),
      armLine(j),
      h("span", { class: "tl-card-foot" },
        h("span", { class: "m faint" }, `${j.trials_done}/${j.trials_total} attempts`),
        h("span", { class: "tl-card-go" }, "Results →")));
  });
  return h("section", { class: "tl-sec" },
    h("h2", {}, title), h("p", { class: "tl-sub" }, sub),
    cards.length ? h("div", { class: "tl-cards" }, cards) : h("p", { class: "tl-empty" }, empty || ""));
}

function field({ id, label, type = "text", hint, value, onInput, autocomplete }) {
  return h("div", { class: "tf-field" },
    h("label", { for: id }, label),
    h("input", { id, type, value: value || "", autocomplete: autocomplete || "off", spellcheck: "false", required: true, oninput: (e) => onInput(e.target.value) }),
    hint ? h("span", { class: "tf-hint" }, hint) : null);
}

function shell(title, lede, ...kids) {
  return h("div", { class: "tl tl-narrow" },
    h("a", { class: "tl-back", href: "#/" }, "← Back"),
    h("h1", { class: "tl-h1" }, title),
    h("p", { class: "tl-lede" }, lede),
    ...kids);
}

const keys = { provider: "daytona", model_key: "", daytona_api_key: "", modal_token_id: "", modal_token_secret: "", err: "", busy: false };
const invite = { code: "", err: "", busy: false };

function keysForm() {
  const redraw = () => renderView();
  const seg = (id, label) => h("button", {
    type: "button", role: "radio", "aria-checked": String(keys.provider === id),
    class: `tf-seg ${keys.provider === id ? "on" : ""}`,
    onclick: () => { keys.provider = id; redraw(); },
  }, label);
  const set = (k) => (v) => { keys[k] = v; };

  const form = h("form", { class: "tf", onsubmit: async (e) => {
    e.preventDefault();
    keys.busy = true; keys.err = ""; redraw();
    const body = { model_key: keys.model_key.trim(), provider: keys.provider };
    if (keys.provider === "daytona") body.daytona_api_key = keys.daytona_api_key.trim();
    else Object.assign(body, { modal_token_id: keys.modal_token_id.trim(), modal_token_secret: keys.modal_token_secret.trim() });
    const r = await send("POST", "/trial/keys", body);
    keys.busy = false;
    if (!r.ok) { keys.err = r.err; return redraw(); }
    Object.assign(keys, { model_key: "", daytona_api_key: "", modal_token_id: "", modal_token_secret: "" });
    await loadTrialSession();
    location.hash = "#/new";
  } },
    h("fieldset", {},
      h("legend", {}, "Model"),
      field({ id: "k-or", label: "OpenRouter API key", type: "password", value: keys.model_key, onInput: set("model_key"),
        hint: ["Pays for the model's tokens. ", link("https://openrouter.ai/settings/keys", "Create a key"), " with a spending limit."] })),
    h("fieldset", {},
      h("legend", {}, "Sandbox"),
      h("div", { class: "tf-segs", role: "radiogroup", "aria-label": "Sandbox provider" }, seg("daytona", "Daytona"), seg("modal", "Modal")),
      keys.provider === "daytona"
        ? field({ id: "k-day", label: "Daytona API key", type: "password", value: keys.daytona_api_key, onInput: set("daytona_api_key"),
          hint: [link("https://app.daytona.io/dashboard/keys", "Get a Daytona key"), ". The agent works in a sandbox on your account."] })
        : [
          field({ id: "k-mid", label: "Modal token ID", type: "password", value: keys.modal_token_id, onInput: set("modal_token_id") }),
          field({ id: "k-msec", label: "Modal token secret", type: "password", value: keys.modal_token_secret, onInput: set("modal_token_secret"),
            hint: [link("https://modal.com/settings/tokens", "Create a Modal token"), ". The agent works in a sandbox on your account."] }),
        ]),
    keys.err ? h("p", { class: "tf-err", role: "alert" }, keys.err) : null,
    h("button", { type: "submit", class: "tl-btn primary", disabled: keys.busy }, keys.busy ? "Saving…" : "Save keys and continue"),
    termsNote(false));
  return shell("Bring your keys", "They are held in memory for this session so the agent can run for you.", form,
    h("p", { class: "tl-alt" }, "No keys? ", h("a", { href: "#/trial/invite" }, "Use an invite code"), "."));
}

function inviteForm() {
  const form = h("form", { class: "tf", onsubmit: async (e) => {
    e.preventDefault();
    invite.busy = true; invite.err = ""; renderView();
    const r = await send("POST", "/trial/invite", { code: invite.code.trim() });
    invite.busy = false;
    if (!r.ok) { invite.err = r.err; return renderView(); }
    invite.code = "";
    await loadTrialSession();
    location.hash = "#/new";
  } },
    field({ id: "inv", label: "Invite code", value: invite.code, onInput: (v) => { invite.code = v; } }),
    invite.err ? h("p", { class: "tf-err", role: "alert" }, invite.err) : null,
    h("button", { type: "submit", class: "tl-btn primary", disabled: invite.busy }, invite.busy ? "Checking…" : "Use invite code"),
    termsNote(true));
  return shell("Have an invite code?", "An invite lets you run without your own keys.", form,
    h("p", { class: "tl-alt" }, "No code? ", h("a", { href: "#/trial/keys" }, "Bring your own keys"), "."));
}

// The run form. Its choices outlive redraws.
const run = { task: "", models: null, attempts: 1, err: "", busy: false, token: "" };
let tsBox = null; // the Turnstile widget's element: kept so a redraw does not lose the token
let tsLoading = null;

function loadTurnstile() {
  tsLoading = tsLoading || new Promise((resolve, reject) => {
    const s = document.createElement("script");
    s.src = "https://challenges.cloudflare.com/turnstile/v0/api.js?render=explicit";
    s.onload = () => resolve(window.turnstile);
    s.onerror = () => { tsLoading = null; reject(new Error("Couldn't load the Turnstile check")); };
    document.head.append(s);
  });
  return tsLoading;
}

function turnstileBox(siteKey) {
  if (!siteKey) return null;
  if (!tsBox) {
    tsBox = h("div", { class: "tf-ts" });
    loadTurnstile().then((ts) => ts.render(tsBox, {
      sitekey: siteKey,
      callback: (t) => { run.token = t; },
      "expired-callback": () => { run.token = ""; },
    })).catch((e) => { run.err = e.message; renderView(); });
  }
  return tsBox;
}

export function renderTrialNew(view) {
  if (!trialReady()) {
    history.replaceState(null, "", "#/");
    return renderTrialView(view, "landing");
  }
  const lim = trial.session.limits || { models: [], tasks: [], max_attempts: 1 };
  const bases = [...new Set(lim.tasks.map(base))];
  if (!bases.includes(run.task)) run.task = bases.includes(state.form.task) ? state.form.task : bases[0] || "";
  if (!run.models) run.models = lim.models.slice(0, 1);
  run.attempts = Math.min(Math.max(run.attempts, 1), lim.max_attempts || 1);
  const meta = (name) => (state.tasks || []).find((t) => t.name === name);
  const redraw = () => renderView();

  const taskCards = bases.map((b) => {
    const t = meta(b);
    const on = run.task === b;
    return h("button", { type: "button", role: "radio", "aria-checked": String(on), class: `tf-opt ${on ? "on" : ""}`, onclick: () => { run.task = b; redraw(); } },
      h("b", {}, b),
      t ? h("span", { class: "tf-opt-sub" }, (t.title || "").replace(/^Task:\s*/, "")) : null,
      t && t.summary ? h("span", { class: "tf-opt-desc" }, t.summary) : null);
  });

  const modelChips = lim.models.map((m) => {
    const on = run.models.includes(m);
    return h("button", { type: "button", class: `chip ${on ? "on" : ""}`, "aria-pressed": String(on), onclick: () => {
      run.models = on ? run.models.filter((x) => x !== m) : [...run.models, m];
      redraw();
    } }, modelName(m));
  });

  const attempts = [];
  for (let n = 1; n <= (lim.max_attempts || 1); n++) {
    attempts.push(h("button", { type: "button", role: "radio", "aria-checked": String(run.attempts === n), class: `tf-seg ${run.attempts === n ? "on" : ""}`, onclick: () => { run.attempts = n; redraw(); } }, String(n)));
  }

  const submit = async (e) => {
    e.preventDefault();
    if (!run.models.length) { run.err = "Choose at least one model."; return redraw(); }
    if (trial.session.turnstile_site_key && !run.token) { run.err = "Complete the check first."; return redraw(); }
    run.busy = true; run.err = ""; redraw();
    const r = await send("POST", "/jobs", {
      tasks: lim.tasks.filter((t) => base(t) === run.task),
      models: run.models,
      attempts: run.attempts,
      turnstile_token: run.token,
    });
    run.busy = false;
    if (!r.ok) {
      run.err = r.err;
      run.token = "";
      if (tsBox && window.turnstile) window.turnstile.reset(tsBox);
      return redraw();
    }
    await loadTrialSession();
    location.hash = `#/jobs/${encodeURIComponent(r.body.id || r.body.name)}`;
  };

  view.replaceChildren(shell("Start a run", "The agent gets the incident and works on a fix. Kavach's arm can replay it; the control arm cannot.",
    h("form", { class: "tf", onsubmit: submit },
      h("fieldset", {}, h("legend", {}, "Incident"),
        h("div", { class: "tf-opts", role: "radiogroup", "aria-label": "Incident" }, taskCards)),
      h("fieldset", {}, h("legend", {}, "Model"), h("div", { class: "tf-chips" }, modelChips)),
      lim.max_attempts > 1 ? h("fieldset", {}, h("legend", {}, "Attempts"),
        h("div", { class: "tf-segs", role: "radiogroup", "aria-label": "Attempts" }, attempts),
        h("span", { class: "tf-hint" }, "Each attempt runs in both arms.")) : null,
      turnstileBox(trial.session.turnstile_site_key),
      run.err ? h("p", { class: "tf-err", role: "alert" }, run.err) : null,
      h("button", { type: "submit", class: "tl-btn primary", disabled: run.busy || !run.task }, run.busy ? "Starting…" : "Start run"),
      h("p", { class: "tl-trust" }, h("span", { class: "tl-lock", "aria-hidden": "true" }, "🔒"), trust()),
      trial.session.has_keys ? h("button", { type: "button", class: "tl-link", onclick: forgetKeys }, "Forget my keys") : null)));
}
