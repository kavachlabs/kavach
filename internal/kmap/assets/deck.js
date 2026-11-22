/* ==========================================================================
   kavach map: Replay deck
   Spliced by render.go before map.js and exposed as window.KavachDeck.
   Part 1 (derive*) is pure: it turns the Model into steps, hops and
   divergence and never touches the DOM. Part 2 (createDeck) is the view.
   Every DOM node is built with el()/svgEl()/textContent; no HTML strings.
   ========================================================================== */
(function (root) {
  "use strict";

  /* ---------------- Part 1: derivation ---------------- */

  const BAD_OUTCOMES = ["panic", "error", "invariant", "crash", "timeout", "diverged"];

  function isBadStep(step) {
    return Boolean(step && (BAD_OUTCOMES.includes(step.outcome) || (step.detected && step.detected.length)));
  }

  function indexModel(model) {
    const m = model || {};
    const nodes = m.nodes || [];
    const edges = m.edges || [];
    const nodeMap = new Map(nodes.map((n) => [n.id, n]));
    const out = new Map();
    const edgeByKey = new Map();
    const inEdges = new Map();
    edges.forEach((e, idx) => {
      if (!nodeMap.has(e.from) || !nodeMap.has(e.to)) return;
      if (!out.has(e.from)) out.set(e.from, []);
      out.get(e.from).push({ to: e.to, idx });
      if (!inEdges.has(e.to)) inEdges.set(e.to, []);
      inEdges.get(e.to).push({ from: e.from, idx });
      const k = e.from + ">" + e.to;
      if (!edgeByKey.has(k)) edgeByKey.set(k, idx);
    });
    const invNodes = new Map();
    nodes.forEach((n) => {
      if (n.kind !== "invariant" || !n.invariant) return;
      if (!invNodes.has(n.invariant)) invNodes.set(n.invariant, []);
      invNodes.get(n.invariant).push(n.id);
    });
    return { model: m, nodes, edges, nodeMap, out, inEdges, edgeByKey, invNodes, flows: m.flows || [] };
  }

  // A tokenized copy of a capsule replays the same incident as the original.
  const fixtureKey = (x) => String(x || "").split("/").pop().replace(/\.kavach$/, "").replace(/\.tokenized$/, "");

  function flowMatchesFixture(flow, f) {
    if (!flow || !flow.fixture) return false;
    const want = fixtureKey(flow.fixture);
    return want !== "" && (want === fixtureKey(f.path) || want === fixtureKey(f.label) || want === fixtureKey(f.id));
  }

  // The spec's step_names entry for fixture f, or null.
  function stepNamesFor(model, f) {
    const all = (model && model.step_names) || null;
    if (!all || !f) return null;
    for (const k of [f.path, f.label, f.id]) {
      const v = all[fixtureKey(k)];
      if (v) return v;
    }
    return null;
  }

  const CURRENCY_SIGN = { USD: "$", EUR: "€", GBP: "£", JPY: "¥", INR: "₹", CNY: "¥", AUD: "A$", CAD: "C$", NGN: "₦" };

  // Formats an integer amount in minor units as currency: 2000 with
  // minor_units 2 and USD is "$20", 2050 is "$20.50". Anything that isn't an
  // integer comes back unchanged.
  function formatMoney(value, money) {
    const raw = String(value === null || value === undefined ? "" : value).trim();
    const m = /^(-?)0*(\d+)$/.exec(raw);
    if (!money || !m) return raw;
    const units = Math.max(0, Math.min(8, Number(money.minor_units) || 0));
    const digits = m[2].padStart(units + 1, "0");
    const whole = digits.slice(0, digits.length - units).replace(/\B(?=(\d{3})+(?!\d))/g, ",");
    const frac = digits.slice(digits.length - units);
    const num = whole + (units && /[1-9]/.test(frac) ? "." + frac : "");
    const cur = String(money.currency || "");
    const sign = CURRENCY_SIGN[cur];
    const neg = m[1] && /[1-9]/.test(digits) ? "-" : "";
    if (sign) return neg + sign + num;
    return neg + (cur ? cur + " " : "") + num;
  }

  function isMoneyField(money, field) {
    return Boolean(money && Array.isArray(money.fields) && money.fields.includes(field));
  }

  // Shortest directed path from any of `sources` (in preference order) to `target`.
  function shortestPath(ix, sources, target) {
    const parent = new Map();
    const q = [];
    for (const s of sources) {
      if (parent.has(s)) continue;
      parent.set(s, null);
      q.push(s);
    }
    for (let h = 0; h < q.length; h++) {
      const cur = q[h];
      if (cur === target) {
        const path = [];
        let c = cur;
        while (c !== null) {
          path.unshift(c);
          c = parent.get(c);
        }
        return path;
      }
      for (const r of ix.out.get(cur) || []) {
        if (!parent.has(r.to)) {
          parent.set(r.to, cur);
          q.push(r.to);
        }
      }
    }
    return null;
  }

  function bugShort(n) {
    const t = String((n && n.title) || (n && n.id) || "");
    const m = /^([A-Za-z]{0,3}\d+[a-z]?)\s*[:\-–]/.exec(t);
    return m ? m[1] : "";
  }

  // Hops for one run's step. A hop is an edge walk from a node to one or more
  // nodes. Recorded hops touch something the capsule or host recorded (the
  // entry hit, an output hit, the host's invariant check); the rest come from
  // spec edges and flows and are marked inferred.
  function deriveHops(ix, f, step) {
    const hops = [];
    if (!step) return hops;
    const hits = step.hits || [];
    const entries = [];
    const sinks = [];
    const failures = [];
    for (const h of hits) {
      if (!ix.nodeMap.has(h.node)) continue;
      const list = h.role === "input" ? entries : h.role === "output" ? sinks : h.role === "failure" ? failures : null;
      if (list && !list.includes(h.node)) list.push(h.node);
    }
    const entrySet = new Set(entries);
    const sinkSet = new Set(sinks);
    const failSet = new Set(failures);
    const start = entries[0] || null;
    const seen = new Set();
    const visited = [];
    const visit = (id) => {
      const i = visited.indexOf(id);
      if (i >= 0) visited.splice(i, 1);
      visited.push(id);
    };
    const notes = new Map();
    const addHop = (from, to, extra) => {
      const key = from + ">" + to;
      if (seen.has(key)) return;
      seen.add(key);
      const edgeIdx = ix.edgeByKey.has(key) ? ix.edgeByKey.get(key) : -1;
      const e = edgeIdx >= 0 ? ix.edges[edgeIdx] : null;
      const toN = ix.nodeMap.get(to);
      const fromN = ix.nodeMap.get(from);
      const hop = {
        key,
        from,
        to: [to],
        edges: [{ key, from, to, idx: edgeIdx }],
        kind: entrySet.has(from) ? "entry" : sinkSet.has(to) ? "sink" : "flow",
        recorded: entrySet.has(from) || sinkSet.has(to),
        bug: (toN && toN.kind === "bug" ? to : null) || (fromN && fromN.kind === "bug" ? from : null),
        bugHit: Boolean((toN && toN.kind === "bug" && failSet.has(to)) || (fromN && fromN.kind === "bug" && failSet.has(from))),
        label: (e && (e.label || e.kind)) || "",
        notes: [],
      };
      if (extra) Object.assign(hop, extra);
      hops.push(hop);
    };
    const walkTo = (cursor, w) => {
      if (w === cursor) return w;
      const direct = ix.edgeByKey.has(cursor + ">" + w);
      if (visited.includes(w)) {
        if (direct) addHop(cursor, w);
        visit(w);
        return w;
      }
      let path = cursor ? shortestPath(ix, [cursor], w) : null;
      if (!path) path = shortestPath(ix, visited.slice().reverse(), w);
      if (!path) return cursor;
      for (let i = 0; i + 1 < path.length; i++) {
        addHop(path[i], path[i + 1]);
        visit(path[i]);
        visit(path[i + 1]);
      }
      return w;
    };

    if (start) {
      visit(start);
      let cursor = start;
      // Flows that explain this step: tied to this fixture and running through
      // a failure the step actually hit. Bug-free flows guide any step that
      // passes through their nodes without hitting an unhit bug.
      const explaining = [];
      for (const fl of ix.flows) {
        const ids = (fl.steps || []).map((s) => s.node).filter((id) => ix.nodeMap.has(id));
        if (!ids.length || !ids.includes(start) && !ids.some((id) => failSet.has(id))) continue;
        const bugIds = ids.filter((id) => (ix.nodeMap.get(id) || {}).kind === "bug");
        const hitsBug = bugIds.some((id) => failSet.has(id));
        if (bugIds.length && (!hitsBug || bugIds.some((id) => !failSet.has(id)))) continue;
        if (fl.fixture && !flowMatchesFixture(fl, f)) continue;
        const order = hitsBug ? Math.min(...bugIds.map((id) => failures.indexOf(id)).filter((x) => x >= 0)) : -1;
        explaining.push({ fl, ids, order, explains: hitsBug });
      }
      explaining.sort((a, b) => a.order - b.order);
      for (const ex of explaining) {
        for (const s of ex.fl.steps || []) {
          if (!ix.nodeMap.has(s.node) || !s.text || !ex.explains) continue;
          if (!notes.has(s.node)) notes.set(s.node, []);
          notes.get(s.node).push({ flow: ex.fl.title || ex.fl.id, text: s.text });
        }
        for (const id of ex.ids) cursor = walkTo(cursor, id);
      }
      for (const id of failures) if (!visited.includes(id)) cursor = walkTo(cursor, id);
      for (const id of sinks) {
        if (visited.includes(id)) continue;
        const before = hops.length;
        cursor = walkTo(cursor, id);
        if (hops.length === before && !visited.includes(id)) {
          addHop(cursor, id);
          visit(id);
          cursor = id;
        }
      }
      if (!hops.length && sinks.length === 0 && failures.length === 0) {
        // Only the entry fired; nothing else to walk.
      }
    }

    // The host's invariant check, when it ran.
    const inv = step.invariants || {};
    const checked = Object.keys(inv).filter((k) => inv[k] === "pass" || inv[k] === "fail");
    if (checked.length) {
      const edgesOut = [];
      const targets = [];
      const srcCount = new Map();
      for (const name of checked) {
        for (const nid of ix.invNodes.get(name) || []) {
          const ins = ix.inEdges.get(nid) || [];
          let pick = ins.find((r) => visited.includes(r.from) && (ix.edges[r.idx].kind === "checks")) || ins.find((r) => visited.includes(r.from)) || null;
          let from = pick ? pick.from : sinks[0] || visited[visited.length - 1] || null;
          if (!from) continue;
          const key = from + ">" + nid;
          edgesOut.push({ key, from, to: nid, idx: ix.edgeByKey.has(key) ? ix.edgeByKey.get(key) : -1, result: inv[name] });
          targets.push(nid);
          srcCount.set(from, (srcCount.get(from) || 0) + 1);
        }
      }
      if (edgesOut.length) {
        let from = edgesOut[0].from;
        for (const [k, v] of srcCount) if (v > (srcCount.get(from) || 0)) from = k;
        const fails = checked.filter((k) => inv[k] === "fail").length;
        hops.push({
          key: "check:" + targets.slice().sort().join(","),
          from,
          to: targets,
          edges: edgesOut,
          kind: "check",
          recorded: true,
          bug: null,
          bugHit: false,
          check: true,
          fails,
          label: fails ? fails + " of " + checked.length + " invariants fail" : checked.length + " invariant" + (checked.length === 1 ? "" : "s") + " pass",
          notes: [],
        });
      }
    }

    for (const hp of hops) {
      for (const id of hp.to) for (const n of notes.get(id) || []) hp.notes.push(n);
      if (hp.kind === "entry") for (const n of notes.get(hp.from) || []) hp.notes.push(n);
    }
    return hops;
  }

  function parseOutput(o) {
    const data = String((o && o.data) || "");
    const fields = [];
    let label = "";
    const t = data.trim();
    if (t[0] === "{") {
      try {
        const obj = JSON.parse(t);
        for (const [k, v] of Object.entries(obj)) {
          if (v === null || typeof v !== "object") fields.push([k, String(v)]);
          else fields.push([k, JSON.stringify(v)]);
        }
        return { label: (o && o.sink) || "", fields, raw: data };
      } catch (_) {}
    }
    if (/[=]/.test(t) && !/\s{2,}/.test(t)) {
      const lab = [];
      // Tokens of the form key=value become fields; anything before the first
      // key=value token becomes the row label.
      for (const seg of t.split(":")) {
        const m = /^([A-Za-z_][\w.\-]*)=(.*)$/.exec(seg);
        if (m) fields.push([m[1], m[2]]);
        else if (!fields.length) lab.push(seg);
        else if (fields.length) fields[fields.length - 1][1] += ":" + seg;
      }
      label = lab.join(":").replace(/->/g, " → ");
      if (!fields.length) label = "";
    }
    return { label, fields, raw: data };
  }

  function invSummary(step) {
    const inv = (step && step.invariants) || {};
    const vals = Object.values(inv);
    if (!vals.length) return { text: "", tone: "" };
    const pass = vals.filter((v) => v === "pass").length;
    const fail = vals.filter((v) => v === "fail").length;
    const skipped = vals.filter((v) => v === "skipped").length;
    if (fail) return { text: fail + " of " + vals.length + " fail", tone: "bad" };
    if (pass === vals.length) return { text: pass + "/" + vals.length + " pass", tone: "good" };
    if (pass) return { text: pass + "/" + vals.length + " pass", tone: "" };
    if (skipped) return { text: "skipped", tone: "bad" };
    return { text: "not checked", tone: "" };
  }

  function divergenceReasons(a, b) {
    const r = [];
    if (!a || !b) return a || b ? ["only one build ran this step"] : r;
    if (a.outcome !== b.outcome) r.push("outcome");
    else if ((a.message || "") !== (b.message || "")) r.push("message");
    const ao = (a.outputs || []).map((o) => (o.sink || "") + ":" + (o.data || ""));
    const bo = (b.outputs || []).map((o) => (o.sink || "") + ":" + (o.data || ""));
    if (ao.length !== bo.length || ao.some((x, i) => x !== bo[i])) r.push("outputs");
    if (JSON.stringify(a.invariants || {}) !== JSON.stringify(b.invariants || {})) r.push("invariants");
    if (JSON.stringify((a.detected || []).slice().sort()) !== JSON.stringify((b.detected || []).slice().sort())) r.push("detected");
    return r;
  }

  function findStringField(obj, keys, depth) {
    if (!obj || typeof obj !== "object" || depth > 3) return "";
    for (const k of keys) if (typeof obj[k] === "string" && obj[k].trim()) return obj[k].trim();
    const vals = Array.isArray(obj) ? obj : Object.values(obj);
    for (const v of vals) {
      const s = findStringField(v, keys, depth + 1);
      if (s) return s;
    }
    return "";
  }

  function stepHeadline(step, entryNode) {
    const keys = ["description", "summary", "title"];
    const input = String((step && step.input) || "");
    let s = "";
    try {
      s = findStringField(JSON.parse(input), keys, 0);
    } catch (_) {
      const m = /"(?:description|summary|title)"\s*:\s*"((?:[^"\\]|\\.)*)"/.exec(input);
      if (m) {
        try {
          s = JSON.parse('"' + m[1] + '"');
        } catch (_) {
          s = m[1];
        }
      }
    }
    if (s) return s;
    if (entryNode && entryNode.plain) return entryNode.plain;
    return (step && (step.position || step.source)) || "";
  }

  function stepShortName(step, entryNode) {
    const title = String((entryNode && entryNode.title) || (step && step.position) || "");
    const paren = /\(([^)]+)\)\s*$/.exec(title);
    if (paren) return paren[1];
    const path = title.replace(/^[A-Z]+\s+/, "").split("?")[0];
    const segs = path.split("/").filter(Boolean);
    for (let i = segs.length - 1; i >= 0; i--) {
      const sg = segs[i];
      if (/\d/.test(sg) && sg.length > 6) continue;
      if (/^[{:]/.test(sg)) continue;
      return sg;
    }
    return (step && step.source) || "step";
  }

  function nodeShort(n) {
    if (!n) return "";
    const b = n.kind === "bug" ? bugShort(n) : "";
    if (b) return b;
    const t = String(n.title || n.id);
    return t.length > 22 ? t.slice(0, 21) + "…" : t;
  }

  function findRuns(f) {
    const runs = (f && f.runs) || [];
    const prod = runs.find((r) => r.mode === "recorded") || null;
    const nw = runs.find((r) => r.build === "new") || runs.find((r) => r.id === "new") || null;
    const old = runs.find((r) => r.build === "old") || runs.find((r) => r.id === "old") || runs.find((r) => r.mode === "replay" && r !== nw) || null;
    return { prod, old, new: nw };
  }

  function makeView(ix, f, run, step) {
    if (!run || !step) return null;
    const hops = deriveHops(ix, f, step);
    return {
      run,
      step,
      hops,
      bad: isBadStep(step),
      outputs: (step.outputs || []).map(parseOutput),
      inv: invSummary(step),
      failNodes: (step.hits || []).filter((h) => h.role === "failure" && ix.nodeMap.has(h.node)).map((h) => h.node).filter((v, i, a) => a.indexOf(v) === i),
    };
  }

  function deriveFixture(model, f, ixIn) {
    const ix = ixIn || indexModel(model);
    const runs = findRuns(f);
    const bySeq = (r) => new Map(((r && r.steps) || []).map((s) => [s.seq, s]));
    const maps = { prod: bySeq(runs.prod), old: bySeq(runs.old), new: bySeq(runs.new) };
    const seqSet = new Set();
    for (const k of ["prod", "old", "new"]) for (const s of maps[k].keys()) seqSet.add(s);
    const seqs = Array.from(seqSet).sort((a, b) => a - b);
    const names = stepNamesFor(ix.model, f);
    const steps = seqs.map((seq, i) => {
      const views = {
        prod: makeView(ix, f, runs.prod, maps.prod.get(seq)),
        old: makeView(ix, f, runs.old, maps.old.get(seq)),
        new: makeView(ix, f, runs.new, maps.new.get(seq)),
      };
      const ref = views.old || views.prod || views.new;
      const refStep = ref.step;
      const entryHit = (refStep.hits || []).find((h) => h.role === "input");
      const entryNode = entryHit ? ix.nodeMap.get(entryHit.node) : null;
      const reasons = runs.old && runs.new ? divergenceReasons(views.old && views.old.step, views.new && views.new.step) : [];
      const diverge = reasons.length > 0;
      let prefix = 0;
      let firstDiv = -1;
      if (diverge && views.old && views.new) {
        const a = views.old.hops;
        const b = views.new.hops;
        while (prefix < a.length && prefix < b.length && a[prefix].key === b[prefix].key) prefix++;
        if (prefix < a.length) firstDiv = prefix;
        else {
          const sinkIdx = a.findIndex((h) => h.kind === "sink");
          const chkIdx = a.findIndex((h) => h.kind === "check");
          firstDiv = reasons.includes("outputs") && sinkIdx >= 0 ? sinkIdx : chkIdx >= 0 ? chkIdx : Math.max(0, a.length - 1);
        }
        prefix = Math.min(prefix, firstDiv);
      }
      return {
        i,
        seq,
        position: refStep.position || refStep.source || "",
        name: (names && names[String(seq)]) || stepShortName(refStep, entryNode),
        headline: stepHeadline(refStep, entryNode),
        entry: entryNode ? entryNode.id : null,
        views,
        diverge,
        reasons,
        prefix,
        firstDiv,
      };
    });
    let firstDivStep = steps.findIndex((s) => s.diverge);
    if (firstDivStep < 0 && runs.new && runs.new.divergence && runs.new.divergence.seq !== undefined) {
      firstDivStep = steps.findIndex((s) => s.seq === runs.new.divergence.seq);
    }
    return { fixture: f, runs, steps, firstDivStep, ix };
  }

  /* ---------------- Part 2: view ---------------- */

  function createDeck(env) {
    const el = env.el;
    const svgEl = env.svgEl;
    const state = env.state;
    const W = 210;
    const H = 50;
    const HOP_MS = 430;
    const HOLD = 2.2;
    const BIG = 1e9;
    const RM = root.matchMedia ? root.matchMedia("(prefers-reduced-motion: reduce)") : { matches: false };
    const ds = { t: BIG, playing: false, onlyStep: false, node: null, raw: false, openPrefix: false };
    let D = null;
    let dom = null;
    let raf = 0;
    let last = 0;
    let drag = false;

    const rm = () => Boolean(RM.matches);
    const cur = () => {
      if (!D || !D.steps.length) return null;
      let i = D.steps.findIndex((s) => s.seq === state.replaySeq);
      if (i < 0) i = D.firstDivStep >= 0 ? D.firstDivStep : 0;
      return D.steps[i];
    };
    const curIdx = () => {
      const s = cur();
      return s ? s.i : -1;
    };
    const level = () => state.replayLevel || 0;
    const hopSel = () => (state.replayHop === undefined || state.replayHop === null ? -1 : state.replayHop);
    const setLevel = (l) => {
      state.replayLevel = l;
    };
    const setHop = (h) => {
      state.replayHop = h;
    };

    function getBuild() {
      if (!D) return "old";
      const R = D.runs;
      if (state.compareMode && R.old && R.new) return "both";
      const run = ((D.fixture && D.fixture.runs) || []).find((r) => r.id === state.replayRunId);
      if (run) {
        if (run === R.prod) return "prod";
        if (run === R.new) return "new";
        if (run === R.old) return "old";
      }
      return R.old ? "old" : R.prod ? "prod" : "new";
    }
    function traceBuild() {
      const b = getBuild();
      if (b === "both") return "old";
      if (b === "prod") return D.runs.old ? "old" : "prod";
      return b;
    }
    const viewOf = (s, b) => (s ? s.views[b] || null : null);
    const hopsOf = (s, b) => {
      const v = viewOf(s, b);
      return v ? v.hops : [];
    };
    const lenOf = (s) => Math.max(1, hopsOf(s, "old").length, hopsOf(s, "new").length, hopsOf(s, "prod").length);

    function reveal(s, b) {
      const hs = hopsOf(s, b);
      if (level() > 0 && hopSel() >= 0 && !ds.playing) return { n: Math.min(hopSel() + 1, hs.length), act: -1, frac: 1, sel: hopSel() };
      const t = Math.min(ds.t, hs.length);
      if (t >= hs.length) return { n: hs.length, act: -1, frac: 1, sel: -1 };
      const k = Math.floor(t);
      return { n: k, act: k, frac: rm() ? 1 : t - k, sel: -1 };
    }

    /* ----- playback ----- */
    function tick(now) {
      const dt = Math.min(64, now - last);
      last = now;
      const s = cur();
      if (!s) return;
      // Under reduced motion reveal() rounds each hop to whole, so lines
      // appear one at a time without sliding.
      ds.t += dt / HOP_MS;
      if (level() > 0) {
        const L = hopsOf(s, traceBuild()).length;
        setHop(Math.max(0, Math.min(L - 1, Math.floor(ds.t))));
        if (ds.t >= L) {
          ds.t = BIG;
          setHop(Math.max(0, L - 1));
          ds.playing = false;
        }
      } else if (ds.t >= lenOf(s) + HOLD) {
        if (!ds.onlyStep && s.i < D.steps.length - 1) {
          state.replaySeq = D.steps[s.i + 1].seq;
          ds.t = 0;
          env.syncHash();
        } else {
          ds.t = BIG;
          ds.playing = false;
          ds.onlyStep = false;
        }
      }
      draw();
      if (ds.playing) raf = requestAnimationFrame(tick);
      else env.syncHash();
    }
    function start() {
      ds.playing = true;
      last = performance.now();
      cancelAnimationFrame(raf);
      raf = requestAnimationFrame(tick);
    }
    function stop() {
      ds.playing = false;
      ds.onlyStep = false;
      cancelAnimationFrame(raf);
    }
    function play() {
      const s = cur();
      if (!s) return;
      if (ds.playing) {
        stop();
        if (level() > 0) setHop(Math.max(0, Math.min(hopsOf(s, traceBuild()).length - 1, Math.floor(ds.t))));
        draw();
        env.syncHash();
        return;
      }
      const lastI = D.steps.length - 1;
      if (level() > 0) {
        const L = hopsOf(s, traceBuild()).length;
        if (!L) return;
        ds.t = hopSel() >= 0 && hopSel() < L - 1 ? hopSel() + 1 : 0;
        setHop(Math.floor(ds.t));
      } else if (s.i === lastI && ds.t >= lenOf(s)) {
        state.replaySeq = D.steps[0].seq;
        ds.t = 0;
      } else if (ds.t >= lenOf(s) && s.i < lastI) {
        state.replaySeq = D.steps[s.i + 1].seq;
        ds.t = 0;
      }
      start();
      draw();
    }
    function defaultHop(s) {
      return s.diverge && traceBuild() === "old" && s.firstDiv >= 0 ? s.firstDiv : 0;
    }
    function seek(i) {
      stop();
      if (!D.steps.length) return;
      const s = D.steps[Math.max(0, Math.min(D.steps.length - 1, i))];
      state.replaySeq = s.seq;
      ds.t = BIG;
      ds.node = null;
      ds.openPrefix = false;
      if (level() > 0) {
        setHop(defaultHop(s));
        if (level() > 1) setLevel(1);
      }
      draw();
      env.syncHash();
    }
    function selectHop(k, lvl) {
      stop();
      const s = cur();
      const L = hopsOf(s, traceBuild()).length;
      if (!L) return;
      setHop(Math.max(0, Math.min(L - 1, k)));
      setLevel(lvl || Math.max(1, level()));
      ds.t = BIG;
      if (hopSel() < s.prefix) ds.openPrefix = true;
      draw();
      env.syncHash();
    }
    function deeper() {
      stop();
      const s = cur();
      if (!s) return;
      const L = hopsOf(s, traceBuild()).length;
      if (!L) return;
      if (level() === 0) {
        setLevel(1);
        setHop(defaultHop(s));
        if (hopSel() < s.prefix) ds.openPrefix = true;
      } else if (level() === 1) setLevel(2);
      draw();
      env.syncHash();
    }
    function shallower() {
      stop();
      if (level() === 2) setLevel(1);
      else if (level() === 1) {
        setLevel(0);
        setHop(-1);
        ds.raw = false;
      } else if (ds.node) ds.node = null;
      draw();
      env.syncHash();
    }
    function watchSplit() {
      if (!D || D.firstDivStep < 0) return;
      stop();
      setLevel(0);
      setHop(-1);
      state.replaySeq = D.steps[D.firstDivStep].seq;
      ds.t = 0;
      ds.node = null;
      start();
      ds.onlyStep = true;
      draw();
      env.syncHash();
    }
    function restart() {
      if (!D.steps.length) return;
      stop();
      setLevel(0);
      setHop(-1);
      state.replaySeq = D.steps[0].seq;
      ds.t = 0;
      start();
      draw();
    }
    function buildAvailable(b) {
      if (!D) return false;
      const R = D.runs;
      if (b === "both") return Boolean(R.old && R.new);
      return Boolean(R[b]);
    }
    function setBuild(b) {
      if (!buildAvailable(b)) return;
      const R = D.runs;
      if (b === "both") {
        state.compareMode = true;
        state.replayRunId = R.old.id;
      } else {
        state.compareMode = false;
        state.replayRunId = R[b].id;
      }
      if (level() > 0) {
        const s = cur();
        const L = hopsOf(s, traceBuild()).length;
        setHop(Math.min(hopSel(), L - 1));
        if (hopSel() < 0) setHop(defaultHop(s));
        if (L === 0) {
          setLevel(0);
          setHop(-1);
        }
      }
      draw();
      env.syncHash();
    }
    const back = () => (level() > 0 ? selectHop(hopSel() - 1) : seek(curIdx() - 1));
    const fwd = () => (level() > 0 ? selectHop(hopSel() + 1) : seek(curIdx() + 1));

    /* ----- layout ----- */
    function layout(ix, aspect) {
      const ids = ix.nodes.map((n) => n.id);
      const ord = new Map(ids.map((id, i) => [id, i]));
      const grpIdx = new Map(((ix.model.groups || []).map((g, i) => [g.id, i])));
      const succ = new Map(ids.map((id) => [id, []]));
      for (const e of ix.edges) if (succ.has(e.from) && succ.has(e.to) && e.from !== e.to) succ.get(e.from).push(e.to);
      const mark = new Map();
      const back = new Set();
      const dfs = (u) => {
        mark.set(u, 1);
        for (const v of succ.get(u)) {
          const m = mark.get(v);
          if (m === 1) back.add(u + ">" + v);
          else if (!m) dfs(v);
        }
        mark.set(u, 2);
      };
      const kindOf = (id) => (ix.nodeMap.get(id) || {}).kind;
      ids.slice().sort((a, b) => (kindOf(a) === "entry" ? 0 : 1) - (kindOf(b) === "entry" ? 0 : 1) || ord.get(a) - ord.get(b)).forEach((id) => {
        if (!mark.get(id)) dfs(id);
      });
      const preds = new Map(ids.map((id) => [id, []]));
      const indeg = new Map(ids.map((id) => [id, 0]));
      for (const [u, vs] of succ) for (const v of vs) if (!back.has(u + ">" + v)) {
        preds.get(v).push(u);
        indeg.set(v, indeg.get(v) + 1);
      }
      const layer = new Map(ids.map((id) => [id, 0]));
      const q = ids.filter((id) => indeg.get(id) === 0);
      for (let h = 0; h < q.length; h++) {
        const u = q[h];
        for (const v of succ.get(u)) {
          if (back.has(u + ">" + v)) continue;
          layer.set(v, Math.max(layer.get(v), layer.get(u) + 1));
          indeg.set(v, indeg.get(v) - 1);
          if (indeg.get(v) === 0) q.push(v);
        }
      }
      // Isolated nodes sit with their group's typical column.
      const isolated = ids.filter((id) => !preds.get(id).length && !succ.get(id).length);
      const nCols = Math.max(0, ...layer.values()) + 1;
      const cols = Array.from({ length: nCols }, () => []);
      for (const id of ids) if (!isolated.includes(id)) cols[layer.get(id)].push(id);
      for (const id of isolated) {
        const g = (ix.nodeMap.get(id) || {}).group;
        const mates = ids.filter((x) => x !== id && !isolated.includes(x) && (ix.nodeMap.get(x) || {}).group === g);
        const L = mates.length ? Math.round(mates.reduce((a, x) => a + layer.get(x), 0) / mates.length) : 0;
        layer.set(id, L);
        cols[L].push(id);
      }
      const ROW = H + 26;
      const STEP_X = W + 46;
      const pos = new Map();
      cols.forEach((col, ci) => {
        col.sort((a, b) => (grpIdx.get((ix.nodeMap.get(a) || {}).group) || 0) - (grpIdx.get((ix.nodeMap.get(b) || {}).group) || 0) || ord.get(a) - ord.get(b));
        if (ci > 0) {
          const want = new Map(col.map((id, k) => {
            const ps = preds.get(id).filter((p) => pos.has(p));
            return [id, ps.length ? ps.reduce((a, p) => a + pos.get(p).y, 0) / ps.length : k * ROW];
          }));
          col.sort((a, b) => want.get(a) - want.get(b) || ord.get(a) - ord.get(b));
          let y = -Infinity;
          const ys = col.map((id) => {
            y = Math.max(want.get(id), y + ROW);
            return y;
          });
          // Pull the column back up so it is centred on what it wanted.
          const shift = (ys.reduce((a, v) => a + v, 0) - col.reduce((a, id) => a + want.get(id), 0)) / Math.max(1, col.length);
          col.forEach((id, k) => pos.set(id, { x: 20 + ci * STEP_X, y: ys[k] - shift }));
        } else col.forEach((id, k) => pos.set(id, { x: 20, y: k * ROW }));
      });
      let minY = Infinity;
      let maxY = -Infinity;
      for (const p of pos.values()) {
        minY = Math.min(minY, p.y);
        maxY = Math.max(maxY, p.y);
      }
      if (!isFinite(minY)) {
        minY = 0;
        maxY = 0;
      }
      const w = 20 + Math.max(1, nCols) * STEP_X;
      // Wide, shallow graphs left most of the panel empty. Spread the rows so
      // the drawing roughly matches the panel's shape (aspect = height / width).
      const span = maxY - minY;
      const wantH = w * (aspect || 0.5);
      const k = span > 0 ? Math.max(1, Math.min(2.4, (wantH - H - 48) / span)) : 1;
      for (const p of pos.values()) p.y = (p.y - minY) * k + 24;
      return { pos, w, h: span * k + H + 48 };
    }

    function edgeD(A, B) {
      if (Math.abs(A.x - B.x) < 5) {
        const x = A.x + W;
        const y1 = A.y + H / 2;
        const y2 = B.y + H / 2;
        const bx = x + 22;
        return "M" + x + "," + y1 + " C" + bx + "," + y1 + " " + bx + "," + y2 + " " + x + "," + y2;
      }
      if (B.x > A.x) {
        const x1 = A.x + W;
        const y1 = A.y + H / 2;
        const x2 = B.x;
        const y2 = B.y + H / 2;
        const m = (x1 + x2) / 2;
        return "M" + x1 + "," + y1 + " C" + m + "," + y1 + " " + m + "," + y2 + " " + x2 + "," + y2;
      }
      const x1 = A.x;
      const y1 = A.y + H / 2;
      const x2 = B.x + W;
      const y2 = B.y + H / 2;
      const m = (x1 + x2) / 2;
      return "M" + x1 + "," + y1 + " C" + m + "," + (y1 - 40) + " " + m + "," + (y2 - 40) + " " + x2 + "," + y2;
    }

    const GLYPH = { entry: "→", function: "ƒ", state: "≡", sink: "↧", invariant: "✓", bug: "✕", external: "◌" };
    const trunc = (s, n) => {
      s = String(s || "");
      return s.length > n ? s.slice(0, n - 1) + "…" : s;
    };
    const shortRef = (ref) => {
      const r = String(ref || "");
      const parts = r.split("/");
      return parts.length > 2 ? parts.slice(-2).join("/") : r;
    };

    function buildMap(host) {
      const ix = D.ix;
      // The panel isn't attached yet, so estimate its shape from the window:
      // it sits beside the ~370px rail, above the tape and below the headers.
      const wide = window.innerWidth > 980;
      const hw = wide ? window.innerWidth - 420 : window.innerWidth - 40;
      const hh = wide ? window.innerHeight - 330 : Math.max(300, hw * 0.62);
      const aspect = hw > 200 && hh > 150 ? Math.max(0.35, Math.min(0.8, (hh - 64) / (hw - 16))) : 0.5;
      const L = layout(ix, aspect);
      const svg = svgEl("svg", {
        class: "kd-svg",
        viewBox: "0 0 " + L.w + " " + L.h,
        preserveAspectRatio: "xMidYMid meet",
        role: "img",
        "aria-label": "Code map for " + (D.fixture.label || D.fixture.id),
      });
      const gDefs = svgEl("defs");
      const gE = svgEl("g");
      const gO = svgEl("g");
      const gN = svgEl("g");
      const gB = svgEl("g");
      const gD = svgEl("g");
      const gL = svgEl("g");
      svg.append(gDefs, gE, gO, gN, gB, gD, gL);
      const edges = new Map();
      const addEdge = (from, to, synthetic) => {
        const k = from + ">" + to;
        if (edges.has(k)) return;
        const A = L.pos.get(from);
        const B = L.pos.get(to);
        if (!A || !B) return;
        const d = edgeD(A, B);
        const base = svgEl("path", { d, class: synthetic ? "kd-e kd-syn" : "kd-e" });
        gE.appendChild(base);
        const o = svgEl("path", { d, class: "kd-ov" });
        const n = svgEl("path", { d, class: "kd-ov" });
        gO.append(o, n);
        const idn = edges.size;
        const mk = (lane) => {
          const id = "kd-m-" + lane + "-" + idn;
          const mp = svgEl("path", { d, fill: "none", stroke: "#fff", "stroke-width": 14 });
          gDefs.appendChild(svgEl("mask", { id, maskUnits: "userSpaceOnUse", x: -80, y: -120, width: L.w + 160, height: L.h + 240 }, mp));
          return { url: "url(#" + id + ")", mp };
        };
        edges.set(k, { base, o, n, len: 0, mid: null, mo: mk("o"), mn: mk("n") });
      };
      for (const e of ix.edges) addEdge(e.from, e.to, false);
      for (const s of D.steps) for (const b of ["prod", "old", "new"]) for (const hp of hopsOf(s, b)) for (const e of hp.edges) addEdge(e.from, e.to, e.idx < 0);
      const nodeEls = new Map();
      for (const n of ix.nodes) {
        const p = L.pos.get(n.id);
        if (!p) continue;
        const sub = (n.code && n.code[0] && shortRef(n.code[0])) || n.plain || n.kind;
        const g = svgEl(
          "g",
          { class: "kd-n k-" + n.kind, transform: "translate(" + p.x + "," + p.y + ")", tabindex: "0", role: "button", "aria-label": n.title || n.id },
          svgEl("rect", { class: "kd-card", width: W, height: H, rx: 7 }),
          svgEl("title", null, (n.title || n.id) + ((n.code || []).length ? "  " + n.code.join(", ") : "") + (n.plain ? "\n" + n.plain : "")),
          svgEl("text", { class: "kd-g", x: 12, y: 31 }, GLYPH[n.kind] || "•"),
          svgEl("text", { x: 30, y: 22 }, trunc(n.title || n.id, 23)),
          svgEl("text", { class: "kd-sub", x: 30, y: 38 }, trunc(sub, 29))
        );
        g.addEventListener("click", () => pickNode(n.id));
        g.addEventListener("keydown", (e) => {
          if (e.key === "Enter" || e.key === " ") {
            e.preventDefault();
            e.stopPropagation();
            pickNode(n.id);
          }
        });
        gN.appendChild(g);
        nodeEls.set(n.id, g);
      }
      host.appendChild(svg);
      return { svg, edges, nodeEls, gB, gD, gL, dots: {}, w: L.w };
    }

    function edgeGeom(E) {
      if (!E.len) {
        try {
          E.len = E.base.getTotalLength() || 0;
          E.mid = E.len ? E.base.getPointAtLength(E.len / 2) : null;
        } catch (_) {
          E.len = 0;
        }
      }
      return E;
    }
    function pointOn(E, f) {
      edgeGeom(E);
      if (!E.len) return null;
      try {
        return E.base.getPointAtLength(E.len * f);
      } catch (_) {
        return null;
      }
    }
    function setDash(p, E, v, M) {
      edgeGeom(E);
      const len = E.len || 1000;
      M.mp.style.strokeDasharray = len + " " + (len + 20);
      M.mp.style.strokeDashoffset = String(len * (1 - v.frac));
      if (v.inf) {
        p.style.strokeDasharray = "6 5";
      } else {
        p.style.strokeDasharray = "none";
      }
      p.style.strokeDashoffset = "0";
      p.setAttribute("mask", M.url);
    }

    function paint(spec) {
      const mp = dom.map;
      for (const [k, E] of mp.edges) {
        const q = spec.edges[k] || {};
        for (const lane of ["o", "n"]) {
          const p = E[lane];
          const v = q[lane];
          if (v) {
            p.setAttribute("class", "kd-ov on " + v.cls);
            setDash(p, E, v, lane === "o" ? E.mo : E.mn);
            if (spec.offset) p.setAttribute("transform", "translate(0," + (lane === "o" ? -3 : 3) + ")");
            else p.removeAttribute("transform");
          } else p.setAttribute("class", "kd-ov");
        }
      }
      for (const [id, g] of mp.nodeEls) {
        const n = D.ix.nodeMap.get(id);
        g.setAttribute("class", "kd-n k-" + n.kind + (spec.nodes[id] ? " " + spec.nodes[id] : " dim") + (ds.node === id ? " pick" : ""));
      }
      const used = {};
      for (const d of spec.dots || []) {
        const E = mp.edges.get(d.key);
        if (!E || rm()) continue;
        if (!mp.dots[d.name]) {
          mp.dots[d.name] = svgEl("circle", { r: 6 });
          mp.gD.appendChild(mp.dots[d.name]);
        }
        const c = mp.dots[d.name];
        const pt = pointOn(E, d.frac);
        if (!pt) continue;
        c.setAttribute("class", "kd-dot " + d.cls);
        c.setAttribute("cx", pt.x);
        c.setAttribute("cy", pt.y + (spec.offset ? (d.name === "n" ? 3 : -3) : 0));
        c.style.display = "";
        used[d.name] = 1;
      }
      for (const k in mp.dots) if (!used[k]) mp.dots[k].style.display = "none";
      mp.gB.textContent = "";
      const badgeAt = (b) => {
        const E = mp.edges.get(b.key);
        if (!E) return null;
        return b.check ? pointOn(E, 0.35) : (edgeGeom(E), E.mid);
      };
      for (const b of spec.badges || []) {
        const pt = badgeAt(b);
        if (!pt) continue;
        const g = svgEl("g", { class: "kd-badge " + (b.cls || "") }, svgEl("circle", { cx: pt.x, cy: pt.y, r: 10 }), svgEl("text", { x: pt.x, y: pt.y + 3.8 }, String(b.num)));
        g.addEventListener("click", () => selectHop(b.idx, Math.max(1, level())));
        mp.gB.appendChild(g);
      }
      mp.gL.textContent = "";
      if (spec.label) {
        const pt = badgeAt(spec.label);
        if (pt) {
          const g = svgEl("g", { class: "kd-hlabel " + (spec.label.cls || "") });
          const t = svgEl("text", { x: pt.x + 16, y: pt.y - 30 }, spec.label.text);
          g.appendChild(t);
          mp.gL.appendChild(g);
          try {
            let bb = t.getBBox();
            const maxX = mp.w - 10;
            if (bb.x + bb.width > maxX) {
              t.setAttribute("x", pt.x + 16 - (bb.x + bb.width - maxX));
              bb = t.getBBox();
            }
            g.insertBefore(svgEl("rect", { x: bb.x - 7, y: bb.y - 4, width: bb.width + 14, height: bb.height + 8, rx: 6 }), t);
            g.appendChild(svgEl("line", { x1: pt.x, y1: pt.y - 10, x2: pt.x + 10, y2: bb.y + bb.height + 4 }));
          } catch (_) {}
        }
      }
    }

    const keysOf = (hp) => hp.edges.map((e) => e.key);
    const isBugNode = (id) => ((D.ix.nodeMap.get(id) || {}).kind === "bug");

    function singleSpec(b) {
      const s = cur();
      const v = viewOf(s, b);
      const hs = hopsOf(s, b);
      const r = reveal(s, b);
      const spec = { edges: {}, nodes: {}, dots: [], badges: [] };
      const mark = (id, c) => {
        if (spec.nodes[id] === "bad" && c !== "now") return;
        spec.nodes[id] = c;
      };
      const activeIdx = r.act >= 0 ? r.act : r.sel;
      hs.forEach((hp, idx) => {
        if (idx > r.n || (idx === r.n && r.act < 0)) return;
        const active = idx === activeIdx;
        const frac = idx === r.act ? r.frac : 1;
        const cls = active ? "now" : hp.bug ? "bad" : "lit";
        for (const e of hp.edges) spec.edges[e.key] = { o: { cls: hp.check && e.result === "fail" && !active ? "bad" : cls, frac, inf: !hp.recorded } };
        mark(hp.from, isBugNode(hp.from) ? "bad" : "lit");
        if (frac >= 1) for (const e of hp.edges) mark(e.to, isBugNode(e.to) || e.result === "fail" ? "bad" : "lit");
        if (active) {
          for (const e of hp.edges) mark(e.to, isBugNode(e.to) ? "bad" : "now");
          if (idx === r.act) spec.dots.push({ name: "o", key: keysOf(hp)[0], frac, cls: hp.bug ? "old" : "now" });
        }
        spec.badges.push({ key: keysOf(hp)[0], check: hp.check, num: idx + 1, idx, cls: active ? "now" : hp.bug ? "bad" : "" });
      });
      if (level() > 0 && r.sel >= 0 && hs[r.sel]) {
        const hp = hs[r.sel];
        spec.label = { key: keysOf(hp)[0], check: hp.check, text: r.sel + 1 + "  " + hopText(hp), cls: hp.bug ? "bad" : "" };
      }
      if (v && s.diverge && r.n >= hs.length && r.act < 0) {
        for (const hp of hs) if (hp.kind === "sink") for (const id of hp.to) mark(id, v.bad ? "bad" : "good");
      }
      for (const id of (v && v.failNodes) || []) if (r.n >= hs.length && r.act < 0) mark(id, "bad");
      return spec;
    }
    function bothSpec() {
      const s = cur();
      const ro = reveal(s, "old");
      const rn = reveal(s, "new");
      const spec = { edges: {}, nodes: {}, dots: [], offset: true };
      const so = {};
      const sn = {};
      const walk = (hs, r, tgt, lane) =>
        hs.forEach((hp, idx) => {
          if (idx > r.n || (idx === r.n && r.act < 0)) return;
          const active = idx === r.act;
          for (const k of keysOf(hp)) tgt[k] = { frac: active ? r.frac : 1, active, inf: !hp.recorded };
          if (active) spec.dots.push({ name: lane, key: keysOf(hp)[0], frac: r.frac, cls: lane === "o" ? "old" : "new" });
        });
      const ho = hopsOf(s, "old");
      const hn = hopsOf(s, "new");
      walk(ho, ro, so, "o");
      walk(hn, rn, sn, "n");
      const inO = new Set(ho.flatMap(keysOf));
      const inN = new Set(hn.flatMap(keysOf));
      new Set([...Object.keys(so), ...Object.keys(sn)]).forEach((k) => {
        const a = so[k];
        const b = sn[k];
        const shared = inO.has(k) && inN.has(k);
        const q = {};
        if (shared) {
          if (a) q.o = { cls: a.active ? "now" : "lit", frac: a.frac, inf: a.inf };
          if (b) q.n = { cls: b.active ? "now" : "lit", frac: b.frac, inf: b.inf };
        } else {
          if (a) q.o = { cls: "bad", frac: a.frac, inf: a.inf };
          if (b) q.n = { cls: "good", frac: b.frac, inf: b.inf };
        }
        spec.edges[k] = q;
      });
      const ns = (hs, r) => {
        const z = new Set();
        hs.forEach((hp, idx) => {
          if (idx > r.n || (idx === r.n && r.act < 0)) return;
          z.add(hp.from);
          if (idx < r.n || r.act < 0 || r.frac > 0.95) for (const id of hp.to) z.add(id);
        });
        return z;
      };
      const no = ns(ho, ro);
      const nn = ns(hn, rn);
      new Set([...no, ...nn]).forEach((id) => {
        spec.nodes[id] = isBugNode(id) ? "bad" : no.has(id) && nn.has(id) ? "lit" : no.has(id) ? "only-old" : "only-new";
      });
      if (level() > 0 && hopSel() >= 0 && ho[hopSel()]) {
        const hp = ho[hopSel()];
        spec.label = { key: keysOf(hp)[0], check: hp.check, text: hopSel() + 1 + "  " + hopText(hp), cls: hp.bug ? "bad" : "" };
      }
      return spec;
    }
    function prodSpec() {
      const s = cur();
      const v = viewOf(s, "prod");
      const spec = { edges: {}, nodes: {}, dots: [] };
      if (!v) return spec;
      const done = ds.t >= 1 || level() > 0;
      for (const h of v.step.hits || []) {
        if (h.role === "input") spec.nodes[h.node] = "lit";
        else if (done && h.role === "output") spec.nodes[h.node] = v.bad ? "bad" : "lit";
        else if (done && h.role === "failure") spec.nodes[h.node] = "bad";
      }
      return spec;
    }

    /* ----- text helpers ----- */
    const nodeName = (id) => nodeShort(D.ix.nodeMap.get(id)) || id;
    function hopWhat(hp) {
      const to = hp.check ? hp.to.length + " invariant" + (hp.to.length === 1 ? "" : "s") : nodeName(hp.to[0]);
      return nodeName(hp.from) + " → " + to;
    }
    function hopText(hp) {
      if (hp.bug) {
        const n = D.ix.nodeMap.get(hp.bug);
        return (n && n.title) || hp.label;
      }
      if (hp.kind === "sink") {
        const v = viewOf(cur(), traceBuild());
        const c = v ? v.outputs.length : 0;
        return c + " write" + (c === 1 ? "" : "s");
      }
      return hp.label || "";
    }
    function buildLabel(b) {
      return { prod: "Prod", old: "Old build", new: "New build", both: "Old and new together" }[b] || b;
    }
    function outcomeText(v) {
      if (!v) return "did not run";
      const st = v.step;
      const parts = [];
      if (st.outcome === "not_reached") parts.push("not reached");
      else parts.push(v.bad ? st.outcome + (st.message ? ": " + st.message : "") : st.outcome);
      parts.push(v.outputs.length + " write" + (v.outputs.length === 1 ? "" : "s"));
      if (v.inv.text) parts.push("invariants " + v.inv.text);
      if (st.detected && st.detected.length) parts.push("violated " + st.detected.join(", "));
      return parts.join(", ");
    }
    function sameAs(a, b) {
      return Boolean(a && b && a.step.outcome === b.step.outcome && (a.step.message || "") === (b.step.message || ""));
    }
    function kbd(t) {
      return el("kbd", { text: t });
    }

    /* ----- rail ----- */
    function renderCrumbs() {
      const c = dom.crumbs;
      c.textContent = "";
      const s = cur();
      if (!s) return;
      const hs = hopsOf(s, traceBuild());
      const go = (l) => () => {
        stop();
        if (l === 0) {
          setLevel(0);
          setHop(-1);
        } else setLevel(l);
        draw();
        env.syncHash();
      };
      c.appendChild(el("button", { type: "button", className: level() === 0 ? "cur" : "", text: "seq " + s.seq, on: { click: go(0) } }));
      if (level() >= 1) {
        c.appendChild(el("span", { text: "›" }));
        c.appendChild(el("button", { type: "button", className: level() === 1 ? "cur" : "", text: "hop " + (hopSel() + 1) + " of " + hs.length, on: { click: go(1) } }));
      }
      if (level() >= 2 && hs[hopSel()]) {
        c.appendChild(el("span", { text: "›" }));
        c.appendChild(el("button", { type: "button", className: "cur", text: evTitle(hs[hopSel()]) }));
      }
    }
    function evTitle(hp) {
      if (hp.kind === "entry") return "request payload";
      if (hp.kind === "sink") return "writes";
      if (hp.check) return "invariants";
      const n = D.ix.nodeMap.get(hp.bug || hp.to[0]);
      const ref = n && n.code && n.code[0];
      return ref ? String(ref).split("/").pop() : "hop detail";
    }
    function cellVal(p, field) {
      if (!p) return null;
      if (!field) return p.fields.length ? p.fields[0][1] : trunc(p.raw, 28);
      const f = p.fields.find((x) => x[0] === field);
      return f ? f[1] : "—";
    }
    // A field value as shown: money fields as currency, the rest as written.
    // Comparisons use the raw value; the raw toggle shows it unformatted.
    function showVal(field, v) {
      const money = D && D.ix.model.money;
      if (v === null || v === undefined || v === "—" || !isMoneyField(money, field)) return v;
      return formatMoney(v, money);
    }
    function pickField(a, b) {
      if (!a && !b) return "";
      const base = (a || b).fields;
      if (!base.length) return "";
      if (a && b) for (const [k, v] of a.fields) {
        const o = b.fields.find((x) => x[0] === k);
        if (o && o[1] !== v) return k;
      }
      const num = base.find(([, v]) => /^-?\d+(\.\d+)?$/.test(v));
      return (num || base[0])[0];
    }
    function rowLabel(p, k) {
      if (!p || !p.label) return "output " + (k + 1);
      // "tx:@A → @B" reads as two unbreakable account names split at the arrow.
      const parts = p.label.replace(/^[a-z]+:(?=@)/i, "").split(" → ");
      if (parts.length !== 2) return p.label;
      return el("span", { className: "kd-route" }, el("span", { text: parts[0] }), el("span", { text: "→ " + parts[1] }));
    }
    function renderL0(body) {
      const s = cur();
      const tb = traceBuild();
      const vO = viewOf(s, "old");
      const vN = viewOf(s, "new");
      const vP = viewOf(s, "prod");
      body.appendChild(
        el("div", { className: "kd-seqline" }, el("b", { text: "seq " + s.seq }), "  step " + (s.i + 1) + " of " + D.steps.length + "  ", el("span", { className: "kd-code", text: s.position }))
      );
      body.appendChild(el("h2", { text: s.headline }));
      if (s.diverge) {
        const badV = vO && vO.bad ? vO : vN && vN.bad ? vN : vO;
        const bugs = ((badV && badV.failNodes) || []).map((id) => D.ix.nodeMap.get(id)).filter((n) => n && n.kind === "bug");
        if (bugs.length) {
          const p = el("div", { className: "kd-cause" });
          bugs.forEach((n) => p.appendChild(el("p", null, el("b", { text: bugShort(n) || n.title }), " " + (bugShort(n) ? String(n.title).replace(/^[^:]*:\s*/, "") : "") + (n.plain ? ". " + n.plain : ""))));
          body.appendChild(p);
        } else if (badV && badV.bad && badV.step.message) {
          body.appendChild(el("div", { className: "kd-cause" }, el("p", null, el("b", { text: badV === vO ? "Old build" : "New build" }), " " + badV.step.message)));
        }
        const obs = el("div", { className: "kd-obs" });
        if (vO) obs.append(el("span", { className: "k o", text: "Old build" }), el("span", { className: "v", text: outcomeText(vO) }));
        if (vN) obs.append(el("span", { className: "k n", text: "New build" }), el("span", { className: "v", text: outcomeText(vN) }));
        if (vP) obs.append(el("span", { className: "k", text: "Prod" }), el("span", { className: "v", text: sameAs(vP, vO) ? "same " + (vP.bad ? "error" : "outcome") + " as the old build" : outcomeText(vP) }));
        body.appendChild(obs);
      } else {
        const v = viewOf(s, tb) || vP;
        const who = vO && vN ? "Both builds" : buildLabel(tb);
        body.appendChild(el("div", { className: "kd-obs" }, el("span", { className: "k", text: who }), el("span", { className: "v", text: outcomeText(v) })));
      }
      // Old vs new writes.
      const two = s.diverge && vO && vN;
      const A = (vO || vP || vN).outputs;
      const B = two ? vN.outputs : null;
      const n = Math.max(A.length, B ? B.length : 0);
      if (n || two) {
        const tbody = el("tbody");
        for (let k = 0; k < n; k++) {
          const a = A[k];
          const b = B ? B[k] : null;
          const field = pickField(a, b);
          const av = a ? cellVal(a, field) : null;
          const bv = b ? cellVal(b, field) : null;
          const diff = two && av !== bv;
          const notWritten = (step) => el("td", { className: "amt bad" }, "✕ not written", step && step.message ? el("small", { text: step.message }) : null);
          tbody.appendChild(
            el(
              "tr",
              { className: "click", title: "Open the parsed writes", on: { click: () => openSink() } },
              el("td", null, rowLabel(a || b, k), field ? el("small", { text: field }) : null),
              a ? el("td", { className: "amt" + (diff ? " bad" : ""), text: showVal(field, av) }) : notWritten(vO && vO.step),
              two ? (b ? el("td", { className: "amt" + (diff ? " good" : ""), text: showVal(field, bv) }) : notWritten(vN.step)) : null
            )
          );
        }
        if (two && (vO.inv.text || vN.inv.text)) {
          tbody.appendChild(
            el(
              "tr",
              null,
              el("td", null, "Invariants"),
              el("td", { className: "amt " + vO.inv.tone, text: vO.inv.text || "—" }),
              el("td", { className: "amt " + vN.inv.tone, text: vN.inv.text || "—" })
            )
          );
        }
        body.appendChild(
          el(
            "table",
            { className: "kd-cmp" },
            el("thead", null, el("tr", null, el("th", { text: "Write" }), two ? [el("th", { className: "o", text: "Old" }), el("th", { className: "n", text: "New" })] : el("th", { text: vO && vN ? "Old and new" : buildLabel(tb) }))),
            tbody
          )
        );
      }
      const L = hopsOf(s, tb).length;
      body.appendChild(
        el(
          "button",
          { className: "kd-drill", type: "button", disabled: L === 0, on: { click: deeper } },
          L ? "Trace the " + L + " hop" + (L === 1 ? "" : "s") + (s.diverge && tb === "old" && s.firstDiv >= 0 ? " from the first divergence" : "") : "No hops to trace for this step",
          el("span", null, kbd("Enter"))
        )
      );
    }
    function openSink() {
      const hs = hopsOf(cur(), traceBuild());
      const k = hs.findIndex((x) => x.kind === "sink");
      if (k >= 0) selectHop(k, 2);
    }
    function prettyPayload(x) {
      if (!x) return "";
      try {
        return JSON.stringify(JSON.parse(x), null, 2);
      } catch (_) {
        return env.prettyJson ? env.prettyJson(x) : x;
      }
    }
    function nodeRefs(n, skip) {
      if (!n) return null;
      let links = n.links && n.links.length ? n.links : (n.code || []).map((c) => ({ text: c }));
      if (skip) links = links.filter((lk) => !skip.has(lk.text));
      if (!links.length) return null;
      return el("div", { className: "kd-refs" }, links.map((lk) => env.safeLink(lk.url, lk.text, "kd-ref")));
    }
    function evidence(hp) {
      const s = cur();
      const tb = traceBuild();
      const v = viewOf(s, tb);
      const box = el("div", { className: "kd-ev" });
      if (!v) return box;
      if (hp.kind === "entry") {
        const st = v.step;
        box.appendChild(el("p", { text: "The request exactly as the capsule recorded it." }));
        if (st.omitted) box.appendChild(el("p", { className: "kd-faint", text: "Payload omitted (the map was built with --max-data 0)." }));
        else {
          box.appendChild(el("h4", { text: "Payload" + (st.input_truncated ? " (cut at the map's --max-data limit)" : "") }));
          box.appendChild(el("pre", { className: "kd-raw", text: prettyPayload(st.input) || "(empty)" }));
        }
        for (const nt of hp.notes) box.appendChild(el("p", { className: "kd-note" }, el("b", { text: nt.flow }), " " + nt.text));
        const n = D.ix.nodeMap.get(hp.from);
        const refs = nodeRefs(n);
        if (refs) box.append(el("h4", { text: "Entry point" }), refs);
        return box;
      }
      if (hp.kind === "sink") {
        const vO = viewOf(s, "old");
        const vN = viewOf(s, "new");
        const two = s.diverge && vO && vN;
        const A = two ? vO.outputs : v.outputs;
        const B = two ? vN.outputs : null;
        const n = Math.max(A.length, B ? B.length : 0);
        box.appendChild(el("p", { text: two ? "What each build wrote, parsed from the sink output." : "What this build wrote, parsed from the sink output." }));
        box.appendChild(
          el(
            "div",
            { className: "kd-evbar" },
            el("button", {
              className: "kd-mini",
              type: "button",
              attrs: { "aria-pressed": String(ds.raw) },
              on: {
                click: () => {
                  ds.raw = !ds.raw;
                  draw();
                },
              },
            }, "Raw strings ", kbd("r"))
          )
        );
        for (let k = 0; k < n; k++) {
          const a = A[k];
          const b = B ? B[k] : null;
          box.appendChild(el("h4", { text: rowLabel(a || b, k) }));
          if (ds.raw || !((a && a.fields.length) || (b && b.fields.length))) {
            box.appendChild(el("pre", { className: "kd-raw", text: (two ? "old  " : "") + (a ? a.raw : "(not written)") }));
            if (two) box.appendChild(el("pre", { className: "kd-raw", text: "new  " + (b ? b.raw : "(not written)") }));
            continue;
          }
          const keys = [];
          for (const p of [a, b]) if (p) for (const [fk] of p.fields) if (!keys.includes(fk)) keys.push(fk);
          const tbody = el("tbody");
          for (const fk of keys) {
            const av = a ? cellVal(a, fk) : "—";
            const bv = b ? cellVal(b, fk) : "—";
            const d = two && av !== bv;
            tbody.appendChild(el("tr", null, el("td", { text: fk }), el("td", { className: d ? "d" : "", text: showVal(fk, av) }), two ? el("td", { text: showVal(fk, bv) }) : null));
          }
          box.appendChild(
            el("table", { className: "kd-wt" }, el("thead", null, el("tr", null, el("th"), el("th", { className: two ? "o" : "", text: two ? "Old" : vO && vN ? "Both" : buildLabel(tb) }), two ? el("th", { className: "n", text: "New" }) : null)), tbody)
          );
        }
        if (!n) box.appendChild(el("p", { className: "kd-faint", text: "No outputs were written in this step." }));
        if (v.step.outputs && v.step.outputs.some((o) => o.truncated)) box.appendChild(el("p", { className: "kd-faint", text: "Some outputs were cut at the map's --max-data limit." }));
        for (const nt of hp.notes) box.appendChild(el("p", { className: "kd-note" }, el("b", { text: nt.flow }), " " + nt.text));
        return box;
      }
      if (hp.check) {
        const vO = viewOf(s, "old");
        const vN = viewOf(s, "new");
        const two = s.diverge && vO && vN;
        box.appendChild(el("p", { text: "Invariants the host declares, checked after the step." }));
        const kv = el("div", { className: "kd-kv" });
        const names = Object.keys((two ? Object.assign({}, vO.step.invariants, vN.step.invariants) : v.step.invariants) || {});
        for (const name of names) {
          const ov = two ? (vO.step.invariants || {})[name] || "—" : null;
          const nv = two ? (vN.step.invariants || {})[name] || "—" : (v.step.invariants || {})[name] || "—";
          const tone = nv === "pass" ? "good" : nv === "fail" || ov === "fail" ? "bad" : "";
          kv.append(el("span", { text: name }), el("span", { className: tone, text: two ? "old " + ov + ", new " + nv : nv }));
        }
        box.appendChild(kv);
        if (v.step.detected && v.step.detected.length) box.appendChild(el("p", { className: "kd-bad", text: "Violated: " + v.step.detected.join(", ") }));
        return box;
      }
      const target = D.ix.nodeMap.get(hp.bug || hp.to[0]);
      const from = D.ix.nodeMap.get(hp.from);
      box.appendChild(
        el(
          "p",
          null,
          (from && from.title) || hp.from,
          " → ",
          (target && target.title) || hp.to[0],
          hp.recorded ? "." : ". This hop comes from the spec's edges and flows; the capsule doesn't record it."
        )
      );
      for (const nt of hp.notes) box.appendChild(el("p", { className: "kd-note" }, el("b", { text: nt.flow }), " " + nt.text));
      if (target && target.plain) box.appendChild(el("p", { className: hp.bug ? "kd-bad" : "kd-faint", text: target.plain }));
      if (target && target.detail) box.appendChild(el("p", { className: "kd-faint", text: target.detail }));
      const src = sourceEvidence(target);
      for (const b of src.blocks) box.appendChild(b);
      const refs = nodeRefs(target, src.covered);
      if (refs) box.append(el("h4", { text: src.blocks.length ? "Other refs" : "Code" }), refs);
      return box;
    }
    // The commit or branch a group's code_base URL is pinned to, shortened.
    function pinOf(codeBase) {
      const m = /\/(?:blob|tree|src|commit)\/([^/]+)\/?$/.exec(String(codeBase || ""));
      if (!m) return "";
      return /^[0-9a-f]{40}$/i.test(m[1]) ? m[1].slice(0, 10) : m[1];
    }
    function groupBase(id) {
      const g = (D.ix.model.groups || []).find((x) => x.id === id);
      return (g && g.code_base) || "";
    }
    // Source lines with numbers, the shared leading indent removed so the
    // excerpt fits the rail.
    function commonIndent(lines) {
      let pre = null;
      for (const ln of lines) {
        if (!ln.trim()) continue;
        const lead = /^[\t ]*/.exec(ln)[0];
        if (pre === null) pre = lead;
        else {
          let i = 0;
          while (i < pre.length && i < lead.length && pre[i] === lead[i]) i++;
          pre = pre.slice(0, i);
        }
      }
      return pre ? pre.length : 0;
    }
    function srcLines(lines, start, cls) {
      const cut = commonIndent(lines);
      return el(
        "pre",
        { className: "kd-lines" },
        lines.map((ln, k) => el("span", { className: cls(start + k) }, el("i", { text: String(start + k) }), ln.slice(cut) || " "))
      );
    }
    function hunkBlock(h) {
      // The gutter counts old-side lines, matching the excerpt above; added
      // lines have none.
      let o = h.old_start;
      const rows = [el("span", { className: "at" }, el("i"), "@@ -" + h.old_start + "," + h.old_lines + " +" + h.new_start + "," + h.new_lines + " @@" + (h.section ? " " + h.section : ""))];
      const body = h.lines || [];
      const cut = commonIndent(body.filter((ln) => ln[0] !== "\\").map((ln) => ln.slice(1)));
      for (const raw of body) {
        const c = raw[0];
        const ln = c === "\\" ? raw : c + raw.slice(1 + cut);
        let num = "";
        let cls = "ctx";
        if (c === "-") {
          num = o++;
          cls = "del";
        } else if (c === "+") cls = "add";
        else if (c === "\\") cls = "at";
        else num = o++;
        rows.push(el("span", { className: cls }, el("i", { text: String(num) }), ln));
      }
      return el(
        "div",
        { className: "kd-src" },
        el("h4", null, el("span", { text: h.patch }), el("small", { text: "at " + h.file + ":" + h.old_start })),
        el("pre", { className: "kd-lines" }, rows),
        h.truncated ? el("p", { className: "kd-faint", text: "Hunk cut at the map's line limit." }) : null
      );
    }
    // Source excerpts for node n, each followed by the patch hunks that touch
    // it. covered lists the refs that got an excerpt or a hunk.
    function sourceEvidence(n) {
      const out = { blocks: [], covered: new Set() };
      const ev = n && D.ix.model.evidence && D.ix.model.evidence[n.id];
      if (!ev) return out;
      const hunks = D.ix.model.hunks || [];
      const shown = new Set();
      const addHunk = (i) => {
        if (shown.has(i) || !hunks[i]) return;
        shown.add(i);
        out.blocks.push(hunkBlock(hunks[i]));
      };
      for (const e of ev.excerpts || []) {
        out.covered.add(e.ref);
        const base = groupBase(e.root || n.group);
        const range = e.hl_start + (e.hl_end > e.hl_start ? "-" + e.hl_end : "");
        const url = base ? base + e.path + "#L" + e.hl_start + (e.hl_end > e.hl_start ? "-L" + e.hl_end : "") : "";
        const pin = pinOf(base);
        out.blocks.push(
          el(
            "div",
            { className: "kd-src" },
            el("h4", null, env.safeLink(url, e.path + ":" + range, "kd-ref"), pin ? el("small", { text: "at " + pin }) : null),
            srcLines(e.lines || [], e.start, (k) => (k >= e.hl_start && k <= e.hl_end ? "hl" : "")),
            e.truncated ? el("p", { className: "kd-faint", text: "Excerpt cut at " + (e.lines || []).length + " lines." }) : null
          )
        );
        for (const i of e.hunks || []) addHunk(i);
      }
      for (const i of ev.hunks || []) addHunk(i);
      return out;
    }
    function renderTrace(body) {
      const s = cur();
      const b = traceBuild();
      const hs = hopsOf(s, b);
      const r = reveal(s, b);
      const build = getBuild();
      const lab = buildLabel(b) + (build === "both" ? " (map shows both)" : build === "prod" ? " (prod recorded only the tagged hops)" : "");
      body.appendChild(el("div", { className: "kd-seqline" }, el("b", { text: "seq " + s.seq }), "  " + s.headline));
      const ol = el("ol", { className: "kd-trace" });
      ol.appendChild(el("li", { className: "th" }, el("span", null, el("b", { text: lab }), ", " + hs.length + " hop" + (hs.length === 1 ? "" : "s")), el("span", null, kbd("↑"), kbd("↓"), " hop")));
      const curHop = ds.playing ? Math.floor(Math.min(ds.t, hs.length - 1)) : hopSel();
      const row = (hp, idx) => {
        const done = idx < r.n || (r.act < 0 && !ds.playing);
        const cls = ["kd-hop", done ? "done" : "", hp.bug ? "bug" : "", idx === curHop ? "cur" : "", idx === hopSel() && level() > 0 ? "sel" : ""].filter(Boolean).join(" ");
        const tag = hp.kind === "entry" ? "recorded input" : hp.kind === "sink" ? "recorded output" : hp.check ? "host checks" : "from spec";
        const bugN = hp.bug ? D.ix.nodeMap.get(hp.bug) : null;
        const li = el(
          "li",
          null,
          el(
            "button",
            {
              type: "button",
              className: cls,
              on: {
                click: () => {
                  if (level() === 2 && hopSel() === idx) {
                    setLevel(1);
                    draw();
                    env.syncHash();
                  } else selectHop(idx, 2);
                },
              },
            },
            el("span", { className: "num", text: String(idx + 1) }),
            el("span", { className: "what", text: hopWhat(hp) }),
            el("span", { className: "tag" + (hp.recorded ? " rec" : ""), text: tag }),
            el("span", { className: "lab" }, bugN && bugShort(bugN) ? el("b", { text: bugShort(bugN) + "  " }) : null, bugN && bugShort(bugN) ? String(bugN.title).replace(/^[^:]*:\s*/, "") : hopText(hp)),
            idx === s.firstDiv && s.diverge && (b === "old" || b === "new") ? el("span", { className: "first", text: "First divergence from the " + (b === "old" ? "new" : "old") + " build" }) : null
          )
        );
        if (level() === 2 && idx === hopSel()) li.appendChild(evidence(hp));
        return li;
      };
      if (!hs.length) ol.appendChild(el("li", { className: "kd-faint", text: "No hops: this step recorded no entry hit." }));
      if (s.diverge && s.prefix > 0 && !ds.openPrefix && (b === "old" || b === "new")) {
        ol.appendChild(
          el(
            "li",
            null,
            el(
              "button",
              {
                type: "button",
                className: "kd-collapsed",
                on: {
                  click: () => {
                    ds.openPrefix = true;
                    draw();
                  },
                },
              },
              el("span", { text: (s.prefix === 1 ? "Hop 1 is" : "Hops 1–" + s.prefix + " are") + " the same in both builds" }),
              el("span", { text: "show" })
            )
          )
        );
        hs.forEach((hp, idx) => {
          if (idx >= s.prefix) ol.appendChild(row(hp, idx));
        });
      } else hs.forEach((hp, idx) => ol.appendChild(row(hp, idx)));
      body.appendChild(ol);
      if (s.diverge && (b === "old" || b === "new")) {
        const other = b === "old" ? "new" : "old";
        const ov = viewOf(s, other);
        if (ov) {
          const bugs = ov.hops.filter((h) => h.bug).length;
          const bits = [buildLabel(other).replace(/^./, (c) => c.toUpperCase()) + " takes " + ov.hops.length + " hop" + (ov.hops.length === 1 ? "" : "s")];
          if (!bugs) bits.push("no bug nodes");
          if (ov.inv.text) bits.push("invariants " + ov.inv.text);
          body.appendChild(el("div", { className: "kd-other " + (other === "new" ? "n" : "o") }, "The " + bits.join(", ") + ". Press ", kbd(other === "new" ? "n" : "o"), " to trace it."));
        }
      }
    }
    function renderRail() {
      const body = dom.rbody;
      const keep = body.scrollTop;
      body.textContent = "";
      const s = cur();
      if (!s) {
        body.appendChild(el("p", { className: "kd-faint", text: "This fixture has no steps." }));
        return;
      }
      if (level() === 0) renderL0(body);
      else renderTrace(body);
      body.scrollTop = keep;
      const sel = body.querySelector(".kd-hop.sel, .kd-hop.cur");
      if (sel) {
        const top = sel.offsetTop - body.offsetTop;
        if (top < body.scrollTop || top > body.scrollTop + body.clientHeight - 60) body.scrollTop = top - 40;
      }
    }
    function renderFoot() {
      const f = dom.rfoot;
      f.textContent = "";
      const hint = (k, t) => el("span", null, kbd(k), " " + t);
      if (level() === 0) f.append(hint("Enter", "trace hops"), el("span", null, kbd("←"), kbd("→"), " step"), hint("space", "play"), D.firstDivStep >= 0 ? hint("d", "watch the split") : null);
      else f.append(hint("Enter", "evidence"), hint("Esc", "back"), el("span", null, kbd("←"), kbd("→"), " hop"), hint("space", "play this step"));
    }

    /* ----- tape ----- */
    function icon(name) {
      const P = {
        play: "M4 2.5v11l9-5.5z",
        pause: "M3.5 2.5h3v11h-3zM9.5 2.5h3v11h-3z",
        prev: "M3 2.5h2v11H3zM14 2.5v11L6 8z",
        next: "M11 2.5h2v11h-2zM2 2.5v11L10 8z",
        restart: "M8 2.5a5.5 5.5 0 1 1-5.2 7.3l1.9-.6A3.5 3.5 0 1 0 8 4.5V7L4 3.5 8 0z",
        fork: "M5 1.5h2v5.2l4.3 4.3V8.5h2v6h-6v-2h2.6L7 9.3V14.5H5z",
      };
      return svgEl("svg", { viewBox: "0 0 16 16", "aria-hidden": "true" }, svgEl("path", { d: P[name] }));
    }
    function tapeRows() {
      const R = D.runs;
      const rows = [];
      if (R.prod) rows.push(["prod", "Prod", "recorded", "f"]);
      if (R.old) rows.push(["old", "Old build", R.old.bin || "--bin", "o"]);
      if (R.new) rows.push(["new", "New build", R.new.bin || "--new", "n"]);
      return rows;
    }
    function buildTape(host) {
      const thead = el("div", { className: "kd-thead" });
      const bPrev = el("button", { className: "kd-tb", type: "button", attrs: { "aria-label": "Back" }, on: { click: back } }, icon("prev"));
      const bPlay = el("button", { className: "kd-tb play", type: "button", on: { click: play } });
      const bNext = el("button", { className: "kd-tb", type: "button", attrs: { "aria-label": "Forward" }, on: { click: fwd } }, icon("next"));
      const bRe = el("button", { className: "kd-tb", type: "button", title: "Replay from step 1", attrs: { "aria-label": "Replay from step 1" }, on: { click: restart } }, icon("restart"));
      const bSplit = el(
        "button",
        { className: "kd-tb split", type: "button", disabled: D.firstDivStep < 0, title: D.firstDivStep < 0 ? "Old and new agree on every step" : "Jump to the first step where old and new differ and play it (d)", on: { click: watchSplit } },
        icon("fork"),
        "Watch the split"
      );
      const readout = el("span", { className: "kd-readout" });
      const thint = el("span", { className: "kd-hint" });
      thead.append(bPrev, bPlay, bNext, bRe, bSplit, readout, thint);
      const grid = el("div", { className: "kd-tgrid" });
      const ruler = el("div", { className: "kd-cols ruler" });
      grid.append(el("span"), ruler, el("span"));
      D.steps.forEach((s) => {
        ruler.appendChild(
          el(
            "span",
            { className: s.diverge ? "div" : "", title: "seq " + s.seq + ": " + s.headline + ". Seq counts every record in the capsule; only inputs become steps.", on: { click: () => seek(s.i) } },
            el("b", { text: String(s.seq) }),
            s.name
          )
        );
      });
      const rows = {};
      const trs = tapeRows();
      const ds0 = D.firstDivStep >= 0 ? D.steps[D.firstDivStep] : null;
      let ph = null;
      trs.forEach(([id, nm, sub, c], ri) => {
        grid.appendChild(el("div", { className: "kd-tl " + c }, el("span", { text: nm }), el("small", { text: sub })));
        const row = el("div", { className: "kd-cols" });
        rows[id] = [];
        D.steps.forEach((s) => {
          const cell = el("button", {
            type: "button",
            className: "kd-cell",
            on: {
              click: (e) => {
                if (e.target.closest(".kd-tick")) return;
                const bld = getBuild();
                if (s.i !== curIdx() || bld !== id) {
                  if (id !== bld && !(bld === "both" && id !== "prod")) setBuild(id);
                  seek(s.i);
                }
              },
            },
          });
          cell._memo = "";
          row.appendChild(cell);
          rows[id].push(cell);
        });
        if (ri === 0) {
          ph = el("div", { className: "kd-playhead" });
          row.appendChild(ph);
        }
        grid.appendChild(row);
        // Gutter: does the old build reproduce prod at the split?
        if (id === "prod") {
          const vP = ds0 && viewOf(ds0, "prod");
          const vO = ds0 && viewOf(ds0, "old");
          const span = Boolean(D.runs.old);
          const txt = vP && vO ? (sameAs(vP, vO) ? "same " + (vP.bad ? "error" : "outcome") + " as prod" : "differs from prod") : "";
          grid.appendChild(el("div", { className: "kd-gut" + (span ? " br" : "") + (vP && vO && !sameAs(vP, vO) ? " warn" : ""), text: txt }));
        } else if (id === "old" && !D.runs.prod) {
          grid.appendChild(el("div", { className: "kd-gut" }));
        } else if (id === "new") {
          const vN = ds0 && viewOf(ds0, "new");
          grid.appendChild(el("div", { className: "kd-gut n" + (vN && vN.bad ? " bad" : ""), text: vN ? (vN.bad ? vN.step.outcome : "ok") + (vN.inv.text ? ", invariants " + vN.inv.text : "") : "" }));
        }
      });
      if (!trs.length) grid.appendChild(el("p", { className: "kd-faint", text: "No runs for this fixture." }));
      host.append(thead, grid);
      const cols = [ruler].concat(trs.map(([id]) => rows[id].length ? rows[id][0].parentNode : null).filter(Boolean));
      const minW = Math.max(420, D.steps.length * 26);
      cols.forEach((c) => (c.style.minWidth = minW + "px"));
      if (cols[1]) {
        cols[1].addEventListener("transitionend", (e) => {
          if (e.propertyName === "grid-template-columns") placePlayhead();
        });
      }
      grid.addEventListener("pointerdown", (e) => {
        if (!e.target.closest(".kd-cols:not(.ruler)")) return;
        if (e.target.closest(".kd-tick")) return;
        drag = true;
        stop();
      });
      return { bPlay, bPrev, bNext, readout, thint, ruler, rows, cols, ph, trs };
    }
    function scrubAt(x) {
      if (!dom || !dom.tape.trs.length) return;
      const cells = dom.tape.rows[dom.tape.trs[0][0]];
      for (let i = 0; i < cells.length; i++) {
        const r = cells[i].getBoundingClientRect();
        if (x >= r.left - 2 && x <= r.right + 2) {
          const f = Math.max(0, Math.min(0.999, (x - r.left) / r.width));
          const s = D.steps[i];
          if (level() > 0 && i === curIdx()) {
            const L = hopsOf(s, traceBuild()).length;
            if (L) setHop(Math.min(L - 1, Math.floor(f * L)));
            ds.t = BIG;
          } else {
            if (level() > 0) {
              setLevel(0);
              setHop(-1);
            }
            state.replaySeq = s.seq;
            ds.t = f * (lenOf(s) + HOLD);
          }
          draw();
          return;
        }
      }
    }
    function touches(s, b, id) {
      return hopsOf(s, b).some((hp) => hp.from === id || hp.to.includes(id));
    }
    function renderTape() {
      const T = dom.tape;
      const s = cur();
      if (!s) return;
      const build = getBuild();
      const tb = traceBuild();
      T.bPlay.textContent = "";
      T.bPlay.append(icon(ds.playing ? "pause" : "play"), el("span", { text: ds.playing ? "Pause" : "Play" }));
      T.bPrev.title = level() > 0 ? "Previous hop (←)" : "Previous step (←)";
      T.bNext.title = level() > 0 ? "Next hop (→)" : "Next step (→)";
      const L = hopsOf(s, tb).length;
      T.readout.textContent = "";
      if (level() > 0) T.readout.append("seq " + s.seq + "  hop ", el("b", { text: String(ds.playing ? Math.min(Math.floor(ds.t) + 1, L) : hopSel() + 1) }), "/" + L);
      else T.readout.append("step ", el("b", { text: String(s.i + 1) }), "/" + D.steps.length + "  seq " + s.seq);
      const pick = ds.node ? D.ix.nodeMap.get(ds.node) : null;
      T.thint.textContent = pick ? "Dots mark steps that reach " + (pick.title || pick.id) + ". Esc clears." : level() > 0 ? "Zoomed into seq " + s.seq + ". Ticks are hops; red ticks are bugs." : "Drag across the tape to scrub. Click a cell to jump.";
      const colsTpl = D.steps.map((x) => (level() > 0 ? (x.i === s.i ? "8fr" : "0.55fr") : x.diverge ? "3.2fr" : "1fr")).join(" ");
      T.cols.forEach((c) => (c.style.gridTemplateColumns = colsTpl));
      Array.from(T.ruler.children).forEach((sp, i) => sp.classList.toggle("cur", i === s.i));
      for (const [id] of T.trs) {
        T.rows[id].forEach((cell, i) => {
          const x = D.steps[i];
          const v = viewOf(x, id);
          const err = Boolean(v && v.bad);
          const ok2 = Boolean(x.diverge && v && !v.bad && id === "new");
          const missing = !v || v.step.outcome === "not_reached";
          const follow = i === s.i && (build === id || (build === "both" && id !== "prod"));
          cell.className = "kd-cell" + (err ? " err" : "") + (ok2 ? " ok2" : "") + (missing ? " miss" : "") + (i < s.i ? " past" : "") + (i === s.i ? " colcur" : "") + (follow ? " follow" : "");
          const zoom = level() > 0 && i === s.i;
          const hb = id === "prod" ? "prod" : id;
          const hs = hopsOf(x, hb);
          const curK = ds.playing ? Math.floor(ds.t) : hopSel();
          const touch = ds.node && touches(x, hb, ds.node);
          const memo = [zoom ? "z" + curK + ":" + hopSel() + ":" + ds.playing + ":" + tb : level() > 0 ? "e" : "c", err, ok2, missing, touch].join("|");
          if (cell._memo === memo) return;
          cell._memo = memo;
          cell.textContent = "";
          if (zoom) {
            const ticks = el("span", { className: "kd-ticks" });
            hs.forEach((hp, k) => {
              const c = ["kd-tick", k <= curK ? "done" : "", hp.bug ? "bug" : "", k === curK && hb === tb && id !== "prod" ? "cur" : "", id === "prod" && !hp.recorded ? "dimt" : "", k === hopSel() && hb === tb && !ds.playing && id !== "prod" ? "sel" : ""].filter(Boolean).join(" ");
              ticks.appendChild(
                el("span", {
                  className: c,
                  title: "hop " + (k + 1) + ": " + hopWhat(hp) + (hopText(hp) ? ", " + hopText(hp) : ""),
                  attrs: { role: "button", "aria-label": "hop " + (k + 1) },
                  on: {
                    click: (e) => {
                      e.stopPropagation();
                      if (id !== "prod" && getBuild() !== "both") setBuild(id);
                      selectHop(k, Math.max(1, level()));
                    },
                  },
                })
              );
            });
            cell.appendChild(ticks);
          } else if (level() === 0) {
            if (missing) cell.appendChild(el("span", { className: "kd-faint", text: v ? "not reached" : "—" }));
            else if (err) cell.append("✕ " + v.step.outcome);
            else if (ok2) cell.append("ok, " + v.outputs.length + " write" + (v.outputs.length === 1 ? "" : "s"));
            else cell.appendChild(el("span", { className: "kd-dotm" }));
          }
          if (touch) cell.appendChild(el("span", { className: "kd-touch", title: "reaches " + (pick ? pick.title || pick.id : "") }));
          cell.setAttribute("aria-label", id + " seq " + x.seq + ": " + (missing ? "not run" : err ? v.step.outcome : "ok"));
        });
      }
      placePlayhead();
    }
    function placePlayhead() {
      if (!dom || !dom.tape.ph) return;
      const T = dom.tape;
      const s = cur();
      if (!s) return;
      const first = T.rows[T.trs[0][0]];
      const cell = first[s.i];
      if (!cell) return;
      if (level() > 0) {
        const b = traceBuild();
        const row = T.rows[b] || first;
        const ticks = row[s.i].querySelectorAll(".kd-tick");
        const k = ds.playing ? Math.min(ticks.length - 1, Math.floor(ds.t)) : hopSel();
        const tk = ticks[k];
        if (tk) {
          const rr = cell.parentNode.getBoundingClientRect();
          const tr = tk.getBoundingClientRect();
          const f = ds.playing ? ds.t - Math.floor(ds.t) : 1;
          T.ph.style.left = tr.left - rr.left + f * tr.width + "px";
          return;
        }
      }
      const f = ds.t >= BIG / 10 ? 1 : Math.min(1, ds.t / (lenOf(s) + HOLD));
      T.ph.style.left = cell.offsetLeft + f * cell.offsetWidth - 1 + "px";
    }

    /* ----- node cross-filter ----- */
    function pickNode(id) {
      stop();
      if (ds.node === id) {
        ds.node = null;
        draw();
        return;
      }
      ds.node = id;
      const s = cur();
      const hs = hopsOf(s, traceBuild());
      let k = hs.findIndex((hp) => hp.to.includes(id));
      if (k < 0) k = hs.findIndex((hp) => hp.from === id);
      if (k >= 0) {
        setHop(k);
        setLevel(Math.max(1, level()));
        ds.t = BIG;
        if (k < s.prefix) ds.openPrefix = true;
      }
      draw();
      env.syncHash();
    }

    /* ----- draw ----- */
    function draw() {
      if (!dom || !D || !D.steps.length) return;
      const b = getBuild();
      paint(b === "both" ? bothSpec() : b === "prod" ? prodSpec() : singleSpec(b));
      dom.maptag.textContent = "";
      const msg = {
        prod: "Prod recorded the request and the writes. The path between them is replayed, not recorded.",
        old: "Numbers on the lines are hop order.",
        new: "Numbers on the lines are hop order.",
        both: "Neutral: both builds. Red: only old. Green: only new.",
      }[b];
      dom.maptag.append(el("span", null, "Following ", el("b", { text: buildLabel(b) })), el("span", { className: "msg", text: msg }));
      for (const btn of dom.seg) btn.setAttribute("aria-pressed", String(btn.dataset.b === b));
      renderCrumbs();
      renderRail();
      renderFoot();
      renderTape();
    }

    /* ----- mount ----- */
    function mount(fixture) {
      cancelAnimationFrame(raf);
      D = deriveFixture(env.getModel(), fixture);
      if (!D.steps.length) {
        dom = null;
        return el("div", { className: "kd kd-empty" }, el("p", { text: "This fixture has no input steps to replay." }));
      }
      const s = cur();
      if (s && s.seq !== state.replaySeq) state.replaySeq = s.seq;
      const L = hopsOf(s, traceBuild()).length;
      if (level() > 0 && !L) {
        setLevel(0);
        setHop(-1);
      } else if (level() > 0) setHop(Math.max(0, Math.min(L - 1, hopSel() < 0 ? defaultHop(s) : hopSel())));
      if (ds.node && !D.ix.nodeMap.has(ds.node)) ds.node = null;

      const fixtures = env.getModel().fixtures || [];
      const fxSel = el(
        "select",
        {
          className: "kd-fxsel",
          title: "Fixture",
          attrs: { "aria-label": "Fixture" },
          on: {
            change: (e) => {
              stop();
              env.selectFixture(e.target.value);
            },
          },
        },
        fixtures.map((x) => el("option", { value: x.id, text: x.label || x.id, selected: x.id === fixture.id }))
      );
      const R = D.runs;
      const verdicts = [];
      if (R.old) verdicts.push(el("span", null, "old ", el("code", { text: R.old.verdict || R.old.status || "?" })));
      if (R.new) verdicts.push(el("span", null, "new ", el("code", { text: R.new.verdict || R.new.status || "?" })));
      if (!R.old && R.prod) verdicts.push(el("span", null, "recorded ", el("code", { text: R.prod.verdict || R.prod.status || "?" })));
      const fxInfo = el("div", { className: "kd-fx" }, fxSel, fixture.service ? el("span", { text: fixture.service }) : null, /\.tokenized$/.test(fixture.id) ? el("span", { text: "tokenized capsule" }) : null, verdicts);
      const seg = [
        ["prod", "Prod", "Only what prod recorded (p)", ""],
        ["old", "Old", "Old build replayed (o)", "o"],
        ["new", "New", "New build replayed (n)", "n"],
        ["both", "Both", "Both builds on one map (c)", ""],
      ].map(([b, t, tip, c]) =>
        el("button", {
          type: "button",
          className: c,
          text: t,
          title: buildAvailable(b) ? tip : "Not available: " + (b === "new" ? "map built without --new" : b === "old" ? "map built without --bin" : b === "both" ? "needs --bin and --new" : "no recorded run"),
          disabled: !buildAvailable(b),
          dataset: { b },
          on: { click: () => setBuild(b) },
        })
      );
      const hdr = el("header", { className: "kd-hdr" }, fxInfo, el("span", { className: "sp" }), el("div", { className: "kd-seg", attrs: { role: "group", "aria-label": "Which run the map follows" } }, seg));
      const maptag = el("div", { className: "kd-maptag" });
      const mapPanel = el("section", { className: "kd-panel kd-map", attrs: { "aria-label": "Code map" } }, maptag);
      const hasSpecHops = D.steps.some((x) => ["old", "new", "prod"].some((b) => hopsOf(x, b).some((h) => !h.recorded)));
      mapPanel.appendChild(
        el(
          "div",
          { className: "kd-mapfoot" },
          el("span", { text: untouchedText() }),
          el("span", null, el("span", { className: "kd-lg" }, el("i"), "recorded"), hasSpecHops ? el("span", { className: "kd-lg" }, el("i", { className: "d" }), "from spec") : null, el("span", { className: "kd-lg", text: "click a card to filter" }))
        )
      );
      const crumbs = el("nav", { className: "kd-crumbs", attrs: { "aria-label": "Drill-down path" } });
      const rbody = el("div", { className: "kd-rbody" });
      const rfoot = el("div", { className: "kd-rfoot" });
      const rail = el("aside", { className: "kd-panel kd-rail", attrs: { "aria-label": "Step detail" } }, crumbs, rbody, rfoot);
      const tapeEl = el("section", { className: "kd-panel kd-tape", attrs: { "aria-label": "Replay timeline" } });
      const rootEl = el("div", { className: "kd" }, hdr, el("div", { className: "kd-main" }, mapPanel, rail), tapeEl);
      dom = { root: rootEl, maptag, crumbs, rbody, rfoot, seg, map: null, tape: null };
      dom.map = buildMap(mapPanel);
      dom.tape = buildTape(tapeEl);
      return rootEl;
    }
    function untouchedText() {
      const reached = new Set();
      for (const s of D.steps) for (const b of ["prod", "old", "new"]) for (const hp of hopsOf(s, b)) {
        reached.add(hp.from);
        hp.to.forEach((x) => reached.add(x));
      }
      const miss = D.ix.nodes.filter((n) => (n.kind === "entry" || n.kind === "bug") && !reached.has(n.id)).map((n) => nodeShort(n));
      return miss.length ? "Not reached by this fixture: " + miss.join(", ") : "";
    }
    function attached() {
      if (!dom) return;
      draw();
      if (ds.playing) {
        last = performance.now();
        cancelAnimationFrame(raf);
        raf = requestAnimationFrame(tick);
      }
    }
    function detach() {
      cancelAnimationFrame(raf);
    }
    function resetForJump() {
      stop();
      ds.t = BIG;
      ds.node = null;
      ds.raw = false;
      ds.openPrefix = false;
    }

    function onKey(e) {
      if (!dom || e.metaKey || e.ctrlKey || e.altKey) return false;
      const inBtn = e.target && e.target.closest && e.target.closest("button,[role=button],a");
      if (inBtn && (e.key === " " || e.key === "Enter")) return false;
      const k = e.key;
      if (k === " ") play();
      else if (k === "ArrowRight") fwd();
      else if (k === "ArrowLeft") back();
      else if (k === "ArrowDown") level() > 0 ? selectHop(hopSel() + 1) : deeper();
      else if (k === "ArrowUp") level() > 0 && hopSel() > 0 ? selectHop(hopSel() - 1) : shallower();
      else if (k === "Enter") deeper();
      else if (k === "Escape") {
        if (!ds.playing && level() === 0 && !ds.node) return false;
        if (ds.playing) {
          stop();
          draw();
        } else shallower();
      } else if (k === "d") watchSplit();
      else if (k === "o") setBuild("old");
      else if (k === "n") setBuild("new");
      else if (k === "c") setBuild(getBuild() === "both" ? "old" : "both");
      else if (k === "p") setBuild("prod");
      else if (k === "r" && level() === 2) {
        ds.raw = !ds.raw;
        draw();
      } else return false;
      e.preventDefault();
      return true;
    }

    root.addEventListener("pointermove", (e) => {
      if (drag && e.buttons) scrubAt(e.clientX);
    });
    root.addEventListener("pointerup", () => {
      if (drag) {
        drag = false;
        env.syncHash();
      }
    });
    root.addEventListener("resize", () => {
      if (dom) placePlayhead();
    });

    return {
      mount,
      attached,
      detach,
      stop: () => {
        stop();
        if (dom) draw();
      },
      isPlaying: () => ds.playing,
      resetForJump,
      onKey,
    };
  }

  root.KavachDeck = {
    indexModel,
    deriveFixture,
    deriveHops,
    divergenceReasons,
    parseOutput,
    formatMoney,
    stepNamesFor,
    invSummary,
    isBadStep,
    createDeck,
  };
})(typeof window !== "undefined" ? window : globalThis);
