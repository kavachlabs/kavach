// What an attempt's Kavach checks found, drawn for a person in a hurry:
//
//   - the headline: the recorded failure and the verdict, in plain words;
//   - the probe strip: the recording's inputs as an axis, the failure on it,
//     and every variant kavach diff tried at the input it changed, with how
//     the old and the new build fared on it;
//   - the trail: the agent's Kavach calls as rounds, with what it changed
//     between them;
//   - the scrubber: the agent's steps as a track, Kavach calls as chapters.
//
// The incident and its diff come from GET /api/trials/…/incident; the trail
// and the scrubber from the steps app.js already loaded.

import { h, state, api, renderView } from "./app.js";
import { checksOf } from "./invariants.js";
import { openIncident } from "./replay.js";

const enc = encodeURIComponent;
const tkey = (t) => `${t.job}/${t.id}`;
const cache = new Map(); // "job/trial#status" -> incident | { error, status } | "loading"

// incidentFor returns the trial's incident, loading it the first time. The
// status is in the key: grading adds the diff.
export function incidentFor(t) {
  const key = `${tkey(t)}#${t.status}`;
  const got = cache.get(key);
  if (got) return got === "loading" ? null : got;
  cache.set(key, "loading");
  api(`/trials/${enc(t.job)}/${enc(t.id)}/incident`)
    .then((d) => cache.set(key, d))
    .catch((e) => cache.set(key, { error: e.message, status: e.status }))
    .finally(() => { if (state.stepsFor === tkey(t)) renderView(); });
  return null;
}

