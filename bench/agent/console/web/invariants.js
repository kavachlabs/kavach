// The invariants views of the attempt inspector: what each contract check
// says about the build's declared invariants, from the reports the steps
// carry (Step.contract). A report from an older Kavach has no variant kinds
// or declared list; the views say what they can't show rather than guess.

import { h, state, renderView, api, CODE_EDITORS, editorURL, setCodeEditor } from "./app.js";
import { cachedElement, languageOf, monacoStatus, patchEditor, snippetEditor, useTrial } from "./monaco.js";
import { isSaved, openReplay, savedVariants } from "./replay.js";

// The classes of variant, in the order Kavach generates them
// (replay.KindField … replay.KindClock).
export const VKINDS = ["field", "pair", "move", "drop", "duplicate", "clock"];
const VK = {
  field: { short: "Field", axis: "fld", label: "a field changed, removed or added" },
  pair: { short: "Pair", axis: "pair", label: "two fields changed together" },
  move: { short: "Move", axis: "mov", label: "moved earlier" },
  drop: { short: "Drop", axis: "drop", label: "earlier inputs dropped" },
  duplicate: { short: "Redeliver", axis: "dup", label: "delivered twice" },
  clock: { short: "Clock", axis: "clk", label: "clock shifted" },
};

const SVGNS = "http://www.w3.org/2000/svg";
function s(tag, attrs, ...kids) {
  const el = document.createElementNS(SVGNS, tag);
  for (const [k, v] of Object.entries(attrs || {})) if (v != null && v !== false) el.setAttribute(k, v);
  for (const kid of kids.flat(Infinity)) if (kid != null && kid !== false) el.append(kid instanceof Node ? kid : String(kid));
  return el;
}

const invOf = (verdict) => (/^invariant_violated\(([^)]+)\)/.exec(verdict) || [])[1] || null;
const seqOf = (verdict) => { const m = /@(\d+)$/.exec(verdict); return m ? +m[1] : null; };
// roundLabel names a verdict in the few characters a round's tile has.
const ROUND = { invariant_violated: "broke", variant_failed: "variant", missing_invariant: "missing", baseline_mismatch: "baseline", still_failing: "failing" };
const roundLabel = (verdict) => { const v = verdict.replace(/[(@].*$/, ""); return ROUND[v] || v; };

// guessKind classifies a failure from an older report by its mutation text.
function guessKind(m) {
  m = m || "";
  if (/clock shifted/.test(m)) return "clock";
  if (/delivered twice/.test(m)) return "duplicate";
  if (/earlier, before/.test(m)) return "move";
  if (/dropped/.test(m)) return "drop";
  if ((m.match(/"[^"]+"/g) || []).length > 2 && /,/.test(m)) return "pair";
  return "field";
}

// checksOf returns the attempt's contract checks, oldest first.
export function checksOf(steps) {
  if (!Array.isArray(steps)) return [];
  return steps.filter((st) => st.contract).map((st, i) => ({ ...st.contract, n: i + 1, step: st }));
}

// model derives what the views draw from one check's report.
function model(c) {
  const failures = (c.variant_failures || []).map((f) => ({ ...f, kind: f.kind || guessKind(f.mutation), inv: invOf(f.verdict) }));
  const names = [];
  const add = (n) => { if (n && !names.includes(n)) names.push(n); };
  (c.invariants || []).forEach(add);
  (c.declared || []).forEach(add);
  (c.missing_invariants || []).forEach(add);
  add(invOf(c.verdict));
  failures.forEach((f) => add(f.inv));
  const kinds = c.variant_kinds;
  const ran = c.variant_candidates > 0;
  // tot is how many variants of a kind the old build failed the same way,
  // so the new build had something to hold against.
  const tot = {};
  for (const k of VKINDS) tot[k] = kinds && kinds[k] ? kinds[k].candidates - kinds[k].preexisting : null;
  const trips = {};
  for (const n of names) trips[n] = Object.fromEntries(VKINDS.map((k) => [k, 0]));
  let outright = 0; // variants that failed without breaking an invariant
  for (const f of failures) {
    if (f.inv) trips[f.inv][f.kind]++;
    else outright++;
  }
  const missing = new Set(c.missing_invariants || []);
  const declared = c.declared && c.declared.length ? new Set(c.declared) : null;
  const broke = invOf(c.verdict);
  const status = {};
  for (const n of names) {
    const tripped = VKINDS.some((k) => trips[n][k]);
    if (missing.has(n) || (declared && !declared.has(n))) status[n] = "missing";
    else if (n === broke) status[n] = "broke";
    else if (tripped) status[n] = "trip";
    else if (/^unreached/.test(c.verdict)) status[n] = "unreached";
    else if (!ran) status[n] = "traffic";
    else status[n] = "held";
  }
  return { c, names, failures, kinds, ran, tot, trips, outright, status, broke };
}

const STATUS = {
  held: "held on the traffic and every variant",
  trip: "a variant trips it",
  broke: "broke on the recorded traffic",
  traffic: "held on the traffic; variants didn't run",
  unreached: "held, but the feature isn't reached yet",
  missing: "not declared by the build",
};

function lamp(st, title) {
  return h("span", { class: `lamp ${st}`, title, "aria-label": title });
}

// verdictClass buckets a verdict for the round dots.
function verdictClass(v) {
  if (v === "pass") return "pass";
  if (/^variant_failed/.test(v)) return "trip";
  if (/^unreached/.test(v)) return "unreached";
  if (/^(unapproved|stale_lock)/.test(v)) return "pending";
  return "fail";
}

// renderInvariantBand is the strip under the attempt's card: the latest
// check's invariants as lamps, every round so far, and the streak toward the
// stop rule (the same outcome three checks in a row, or eight rounds).
export function renderInvariantBand(checks, onOpen) {
  if (!checks.length) return null;
  const last = checks[checks.length - 1];
  const m = model(last);
  const rounds = Math.max(8, checks.length);
  const streak = Math.min(3, last.streak || 1);
  return h("button", { type: "button", class: "inv-band", onclick: onOpen, title: "Open the Invariants tab" },
    h("span", { class: "inv-band-lamps" },
      m.names.length
        ? m.names.map((n) => h("span", { class: "inv-band-inv" }, lamp(m.status[n], `${n}: ${STATUS[m.status[n]]}`), h("span", { class: "m" }, n)))
        : h("span", { class: "faint" }, "This check's report doesn't list invariants.")),
    h("span", { class: "inv-band-meta" },
      h("span", { class: "rounds", "aria-label": `round ${last.round || checks.length} of at most 8` },
        Array.from({ length: rounds }, (_, i) => {
          const ck = checks[i];
          return h("i", { class: ck ? verdictClass(ck.verdict) : "", title: ck ? `round ${ck.round || i + 1}: ${ck.verdict}` : `round ${i + 1}: not run` });
        })),
      last.passed ? null : h("span", { class: "streak", title: `the same outcome ${streak} check${streak === 1 ? "" : "s"} in a row; the agent stops at 3` },
        Array.from({ length: 3 }, (_, i) => h("i", { class: i < streak ? "on" : "" })), `streak ${streak} of 3`)));
}

// radar draws one invariant's share of variants held, by kind. The radius
// runs from 50% held at the centre to 100% at the rim, so a dent shows which
// kind of variant breaks it.
const R = 40;
const pt = (i, r) => { const a = -Math.PI / 2 + (i * 2 * Math.PI) / VKINDS.length; return [+(r * Math.cos(a)).toFixed(1), +(r * Math.sin(a)).toFixed(1)]; };
const scale = (rate) => Math.max(0.04, (rate - 0.5) / 0.5) * R;
function rates(m, name) {
  return VKINDS.map((k) => {
    const t = m.trips[name][k];
    const n = m.tot[k];
    if (n == null) return t ? 0.5 : 1;
    return n > 0 ? Math.max(0, 1 - t / n) : 1;
  });
}
const poly = (rs) => rs.map((r, i) => pt(i, scale(r)).join(",")).join(" ");

function radarSVG(m, name, { prev, kindF, dotKind, size = 112, labels = true } = {}) {
  const rs = rates(m, name);
  const bad = m.status[name] === "trip" || m.status[name] === "broke";
  const off = !m.ran || m.status[name] === "missing";
  let rings = "";
  for (const f of [0.25, 0.5, 0.75, 1]) for (let i = 0; i <= VKINDS.length; i++) { const p = pt(i % VKINDS.length, R * f); rings += `${i ? "L" : "M"}${p[0]} ${p[1]} `; }
  let spokes = "";
  VKINDS.forEach((_, i) => { const p = pt(i, R); spokes += `M0 0 L${p[0]} ${p[1]} `; });
  const ki = kindF ? VKINDS.indexOf(kindF) : -1;
  const kids = [
    s("path", { d: rings, class: "r-grid" }),
    s("path", { d: spokes, class: "r-grid" }),
    ki >= 0 ? s("path", { d: `M0 0 L${pt(ki, R).join(" ")}`, class: "r-hi" }) : null,
    prev && prev.ran ? s("polygon", { points: poly(rates(prev, name)), class: "r-ghost" }) : null,
    off ? s("polygon", { points: poly(VKINDS.map(() => 1)), class: "r-off" })
      : s("polygon", { points: poly(rs), class: `r-poly ${bad ? "bad" : "ok"}` }),
  ];
  if (dotKind && !off) {
    const di = VKINDS.indexOf(dotKind);
    const p = pt(di, scale(rs[di]));
    kids.push(s("circle", { cx: p[0], cy: p[1], r: 5, class: "r-dot" }));
  }
  if (labels) {
    VKINDS.forEach((k, i) => {
      const [x, y] = pt(i, R + 14);
      kids.push(s("text", { x, y: y + 4, "text-anchor": "middle", class: `r-lbl ${m.trips[name][k] ? "bad" : ""}` }, VK[k].axis));
    });
  }
  const pad = labels ? 64 : 46;
  return s("svg", { width: size, height: Math.round(size * 0.88), viewBox: `${-pad} ${-pad + 6} ${pad * 2} ${(pad - 3) * 2}`, "aria-hidden": "true" }, kids);
}

function radarNote(m, name) {
  const st = m.status[name];
  if (st === "missing") return "not declared";
  if (!m.ran) return st === "broke" ? "broke on the traffic" : "variants didn't run";
  const dents = VKINDS.filter((k) => m.trips[name][k]);
  if (!dents.length) return st === "broke" ? "broke on the traffic" : "every kind held";
  return dents.map((k) => {
    const t = m.trips[name][k];
    return m.tot[k] != null ? `${VK[k].short.toLowerCase()} ${m.tot[k] - t}/${m.tot[k]}` : `${VK[k].short.toLowerCase()} ${t} tripped`;
  }).join(", ");
}

function ariaFor(m, name) {
  return `${name}: ${STATUS[m.status[name]]}. ${radarNote(m, name)}`;
}

// The new build's host command, from the check's tool call, for the replay
// command a person can paste.
function newHost(step) {
  const t = step.title;
  let m = /^kavach contract_check\s+(.+)$/.exec(t);
  if (m) return m[1];
  m = /--new\s+('([^']*)'|"([^"]*)"|(\S+))/.exec(t);
  return m ? (m[2] ?? m[3] ?? m[4]) : "";
}
const shq = (s) => /^[\w@%+=:,./-]+$/.test(s) ? s : `'${s.replace(/'/g, `'\\''`)}'`;

const calm = () => matchMedia("(prefers-reduced-motion: reduce)").matches;

// rerender draws the view again for a change that reshapes it (another
// invariant, round or page), crossfading so it doesn't snap.
function rerender() {
  if (!document.startViewTransition || calm() || document.hidden) return renderView();
  // A transition is skipped, and these reject, when another one starts.
  const vt = document.startViewTransition(renderView);
  vt.ready.catch(() => {});
  vt.finished.catch(() => {});
}

const SLIDE = { duration: 180, easing: "cubic-bezier(0.2, 0.8, 0.2, 1)" };

// slide grows el from nothing to its height, or shrinks it away and then
// calls done, so what sits below it moves instead of jumping.
function slide(el, open, done) {
  if (calm()) return done && done();
  const full = `${el.offsetHeight}px`;
  el.style.overflow = "hidden";
  const a = el.animate(open
    ? [{ height: "0px", opacity: 0 }, { height: full, opacity: 1 }]
    : [{ height: full, opacity: 1 }, { height: "0px", opacity: 0 }], SLIDE);
  a.onfinish = () => {
    el.style.overflow = "";
    if (done) done();
  };
}

// expand shows or hides the part of box that makeBody draws, in place: the
// rest of the view, its editors and its scroll stay as they are.
function expand(box, sel, open, makeBody) {
  for (const b of box.querySelectorAll(`:scope > ${sel}.shutting`)) b.remove();
  const cur = box.querySelector(`:scope > ${sel}`);
  if (open && !cur) {
    const b = makeBody();
    if (!b) return;
    box.append(b);
    slide(b, true);
  } else if (!open && cur) {
    cur.classList.add("shutting");
    slide(cur, false, () => cur.remove());
  }
}

// put appends the views that have something to show.
const put = (box, ...kids) => box.append(...kids.filter(Boolean));

// renderInvariants is the Invariants tab: what the checks found (Results),
// or what each invariant is (Definitions).
export function renderInvariants(t, checks) {
  const iv = state.iv;
  const defs = loadDefs(t, checks);
  const page = checks.length ? iv.page : "definitions";
  const box = h("div", { class: "invs", dataset: { key: state.stepsFor } });
  if (checks.length) {
    put(box, h("div", { class: "inv-pages" },
      h("span", { class: "seg", role: "group", "aria-label": "Show" },
        [["results", "Results"], ["definitions", "Definitions"]].map(([id, label]) => h("button", {
          type: "button", class: page === id ? "on" : "", "aria-pressed": String(page === id),
          onclick: () => { iv.page = id; rerender(); },
        }, label)))));
  }
  if (page === "definitions") {
    put(box, renderDefinitions(t, defs, checks));
    return box;
  }
  return renderResults(box, checks, defs, t);
}

function renderResults(box, checks, defs, t) {
  const iv = state.iv;
  const idx = iv.round != null && checks[iv.round] ? iv.round : checks.length - 1;
  const c = checks[idx];
  // The variants the checks saved, which the replay scene can open.
  const sv = savedVariants(t, checks.length);
  const rv = { t, sv, round: c.round || c.n };
  const m = model(c);
  const prev = idx > 0 ? model(checks[idx - 1]) : null;
  const name = m.names.includes(iv.inv) ? iv.inv : m.names.find((n) => m.status[n] === "broke" || m.status[n] === "trip") || m.names[0];
  const sel = m.failures.find((f) => f.id === iv.variant) || null;
  const kindF = iv.kind;

  const vparts = [];
  if (m.ran) {
    vparts.push(`${c.variants_passed} of ${c.variant_candidates - c.variants_preexisting} variants held`);
    if (m.failures.length) vparts.push(`${m.failures.length} tripped`);
    if (c.variants_preexisting) vparts.push(`${c.variants_preexisting} fail${c.variants_preexisting === 1 ? "s" : ""} on the old build too`);
  } else {
    vparts.push("variants run only once the traffic checks out");
  }
  put(box, h("div", { class: "inv-check" },
    h("div", { class: "inv-check-row" },
      h("span", { class: "m" }, h("span", { class: "peach" }, "kavach contract check "), h("b", {}, c.verdict)),
      checks.length > 1 ? h("span", { class: "inv-stepper" },
        h("button", { type: "button", class: "chip-btn", disabled: idx === 0, "aria-label": "Previous round", onclick: () => { iv.round = idx - 1; iv.variant = null; rerender(); } }, "‹"),
        h("span", { class: "faint" }, `round ${c.round || idx + 1}`),
        h("button", { type: "button", class: "chip-btn", disabled: idx === checks.length - 1, "aria-label": "Next round", onclick: () => { iv.round = idx + 1 === checks.length - 1 ? null : idx + 1; iv.variant = null; rerender(); } }, "›")) : null),
    h("span", { class: "dim" }, vparts.join(", ")),
    c.detail ? h("code", { class: "inv-detail" }, c.detail) : null));

  if (!m.names.length) {
    put(box, h("div", { class: "dim inv-empty" }, "This report doesn't name the build's invariants: it was printed by a Kavach older than per-invariant results. Rerun the check with the current kavach to see them."));
    put(box, renderSaved(m, rv));
    put(box, renderFeatureStrip(c, m));
    put(box, renderRounds(checks, idx, null));
    return box;
  }

  const view = iv.view;
  const head = h("div", { class: "inv-sec-head" },
    h("span", { class: "inv-sec" }, "Variants held, by kind"),
    h("span", { class: "seg", role: "group", "aria-label": "Show as" },
      ["radar", "table"].map((v) => h("button", { type: "button", class: view === v ? "on" : "", "aria-pressed": String(view === v), onclick: () => { iv.view = v; rerender(); } }, v === "radar" ? "Radar" : "Table"))));
  let body;
  if (view === "table") {
    body = h("table", { class: "inv-table" },
      h("thead", {}, h("tr", {}, h("th", {}, "invariant"), VKINDS.map((k) => h("th", { class: kindF === k ? "hi" : "" }, VK[k].short)))),
      h("tbody", {}, m.names.map((n) => h("tr", { class: n === name ? "on" : "", onclick: () => { iv.inv = n; rerender(); } },
        h("th", { class: "m" }, lamp(m.status[n], STATUS[m.status[n]]), n),
        VKINDS.map((k) => {
          const t = m.trips[n][k], tot = m.tot[k];
          const text = !m.ran ? "–" : tot == null ? (t ? `${t} ✕` : "✓") : `${tot - t}/${tot}`;
          return h("td", { class: `m ${t ? "bad" : ""} ${kindF === k ? "hi" : ""}` }, text);
        })))));
  } else {
    body = h("div", { class: "radars" }, m.names.map((n) => h("button", {
      type: "button", class: `radar ${n === name ? "on" : ""} ${m.status[n]}`, "aria-pressed": String(n === name), "aria-label": ariaFor(m, n),
      onclick: () => { iv.inv = n; iv.variant = null; rerender(); },
    },
      radarSVG(m, n, { prev, kindF, dotKind: sel && sel.inv === n ? sel.kind : null }),
      h("span", { class: "radar-name m" }, lamp(m.status[n], STATUS[m.status[n]]), n),
      h("span", { class: `radar-note ${m.status[n] === "trip" || m.status[n] === "broke" ? "bad" : ""}` }, radarNote(m, n)))));
  }
  put(box, h("section", { class: "inv-sec-box" }, head, body,
    view === "radar" ? h("p", { class: "inv-hint" }, m.kinds
      ? "The rim is every variant of a kind held, the centre half of them. A dent points at the kind that breaks the invariant; the dashed outline is the round before."
      : "This report doesn't count variants by kind, so a dent only marks a kind that tripped.") : null));

  if (m.ran) put(box, renderVariants(m, kindF, sel, rv));

  put(box, renderDetail(c, m, name, sel, defs, rv));
  put(box, renderSaved(m, rv));
  put(box, renderFeatureStrip(c, m));
  put(box, renderRounds(checks, idx, name));
  return box;
}

function renderVariants(m, kindF, sel, rv) {
  const iv = state.iv;
  const c = m.c;
  const chips = VKINDS.map((k) => {
    const fails = m.failures.filter((f) => f.kind === k).length;
    const cnt = m.kinds && m.kinds[k];
    if (!cnt && !fails) return null;
    const n = cnt ? cnt.candidates - cnt.preexisting : null;
    const on = kindF === k;
    return h("button", { type: "button", class: `kchip ${on ? "on" : ""}`, "aria-pressed": String(on), title: VK[k].label, onclick: () => { iv.kind = on ? null : k; rerender(); } },
      h("span", {}, VK[k].short),
      h("span", { class: `m ${fails ? "bad" : "faint"}` }, fails ? (n != null ? `${fails} of ${n} trip` : `${fails} trip`) : `${n} held`));
  });
  // One cell per variant: tripped ones carry an id and open their detail;
  // the report only counts the ones that held.
  const cells = [];
  const group = (k) => {
    const fs = m.failures.filter((f) => (k ? f.kind === k : true));
    for (const f of fs) cells.push({ f, k: f.kind });
    if (k && m.kinds && m.kinds[k]) {
      for (let i = 0; i < m.kinds[k].preexisting; i++) cells.push({ pre: true, k });
      for (let i = 0; i < m.kinds[k].passed; i++) cells.push({ held: true, k });
    }
  };
  if (m.kinds) VKINDS.forEach(group);
  else {
    group(null);
    for (let i = 0; i < c.variants_preexisting; i++) cells.push({ pre: true });
    for (let i = 0; i < c.variants_passed; i++) cells.push({ held: true });
  }
  const grid = h("div", { class: "vgrid", role: "group", "aria-label": "Variants of this check" }, cells.map((x) => {
    const dim = kindF && x.k !== kindF;
    if (x.f) {
      const saved = savedHere(rv, x.f);
      const cell = h("button", {
        type: "button", class: `vcell trip ${dim ? "dim" : ""}`, dataset: { v: x.f.id },
        onclick: () => {
          const on = iv.variant === x.f.id;
          if (on && saved) return openReplay(rv.t, rv.round, x.f.id);
          if (x.f.inv && x.f.inv !== iv.inv) {
            iv.variant = x.f.id;
            iv.inv = x.f.inv;
            return rerender();
          }
          showVariant(on ? null : x.f.id);
        },
      });
      cell.mark = (on) => {
        cell.classList.toggle("on", on);
        cell.title = `variant ${x.f.id}: ${x.f.mutation}${on && saved ? " (click again to replay it)" : ""}`;
        cell.setAttribute("aria-label", `Variant ${x.f.id}, ${VK[x.k].short}: ${x.f.verdict}${on && saved ? ". Press again to replay it" : ""}`);
        cell.setAttribute("aria-pressed", String(on));
      };
      cell.mark(!!sel && sel.id === x.f.id);
      return cell;
    }
    return h("span", { class: `vcell ${x.pre ? "pre" : "held"} ${dim ? "dim" : ""}`, title: x.pre ? "fails on the old build too, so it says nothing about the change" : "held" });
  }));
  return h("section", { class: "inv-sec-box" },
    h("div", { class: "inv-sec-head" }, h("span", { class: "inv-sec" }, "Variants by kind")),
    h("div", { class: "kchips" }, chips),
    grid,
    h("div", { class: "vlegend" },
      h("span", {}, h("i", { class: "vcell held" }), "held"),
      h("span", {}, h("i", { class: "vcell trip" }), "tripped: select one for its detail"),
      h("span", {}, h("i", { class: "vcell pre" }), "fails on the old build too")));
}

function renderDetail(c, m, name, sel, defs, rv) {
  const iv = state.iv;
  const st = m.status[name];
  const mine = m.failures.filter((f) => f.inv === name);
  const order = (c.declared && c.declared.length ? c.declared : m.names).indexOf(name);
  const kids = [
    h("div", { class: "inv-d-head" },
      h("span", { class: "m b" }, name),
      order >= 0 ? h("span", { class: "faint" }, `checked ${order + 1} of ${(c.declared && c.declared.length) || m.names.length}`) : null),
    h("span", { class: `inv-d-status ${st}` }, lamp(st, STATUS[st]), STATUS[st]),
  ];
  const def = defs.list && defs.list.find((d) => d.name === name);
  if (def) {
    kids.push(h("p", { class: "inv-d-doc" },
      def.doc || h("span", { class: "faint" }, "No comment in the source describes it."), " ",
      h("button", { type: "button", class: "link-btn", onclick: () => { iv.page = "definitions"; iv.inv = name; rerender(); } }, def.doc ? "Definition" : "Read its check")));
  }
  if (st === "broke" && c.detail) kids.push(h("code", { class: "inv-detail bad" }, c.detail));
  if (mine.length) {
    kids.push(h("div", { class: "vlist" }, mine.map((f) => {
      const on = !!sel && sel.id === f.id;
      const host = newHost(c.step);
      const more = () => f.detail || f.file ? h("div", { class: "vitem-more" },
        f.detail ? h("code", { class: "inv-detail bad" }, f.detail) : null,
        f.file ? h("div", { class: "vcmd" },
          h("span", { class: "faint" }, "Replay it in the task's container until the next check replaces it:"),
          h("code", { class: "m" }, `kavach replay ${shq(f.file)} --bin ${host ? shq(host) : "<new build>"}`)) : null) : null;
      const head = h("button", { type: "button", class: "vitem-head", "aria-expanded": String(on), onclick: () => showVariant(iv.variant === f.id ? null : f.id) },
        h("span", { class: "m b" }, `variant ${f.id}`),
        h("span", { class: "vkind" }, VK[f.kind] ? VK[f.kind].short : f.kind),
        h("span", { class: "m faint" }, f.verdict));
      const item = h("div", { class: `vitem ${on ? "on" : ""}`, dataset: { v: f.id } },
        head,
        h("div", { class: "vitem-row" },
          h("span", { class: "m vmut" }, f.mutation),
          replayBtn(rv, f)),
        on ? more() : null);
      item.mark = (on) => {
        item.classList.toggle("on", on);
        head.setAttribute("aria-expanded", String(on));
        expand(item, ".vitem-more", on, more);
      };
      return item;
    })));
  } else if (st === "held") {
    kids.push(h("span", { class: "dim" }, `Every variant held it${m.kinds ? `, across ${VKINDS.filter((k) => m.tot[k]).length} kinds` : ""}.`));
  }
  return h("section", { class: "inv-sec-box inv-d" }, kids);
}

// showVariant selects variant id (or none) of the invariant on screen: its
// cell in the grid and its item in the detail, which opens in place.
function showVariant(id) {
  state.iv.variant = id;
  for (const el of document.querySelectorAll(".vcell.trip[data-v], .vitem[data-v]")) {
    if (el.mark) el.mark(el.dataset.v === String(id));
  }
}

// replayBtn opens a saved variant in the replay scene. A variant missing
// from the saved list gets a note instead: the run's checks didn't save it.
// The report's own saved path counts too, as long as the API serves
// saved variants at all.
const savedHere = (rv, f, round = rv.round) => isSaved(rv.sv, round, f.id) || (!!f.saved && !rv.sv.loading && !rv.sv.error);
function replayBtn(rv, f, round = rv.round) {
  if (rv.sv.loading) return null;
  if (!savedHere(rv, f, round)) return h("span", { class: "faint vnosave" }, "Not saved: this run predates saved variants");
  return h("button", {
    type: "button", class: "rp-open", dataset: { replay: `${round}/${f.id}` },
    "aria-label": `Replay variant ${f.id} of round ${round}`, onclick: () => openReplay(rv.t, round, f.id),
  }, "Replay");
}

// renderSaved lists every variant the attempt's checks saved, the shown
// round's first. It includes variants that failed without breaking an
// invariant, which no invariant's detail lists, and rounds whose report the
// attempt's steps don't carry.
function renderSaved(m, rv) {
  const here = (v) => v.round === rv.round;
  const list = [...rv.sv.list.filter(here), ...rv.sv.list.filter((v) => !here(v)).sort((a, b) => b.round - a.round || a.id - b.id)];
  // Kavach saves a variant only when the new build fails it, so a build
  // that held every variant leaves nothing to replay; say so, rather than
  // leave the replay scene with no way in.
  if (!list.length) {
    if (rv.sv.loading) return null;
    return h("section", { class: "inv-sec-box" },
      h("div", { class: "inv-sec-head" }, h("span", { class: "inv-sec" }, "Replay")),
      h("span", { class: "dim" }, rv.sv.error
        ? `The saved variants could not be read: ${rv.sv.error}`
        : "No variant failed in any round, so none was saved to step through. A variant the new build fails is saved and can be replayed here."));
  }
  const byId = new Map(m.failures.map((f) => [f.id, f]));
  const n = list.filter(here).length;
  return h("section", { class: "inv-sec-box" },
    h("div", { class: "inv-sec-head" },
      h("span", { class: "inv-sec" }, "Saved variants"),
      h("span", { class: "faint" }, `${list.length} saved${n ? `, ${n} from round ${rv.round}` : ""}`)),
    h("div", { class: "vlist" }, list.map((v) => {
      const f = here(v) ? byId.get(v.id) : null;
      const vv = v.variant || {};
      const kind = f ? f.kind : guessKind(vv.mutation);
      return h("div", { class: "vitem" },
        h("div", { class: "vitem-row" },
          h("span", { class: "m b" }, `variant ${v.id}`),
          h("span", { class: "faint" }, `round ${v.round}`),
          VK[kind] ? h("span", { class: "vkind" }, VK[kind].short) : null,
          h("span", { class: `m vsaved-v ${v.error ? "bad" : "faint"}` }, v.error || (f && f.verdict) || v.verdict || (!v.has_result ? "no new-build result saved" : "") || vv.failure || ""),
          replayBtn(rv, { id: v.id }, v.round)),
        h("span", { class: "m vmut" }, vv.mutation || (f && f.mutation) || ""));
    })));
}

// renderFeatureStrip shows the contract's feature steps in order: which the
// build changed, which it hasn't reached, and where it, or a variant, broke.
function renderFeatureStrip(c, m) {
  const feats = c.feature_steps || [];
  if (!feats.length) return null;
  const changed = new Set(c.changed);
  const unreached = new Set(c.unreached || []);
  const failAt = seqOf(c.verdict);
  const tripAt = new Map();
  for (const f of m.failures) tripAt.set(f.feature_seq, (tripAt.get(f.feature_seq) || 0) + 1);
  const cells = feats.map((q) => {
    let cls = changed.has(q) ? "changed" : unreached.has(q) ? "unreached" : "same";
    if (q === failAt && !/^(unreached|variant_failed)/.test(c.verdict)) cls = "fail";
    else if (tripAt.has(q)) cls += " trip";
    const what = cls.startsWith("fail") ? "the check stopped here" : changed.has(q) ? "outputs changed" : unreached.has(q) ? "not reached: the old build's outputs" : "unchanged";
    return h("i", { class: cls, title: `@${q}: ${what}${tripAt.has(q) ? `; ${tripAt.get(q)} variant${tripAt.get(q) === 1 ? "" : "s"} of it trip` : ""}` });
  });
  const nc = feats.filter((q) => changed.has(q)).length;
  const nu = feats.filter((q) => unreached.has(q)).length;
  return h("section", { class: "inv-sec-box" },
    h("div", { class: "inv-sec-head" },
      h("span", { class: "inv-sec" }, "Feature steps"),
      h("span", { class: "faint" }, `${nc} of ${feats.length} changed${nu ? `, ${nu} not reached` : ""}`)),
    h("div", { class: "fstrip", "aria-label": `Feature steps @${feats[0]} to @${feats[feats.length - 1]}` }, cells),
    h("div", { class: "fstrip-ends m faint" }, h("span", {}, `@${feats[0]}`), h("span", {}, `@${feats[feats.length - 1]}`)));
}

// renderRounds lays every check of the attempt side by side, each as the
// selected invariant's radar with the round before it ghosted.
function renderRounds(checks, idx, name) {
  if (checks.length < 2) return null;
  const iv = state.iv;
  const ms = checks.map(model);
  return h("section", { class: "inv-sec-box" },
    h("div", { class: "inv-sec-head" },
      h("span", { class: "inv-sec" }, name ? `Rounds, for ${name}` : "Rounds"),
      h("span", { class: "faint" }, `${checks.length} check${checks.length === 1 ? "" : "s"}`)),
    h("div", { class: "rounds-row" }, checks.map((ck, i) => {
      const m = ms[i];
      const has = name && m.names.includes(name);
      return h("button", {
        type: "button", class: `round ${i === idx ? "on" : ""} ${verdictClass(ck.verdict)}`, "aria-pressed": String(i === idx),
        "aria-label": `Round ${ck.round || i + 1}: ${ck.verdict}`,
        onclick: () => { iv.round = i === checks.length - 1 ? null : i; iv.variant = null; rerender(); },
      },
        has ? radarSVG(m, name, { prev: i > 0 ? ms[i - 1] : null, size: 56, labels: false }) : h("span", { class: "round-blank" }),
        h("span", { class: "round-n" }, `R${ck.round || i + 1}`),
        h("span", { class: "round-v m", title: ck.verdict }, roundLabel(ck.verdict)));
    })));
}

// The definitions of the selected attempt's invariants, as the API reads
// them from the source that declares them (GET /api/trials/…/invariants).
const defsCache = new Map(); // trial key -> { list, sources, note } | { error } | "loading"

function loadDefs(t, checks) {
  const key = `${t.job}/${t.id}`;
  const got = defsCache.get(key);
  if (got && got !== "loading") return got;
  if (!got) {
    defsCache.set(key, "loading");
    const names = new Set();
    for (const c of checks) for (const n of [...(c.invariants || []), ...(c.declared || [])]) names.add(n);
    const q = names.size ? `?names=${encodeURIComponent([...names].join(","))}` : "";
    api(`/trials/${encodeURIComponent(t.job)}/${encodeURIComponent(t.id)}/invariants${q}`)
      .then((r) => defsCache.set(key, { list: r.invariants || [], sources: r.sources, note: r.note || "", source: r.source || "patch", sourceNote: r.source_note || "" }))
      .catch((e) => defsCache.set(key, { error: e.message }))
      .finally(() => { if (state.stepsFor === key) rerender(); });
  }
  return { loading: true };
}

const ROLE = { declaration: "Declared", check: "Checked by", fails: "Fails here" };

// What a person did to the code blocks, kept across renders: blocks opened
// or closed (block key -> open), and blocks showing their whole patch.
const codeOpen = new Map();
const patchOn = new Set();
// The patches, per trial and index into its sources:
// `${trial}|${i}` -> { text } | { error } | "loading".
const patchText = new Map();

const domId = (key) => "ic-" + key.replace(/[^\w-]/g, "_");
const fileName = (p) => p.split("/").pop();
const sourceName = (p) => /\/artifacts\/logs\/artifacts\/patch\.diff$/.test(p) ? "the agent's patch" : fileName(p);

// rerenderFocus renders again and gives focus back to the control id names,
// which the render replaced. A crossfade would draw it too late to focus.
function rerenderFocus(id) {
  renderView();
  const el = document.getElementById(id);
  if (el) el.focus({ preventScroll: true });
}

const trialURL = (t) => `/api/trials/${encodeURIComponent(t.job)}/${encodeURIComponent(t.id)}`;
const loadPatch = (ctx, i) => loadText(ctx, i, `${trialURL(ctx.t)}/invariants/source?i=${i}`);
const loadFile = (ctx, path) => loadText(ctx, `file|${path}`, `${trialURL(ctx.t)}/invariants/file?path=${encodeURIComponent(path)}`);

// loadText fetches a plain-text resource of the trial once, as sub names it.
function loadText(ctx, sub, url) {
  const k = `${ctx.trial}|${sub}`;
  const got = patchText.get(k);
  if (got && got !== "loading") return got;
  if (!got) {
    patchText.set(k, "loading");
    fetch(url)
      .then(async (r) => {
        const text = await r.text();
        if (!r.ok) {
          let msg = `${r.status} ${r.statusText}`;
          try { msg = JSON.parse(text).error || msg; } catch {}
          throw new Error(msg);
        }
        patchText.set(k, { text });
      })
      .catch((e) => patchText.set(k, { error: e.message }))
      .finally(() => { if (state.stepsFor === ctx.trial) rerender(); });
  }
  return { loading: true };
}

// patchEnd is the last line of the patch that shows a snippet of n lines
// starting at line from: lines the patch removes sit between them.
function patchEnd(text, from, n) {
  const lines = text.split("\n");
  let to = from;
  for (let l = from, seen = 0; l <= lines.length && seen < n; l++) {
    if (!lines[l - 1].startsWith("-")) seen++;
    to = l;
  }
  return to;
}

// preBlock is a snippet as plain text, without the indentation its lines
// share, with the lines that name the invariant marked: what shows while
// Monaco loads, or when it can't.
function preBlock(sn, name) {
  const lines = sn.text.split("\n");
  const indent = Math.min(...lines.filter((l) => l.trim()).map((l) => l.length - l.trimStart().length));
  return h("pre", {}, lines.map((l, i) => h("span", { class: `ln ${l.includes(name) ? "hit" : ""}` }, h("i", {}, sn.line + i), l.slice(indent) || " ")));
}

// patchPre is a whole patch as plain text, scrolled to its hit line once it
// is on screen.
function patchPre(text, from, to, hit) {
  const pre = h("pre", { class: "inv-patch-pre", tabindex: "0" }, text.split("\n").map((l, i) => {
    const n = i + 1;
    return h("span", { class: `ln ${n === hit ? "hit" : n >= from && n <= to ? "near" : ""}` }, h("i", {}, n), l || " ");
  }));
  requestAnimationFrame(() => {
    const el = pre.querySelector(".ln.hit");
    if (el) pre.scrollTop = el.offsetTop - pre.clientHeight / 2;
  });
  return pre;
}

function wholePatch(ctx, key, sn, src, hit) {
  const name = sourceName(sn.file);
  const head = h("div", { class: "inv-patch-head" },
    h("span", { title: sn.file }, name),
    h("span", { class: "m faint" }, `line ${hit}`));
  const p = loadPatch(ctx, src);
  if (p.loading) return h("div", { class: "inv-patch" }, head, h("div", { class: "dim inv-patch-msg" }, "Reading the patch…"));
  if (p.error) return h("div", { class: "inv-patch" }, head, h("div", { class: "bad inv-patch-msg" }, "Couldn't read the patch: " + p.error));
  const from = sn.file_line || hit;
  const to = patchEnd(p.text, from, sn.text.split("\n").length);
  const view = ctx.mon === "ready"
    ? patchEditor(`${key}|patch`, ctx.trial, src, { text: p.text, from, to, hit, label: `${name}, line ${hit}` })
    : cachedElement(`${key}|patch-pre`, ctx.trial, () => patchPre(p.text, from, to, hit));
  return h("div", { class: "inv-patch" }, head, view);
}

// realFile is the whole real file a snippet lives in, scrolled to and
// marking the snippet, in an editor (or plain text without Monaco).
function realFile(ctx, key, sn) {
  const name = fileName(sn.src_file);
  const head = h("div", { class: "inv-patch-head" },
    h("span", { title: sn.src_file }, sn.path),
    h("span", { class: "m faint" }, `line ${sn.src_hit}`));
  const f = loadFile(ctx, sn.path);
  if (f.loading) return h("div", { class: "inv-patch" }, head, h("div", { class: "dim inv-patch-msg" }, `Reading ${name}…`));
  if (f.error) return h("div", { class: "inv-patch" }, head, h("div", { class: "bad inv-patch-msg" }, `Couldn't read ${name}: ` + f.error));
  const from = sn.src_line || sn.src_hit;
  const to = from + sn.text.split("\n").length - 1;
  const view = ctx.mon === "ready"
    ? patchEditor(`${key}|file`, ctx.trial, `file|${sn.path}`, { text: f.text, from, to, hit: sn.src_hit, language: languageOf(sn.path), label: `${sn.path}, line ${sn.src_hit}` })
    : cachedElement(`${key}|file-pre`, ctx.trial, () => patchPre(f.text, from, to, sn.src_hit));
  return h("div", { class: "inv-patch" }, head, view);
}

// Arrow keys, Home and End move between a menu's options.
function menuKeys(e) {
  const opts = [...e.currentTarget.querySelectorAll(".cselect-opt")];
  let i = opts.indexOf(document.activeElement);
  if (e.key === "ArrowDown") i = (i + 1) % opts.length;
  else if (e.key === "ArrowUp") i = (i - 1 + opts.length) % opts.length;
  else if (e.key === "Home") i = 0;
  else if (e.key === "End") i = opts.length - 1;
  else return;
  e.preventDefault();
  opts[i].focus();
}

// openIn links to line of file (a patch) in the chosen editor, with a menu
// beside it that picks another.
function openIn(key, file, line) {
  const ed = CODE_EDITORS.find((e) => e.id === state.codeEditor) || CODE_EDITORS[0];
  const menu = domId(key) + "-ed";
  const open = state.openSelect === menu;
  return h("span", { class: `open-in cselect ${open ? "open" : ""}`, id: `cselect-${menu}` },
    h("a", { class: "open-in-link", href: editorURL(file, line, ed.id), title: `Opens ${fileName(file)} at line ${line} in ${ed.label}` }, `Open in ${ed.label}`),
    h("button", {
      type: "button", class: "open-in-pick", id: `${menu}-b`,
      "aria-label": "Choose the editor code opens in", title: "Choose the editor",
      "aria-haspopup": "listbox", "aria-expanded": String(open),
      onclick: (e) => {
        e.stopPropagation();
        state.openSelect = open ? "" : menu;
        rerender();
        const el = document.querySelector(`#cselect-${menu} .cselect-opt.sel`) || document.getElementById(`${menu}-b`);
        if (el) el.focus();
      },
    }, h("span", { "aria-hidden": "true" }, "▾")),
    open ? h("div", { class: "cselect-menu", role: "listbox", "aria-label": "Open code in", onclick: (e) => e.stopPropagation(), onkeydown: menuKeys },
      CODE_EDITORS.map((o) => {
        const sel = o.id === ed.id;
        return h("button", {
          type: "button", role: "option", "aria-selected": String(sel), class: `cselect-opt ${sel ? "sel" : ""}`,
          onclick: () => {
            state.openSelect = "";
            setCodeEditor(o.id);
            const b = document.getElementById(`${menu}-b`);
            if (b) b.focus();
          },
        },
          h("span", { class: "cselect-check", "aria-hidden": "true" }, sel ? "✓" : ""),
          h("span", { class: "cselect-main" }, h("span", { class: "cselect-lbl" }, o.label)));
      })) : null);
}

// codeBlock is one snippet of an invariant's code: a read-only editor of it
// (or plain text without Monaco), its whole patch on request, and a link
// that opens the patch at the line in a local editor.
function codeBlock(ctx, d, sn, i, defaultOpen) {
  const key = `${ctx.trial}|${d.name}|${sn.role}|${i}`;
  const id = domId(key);
  const isOpen = () => codeOpen.has(key) ? codeOpen.get(key) : defaultOpen;
  const open = isOpen();
  const lines = sn.text.split("\n");
  const hits = lines.flatMap((l, k) => l.includes(d.name) ? [k] : []);
  const src = sn.file ? ctx.defs.sources.indexOf(sn.file) : -1;
  const fileHit = sn.file_hit || sn.file_line;
  const real = !!sn.src_file;
  const showPatch = src >= 0 && fileHit > 0 && patchOn.has(key);
  const role = ROLE[sn.role] || sn.role;
  const makeBody = () => h("div", { class: "inv-code-body", id: `${id}-body` },
    real ? realFile(ctx, key, sn)
    : ctx.mon === "ready"
      ? snippetEditor(key, ctx.trial, { text: sn.text, language: languageOf(sn.path), firstLine: sn.line, hits, label: `${role} ${d.name}: ${sn.path} from line ${sn.line}` })
      : preBlock(sn, d.name),
    showPatch ? wholePatch(ctx, key, sn, src, fileHit) : null);
  const chev = h("span", { class: "inv-code-chev", "aria-hidden": "true" }, open ? "▾" : "▸");
  const toggle = h("button", {
    type: "button", class: "inv-code-toggle", id: `${id}-t`, "aria-expanded": String(open), "aria-controls": open ? `${id}-body` : null,
    onclick: () => {
      const now = !isOpen();
      codeOpen.set(key, now);
      block.classList.toggle("open", now);
      chev.textContent = now ? "▾" : "▸";
      toggle.setAttribute("aria-expanded", String(now));
      if (now) toggle.setAttribute("aria-controls", `${id}-body`);
      else toggle.removeAttribute("aria-controls");
      expand(block, ".inv-code-body", now, makeBody);
    },
  },
    chev,
    h("span", {}, role),
    h("span", { class: "m faint inv-code-path" }, `${sn.path}:${real ? sn.src_hit : sn.line + (hits[0] || 0)}`),
    sn.agent ? h("span", { class: "agent-tag" }, "agent") : null);
  const block = h("div", { class: `inv-code ${open ? "open" : ""}` },
    h("div", { class: "inv-code-head" },
      toggle,
      real || (sn.file && fileHit > 0) ? h("span", { class: "inv-code-acts" },
        src >= 0 ? h("button", {
          type: "button", class: `link-btn ${showPatch ? "on" : ""}`, id: `${id}-p`, "aria-pressed": String(showPatch),
          title: `${showPatch ? "Hide" : "Show"} ${sourceName(sn.file)} at line ${fileHit}`,
          onclick: () => {
            if (showPatch) patchOn.delete(key);
            else { patchOn.add(key); codeOpen.set(key, true); }
            rerenderFocus(`${id}-p`);
          },
        }, "Whole patch") : null,
        real ? openIn(key, sn.src_file, sn.src_hit) : openIn(key, sn.file, fileHit)) : null),
    open ? makeBody() : null);
  return block;
}

// renderDefinitions lists every invariant the service declares: what its
// source says it means, where it is declared, and the code that checks it.
function renderDefinitions(t, defs, checks) {
  const iv = state.iv;
  const trial = `${t.job}/${t.id}`;
  useTrial(trial);
  const mon = monacoStatus(rerender);
  if (defs.loading) return h("div", { class: "dim" }, "Reading the invariants from the source…");
  if (defs.error) return h("div", { class: "bad" }, "Couldn't read the invariants: " + defs.error);
  const last = checks.length ? model(checks[checks.length - 1]) : null;
  const sec = h("section", { class: "inv-defs" },
    h("p", { class: "inv-hint" },
      "Kavach knows an invariant only by its name; what it means is in the code that declares and checks it. ",
      defs.source === "patch"
        ? (defs.sources.length ? ["Read from ", defs.sources.map((p, i) => [i ? " and " : "", h("span", { class: "m", title: p }, sourceName(p))]), "."] : "")
        : `Showing the files as the agent left them, from ${defs.source === "container" ? "the trial's running container" : "the task's image with the agent's patch applied"}.`,
      defs.sourceNote ? h("span", { class: `inv-src-note ${defs.source === "patch" ? "" : "warn"}` }, " ", defs.sourceNote) : null));
  if (!defs.list.length) {
    sec.append(h("div", { class: "dim inv-empty" }, defs.note || "The source declares no invariants."));
    return sec;
  }
  const ctx = { t, trial, defs, mon };
  for (const d of defs.list) {
    const st = last && last.status[d.name];
    const on = d.name === iv.inv;
    const hasCheck = d.code.some((c) => c.role === "check");
    sec.append(h("article", { class: `inv-def ${on ? "on" : ""}`, id: `inv-def-${d.name}` },
      h("header", {},
        st ? lamp(st, STATUS[st]) : null,
        h("span", { class: "m b" }, d.name),
        d.agent ? h("span", { class: "agent-tag", title: "The agent's patch adds or changes lines that name or describe it" }, "changed by the agent") : null,
        st ? h("span", { class: "faint inv-def-st" }, STATUS[st]) : null),
      d.doc ? h("p", { class: "inv-def-doc" }, d.doc)
        : h("p", { class: "inv-def-doc faint" }, "No comment in the source describes it; its check below is its definition."),
      d.code.map((c, i) => codeBlock(ctx, d, c, i, on && (c.role === "check" || (!hasCheck && i === 0)) || (!d.doc && c.role === "check")))));
  }
  if (iv.inv) requestAnimationFrame(() => {
    const el = document.getElementById(`inv-def-${iv.inv}`);
    if (el && !el.dataset.shown) { el.dataset.shown = "1"; el.scrollIntoView({ block: "nearest" }); }
  });
  return sec;
}