const ok = (inc) => inc && !inc.error;
const seqOf = (v) => { const m = /@(\d+)\s*$/.exec(v || ""); return m ? +m[1] : null; };
const argOf = (v) => (/\(([^)]*)\)/.exec(v || "") || [])[1] || "";
const statusOf = (v) => (v || "").replace(/[(@].*$/, "");
const plural = (n, one, many = `${one}s`) => `${n} ${n === 1 ? one : many}`;

// counts reads the variant counts from the JSON report, else from the
// summary line ("46 of 64 reproduce the incident on the old build · 46 of
// those pass on the new build").
function counts(inc, t) {
  const v = ok(inc) && inc.diff && inc.diff.variants;
  if (v) return { candidates: v.candidates, reproducing: v.reproducing, passing: v.passing };
  const line = (ok(inc) && inc.diff && inc.diff.variants_line) || (t.kavach && t.kavach.variants) || "";
  const m = /(\d+) of (\d+) (?:variants )?reproduce.*?(\d+) of those pass/.exec(line);
  return m ? { candidates: +m[2], reproducing: +m[1], passing: +m[3] } : null;
}

function regCounts(inc) {
  const r = ok(inc) && inc.diff && inc.diff.regressions;
  if (r) return { counted: r.counted, passing: r.passing, candidates: r.candidates };
  const line = (ok(inc) && inc.diff && inc.diff.regressions_line) || "";
  const m = /(\d+) of (\d+) match the old build/.exec(line);
  return m ? { counted: +m[2], passing: +m[1] } : null;
}

const strip = (mut) => (mut || "").replace(/^failing input \(seq \d+\)[:\s]*/, "").replace(/^input \(seq \d+\)[:\s]*/, "");

function failureWords(inc, t) {
  const f = ok(inc) && inc.failure;
  if (f) {
    const what = f.kind === "panic" ? "panicked" : f.kind === "error" ? "returned an error" : f.kind === "crash" ? "crashed"
      : f.kind === "invariant" ? "broke an invariant" : f.kind;
    return [`input @${f.seq} ${what}`, f.message];
  }
  const old = t.kavach && t.kavach.old;
  if (!old) return null;
  const at = seqOf(old);
  const st = statusOf(old);
  const what = st === "invariant_violated" ? `broke invariant ${argOf(old)}` : st === "still_failing" ? "failed" : st;
  return [at != null ? `input @${at} ${what}` : what, ""];
}

// verdictWords says what a verdict means for this attempt: [tone, words].
function verdictWords(verdict, inc, t) {
  const st = statusOf(verdict);
  const at = seqOf(verdict);
  const k = argOf(verdict);
  const c = counts(inc, t);
  const rc = regCounts(inc);
  const probe = (set, id) => {
    const p = ok(inc) && inc.diff && inc.diff[set] && inc.diff[set].checks.find((x) => String(x.id) === k);
    return p ? strip(p.mutation) : "";
  };
  switch (st) {
    case "fixed": {
      const v = c ? `Fix verified on ${c.passing} of ${plural(c.reproducing, "variant")} that reproduce the failure` : "Fix verified";
      const r = rc && rc.counted ? `; ${rc.passing} of ${rc.counted} earlier inputs like it behave as before` : "";
      return ["ok", v + r];
    }
    case "variant_failed": {
      const mut = probe("variants", k);
      if (isContract(verdict, t)) return ["bad", `A variant of feature step @${at} failed${mut ? `: ${mut}` : ""}`];
      return ["bad", `Too narrow: variant ${k}${mut ? ` (${mut})` : ""} still fails on the new build, at its input @${at}`];
    }
    case "regressed": {
      const mut = probe("regressions", k);
      return ["bad", `Changed behaviour: on variant ${k}${mut ? ` (${mut})` : ""} of an earlier input, the new build's outputs at @${at} differ from the old build's`];
    }
    case "still_failing": return ["bad", `Not fixed: input @${at} still fails on the new build`];
    case "invariant_violated": return ["bad", `Not fixed: invariant ${k} breaks after input @${at}`];
    case "diverged": return ["bad", `Changed what production did: input @${at}, before the failure, has different outputs`];
    case "nondeterministic": return ["bad", `Reads the clock or randomness differently from the recording at input @${at}`];
    case "unverified": return ["warn", `Passes the recording, but only ${c ? c.reproducing : "a few"} variants reproduce the failure on the old build: not verified`];
    case "ok": return ["ok", "The recording has no failure, and the new build replays it as recorded"];
    case "pass": return ["ok", "Meets its contract: every feature step changed and the invariants hold"];
    case "regression": return ["bad", `Regression: step @${at}, outside the feature, changed its outputs`];
    case "unreached": return ["bad", `Feature not wired in: step @${at} gives the old build's outputs`];
    case "missing_invariant": return ["bad", `The build does not declare invariant ${k}`];
    case "baseline_mismatch": return ["bad", "The old build does not replay the recorded traffic"];
    case "unapproved": return ["warn", "Meets the contract, but no one has approved the contract"];
    case "stale_lock": return ["warn", "Meets the contract, but the contract changed since it was approved"];
    case "n/a": case "-": case "": return ["none", "No Kavach verdict: the change did not build or apply"];
    default: return ["none", verdict];
  }
}

function isContract(verdict, t) {
  return Boolean((t.kavach && t.kavach.features) || /^(pass|unreached|regression|missing_invariant|baseline_mismatch|unapproved|stale_lock)/.test(verdict || ""));
}

// renderHeadline is the attempt's verdict in plain words, or null before
// there is anything to say.
export function renderHeadline(t) {
  const inc = incidentFor(t);
  const verdict = (t.kavach && t.kavach.verdict) || (ok(inc) && inc.diff && inc.diff.verdict) || "";
  const fw = failureWords(inc, t);
  if (!verdict && !fw) return null;
  const [tone, words] = verdict ? verdictWords(verdict, inc, t) : ["none", "Not checked yet"];
  return h("div", { class: `headline ${tone}`, dataset: { incident: tkey(t) } },
    fw ? h("div", { class: "hl-row" },
      h("span", { class: "hl-k" }, "Recorded failure"),
      h("span", { class: "hl-v" }, fw[0], fw[1] ? h("span", { class: "faint m hl-msg", title: fw[1] }, ` · ${fw[1]}`) : null)) : null,
    h("div", { class: "hl-row main" },
      h("span", { class: "hl-k" }, verdict ? (isContract(verdict, t) ? "Contract" : "Verdict") : "Verdict"),
      h("span", { class: "hl-v" }, h("b", {}, words)),
      verdict ? h("span", { class: `pill ${tone === "ok" ? "pass" : tone === "bad" ? "fail" : "queued"} m hl-pill`, title: "the verdict as Kavach printed it" }, verdict) : null));
}

// axisSeq is the input of the recording a mutation changed, on the
// recording's own seqs. A clock shift changes every read, so it is drawn at
// the failure.
function axisSeq(mut, fail, first) {
  let m;
  if ((m = /moved \d+ inputs? earlier, before seq (\d+)/.exec(mut))) return +m[1];
  if ((m = /^input seq (\d+) (?:dropped|delivered twice)/.exec(mut))) return +m[1];
  if ((m = /^(?:failing input|input) \(seq (\d+)\)/.exec(mut))) return +m[1];
  if (/^all \d+ inputs? before the failing one dropped/.test(mut)) return first;
  return fail;
}

const probeKind = (mut) => (/\bmoved \d+ inputs? earlier/.test(mut) ? "moved earlier" : /\bdropped\b/.test(mut) ? "dropped" : /delivered twice/.test(mut) ? "redelivered"
  : /^clock shifted/.test(mut) ? "clock shifted" : "fields changed");

const selCol = new Map(); // trial key -> selected column of the strip

// renderProbe draws the recording as an axis with every variant diff tried
// on it. It returns null when there is no recording.
export function renderProbe(t) {
  const inc = incidentFor(t);
  if (!ok(inc) || !inc.steps.length) {
    if (inc && inc.error && inc.status !== 404) return h("div", { class: "bad probe-err" }, `Couldn't load the incident: ${inc.error}`);
    return null;
  }
  const steps = inc.steps;
  const n = steps.length;
  const idx = new Map();
  steps.forEach((s, i) => { if (!idx.has(s.seq)) idx.set(s.seq, i); });
  const traffic = inc.kind === "traffic";
  const failSeq = inc.failure ? inc.failure.seq : null;
  const failI = failSeq != null && idx.has(failSeq) ? idx.get(failSeq) : -1;
  const d = inc.diff;
  const divI = d && d.divergence && idx.has(d.divergence.seq) && d.divergence.seq !== failSeq ? idx.get(d.divergence.seq) : -1;
  const at = (seq) => (idx.has(seq) ? idx.get(seq) : seq != null ? nearest(steps, seq) : -1);

  // Columns hold the probes drawn at each input.
  const cols = steps.map(() => ({ up: [], down: [] }));
  let feats = new Set();
  if (traffic) {
    const checks = state.stepsFor === tkey(t) ? checksOf(state.steps) : [];
    const last = checks[checks.length - 1];
    if (last) {
      feats = new Set(last.feature_steps || []);
      for (const f of last.variant_failures || []) {
        const i = at(f.feature_seq);
        if (i >= 0) cols[i].up.push({ id: f.id, mutation: f.mutation, old: "", new: f.verdict, cls: "bad", set: "feature variant" });
      }
    }
  } else if (d) {
    const first = steps[0].seq;
    for (const p of (d.variants && d.variants.checks) || []) {
      const i = at(axisSeq(p.mutation, failSeq, first));
      if (i < 0) continue;
      const cls = p.reproduces === false ? "skip" : p.passed ? "ok" : "bad";
      cols[i].up.push({ ...p, cls, set: "variant" });
    }
    for (const p of (d.regressions && d.regressions.checks) || []) {
      const i = at(axisSeq(p.mutation, failSeq, first));
      if (i < 0) continue;
      const cls = p.counted === false ? "skip" : p.passed ? "ok" : "bad";
      cols[i].down.push({ ...p, cls, set: "regression" });
    }
  }
  const order = { bad: 0, ok: 1, skip: 2 };
  for (const c of cols) { c.up.sort((a, b) => order[a.cls] - order[b.cls]); c.down.sort((a, b) => order[a.cls] - order[b.cls]); }
  const hasProbes = cols.some((c) => c.up.length || c.down.length);
  const maxUp = Math.max(1, ...cols.map((c) => c.up.length));
  const maxDown = Math.max(1, ...cols.map((c) => c.down.length));
  const hasDown = cols.some((c) => c.down.length);

  const key = tkey(t);
  let sel = selCol.get(key);
  if (sel == null || sel >= n || !(cols[sel].up.length || cols[sel].down.length)) {
    sel = cols.findIndex((c) => c.up.some((p) => p.cls === "bad") || c.down.some((p) => p.cls === "bad"));
    if (sel < 0) sel = failI >= 0 && (cols[failI].up.length || cols[failI].down.length) ? failI : cols.findIndex((c) => c.up.length || c.down.length);
  }

  const bar = (list, max, dir) => {
    const tally = { bad: 0, ok: 0, skip: 0 };
    list.forEach((p) => tally[p.cls]++);
    // Square roots, so a column of one or two still shows beside one of 40.
    const px = (k) => `${Math.max(tally[k] ? 3 : 0, Math.round(Math.sqrt(tally[k] / max) * 40))}px`;
    return h("span", { class: `pr-bar ${dir}` },
      ["bad", "ok", "skip"].map((k) => (tally[k] ? h("span", { class: `pr-seg ${k}`, style: `height:${px(k)}` }) : null)));
  };
  const colTitle = (i) => {
    const c = cols[i];
    const s = steps[i];
    const parts = [`input @${s.seq}${i === failI ? " · the recorded failure" : ""}${i === divI ? " · first divergence" : ""}${feats.has(s.seq) ? " · feature step" : ""}`];
    const sum = (list, what) => {
      if (!list.length) return;
      const b = list.filter((p) => p.cls === "bad").length, g = list.filter((p) => p.cls === "ok").length, sk = list.length - b - g;
      parts.push(`${plural(list.length, what)}: ${g} pass, ${b} fail${sk ? `, ${sk} not counted` : ""}`);
    };
    sum(c.up, traffic ? "failing variant" : "variant");
    sum(c.down, "regression variant");
    return parts.join("\n");
  };
  const chart = h("div", { class: "pr-chart", style: `grid-template-columns: repeat(${n}, minmax(0, 1fr))`, role: "group", "aria-label": "Inputs of the recording" },
    steps.map((s, i) => {
      const c = cols[i];
      const any = c.up.length || c.down.length;
      const axis = `pr-axis ${i === failI ? "fail" : i === divI ? "div" : feats.has(s.seq) ? "feat" : ""}`;
      const kids = hasProbes ? [bar(c.up, maxUp, "up"), h("span", { class: axis }), hasDown ? bar(c.down, maxDown, "down") : null] : [h("span", { class: axis })];
      return any
        ? h("button", { type: "button", class: `pr-col ${i === sel ? "on" : ""}`, title: colTitle(i), "aria-pressed": String(i === sel), "aria-label": colTitle(i).replace(/\n/g, "; "), onclick: () => { selCol.set(key, i); renderView(); } }, kids)
        : h("span", { class: "pr-col", title: colTitle(i) }, kids);
    }));
  const ticks = h("div", { class: "pr-ticks m faint" },
    h("span", {}, `@${steps[0].seq}`),
    failI > 0 && failI < n - 1 ? h("span", { class: "pr-tick-fail", style: `left:${((failI + 0.5) / n) * 100}%` }, `@${failSeq}`) : null,
    h("span", { class: failI === n - 1 ? "pr-tick-end-fail" : "" }, `@${steps[n - 1].seq}`));

  const summary = [];
  const c = counts(inc, t);
  if (traffic) {
    const checks = state.stepsFor === tkey(t) ? checksOf(state.steps) : [];
    const last = checks[checks.length - 1];
    summary.push(`${plural(n, "input")} of recorded traffic`, `${plural(feats.size, "feature step")}`);
    if (last && last.variant_candidates) summary.push(`${last.variants_passed} of ${last.variant_candidates - last.variants_preexisting} feature variants pass (round ${last.round || last.n})`);
  } else {
    summary.push(`${plural(n, "input")} recorded`);
    if (c) summary.push(`${c.reproducing} of ${plural(c.candidates, "variant")} reproduce the failure on the old build, ${c.passing} pass on the new`);
    const rc = regCounts(inc);
    if (rc && rc.counted) summary.push(`${rc.passing} of ${plural(rc.counted, "regression variant")} match the old build`);
  }

  let detail = null;
  if (sel >= 0 && hasProbes) {
    const col = cols[sel];
    const list = [...col.up, ...col.down];
    const shown = list.slice(0, 80);
    detail = h("div", { class: "pr-detail" },
      h("div", { class: "pr-detail-h" }, h("span", { class: "m b" }, `@${steps[sel].seq}`), h("span", { class: "faint" }, ` · ${plural(list.length, "variant")} at this input`)),
      h("div", { class: "pr-list" }, shown.map((p) => probeRow(p, traffic))),
      list.length > shown.length ? h("div", { class: "faint" }, `and ${list.length - shown.length} more`) : null);
  }

  const note = !traffic && d && d.source === "log" ? "This run's grader saved only the diff's summary, so the variants aren't drawn. Runs graded from now on save each variant." : !traffic && !d ? "Not graded yet: the variants show once the verifier runs kavach diff." : null;

  return h("section", { class: "probe", dataset: { incident: key } },
    h("div", { class: "pr-head" },
      h("span", { class: "b" }, traffic ? "Recorded traffic" : "The recording, and what diff tried"),
      h("span", { class: "pr-spacer" }),
      !traffic ? h("button", { type: "button", class: "btn sm", "data-replay": "incident", title: "Step through the recorded incident as the old and the new build replay it", onclick: () => openIncident(t, inc.new || !inc.old ? "new" : "old") }, "▶ Replay the incident") : null),
    h("div", { class: "pr-sum faint" }, summary.join(" · ")),
    h("div", { class: `pr-wrap ${hasDown ? "two" : ""} ${hasProbes ? "" : "bare"}` },
      !hasProbes ? h("span", {}) : h("div", { class: "pr-lanes faint", "aria-hidden": "true" },
        h("span", { title: traffic ? "variants of feature steps that failed" : "variants of the failing input, at the input each changed" }, traffic ? "failures" : "variants"),
        hasDown ? h("span", { title: "variants of earlier inputs like the failing one, which must give the old build's outputs" }, "earlier inputs") : null),
      h("div", { class: "pr-plot" }, chart, ticks)),
    h("div", { class: "pr-legend faint" },
      hasProbes && !traffic ? h("span", {}, h("i", { class: "pr-sw ok" }), "passes on the new build") : null,
      hasProbes ? h("span", {}, h("i", { class: "pr-sw bad" }), "fails on the new build") : null,
      hasProbes && !traffic ? h("span", {}, h("i", { class: "pr-sw skip" }), "old build doesn't fail it, not counted") : null,
      failI >= 0 ? h("span", {}, h("i", { class: "pr-sw fail" }), "recorded failure") : null,
      divI >= 0 ? h("span", {}, h("i", { class: "pr-sw div" }), "first divergence") : null,
      feats.size ? h("span", {}, h("i", { class: "pr-sw feat" }), "feature step") : null),
    note ? h("div", { class: "faint pr-note" }, note) : null,
    detail);
}

function nearest(steps, seq) {
  let best = -1;
  steps.forEach((s, i) => { if (s.seq <= seq) best = i; });
  return best;
}

// probeRow is one variant: ✓ or ✗ for each build, and the mutation.
function probeRow(p, traffic) {
  const reg = p.set === "regression";
  // A variant counts when the old build fails it the same way; a regression
  // variant when the old build handles it.
  const oldGood = reg ? p.counted !== false && /^ok/.test(p.old) : p.reproduces !== false;
  const mark = (good, verdict, who) => h("span", { class: `pr-mark ${good ? "ok" : "bad"}`, title: `${who}: ${verdict || "—"}` }, good ? "✓" : "✗");
  const oldMark = traffic ? null : reg
    ? mark(oldGood, p.old, "old build")
    : h("span", { class: `pr-mark ${p.reproduces === false ? "skip" : "bad"}`, title: `old build: ${p.old}${p.reproduces === false ? " (doesn't reproduce the failure, so it isn't counted)" : ""}` }, p.reproduces === false ? "○" : "✗");
  return h("div", { class: `pr-row ${p.cls}` },
    h("span", { class: "m faint pr-id" }, `#${p.id}`),
    h("span", { class: "pr-marks" }, oldMark, mark(p.cls === "ok", p.new, "new build")),
    h("span", { class: "pr-kind faint" }, reg ? "earlier input" : probeKind(p.mutation)),
    h("span", { class: "m pr-mut", title: p.mutation }, strip(p.mutation) || p.mutation));
}

// The trail: the agent's Kavach calls, each with what it changed since the
// call before.

const CHECK = /^kavach (diff|replay|contract_check|contract check)\b/;
const isCheck = (s) => s.kavach && (CHECK.test(s.title) || /kavach(?:\s+--[\w-]+)*\s+(diff|replay|contract\s+check)\b/.test(s.title));

// trailRows reads the rows from the shown steps.
export function trailRows(shown) {
  const rows = [];
  let edits = [];
  let round = 0;
  let prevVerdict = "";
  shown.forEach((s, i) => {
    if (s.kind === "edit") edits.push(s.title.split("/").pop());
    if (!s.kavach) return;
    const check = isCheck(s);
    if (check) round++;
    const files = [...new Set(edits)];
    const verdict = s.verdict || (s.contract && s.contract.verdict) || "";
    const changed = [];
    if (edits.length) changed.push(`${plural(edits.length, "edit")} to ${files.slice(0, 3).join(", ")}${files.length > 3 ? ` +${files.length - 3}` : ""}`);
    else if (check && round === 1) changed.push("the code as it started");
    else if (check) changed.push("no edits since the last check");
    if (check && verdict && prevVerdict && verdict !== prevVerdict) changed.push(`${prevVerdict} → ${verdict}`);
    rows.push({ i, s, round: check ? round : null, verdict, changed: changed.join(" · ") });
    if (check) { edits = []; if (verdict) prevVerdict = verdict; }
  });
  return rows;
}

const firstLine = (s) => String(s || "").split("\n").find((l) => l.trim() && !l.trim().startsWith("#")) || "";
const short = (cmd) => firstLine(cmd).replace(/(^|\s)\/[\w./-]*\/(kavach\b)/, "$1$2").replace(/\s+/g, " ").trim();

// renderTrail is the trail as a table. onPick shows a row's step.
export function renderTrail(shown, onPick, clockOf) {
  const rows = trailRows(shown);
  if (!rows.length) return h("div", { class: "dim trail-empty" }, "The agent made no Kavach calls.");
  return h("div", { class: "trail tablewrap" }, h("table", { class: "trail-t" },
    h("thead", {}, h("tr", {}, ["Round", "Command", "Verdict", "What changed", ""].map((x) => h("th", {}, x)))),
    h("tbody", {}, rows.map((r) => {
      const pass = r.s.contract ? r.s.contract.passed : r.s.passed;
      return h("tr", { class: r.round ? "" : "minor", tabindex: "0", title: "Show this step", onclick: () => onPick(r.i), onkeydown: (e) => { if (e.key === "Enter") onPick(r.i); } },
        h("td", { class: "m" }, r.round ? String(r.round) : "·"),
        h("td", { class: "m trail-cmd", title: r.s.title }, short(r.s.title)),
        h("td", {}, r.verdict ? h("span", { class: `pill ${pass ? "pass" : pass === false ? "fail" : "queued"} m` }, r.verdict) : h("span", { class: "faint" }, r.round ? "no verdict printed" : "—")),
        h("td", { class: "trail-what" }, r.changed || h("span", { class: "faint" }, "—")),
        h("td", { class: "m faint" }, clockOf(r.s.t_ms / 1000)));
    }))));
}

// The scrubber: a track over the agent's steps. Its value is a step's index
// among the shown steps; Kavach calls are its chapters.

const scrubAt = new Map(); // trial key -> index

export function renderScrubber(t, shown, clockOf) {
  const n = shown.length;
  if (n < 2) return null;
  const key = tkey(t);
  const rows = trailRows(shown);
  const pct = (i) => `${((i / (n - 1)) * 100).toFixed(3)}%`;
  const cur = Math.min(scrubAt.has(key) ? scrubAt.get(key) : n - 1, n - 1);
  const readout = h("span", { class: "sc-read m faint", "aria-live": "off" }, readText(shown, cur, clockOf));
  const range = h("input", {
    type: "range", class: "sc-range", min: "0", max: String(n - 1), step: "1", value: String(cur), "aria-label": "Scrub through the agent's steps",
    "aria-valuetext": readText(shown, cur, clockOf),
    oninput: (e) => { const i = +e.target.value; scrubAt.set(key, i); readout.textContent = readText(shown, i, clockOf); e.target.setAttribute("aria-valuetext", readout.textContent); showStep(i); },
  });
  const chapters = rows.map((r) => {
    const pass = r.s.contract ? r.s.contract.passed : r.s.passed;
    const tone = pass ? "ok" : pass === false ? "bad" : "none";
    return h("button", {
      type: "button", class: `sc-ch ${tone} ${r.round ? "" : "minor"}`, style: `left:${pct(r.i)}`,
      title: `${r.round ? `Round ${r.round}: ` : ""}${short(r.s.title)}${r.verdict ? ` → ${r.verdict}` : ""} · ${clockOf(r.s.t_ms / 1000)}`,
      "aria-label": `${r.round ? `Round ${r.round}, ` : ""}${short(r.s.title)}${r.verdict ? `, ${r.verdict}` : ""}`,
      onclick: () => { scrubAt.set(key, r.i); range.value = String(r.i); readout.textContent = readText(shown, r.i, clockOf); showStep(r.i); },
    }, r.round ? String(r.round) : "");
  });
  const edits = shown.map((s, i) => (s.kind === "edit" ? h("span", { class: "sc-edit", style: `left:${pct(i)}` }) : null));
  return h("div", { class: "scrub", dataset: { key } },
    h("div", { class: "sc-track" },
      h("div", { class: "sc-chapters" }, chapters),
      h("div", { class: "sc-rail", "aria-hidden": "true" }, edits),
      range),
    h("div", { class: "sc-foot" },
      readout,
      h("span", { class: "sc-legend faint" }, h("i", { class: "sc-sw ok" }), "check passed", h("i", { class: "sc-sw bad" }), "failed", h("i", { class: "sc-sw edit" }), "edit")));
}

function readText(shown, i, clockOf) {
  const s = shown[i];
  if (!s) return "";
  const what = s.kind === "text" ? "the agent wrote" : s.kavach ? short(s.title) : `${s.kind} ${firstLine(s.title).slice(0, 70)}`;
  return `step ${i + 1} of ${shown.length} · ${clockOf(s.t_ms / 1000)} · ${what}`;
}

// showStep scrolls the steps list to step i and marks it.
export function showStep(i) {
  const box = document.querySelector(".steps");
  if (!box) return;
  const el = box.querySelector(`[data-si="${i}"]`);
  if (!el) return;
  box.querySelectorAll(".scrub-at").forEach((x) => x.classList.remove("scrub-at"));
  el.classList.add("scrub-at");
  box.scrollTop += el.getBoundingClientRect().top - box.getBoundingClientRect().top - 8;
}

export const resetScrub = (t) => scrubAt.delete(tkey(t));
