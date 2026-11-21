(function () {
  "use strict";

  const dataEl = document.getElementById("kavach-map-data");
  const appEl = document.getElementById("app");
  if (!dataEl || !appEl) return;

  let model;
  try {
    model = JSON.parse(dataEl.textContent || "{}");
  } catch (err) {
    const pre = document.createElement("pre");
    pre.textContent = "Failed to parse map data: " + String(err);
    appEl.appendChild(pre);
    return;
  }

  if (model.title) {
    document.title = model.title + " — kavach map";
  }

  const SVG_NS = "http://www.w3.org/2000/svg";

  function el(tag, opts, ...children) {
    const node = document.createElement(tag);
    if (opts) {
      if (opts.className) node.className = opts.className;
      if (opts.text !== undefined) node.textContent = String(opts.text);
      if (opts.title !== undefined) node.title = String(opts.title);
      if (opts.type !== undefined) node.type = String(opts.type);
      if (opts.value !== undefined) node.value = String(opts.value);
      if (opts.placeholder !== undefined) node.placeholder = String(opts.placeholder);
      if (opts.disabled !== undefined) node.disabled = Boolean(opts.disabled);
      if (opts.selected !== undefined) node.selected = Boolean(opts.selected);
      if (opts.style !== undefined) node.setAttribute("style", String(opts.style));
      if (opts.dataset) {
        for (const [k, v] of Object.entries(opts.dataset)) {
          if (v !== undefined && v !== null) node.dataset[k] = String(v);
        }
      }
      if (opts.attrs) {
        for (const [k, v] of Object.entries(opts.attrs)) {
          if (v !== undefined && v !== null) node.setAttribute(k, String(v));
        }
      }
      if (opts.on) {
        for (const [ev, fn] of Object.entries(opts.on)) {
          node.addEventListener(ev, fn);
        }
      }
    }
    for (const child of children.flat(Infinity)) {
      if (child === null || child === undefined || child === false) continue;
      if (child instanceof Node) {
        node.appendChild(child);
      } else {
        node.appendChild(document.createTextNode(String(child)));
      }
    }
    return node;
  }

  function svgEl(tag, attrs, ...children) {
    const node = document.createElementNS(SVG_NS, tag);
    if (attrs) {
      for (const [k, v] of Object.entries(attrs)) {
        if (v !== undefined && v !== null) node.setAttribute(k, String(v));
      }
    }
    for (const child of children.flat(Infinity)) {
      if (child === null || child === undefined || child === false) continue;
      if (child instanceof Node) {
        node.appendChild(child);
      } else {
        node.appendChild(document.createTextNode(String(child)));
      }
    }
    return node;
  }

  function safeLink(url, text, className) {
    if (typeof url === "string" && /^https?:\/\//i.test(url)) {
      const a = el("a", { className: className || "code-link-item", text: text });
      a.setAttribute("href", url);
      a.setAttribute("target", "_blank");
      a.setAttribute("rel", "noopener");
      a.addEventListener("click", (e) => e.stopPropagation());
      return a;
    }
    return el("span", { className: className || "code-link-text", text: text });
  }

  function kindIconSVG(kind) {
    const base = { class: "kind-icon", viewBox: "0 0 20 20", fill: "none", stroke: "currentColor", "stroke-width": "1.6", "stroke-linecap": "round", "stroke-linejoin": "round" };
    switch (kind) {
      case "entry":
        return svgEl("svg", base, svgEl("path", { d: "M4 10h11" }), svgEl("path", { d: "M11 6l4 4-4 4" }));
      case "state":
        return svgEl("svg", base, svgEl("ellipse", { cx: "10", cy: "5.5", rx: "6", ry: "2.5" }), svgEl("path", { d: "M4 5.5v9c0 1.4 2.7 2.5 6 2.5s6-1.1 6-2.5v-9" }), svgEl("path", { d: "M4 10c0 1.4 2.7 2.5 6 2.5s6-1.1 6-2.5" }));
      case "sink":
        return svgEl("svg", base, svgEl("rect", { x: "3.5", y: "4.5", width: "13", height: "11", rx: "2" }), svgEl("path", { d: "M7 9.5h6M7 12.5h4" }));
      case "invariant":
        return svgEl("svg", base, svgEl("path", { d: "M10 2.5l6.5 2.5v4.5c0 4-2.8 7-6.5 8.2-3.7-1.2-6.5-4.2-6.5-8.2V5L10 2.5z" }), svgEl("path", { d: "M7.5 10l1.8 1.8 3.4-3.6" }));
      case "bug":
        return svgEl("svg", base, svgEl("path", { d: "M10 3.2L17 16H3L10 3.2z" }), svgEl("path", { d: "M10 8v3.5M10 13.8h.01" }));
      case "external":
        return svgEl("svg", base, svgEl("circle", { cx: "10", cy: "10", r: "6.5" }), svgEl("path", { d: "M3.8 10h12.4M10 3.5c1.8 2 1.8 11 0 13M10 3.5c-1.8 2-1.8 11 0 13" }));
      default:
        return svgEl("svg", base, svgEl("path", { d: "M6.5 6.5L3.5 10l3 3.5M13.5 6.5l3 3.5-3 3.5M11.2 4.5l-2.4 11" }));
    }
  }

  function brandMarkSVG() {
    return svgEl(
      "svg",
      { width: "13", height: "13", viewBox: "0 0 16 16", fill: "none", stroke: "currentColor", "stroke-width": "1.6", "stroke-linecap": "round", "stroke-linejoin": "round" },
      svgEl("rect", { x: "2", y: "2", width: "4.5", height: "4.5", rx: "1" }),
      svgEl("rect", { x: "9.5", y: "9.5", width: "4.5", height: "4.5", rx: "1" }),
      svgEl("path", { d: "M6.5 4.25h2.5a1 1 0 0 1 1 1V9.5" })
    );
  }

  function themeIconSVG(isDark) {
    if (isDark) {
      return svgEl("svg", { width: "15", height: "15", viewBox: "0 0 20 20", fill: "none", stroke: "currentColor", "stroke-width": "1.6", "stroke-linecap": "round" },
        svgEl("circle", { cx: "10", cy: "10", r: "3.5" }),
        svgEl("path", { d: "M10 2.5v1.8M10 15.7v1.8M2.5 10h1.8M15.7 10h1.8M4.7 4.7l1.3 1.3M14 14l1.3 1.3M15.3 4.7L14 6M6 14l-1.3 1.3" })
      );
    }
    return svgEl("svg", { width: "15", height: "15", viewBox: "0 0 20 20", fill: "none", stroke: "currentColor", "stroke-width": "1.6", "stroke-linecap": "round" },
      svgEl("path", { d: "M15.5 12.2A6 6 0 0 1 7.8 4.5a6.5 6.5 0 1 0 7.7 7.7z" })
    );
  }

  // Index lookups
  let groups = [];
  let nodes = [];
  let edges = [];
  let flows = [];
  let fixtures = [];
  let overlays = [];
  let stats = {};
  let warnings = [];

  const groupIndex = new Map();
  const nodeMap = new Map();
  const fixtureMap = new Map();
  const overlayMap = new Map();
  const adjOut = new Map();

  function rebuildIndexes(nextModel) {
    model = nextModel || {};
    if (model.title) {
      document.title = model.title + " — kavach map";
    }
    groups = model.groups || [];
    nodes = model.nodes || [];
    edges = model.edges || [];
    flows = model.flows || [];
    fixtures = model.fixtures || [];
    overlays = model.overlays || [];
    stats = model.stats || {};
    warnings = model.warnings || [];

    groupIndex.clear();
    groups.forEach((g, idx) => groupIndex.set(g.id, { group: g, index: idx }));

    nodeMap.clear();
    nodes.forEach((n) => nodeMap.set(n.id, n));

    fixtureMap.clear();
    fixtures.forEach((f) => fixtureMap.set(f.id, f));

    overlayMap.clear();
    overlays.forEach((o) => overlayMap.set(o.mode, o));

    adjOut.clear();
    edges.forEach((e, idx) => {
      if (!adjOut.has(e.from)) adjOut.set(e.from, []);
      adjOut.get(e.from).push({ to: e.to, edgeIdx: idx });
    });
  }

  rebuildIndexes(model);

  function findFixtureForFlow(flow) {
    if (!flow || !flow.fixture) return null;
    const base = flow.fixture.split("/").pop();
    for (const f of fixtures) {
      if (f.path === flow.fixture || f.label === flow.fixture || f.id === flow.fixture || f.path.split("/").pop() === base) {
        return f;
      }
    }
    return null;
  }

  function defaultRunForFixture(f) {
    if (!f || !f.runs || !f.runs.length) return null;
    return f.runs.find((r) => r.build === "old") || f.runs.find((r) => r.mode === "recorded") || f.runs[0];
  }

  function failingSeqForRun(f, r) {
    if (!r || !r.steps || !r.steps.length) return null;
    if (r.seq !== undefined && r.seq !== null) {
      let best = r.steps[0].seq;
      for (const st of r.steps) {
        if (st.seq <= r.seq) best = st.seq;
      }
      return best;
    }
    if (f && f.recorded && f.recorded.seq !== undefined) {
      const hit = r.steps.find((st) => st.seq === f.recorded.seq);
      if (hit) return hit.seq;
    }
    const bad = r.steps.find((st) => ["panic", "error", "invariant", "crash", "timeout", "diverged"].includes(st.outcome));
    if (bad) return bad.seq;
    return r.steps[r.steps.length - 1].seq;
  }

  function statusPillClass(st) {
    const s = String(st || "").toLowerCase();
    if (s === "ok" || s === "fixed" || s === "pass") return "pass";
    if (s === "still_failing" || s === "fail" || s === "panic" || s === "error" || s === "crash" || s === "timeout" || s === "variant_failed") return "fail";
    if (s === "invariant" || s === "invariant_violated") return "purple";
    if (s === "diverged" || s === "regressed") return "orange";
    if (s === "nondeterministic" || s === "warn" || s === "unverified") return "warn";
    if (s === "skip" || s === "not_reached") return "skip";
    return "info";
  }

  function currentThemeIsDark() {
    const attr = document.documentElement.getAttribute("data-theme");
    if (attr === "dark") return true;
    if (attr === "light") return false;
    return window.matchMedia && window.matchMedia("(prefers-color-scheme: dark)").matches;
  }

  const state = {
    tab: "map",
    showWarnings: false,
    showFlowsMenu: false,
    selectedNodeId: null,
    activeFlowId: null,
    flowStepIdx: 0,
    searchQuery: "",
    replayFixtureId: fixtures[0] ? fixtures[0].id : null,
    replayRunId: null,
    replaySeq: null,
    compareMode: false,
    replayPlaying: false,
    replaySpeed: 1,
    rawInspectorOpen: false,
    compExpanded: false,
    sourceLinksExpanded: false,
    piiXrayView: "stacked",
    invFixtureId: fixtures[0] ? fixtures[0].id : null,
    overlayMode: overlays[0] ? overlays[0].mode : null,
    overlayLaneId: null,
    overlayStageId: null,
  };

  function getCompareRuns(f) {
    if (!f || !f.runs || !f.runs.length) return { oldRun: null, newRun: null, hasCompare: false };
    const oldRun = f.runs.find((r) => r.build === "old") || f.runs.find((r) => r.id === "old") || null;
    const newRun = f.runs.find((r) => r.build === "new") || f.runs.find((r) => r.id === "new") || null;
    return { oldRun, newRun, hasCompare: Boolean(oldRun && newRun) };
  }

  function isCompareActive(f) {
    const { hasCompare } = getCompareRuns(f);
    return Boolean(hasCompare && state.compareMode);
  }

  function getAlignedSeqs(f) {
    if (!f) return [];
    const { oldRun, newRun, hasCompare } = getCompareRuns(f);
    if (hasCompare && isCompareActive(f)) {
      const seqSet = new Set();
      for (const st of oldRun.steps || []) seqSet.add(st.seq);
      for (const st of newRun.steps || []) seqSet.add(st.seq);
      return Array.from(seqSet).sort((a, b) => a - b);
    }
    const r = (f.runs || []).find((x) => x.id === state.replayRunId) || defaultRunForFixture(f);
    return r && r.steps ? r.steps.map((st) => st.seq) : [];
  }

  function isBadOutcome(outcome) {
    return ["panic", "error", "invariant", "crash", "timeout", "diverged"].includes(outcome);
  }

  function outputsSig(step) {
    if (!step || !step.outputs || !step.outputs.length) return "";
    return step.outputs.map((o) => (o.sink || "") + ":" + (o.data || "")).join("|");
  }

  function firstDivergenceSeqForFixture(f, activeRun) {
    if (!f) return null;
    const { oldRun, newRun, hasCompare } = getCompareRuns(f);
    if (hasCompare) {
      if (newRun && newRun.divergence && newRun.divergence.seq !== undefined && newRun.divergence.seq !== null) {
        return newRun.divergence.seq;
      }
      const oldBySeq = new Map((oldRun.steps || []).map((st) => [st.seq, st]));
      const newBySeq = new Map((newRun.steps || []).map((st) => [st.seq, st]));
      const seqs = Array.from(new Set([...oldBySeq.keys(), ...newBySeq.keys()])).sort((a, b) => a - b);
      for (const seq of seqs) {
        const os = oldBySeq.get(seq);
        const ns = newBySeq.get(seq);
        if (!os || !ns) return seq;
        if (isBadOutcome(os.outcome) || (os.detected && os.detected.length)) return seq;
        if (os.outcome !== ns.outcome || outputsSig(os) !== outputsSig(ns)) return seq;
      }
      if (oldRun.seq !== undefined && oldRun.seq !== null) return oldRun.seq;
    }
    return failingSeqForRun(f, activeRun || defaultRunForFixture(f));
  }

  function ensureReplaySelection() {
    if (!fixtures.length) return;
    const f = fixtureMap.get(state.replayFixtureId) || fixtures[0];
    state.replayFixtureId = f.id;
    const runs = f.runs || [];
    const r = runs.find((x) => x.id === state.replayRunId) || defaultRunForFixture(f);
    state.replayRunId = r ? r.id : null;
    const { hasCompare } = getCompareRuns(f);
    if (!hasCompare) {
      state.compareMode = false;
    }
    const seqs = getAlignedSeqs(f);
    if (seqs.length) {
      if (!seqs.includes(state.replaySeq)) {
        const defSeq = firstDivergenceSeqForFixture(f, r);
        state.replaySeq = seqs.includes(defSeq) ? defSeq : seqs[0];
      }
    } else {
      state.replaySeq = null;
    }
  }

  function ensureOverlaySelection(mode) {
    const ov = overlayMap.get(mode) || overlays[0];
    if (!ov) return;
    state.overlayMode = ov.mode;
    const lanes = ov.lanes || [];
    const lane = lanes.find((l) => l.id === state.overlayLaneId) || lanes[0];
    state.overlayLaneId = lane ? lane.id : null;
    if (lane && lane.stages && lane.stages.length) {
      if (!lane.stages.some((st) => st.id === state.overlayStageId)) {
        state.overlayStageId = lane.stages[0].id;
      }
    } else {
      state.overlayStageId = null;
    }
  }

  function setHash(h) {
    if (location.hash !== h) history.replaceState(null, "", h);
  }

  function syncHash() {
    if (state.tab === "map") {
      setHash("#map");
    } else if (state.tab === "replay" && state.replayFixtureId) {
      const f = fixtureMap.get(state.replayFixtureId);
      const r = isCompareActive(f) ? "compare" : (state.replayRunId || "recorded");
      const s = state.replaySeq !== null && state.replaySeq !== undefined ? state.replaySeq : 0;
      setHash("#replay/" + encodeURIComponent(state.replayFixtureId) + "/" + encodeURIComponent(r) + "/" + encodeURIComponent(s));
    } else if (state.tab === "invariants" && state.invFixtureId) {
      setHash("#invariants/" + encodeURIComponent(state.invFixtureId));
    } else if (state.tab.startsWith("mode:") && state.overlayMode) {
      const lanePart = state.overlayLaneId ? "/" + encodeURIComponent(state.overlayLaneId) : "";
      setHash("#mode/" + encodeURIComponent(state.overlayMode) + lanePart);
    }
  }

  function applyHash() {
    const raw = (location.hash || "").replace(/^#/, "");
    if (!raw || raw === "map") {
      state.tab = "map";
      return;
    }
    const parts = raw.split("/").map((p) => decodeURIComponent(p));
    if (parts[0] === "replay" && fixtures.length) {
      state.tab = "replay";
      if (parts[1] && fixtureMap.has(parts[1])) state.replayFixtureId = parts[1];
      if (parts[2] === "compare") {
        state.compareMode = true;
        state.replayRunId = null;
      } else if (parts[2]) {
        state.compareMode = false;
        state.replayRunId = parts[2];
      } else {
        state.compareMode = false;
        state.replayRunId = null;
      }
      if (parts[3] !== undefined && parts[3] !== "") {
        const num = Number(parts[3]);
        if (!Number.isNaN(num)) state.replaySeq = num;
      }
      ensureReplaySelection();
    } else if (parts[0] === "invariants" && fixtures.length) {
      state.tab = "invariants";
      if (parts[1] && fixtureMap.has(parts[1])) state.invFixtureId = parts[1];
    } else if (parts[0] === "mode" && parts[1] && overlayMap.has(parts[1])) {
      state.tab = "mode:" + parts[1];
      state.overlayMode = parts[1];
      if (parts[2]) state.overlayLaneId = parts[2];
      ensureOverlaySelection(parts[1]);
    } else {
      state.tab = "map";
    }
  }

  function jumpToReplay(fixtureId, runId, seq) {
    if (!fixtureMap.has(fixtureId)) return;
    stopReplayPlayback();
    state.tab = "replay";
    state.selectedNodeId = null;
    state.showFlowsMenu = false;
    state.replayFixtureId = fixtureId;
    if (runId === "compare") {
      state.compareMode = true;
      state.replayRunId = null;
    } else if (runId) {
      state.compareMode = false;
      state.replayRunId = runId;
    } else {
      state.compareMode = false;
      state.replayRunId = null;
    }
    state.replaySeq = seq !== undefined && seq !== null ? Number(seq) : null;
    replayRuntime.lastAnimatedKey = null;
    replayRuntime.needsCameraFocus = true;
    replayRuntime.userOverrodeCamera = false;
    ensureReplaySelection();
    syncHash();
    render();
  }

  // Orthogonal rounded-corner path helper (Floorplan architectural wire style)
  function buildOrthoPath(points, radius) {
    if (points.length < 2) return "";
    let d = "M " + points[0][0] + " " + points[0][1];
    for (let i = 1; i < points.length - 1; i++) {
      const prev = points[i - 1];
      const cur = points[i];
      const next = points[i + 1];
      const dx1 = cur[0] - prev[0];
      const dy1 = cur[1] - prev[1];
      const dx2 = next[0] - cur[0];
      const dy2 = next[1] - cur[1];
      const len1 = Math.hypot(dx1, dy1);
      const len2 = Math.hypot(dx2, dy2);
      const r = Math.min(radius, len1 / 2, len2 / 2);
      if (r < 0.5) {
        d += " L " + cur[0] + " " + cur[1];
        continue;
      }
      const p1x = cur[0] - (dx1 / len1) * r;
      const p1y = cur[1] - (dy1 / len1) * r;
      const p2x = cur[0] + (dx2 / len2) * r;
      const p2y = cur[1] + (dy2 / len2) * r;
      d += " L " + p1x + " " + p1y + " Q " + cur[0] + " " + cur[1] + " " + p2x + " " + p2y;
    }
    const last = points[points.length - 1];
    d += " L " + last[0] + " " + last[1];
    return d;
  }

  // Floorplan Map Canvas
  function buildMapCanvas(opts) {
    const CARD_W = 248;
    const COL_W = 340;
    const PAD_X = 40;
    const PAD_Y = 24;

    const wrap = el("div", { className: "canvas-wrap" });
    if (opts.caption) {
      wrap.appendChild(
        el(
          "div",
          { className: "canvas-caption" },
          el("div", { className: "canvas-caption-title", text: opts.caption.title }),
          opts.caption.sub ? el("div", { className: "canvas-caption-sub", text: opts.caption.sub }) : null
        )
      );
    }

    const world = el("div", { className: "canvas-world" });
    const svg = svgEl("svg", { class: "edges-svg" });
    world.appendChild(svg);
    wrap.appendChild(world);

    const cardEls = new Map();
    const colContainers = new Map();
    const nodesByGroup = new Map();
    groups.forEach((g) => nodesByGroup.set(g.id, []));
    nodes.forEach((n) => {
      if (!nodesByGroup.has(n.group)) nodesByGroup.set(n.group, []);
      nodesByGroup.get(n.group).push(n);
    });

    const query = (opts.searchQuery || "").trim().toLowerCase();

    groups.forEach((g, colIdx) => {
      const colEl = el("div", { className: "group-col" });
      colEl.style.left = PAD_X + colIdx * COL_W + "px";
      colEl.style.top = PAD_Y + "px";

      colEl.appendChild(
        el(
          "div",
          { className: "group-header" },
          el("div", { className: "group-title", text: g.title || g.id }),
          g.plain ? el("div", { className: "group-plain", text: g.plain }) : null
        )
      );

      for (const n of nodesByGroup.get(g.id) || []) {
        const isHighlighted = opts.highlightNodes ? opts.highlightNodes.has(n.id) : true;
        const matchesSearch =
          !query ||
          (n.title || "").toLowerCase().includes(query) ||
          (n.plain || "").toLowerCase().includes(query) ||
          (n.id || "").toLowerCase().includes(query) ||
          (n.code || []).some((c) => c.toLowerCase().includes(query));
        const dimmed = (opts.highlightNodes && opts.highlightNodes.size > 0 && !isHighlighted) || !matchesSearch;

        const cls = ["card"];
        if (opts.selectedNodeId === n.id) cls.push("selected");
        if (dimmed) cls.push("dimmed");
        if (opts.flowActiveNodeId === n.id) cls.push("flow-active");
        if (!opts.replayMode) {
          if (opts.hitRoles && opts.hitRoles.has(n.id)) {
            cls.push("hit-" + opts.hitRoles.get(n.id));
          } else if (opts.highlightNodes && opts.highlightNodes.has(n.id)) {
            cls.push("path-active");
          }
        }
        if (opts.extraCardClasses && opts.extraCardClasses.has(n.id)) {
          for (const ec of opts.extraCardClasses.get(n.id)) cls.push(ec);
        }

        const firstLink = n.links && n.links.length ? n.links[0] : null;
        const flowStepInfo = opts.flowStepsByNode ? opts.flowStepsByNode.get(n.id) : null;

        let tagEl = null;
        if (n.kind === "bug") {
          tagEl = el("span", { className: "card-tag bug" }, el("span", { className: "card-tag-dot" }), "Bug");
        } else if (n.kind === "invariant") {
          tagEl = el("span", { className: "card-tag invariant" }, el("span", { className: "card-tag-dot" }), "Invariant");
        }

        const card = el(
          "div",
          {
            className: cls.join(" "),
            dataset: { kind: n.kind, nodeId: n.id },
            on: {
              click: (e) => {
                e.stopPropagation();
                if (opts.onSelectNode) opts.onSelectNode(n.id);
              },
            },
          },
          el("div", { className: "card-head" }, kindIconSVG(n.kind), el("div", { className: "card-title", text: n.title || n.id })),
          n.plain ? el("div", { className: "card-plain", text: n.plain }) : null,
          flowStepInfo
            ? el(
                "div",
                { className: "flow-step-chip" },
                el("span", { className: "flow-num", text: String(flowStepInfo.num) }),
                el("span", { text: flowStepInfo.short })
              )
            : null,
          firstLink || tagEl
            ? el("div", { className: "card-foot" }, firstLink ? safeLink(firstLink.url, firstLink.text, "card-code") : el("span"), tagEl)
            : null
        );
        colEl.appendChild(card);
        cardEls.set(n.id, { el: card, colIdx: colIdx });
      }

      world.appendChild(colEl);
      colContainers.set(g.id, colEl);
    });

    const view = opts.viewState || { tx: 0, ty: 0, scale: 1, initialized: false };

    function applyTransform() {
      world.style.transform = "translate(" + view.tx + "px, " + view.ty + "px) scale(" + view.scale + ")";
      if (pctEl) pctEl.textContent = Math.round(view.scale * 100) + "%";
    }

    function fitToView() {
      const ww = Math.max(420, groups.length * COL_W + PAD_X * 2);
      let wh = 420;
      for (const colEl of colContainers.values()) {
        wh = Math.max(wh, colEl.offsetTop + colEl.offsetHeight + 40);
      }
      const vw = wrap.clientWidth || 960;
      const vh = wrap.clientHeight || 640;
      const topPad = opts.topPad !== undefined ? opts.topPad : (opts.caption ? 76 : 16);
      const bottomPad = opts.bottomPad !== undefined ? opts.bottomPad : (opts.caption ? 168 : 28);
      const sx = (vw - 32) / ww;
      const sy = (vh - topPad - bottomPad) / wh;
      view.scale = Math.max(0.28, Math.min(1.05, Math.min(sx, sy)));
      view.tx = Math.max(16, (vw - ww * view.scale) / 2);
      view.ty = topPad;
      view.initialized = true;
      applyTransform();
    }

    const pctEl = el("span", { className: "zoom-pct", text: Math.round(view.scale * 100) + "%" });
    wrap.appendChild(
      el(
        "div",
        { className: "zoom-bar", on: { click: (e) => e.stopPropagation() } },
        el("button", {
          className: "icon-btn",
          text: "−",
          title: "Zoom out",
          on: {
            click: () => {
              if (opts.onUserInteract) opts.onUserInteract("zoom");
              view.scale = Math.max(0.22, view.scale * 0.85);
              applyTransform();
            },
          },
        }),
        pctEl,
        el("button", {
          className: "icon-btn",
          text: "+",
          title: "Zoom in",
          on: {
            click: () => {
              if (opts.onUserInteract) opts.onUserInteract("zoom");
              view.scale = Math.min(2.2, view.scale * 1.18);
              applyTransform();
            },
          },
        }),
        el("button", {
          className: "btn",
          style: "height:26px;border-radius:999px;padding:0 9px;border:none;",
          text: "Fit",
          title: "Fit to view",
          on: {
            click: () => {
              if (opts.onUserInteract) opts.onUserInteract("fit");
              fitToView();
            },
          },
        })
      )
    );

    let dragging = false;
    let moved = false;
    let dragStartX = 0;
    let dragStartY = 0;
    let startTx = 0;
    let startTy = 0;

    wrap.addEventListener("pointerdown", (e) => {
      if (
        e.target.closest(".card") ||
        e.target.closest(".zoom-bar") ||
        e.target.closest(".walkthrough-bar") ||
        e.target.closest(".inspector") ||
        e.target.closest(".flows-popover")
      ) return;
      if (opts.onUserInteract) opts.onUserInteract("pan");
      dragging = true;
      moved = false;
      dragStartX = e.clientX;
      dragStartY = e.clientY;
      startTx = view.tx;
      startTy = view.ty;
      wrap.classList.add("panning");
      wrap.setPointerCapture(e.pointerId);
    });
    wrap.addEventListener("pointermove", (e) => {
      if (!dragging) return;
      const dx = e.clientX - dragStartX;
      const dy = e.clientY - dragStartY;
      if (Math.abs(dx) > 3 || Math.abs(dy) > 3) moved = true;
      view.tx = startTx + dx;
      view.ty = startTy + dy;
      applyTransform();
    });
    wrap.addEventListener("pointerup", (e) => {
      if (!dragging) return;
      dragging = false;
      wrap.classList.remove("panning");
      try {
        wrap.releasePointerCapture(e.pointerId);
      } catch (_) {}
      if (!moved && opts.onBackgroundClick) opts.onBackgroundClick();
    });

    wrap.addEventListener(
      "wheel",
      (e) => {
        if (!e.ctrlKey && !e.metaKey) return;
        e.preventDefault();
        if (opts.onUserInteract) opts.onUserInteract("wheel");
        const rect = wrap.getBoundingClientRect();
        const mx = e.clientX - rect.left;
        const my = e.clientY - rect.top;
        const factor = e.deltaY < 0 ? 1.1 : 0.9;
        const nextScale = Math.max(0.22, Math.min(2.4, view.scale * factor));
        const ratio = nextScale / view.scale;
        view.tx = mx - (mx - view.tx) * ratio;
        view.ty = my - (my - view.ty) * ratio;
        view.scale = nextScale;
        applyTransform();
      },
      { passive: false }
    );

    const edgePathMap = new Map();
    const cardGeom = new Map();

    function drawEdges() {
      while (svg.firstChild) svg.removeChild(svg.firstChild);
      edgePathMap.clear();
      cardGeom.clear();

      let maxW = groups.length * COL_W + PAD_X * 2;
      let maxH = 420;
      for (const colEl of colContainers.values()) {
        maxW = Math.max(maxW, colEl.offsetLeft + colEl.offsetWidth + PAD_X);
        maxH = Math.max(maxH, colEl.offsetTop + colEl.offsetHeight + 80);
      }
      world.style.width = maxW + "px";
      world.style.height = maxH + "px";
      svg.setAttribute("width", String(maxW));
      svg.setAttribute("height", String(maxH));
      svg.setAttribute("viewBox", "0 0 " + maxW + " " + maxH);

      for (const [nid, info] of cardEls.entries()) {
        const col = info.el.offsetParent;
        if (!col) continue;
        const x = col.offsetLeft + info.el.offsetLeft;
        const y = col.offsetTop + info.el.offsetTop;
        const w = info.el.offsetWidth || CARD_W;
        const h = info.el.offsetHeight || 72;
        cardGeom.set(nid, { x, y, w, h, cx: x + w / 2, cy: y + h / 2, el: info.el });
      }

      const hasEdgeHL = opts.highlightEdges && opts.highlightEdges.size > 0;
      const hasNodeHL = opts.highlightNodes && opts.highlightNodes.size > 0;

      // Distribute vertical routing channels cleanly per column gap
      const gapCounters = new Map();

      // Render dimmed edges first so highlighted edges always sit cleanly on top
      const ordered = edges.map((e, idx) => {
        const isHL = hasEdgeHL
          ? opts.highlightEdges.has(idx)
          : hasNodeHL && opts.highlightNodes.has(e.from) && opts.highlightNodes.has(e.to);
        return { e, idx, isHL };
      });
      ordered.sort((a, b) => Number(a.isHL) - Number(b.isHL));

      for (const item of ordered) {
        const { e, idx, isHL } = item;
        const src = cardEls.get(e.from);
        const dst = cardEls.get(e.to);
        if (!src || !dst) continue;
        const srcCol = src.el.offsetParent;
        const dstCol = dst.el.offsetParent;
        if (!srcCol || !dstCol) continue;

        const x1 = srcCol.offsetLeft + src.el.offsetLeft;
        const y1 = srcCol.offsetTop + src.el.offsetTop;
        const w1 = src.el.offsetWidth || CARD_W;
        const h1 = src.el.offsetHeight || 72;

        const x2 = dstCol.offsetLeft + dst.el.offsetLeft;
        const y2 = dstCol.offsetTop + dst.el.offsetTop;
        const w2 = dst.el.offsetWidth || CARD_W;
        const h2 = dst.el.offsetHeight || 72;

        const cy1 = y1 + h1 / 2;
        const cy2 = y2 + h2 / 2;

        const gapKey = src.colIdx + "->" + dst.colIdx;
        const slot = gapCounters.get(gapKey) || 0;
        gapCounters.set(gapKey, slot + 1);
        const jitter = ((slot % 5) - 2) * 8;

        let pts;
        let labelX, labelY;
        if (src.colIdx === dst.colIdx) {
          const railX = x1 + w1 + 26 + (slot % 4) * 9;
          pts = [
            [x1 + w1, cy1],
            [railX, cy1],
            [railX, cy2],
            [x2 + w2, cy2],
          ];
          labelX = railX;
          labelY = (cy1 + cy2) / 2;
        } else if (src.colIdx < dst.colIdx) {
          const startX = x1 + w1;
          const endX = x2;
          const midX = startX + (endX - startX) * 0.5 + jitter;
          pts = [
            [startX, cy1],
            [midX, cy1],
            [midX, cy2],
            [endX, cy2],
          ];
          labelX = midX;
          labelY = (cy1 + cy2) / 2;
        } else {
          const startX = x1;
          const endX = x2 + w2;
          const midX = endX + (startX - endX) * 0.5 + jitter;
          pts = [
            [startX, cy1],
            [midX, cy1],
            [midX, cy2],
            [endX, cy2],
          ];
          labelX = midX;
          labelY = (cy1 + cy2) / 2;
        }

        const kind = e.kind || "flows";
        const isDim = (hasEdgeHL || hasNodeHL) && !isHL;
        const pathCls = ["edge-path", "kind-" + kind];
        if (isHL && !opts.replayMode) pathCls.push("highlighted");
        if (isDim) pathCls.push("dimmed");
        if (opts.extraEdgeClasses && opts.extraEdgeClasses.has(idx)) {
          for (const ec of opts.extraEdgeClasses.get(idx)) pathCls.push(ec);
        }

        const pathD = buildOrthoPath(pts, 10);
        const pathEl = svgEl("path", { class: pathCls.join(" "), d: pathD });
        svg.appendChild(pathEl);
        edgePathMap.set(idx, { pathEl, pts, d: pathD, e, labelX, labelY });

        const portCls = ["edge-port", "kind-" + kind];
        if (isHL && !opts.replayMode) portCls.push("highlighted");
        if (isDim) portCls.push("dimmed");
        svg.appendChild(svgEl("circle", { class: portCls.join(" "), cx: pts[0][0], cy: pts[0][1], r: isHL ? "3.2" : "2.4" }));
        const lastPt = pts[pts.length - 1];
        svg.appendChild(svgEl("circle", { class: portCls.join(" "), cx: lastPt[0], cy: lastPt[1], r: isHL ? "3.2" : "2.4" }));

        const labelText = e.label || (e.auto && e.count > 0 ? kind + " ×" + e.count : "");
        if (labelText && isHL && !opts.suppressEdgeLabels) {
          const lblCls = ["edge-label", "highlighted"];
          const approxW = Math.min(168, labelText.length * 6 + 12);
          svg.appendChild(svgEl("rect", { class: "edge-label-bg", x: labelX - approxW / 2, y: labelY - 9, width: approxW, height: 16, rx: 4 }));
          svg.appendChild(svgEl("text", { class: lblCls.join(" "), x: labelX, y: labelY + 2.5 }, labelText));
        }
      }
    }

    requestAnimationFrame(() => {
      drawEdges();
      if (!view.initialized) {
        if (!opts.skipInitialFit) fitToView();
      } else {
        applyTransform();
      }
      if (opts.onAfterDraw) {
        opts.onAfterDraw({ svg, world, wrap, cardEls, cardGeom, edgePathMap, view, applyTransform, fitToView });
      }
    });

    return wrap;
  }

  const mapViewState = { tx: 0, ty: 0, scale: 1, initialized: false };
  const replayMapViewState = { tx: 0, ty: 0, scale: 1, initialized: false };
  const overlayMapViewState = { tx: 0, ty: 0, scale: 1, initialized: false };

  function renderTopBar() {
    const rightItems = [];
    const addTab = (id, label) => {
      rightItems.push(
        el("button", {
          className: "nav-btn" + (state.tab === id ? " active" : ""),
          text: label,
          on: {
            click: () => {
              stopReplayPlayback();
              state.tab = id;
              state.showFlowsMenu = false;
              if (id === "replay") {
                replayRuntime.needsCameraFocus = true;
                replayRuntime.userOverrodeCamera = false;
                replayRuntime.lastAnimatedKey = null;
                ensureReplaySelection();
              }
              if (id.startsWith("mode:")) ensureOverlaySelection(id.slice(5));
              syncHash();
              render();
            },
          },
        })
      );
    };

    addTab("map", "Map");
    if (fixtures.length) {
      addTab("replay", "Replay");
      addTab("invariants", "Invariants");
    }
    for (const ov of overlays) {
      addTab("mode:" + ov.mode, ov.mode.charAt(0).toUpperCase() + ov.mode.slice(1));
    }

    if (flows.length && state.tab === "map") {
      rightItems.push(
        el(
          "button",
          {
            className: "nav-btn" + (state.showFlowsMenu || state.activeFlowId ? " active" : ""),
            on: {
              click: () => {
                state.showFlowsMenu = !state.showFlowsMenu;
                if (state.showFlowsMenu) state.selectedNodeId = null;
                render();
              },
            },
          },
          "Flows",
          el("span", { className: "count-dot", text: String(flows.length) })
        )
      );
    }

    if (warnings.length) {
      rightItems.push(
        el(
          "button",
          {
            className: "nav-btn",
            on: {
              click: () => {
                state.showWarnings = !state.showWarnings;
                render();
              },
            },
          },
          "Warnings",
          el("span", { className: "count-dot warn", text: String(warnings.length) })
        )
      );
    }

    const isDark = currentThemeIsDark();
    rightItems.push(
      el(
        "button",
        {
          className: "icon-btn",
          title: isDark ? "Switch to light mode" : "Switch to dark mode",
          on: {
            click: () => {
              document.documentElement.setAttribute("data-theme", isDark ? "light" : "dark");
              render();
            },
          },
        },
        themeIconSVG(isDark)
      )
    );

    const searchBox =
      state.tab === "map"
        ? el(
            "div",
            { className: "search-box" },
            el("input", {
              className: "search-input",
              type: "search",
              placeholder: "Search cards, files, ops…",
              value: state.searchQuery,
              attrs: { "data-kmap-search": "1" },
              on: {
                input: (e) => {
                  state.searchQuery = e.target.value;
                  const pos = e.target.selectionStart;
                  render();
                  const inp = document.querySelector('[data-kmap-search="1"]');
                  if (inp) {
                    inp.focus();
                    try {
                      inp.setSelectionRange(pos, pos);
                    } catch (_) {}
                  }
                },
              },
            }),
            state.searchQuery ? null : el("span", { className: "search-kbd", text: "/" })
          )
        : null;

    return el(
      "header",
      { className: "topbar" },
      el(
        "div",
        { className: "topbar-left" },
        el("span", { className: "brand-mark" }, brandMarkSVG()),
        el("h1", { className: "brand-title", text: model.title || "Kavach Map" }),
        renderLivePill()
      ),
      searchBox,
      el("nav", { className: "topbar-right" }, rightItems)
    );
  }

  function renderNodeInspector(nodeId) {
    const n = nodeMap.get(nodeId);
    if (!n) return null;
    const gInfo = groupIndex.get(n.group);
    const st = stats[n.id] || {};
    const incoming = edges.filter((e) => e.to === n.id);
    const outgoing = edges.filter((e) => e.from === n.id);

    const insp = el("aside", { className: "inspector", on: { click: (e) => e.stopPropagation() } });
    insp.appendChild(
      el(
        "div",
        { className: "inspector-head" },
        el(
          "div",
          null,
          el("h3", { text: n.title || n.id }),
          el(
            "div",
            { className: "insp-meta" },
            el("span", { className: "pill", text: n.kind }),
            gInfo ? el("span", { className: "pill", text: gInfo.group.title }) : null,
            n.auto ? el("span", { className: "pill info", text: "auto" }) : null
          )
        ),
        el("button", {
          className: "icon-btn",
          text: "✕",
          title: "Close",
          on: {
            click: () => {
              state.selectedNodeId = null;
              render();
            },
          },
        })
      )
    );

    if (n.plain) insp.appendChild(el("div", { text: n.plain }));
    if (n.detail) insp.appendChild(el("div", { className: "sub-text", text: n.detail }));

    if (n.links && n.links.length) {
      insp.appendChild(
        el(
          "div",
          { className: "insp-section" },
          el("h4", { text: "Code references" }),
          el("div", { className: "link-list" }, n.links.map((lk) => safeLink(lk.url, lk.text)))
        )
      );
    }

    if (incoming.length || outgoing.length) {
      const list = el("div", { className: "edge-list" });
      for (const e of incoming) {
        const other = nodeMap.get(e.from);
        list.appendChild(
          el(
            "button",
            {
              className: "row-btn",
              on: {
                click: () => {
                  state.selectedNodeId = e.from;
                  render();
                },
              },
            },
            el("span", { text: "From " + (other ? other.title : e.from) }),
            el("span", { className: "pill mono", text: e.label || e.kind || "flows" })
          )
        );
      }
      for (const e of outgoing) {
        const other = nodeMap.get(e.to);
        list.appendChild(
          el(
            "button",
            {
              className: "row-btn",
              on: {
                click: () => {
                  state.selectedNodeId = e.to;
                  render();
                },
              },
            },
            el("span", { text: "To " + (other ? other.title : e.to) }),
            el("span", { className: "pill mono", text: e.label || e.kind || "flows" })
          )
        );
      }
      insp.appendChild(el("div", { className: "insp-section" }, el("h4", { text: "Connections" }), list));
    }

    if (n.kind === "invariant") {
      const invSec = el("div", { className: "insp-section" }, el("h4", { text: "Invariant" }));
      invSec.appendChild(el("span", { className: "pill purple mono", text: n.invariant || n.title }));
      if (st.variants_old || st.variants_new) {
        invSec.appendChild(
          el("div", {
            className: "sub-text",
            style: "margin-top:6px;",
            text: "Broke in " + (st.variants_old || 0) + " variants on old, " + (st.variants_new || 0) + " on new",
          })
        );
      }
      if (st.violations && st.violations.length) {
        const vList = el("div", { className: "ref-list", style: "margin-top:8px;" });
        for (const ref of st.violations) {
          vList.appendChild(
            el(
              "button",
              { className: "row-btn", on: { click: () => jumpToReplay(ref.fixture, ref.run, ref.seq) } },
              el("span", { text: ref.fixture + " (" + ref.run + ")" }),
              el("span", { className: "pill fail mono", text: "seq " + ref.seq })
            )
          );
        }
        invSec.appendChild(vList);
      }
      insp.appendChild(invSec);
    }

    if (st.hits && st.hits.length) {
      const list = el("div", { className: "ref-list" });
      for (const ref of st.hits.slice(0, 30)) {
        list.appendChild(
          el(
            "button",
            { className: "row-btn", on: { click: () => jumpToReplay(ref.fixture, ref.run, ref.seq) } },
            el("span", { text: ref.fixture + " (" + ref.run + ")" }),
            el("span", { className: "pill mono", text: "seq " + ref.seq })
          )
        );
      }
      insp.appendChild(el("div", { className: "insp-section" }, el("h4", { text: "Runtime hits (" + st.hits.length + ")" }), list));
    }

    return insp;
  }

  function renderMapTab() {
    const activeFlow = flows.find((f) => f.id === state.activeFlowId) || null;
    const flowNodes = new Set();
    const flowEdges = new Set();
    const flowStepsByNode = new Map();
    let flowActiveNodeId = null;

    if (activeFlow && activeFlow.steps && activeFlow.steps.length) {
      if (state.flowStepIdx >= activeFlow.steps.length) state.flowStepIdx = activeFlow.steps.length - 1;
      if (state.flowStepIdx < 0) state.flowStepIdx = 0;
      activeFlow.steps.forEach((st, idx) => {
        flowNodes.add(st.node);
        if (!flowStepsByNode.has(st.node)) {
          const short = st.text.length > 62 ? st.text.slice(0, 59) + "…" : st.text;
          flowStepsByNode.set(st.node, { num: idx + 1, short: short });
        }
      });
      flowActiveNodeId = activeFlow.steps[state.flowStepIdx].node;
      edges.forEach((e, idx) => {
        if (flowNodes.has(e.from) && flowNodes.has(e.to)) flowEdges.add(idx);
      });
    }

    const caption = activeFlow
      ? { title: activeFlow.title, sub: activeFlow.plain || "" }
      : null;

    const canvas = buildMapCanvas({
      caption: caption,
      highlightNodes: activeFlow ? flowNodes : null,
      highlightEdges: activeFlow ? flowEdges : null,
      flowStepsByNode: activeFlow ? flowStepsByNode : null,
      flowActiveNodeId: flowActiveNodeId,
      selectedNodeId: state.selectedNodeId,
      searchQuery: state.searchQuery,
      viewState: mapViewState,
      onSelectNode: (nid) => {
        state.showFlowsMenu = false;
        state.selectedNodeId = state.selectedNodeId === nid ? null : nid;
        render();
      },
      onBackgroundClick: () => {
        if (state.selectedNodeId || state.showFlowsMenu) {
          state.selectedNodeId = null;
          state.showFlowsMenu = false;
          render();
        }
      },
    });

    if (state.showFlowsMenu) {
      const pop = el("div", { className: "flows-popover", on: { click: (e) => e.stopPropagation() } });
      pop.appendChild(
        el(
          "div",
          { className: "popover-head" },
          el("h3", { text: "Guided flows" }),
          el("button", {
            className: "icon-btn",
            text: "✕",
            on: {
              click: () => {
                state.showFlowsMenu = false;
                render();
              },
            },
          })
        )
      );
      for (const f of flows) {
        pop.appendChild(
          el(
            "button",
            {
              className: "flow-item" + (state.activeFlowId === f.id ? " active" : ""),
              on: {
                click: () => {
                  state.activeFlowId = f.id;
                  state.flowStepIdx = 0;
                  state.showFlowsMenu = false;
                  state.selectedNodeId = null;
                  mapViewState.initialized = false;
                  render();
                },
              },
            },
            el("div", { className: "flow-item-title", text: f.title || f.id }),
            f.plain ? el("div", { className: "flow-item-sub", text: f.plain }) : null
          )
        );
      }
      canvas.appendChild(pop);
    }

    if (activeFlow && activeFlow.steps && activeFlow.steps.length) {
      const cur = activeFlow.steps[state.flowStepIdx];
      const fixMatch = findFixtureForFlow(activeFlow);
      canvas.appendChild(
        el(
          "div",
          { className: "walkthrough-bar", on: { click: (e) => e.stopPropagation() } },
          el(
            "div",
            { className: "walkthrough-top" },
            el("div", { className: "walkthrough-title", text: "Step " + (state.flowStepIdx + 1) + " of " + activeFlow.steps.length }),
            el(
              "button",
              {
                className: "btn",
                text: "Back to map",
                on: {
                  click: () => {
                    state.activeFlowId = null;
                    state.flowStepIdx = 0;
                    mapViewState.initialized = false;
                    render();
                  },
                },
              }
            )
          ),
          el("div", { className: "walkthrough-text", text: cur.text }),
          el(
            "div",
            { className: "walkthrough-actions" },
            el(
              "div",
              { className: "walkthrough-nav" },
              el("button", {
                className: "btn",
                text: "Previous",
                disabled: state.flowStepIdx === 0,
                on: {
                  click: () => {
                    state.flowStepIdx = Math.max(0, state.flowStepIdx - 1);
                    render();
                  },
                },
              }),
              el("button", {
                className: "btn",
                text: "Next",
                disabled: state.flowStepIdx >= activeFlow.steps.length - 1,
                on: {
                  click: () => {
                    state.flowStepIdx = Math.min(activeFlow.steps.length - 1, state.flowStepIdx + 1);
                    render();
                  },
                },
              })
            ),
            fixMatch
              ? el("button", {
                  className: "btn primary",
                  text: "Open in Replay",
                  on: {
                    click: () => {
                      const run = defaultRunForFixture(fixMatch);
                      jumpToReplay(fixMatch.id, run ? run.id : null, failingSeqForRun(fixMatch, run));
                    },
                  },
                })
              : null
          )
        )
      );
    }

    if (state.selectedNodeId) {
      const insp = renderNodeInspector(state.selectedNodeId);
      if (insp) canvas.appendChild(insp);
    }

    return canvas;
  }

  const replayRuntime = {
    playTimer: null,
    animRaf: null,
    zoomRaf: null,
    hopTimers: [],
    animToken: 0,
    activeCanvasCtx: null,
    lastAnimatedKey: null,
    needsCameraFocus: true,
    userOverrodeCamera: false,
  };

  function prefersReducedMotion() {
    return Boolean(window.matchMedia && window.matchMedia("(prefers-reduced-motion: reduce)").matches);
  }

  function clearHopTimers() {
    for (const id of replayRuntime.hopTimers) clearTimeout(id);
    replayRuntime.hopTimers = [];
  }

  function stopReplayAnim() {
    replayRuntime.animToken += 1;
    clearHopTimers();
    if (replayRuntime.animRaf !== null) {
      cancelAnimationFrame(replayRuntime.animRaf);
      replayRuntime.animRaf = null;
    }
  }

  function stopZoomAnim() {
    if (replayRuntime.zoomRaf !== null) {
      cancelAnimationFrame(replayRuntime.zoomRaf);
      replayRuntime.zoomRaf = null;
    }
  }

  function stopReplayPlayback() {
    if (replayRuntime.playTimer !== null) {
      clearTimeout(replayRuntime.playTimer);
      replayRuntime.playTimer = null;
    }
    state.replayPlaying = false;
  }

  function parseJsonSafe(s) {
    if (!s || typeof s !== "string") return null;
    try {
      return JSON.parse(s);
    } catch (_) {
      return null;
    }
  }

  function formatTruncatedJson(s) {
    const t = String(s || "").trim();
    if (!t || (t[0] !== "{" && t[0] !== "[")) return s;
    let out = "";
    let indent = 0;
    let inStr = false;
    let esc = false;
    for (let i = 0; i < t.length; i++) {
      const ch = t[i];
      if (inStr) {
        out += ch;
        if (esc) esc = false;
        else if (ch === "\\") esc = true;
        else if (ch === '"') inStr = false;
        continue;
      }
      if (ch === '"') {
        inStr = true;
        out += ch;
      } else if (ch === "{" || ch === "[") {
        const next = t[i + 1];
        if ((ch === "{" && next === "}") || (ch === "[" && next === "]")) {
          out += ch + next;
          i++;
        } else {
          indent++;
          out += ch + "\n" + "  ".repeat(indent);
        }
      } else if (ch === "}" || ch === "]") {
        indent = Math.max(0, indent - 1);
        out += "\n" + "  ".repeat(indent) + ch;
      } else if (ch === ",") {
        out += ",\n" + "  ".repeat(indent);
      } else if (ch === ":") {
        out += ": ";
      } else if (ch === " " || ch === "\n" || ch === "\r" || ch === "\t") {
        // strip raw whitespace outside strings
      } else {
        out += ch;
      }
    }
    return out;
  }

  function prettyJson(s) {
    if (!s) return "";
    const obj = parseJsonSafe(s);
    if (obj !== null) return JSON.stringify(obj, null, 2);
    return formatTruncatedJson(s);
  }

  function sourceFileIconSVG() {
    return svgEl(
      "svg",
      {
        class: "sb-source-file-icon",
        width: "12",
        height: "12",
        viewBox: "0 0 16 16",
        fill: "none",
        stroke: "currentColor",
        "stroke-width": "1.5",
        "stroke-linecap": "round",
        "stroke-linejoin": "round",
        "aria-hidden": "true",
      },
      svgEl("path", { d: "M9.5 2H4.5A1.5 1.5 0 0 0 3 3.5v9A1.5 1.5 0 0 0 4.5 14h7a1.5 1.5 0 0 0 1.5-1.5V5.5L9.5 2z" }),
      svgEl("path", { d: "M9.5 2v3.5H13" })
    );
  }

  function parseSourceLinkRef(rawText) {
    const text = String(rawText || "").trim();
    let pathPart = text;
    let linePart = "";
    const colonIdx = text.lastIndexOf(":");
    if (colonIdx > 0 && colonIdx < text.length - 1) {
      const suffix = text.slice(colonIdx + 1);
      if (/^\d+(-\d+)?$/.test(suffix)) {
        pathPart = text.slice(0, colonIdx);
        linePart = ":" + suffix;
      }
    }
    const slashIdx = pathPart.lastIndexOf("/");
    const dirPart = slashIdx >= 0 ? pathPart.slice(0, slashIdx + 1) : "";
    const filePart = slashIdx >= 0 ? pathPart.slice(slashIdx + 1) : pathPart;
    return { full: text, dir: dirPart, file: filePart, lines: linePart };
  }

  function buildSourceLinkRow(lk) {
    const parsed = parseSourceLinkRef(lk.text || lk.url);
    const isExt = typeof lk.url === "string" && /^https?:\/\//i.test(lk.url);
    const pathEl = el(
      "span",
      { className: "sb-source-path" },
      parsed.dir ? el("span", { className: "sb-source-dir", text: parsed.dir }) : null,
      el("span", { className: "sb-source-file", text: parsed.file || parsed.full })
    );
    const lineEl = parsed.lines ? el("span", { className: "sb-source-lines", text: parsed.lines }) : null;

    if (isExt) {
      const a = el(
        "a",
        {
          className: "sb-source-row is-external",
          title: parsed.full || lk.url,
        },
        sourceFileIconSVG(),
        pathEl,
        lineEl,
        el("span", { className: "sb-source-ext", text: "↗", attrs: { "aria-hidden": "true" } })
      );
      a.setAttribute("href", lk.url);
      a.setAttribute("target", "_blank");
      a.setAttribute("rel", "noopener");
      a.addEventListener("click", (e) => e.stopPropagation());
      return a;
    }

    return el(
      "div",
      {
        className: "sb-source-row is-local",
        title: parsed.full,
      },
      sourceFileIconSVG(),
      pathEl,
      lineEl
    );
  }

  function formatStepOutputText(o) {
    const body = prettyJson(decodeMaybeB64((o && o.data) || "(empty)"));
    if (!o || !o.sink) return body;
    return body.includes("\n") ? "[" + o.sink + "]\n" + body : "[" + o.sink + "] " + body;
  }

  function decodeMaybeB64(s) {
    if (!s) return "";
    try {
      const raw = atob(s);
      if (/^[\x20-\x7e\r\n\t]+$/.test(raw)) return raw;
    } catch (_) {}
    return s;
  }

  function findTokenizedPair(f) {
    if (!f) return null;
    if (f.id.endsWith(".tokenized")) {
      const rawId = f.id.slice(0, -".tokenized".length);
      const rawFx = fixtureMap.get(rawId);
      return rawFx ? { rawFixture: rawFx, tokenizedFixture: f, isViewingTokenized: true } : null;
    }
    const tokFx = fixtureMap.get(f.id + ".tokenized");
    return tokFx ? { rawFixture: f, tokenizedFixture: tokFx, isViewingTokenized: false } : null;
  }

  function findStepInFixture(fx, seq, preferredBuild) {
    if (!fx || !fx.runs) return null;
    const run =
      (preferredBuild && fx.runs.find((r) => r.build === preferredBuild || r.id === preferredBuild)) ||
      fx.runs.find((r) => r.build === "old") ||
      fx.runs[0];
    if (!run || !run.steps) return null;
    return run.steps.find((st) => st.seq === seq) || null;
  }

  function findOverlayLaneForFixture(fx) {
    if (!fx || !overlays.length) return null;
    const baseId = fx.id.replace(/\.tokenized$/, "");
    const preferredOv = overlayMap.get("shadow") || overlays[0];
    if (!preferredOv) return null;
    for (const lane of preferredOv.lanes || []) {
      if (lane.id === fx.id || lane.id === baseId) return { overlay: preferredOv, lane };
      for (const st of lane.stages || []) {
        if ((st.runs || []).some((rid) => rid === fx.id || rid === baseId)) {
          return { overlay: preferredOv, lane };
        }
      }
    }
    return preferredOv.lanes && preferredOv.lanes.length ? { overlay: preferredOv, lane: preferredOv.lanes[0] } : null;
  }

  function findCompensateItems(fx) {
    const match = findOverlayLaneForFixture(fx);
    if (!match || !match.lane || !match.lane.stages) return [];
    const compStage = match.lane.stages.find((st) => st.id === "compensate");
    return compStage && compStage.items ? compStage.items : [];
  }

  function formatMoney(amount, currency) {
    if (amount === undefined || amount === null) return "";
    const cur = String(currency || "").toUpperCase();
    if (!cur || cur === "USD") return "$" + amount;
    if (cur === "EUR") return "€" + amount;
    return amount + " " + cur;
  }

  function formatStepTitle(step) {
    if (!step) return "Step";
    const pos = String(step.position || "");
    const obj = parseJsonSafe(step.input);
    const omMatch = pos.match(/^([A-Z]+)\s+\/api\/v\d+\/openmeter\/(.+)$/i);
    if (omMatch) {
      return omMatch[1].toUpperCase() + " /openmeter/" + omMatch[2];
    }
    const fmMatch = pos.match(/^([A-Z]+)\s+\/v\d+\/ledgers\/[^/]+\/apply$/i);
    if (fmMatch) {
      let opName = obj && obj.op ? String(obj.op) : "apply";
      if (opName === "create_tx" && obj && Array.isArray(obj.orders) && obj.orders[0] && obj.orders[0].op === "revert_tx") {
        opName = "revert_tx";
      }
      return fmMatch[1].toUpperCase() + " /ledgers/apply · " + opName;
    }
    return pos || step.source || "Step";
  }

  function formatStepSubtitle(step, isFailure) {
    if (!step) return "";
    if (isFailure) {
      const reason = step.message || (step.detected && step.detected.length ? step.detected.join(", ") : step.outcome);
      return "Failed: " + reason;
    }
    const obj = parseJsonSafe(step.input);
    if (obj && typeof obj === "object") {
      if (obj.op === "transaction" && obj.transaction) {
        const t = obj.transaction;
        const kind = t.inflight ? "hold" : t.source ? "transfer" : "credit";
        const amt = formatMoney(t.amount, t.currency);
        const tail = t.description || (t.source && t.destination ? t.source + " → " + t.destination : t.destination || "");
        return [kind, amt, tail].filter(Boolean).join(" · ");
      }
      if (obj.op === "inflight_commit") {
        return "capture · " + (obj.description || obj.parent_reference || obj.reference || "");
      }
      if (obj.op === "inflight_void") {
        return "release hold · " + (obj.description || obj.parent_reference || obj.reference || "");
      }
      if (obj.bulk && Array.isArray(obj.bulk.transactions)) {
        const txs = obj.bulk.transactions;
        const firstDesc = txs[0] && txs[0].description ? " — " + txs[0].description : "";
        return txs.length + " bulk tx" + firstDesc;
      }
      if (Array.isArray(obj.transactions)) {
        return obj.transactions.length + " transactions";
      }
      if (obj.owner_key) {
        if (obj.op === "create_grant" && obj.amount !== undefined) {
          return obj.owner_key + " · +" + obj.amount + " credits" + (obj.recurrence_interval ? " (recurring)" : "");
        }
        if (obj.op === "ingest_usage" && obj.usage !== undefined) {
          return obj.owner_key + " · -" + obj.usage + " usage";
        }
        if (obj.op === "init_entitlement") {
          return obj.owner_key + (obj.period_interval ? " · " + String(obj.period_interval).toLowerCase() : "");
        }
        if (obj.op === "reset_usage") {
          return obj.owner_key + " · reset period";
        }
        if (obj.op === "verify_balance" || obj.op === "verify_history") {
          return obj.owner_key + " · verify " + (obj.op === "verify_history" ? "history" : "balance");
        }
        return String(obj.owner_key);
      }
      if (obj.op === "set_cluster_policy" && obj.policy) {
        return "max_command_bytes=" + (obj.policy.max_command_bytes !== undefined ? obj.policy.max_command_bytes : "?");
      }
      if (obj.op === "create_ledger" && obj.ledger) {
        return "ledger: " + obj.ledger;
      }
      if (Array.isArray(obj.orders) && obj.orders.length) {
        const o = obj.orders[0];
        if (o.op === "revert_tx") {
          return "revert tx #" + (o.tx_id !== undefined ? o.tx_id : "?") + (obj.orders.length > 1 ? " (+" + (obj.orders.length - 1) + ")" : "");
        }
        if (Array.isArray(o.postings) && o.postings[0]) {
          const p = o.postings[0];
          return (p.source || "?") + " → " + (p.destination || "?") + " (" + p.amount + " " + (p.asset || "") + ")";
        }
        if (o.reference) return String(o.reference);
      }
      if (obj.op) return String(obj.op);
    }
    return step.source || "";
  }

  function buildStepExplanation(f, primaryStep, oldStep, newStep, compareOn, isDivStep, stepNum, totalSteps, activeRun) {
    const titlePrefix = "Step " + stepNum + " of " + totalSteps + " · What happened";
    const refStep = primaryStep || oldStep || newStep;
    if (!refStep) {
      return { tone: "is-ok", title: titlePrefix, text: "No step selected." };
    }
    const opDesc = formatStepTitle(refStep) + (formatStepSubtitle(refStep, false) ? " (" + formatStepSubtitle(refStep, false) + ")" : "");
    const oldBad = Boolean(oldStep && (isBadOutcome(oldStep.outcome) || (oldStep.detected && oldStep.detected.length)));
    const newBad = Boolean(newStep && (isBadOutcome(newStep.outcome) || (newStep.detected && newStep.detected.length)));

    if (compareOn && isDivStep && oldStep && newStep) {
      const oldReason = oldStep.message || (oldStep.detected && oldStep.detected.length ? oldStep.detected.join(", ") : oldStep.outcome);
      const newInvVals = Object.values(newStep.invariants || {});
      const passCount = newInvVals.filter((v) => v === "pass").length;
      if (oldBad && !newBad) {
        return {
          tone: "is-warn",
          title: titlePrefix,
          text:
            opDesc +
            " diverged between builds. Unpatched failed (" +
            oldReason +
            "), whereas Patched completed cleanly" +
            (passCount ? " with " + passCount + "/" + newInvVals.length + " invariants passing." : "."),
        };
      }
      return {
        tone: "is-warn",
        title: titlePrefix,
        text: opDesc + " produced different outputs between Unpatched (" + oldStep.outcome + ") and Patched (" + newStep.outcome + ").",
      };
    }

    const isBad = isBadOutcome(refStep.outcome) || (refStep.detected && refStep.detected.length > 0);
    const invEntries = Object.entries(refStep.invariants || {});
    const failedInvs = invEntries.filter(([, v]) => v === "fail").map(([k]) => k);
    const passCount = invEntries.filter(([, v]) => v === "pass").length;

    if (isBad) {
      const reason = refStep.message || (refStep.detected && refStep.detected.length ? refStep.detected.join(", ") : refStep.outcome);
      const invNote = failedInvs.length ? " Violated invariant: " + failedInvs.join(", ") + "." : "";
      return {
        tone: "is-fail",
        title: titlePrefix,
        text: opDesc + " failed: " + reason + "." + invNote,
      };
    }

    const isFixedStep = Boolean(activeRun && activeRun.build === "new" && oldStep && oldBad);
    if (isFixedStep) {
      return {
        tone: "is-ok",
        title: titlePrefix,
        text:
          "Fixed in Patched: " +
          opDesc +
          " succeeded cleanly" +
          (invEntries.length ? " and " + passCount + "/" + invEntries.length + " invariants passed." : "."),
      };
    }

    return {
      tone: "is-ok",
      title: titlePrefix,
      text:
        opDesc +
        " completed normally" +
        (invEntries.length ? " with " + passCount + "/" + invEntries.length + " invariants passing." : "."),
    };
  }

  function buildCausalPlan(step) {
    const hlNodes = new Set();
    const hlEdges = new Set();
    const hitRoles = new Map();
    const bugNodes = new Set();
    const passNodes = new Set();
    const stageByEdge = new Map();
    const inputs = [];
    if (!step) return { hlNodes, hlEdges, hitRoles, bugNodes, passNodes, inputs, stageByEdge, maxStage: -1 };

    const targets = [];
    for (const h of step.hits || []) {
      hlNodes.add(h.node);
      const prev = hitRoles.get(h.node);
      if (h.role === "failure" || !prev) hitRoles.set(h.node, h.role);
      if (h.role === "input" && !inputs.includes(h.node)) inputs.push(h.node);
      if (h.role === "output" || h.role === "failure") targets.push(h.node);
      if (h.role === "failure") bugNodes.add(h.node);
    }
    for (const invName of step.detected || []) {
      for (const n of nodes) {
        if (n.kind === "invariant" && n.invariant === invName) {
          hlNodes.add(n.id);
          hitRoles.set(n.id, "failure");
          bugNodes.add(n.id);
          targets.push(n.id);
        }
      }
    }

    for (const src of inputs) {
      const parent = new Map();
      const q = [src];
      parent.set(src, null);
      let head = 0;
      while (head < q.length) {
        const cur = q[head++];
        for (const edgeRef of adjOut.get(cur) || []) {
          if (!parent.has(edgeRef.to)) {
            parent.set(edgeRef.to, { from: cur, edgeIdx: edgeRef.edgeIdx });
            q.push(edgeRef.to);
          }
        }
      }
      for (const dst of targets) {
        if (!parent.has(dst)) continue;
        let cur = dst;
        while (cur && parent.get(cur)) {
          const p = parent.get(cur);
          hlNodes.add(cur);
          hlNodes.add(p.from);
          hlEdges.add(p.edgeIdx);
          cur = p.from;
        }
      }
    }

    if (bugNodes.size > 0) {
      edges.forEach((e, idx) => {
        if (e.kind === "breaks" && hlNodes.has(e.from) && hlNodes.has(e.to)) {
          hlEdges.add(idx);
        }
      });
    }

    if (step.invariants) {
      edges.forEach((e, idx) => {
        if (e.kind !== "checks" || !hlNodes.has(e.from)) return;
        const dstNode = nodeMap.get(e.to);
        if (!dstNode || dstNode.kind !== "invariant") return;
        const stVal = step.invariants[dstNode.invariant];
        if (stVal === "pass" || stVal === "fail") {
          hlNodes.add(e.to);
          hlEdges.add(idx);
          if (stVal === "pass") passNodes.add(e.to);
          if (stVal === "fail") {
            bugNodes.add(e.to);
            hitRoles.set(e.to, "failure");
          }
        }
      });
    }

    const dist = new Map();
    const q = [];
    for (const src of inputs) {
      dist.set(src, 0);
      q.push(src);
    }
    let head = 0;
    while (head < q.length) {
      const cur = q[head++];
      const d = dist.get(cur);
      for (const edgeRef of adjOut.get(cur) || []) {
        if (!hlEdges.has(edgeRef.edgeIdx)) continue;
        if (!dist.has(edgeRef.to)) {
          dist.set(edgeRef.to, d + 1);
          q.push(edgeRef.to);
        }
      }
    }

    let maxNonCheckStage = -1;
    for (const eIdx of hlEdges) {
      const e = edges[eIdx];
      if (e.kind === "checks") continue;
      const stg = dist.has(e.from) ? dist.get(e.from) : 0;
      stageByEdge.set(eIdx, stg);
      if (stg > maxNonCheckStage) maxNonCheckStage = stg;
    }

    let maxStage = maxNonCheckStage;
    const checkStage = maxNonCheckStage >= 0 ? maxNonCheckStage + 1 : 0;
    for (const eIdx of hlEdges) {
      const e = edges[eIdx];
      if (e.kind !== "checks") continue;
      const stg = Math.max(dist.has(e.from) ? dist.get(e.from) : 0, checkStage);
      stageByEdge.set(eIdx, stg);
      if (stg > maxStage) maxStage = stg;
    }

    return { hlNodes, hlEdges, hitRoles, bugNodes, passNodes, inputs, stageByEdge, maxStage };
  }

  function animateViewTransform(ctx, targetTx, targetTy, targetScale, durationMs) {
    stopZoomAnim();
    if (!ctx || !ctx.view || !ctx.applyTransform) return;
    if (prefersReducedMotion() || durationMs <= 0) {
      ctx.view.tx = targetTx;
      ctx.view.ty = targetTy;
      ctx.view.scale = targetScale;
      ctx.view.initialized = true;
      ctx.applyTransform();
      return;
    }
    const startTx = ctx.view.tx;
    const startTy = ctx.view.ty;
    const startScale = ctx.view.scale;
    ctx.view.initialized = true;
    const t0 = performance.now();
    function tick(now) {
      const u = Math.min(1, (now - t0) / durationMs);
      const ease = 1 - Math.pow(1 - u, 3);
      ctx.view.tx = startTx + (targetTx - startTx) * ease;
      ctx.view.ty = startTy + (targetTy - startTy) * ease;
      ctx.view.scale = startScale + (targetScale - startScale) * ease;
      ctx.applyTransform();
      if (u < 1) replayRuntime.zoomRaf = requestAnimationFrame(tick);
      else replayRuntime.zoomRaf = null;
    }
    replayRuntime.zoomRaf = requestAnimationFrame(tick);
  }

  function focusNodesFitSmooth(ctx, nodeIds, immediate) {
    if (!ctx || !ctx.cardGeom || !ctx.wrap) return;
    let minX = Infinity;
    let minY = Infinity;
    let maxX = -Infinity;
    let maxY = -Infinity;
    let count = 0;
    for (const nid of nodeIds) {
      const g = ctx.cardGeom.get(nid);
      if (!g) continue;
      minX = Math.min(minX, g.x);
      minY = Math.min(minY, g.y);
      maxX = Math.max(maxX, g.x + g.w);
      maxY = Math.max(maxY, g.y + g.h);
      count++;
    }
    if (!count) {
      if (!ctx.view.initialized && ctx.fitToView) ctx.fitToView();
      return;
    }
    const vw = ctx.wrap.clientWidth || 960;
    const vh = ctx.wrap.clientHeight || 640;
    const padX = 28;
    const padY = 40;
    const boxW = Math.max(248, maxX - minX) + padX * 2;
    const boxH = Math.max(96, maxY - minY) + padY * 2;
    const rawScale = Math.min((vw - 32) / boxW, (vh - 32) / boxH);
    const targetScale = Math.max(0.62, Math.min(1.15, rawScale));
    const cx = (minX + maxX) / 2;
    const cy = (minY + maxY) / 2;
    const targetTx = vw / 2 - cx * targetScale;
    const targetTy = vh / 2 - cy * targetScale;
    animateViewTransform(ctx, targetTx, targetTy, targetScale, immediate ? 0 : 360);
  }

  function scheduleHopTimeout(token, fn, ms) {
    const id = setTimeout(() => {
      replayRuntime.hopTimers = replayRuntime.hopTimers.filter((x) => x !== id);
      if (replayRuntime.animToken !== token) return;
      fn();
    }, ms);
    replayRuntime.hopTimers.push(id);
    return id;
  }

  function runSequentialHopAnimation(ctx, config, onDone) {
    stopReplayAnim();
    const myToken = replayRuntime.animToken;
    if (!ctx || !ctx.svg || !ctx.edgePathMap || !ctx.cardGeom) {
      if (onDone) onDone(myToken);
      return;
    }

    const { isSplitCompare, oldPlan, newPlan, singlePlan, singleBad, animatePackets } = config;
    const fxLayer = svgEl("g", { class: "kmap-fx-layer" });
    ctx.svg.appendChild(fxLayer);
    const overlayEdgesLayer = svgEl("g", { class: "kmap-hop-edges-layer" });
    ctx.svg.insertBefore(overlayEdgesLayer, fxLayer);

    const speed = state.replaySpeed || 1;
    const hopMs = Math.max(200, Math.round(420 / speed));

    function cardEl(nid) {
      const g = ctx.cardGeom.get(nid);
      return g ? g.el : null;
    }

    function spawnRipple(nid, isBug) {
      if (prefersReducedMotion()) return;
      const g = ctx.cardGeom.get(nid);
      if (!g) return;
      fxLayer.appendChild(
        svgEl("circle", {
          class: isBug ? "fx-shockwave" : "fx-pass-ripple",
          cx: g.cx,
          cy: g.cy,
          r: isBug ? "24" : "22",
        })
      );
    }

    function addOverlayPath(edgeIdx, clsName, splitOffset) {
      const info = ctx.edgePathMap.get(edgeIdx);
      if (!info) return null;
      const pts = splitOffset ? info.pts.map(([x, y]) => [x, y + splitOffset]) : info.pts;
      const p = svgEl("path", { class: "edge-path " + clsName, d: buildOrthoPath(pts, 10) });
      overlayEdgesLayer.appendChild(p);
      return p;
    }

    function buildStages(plan, modeKind, useSplitOffsets) {
      const byStage = new Map();
      for (const eIdx of plan.hlEdges) {
        const stg = plan.stageByEdge.get(eIdx) || 0;
        if (!byStage.has(stg)) byStage.set(stg, []);
        const e = edges[eIdx];
        const info = ctx.edgePathMap.get(eIdx);
        if (!info || !info.pathEl) continue;

        let pathEl = info.pathEl;
        let isOverlay = false;
        if (useSplitOffsets) {
          const sharedBoth = oldPlan && newPlan && oldPlan.hlEdges.has(eIdx) && newPlan.hlEdges.has(eIdx);
          const dstNode = nodeMap.get(e.to);
          const needsSplit = sharedBoth && dstNode && (dstNode.kind === "sink" || dstNode.kind === "invariant");
          const offset = needsSplit ? (modeKind === "old" ? 3.5 : -3.5) : 0;
          pathEl = addOverlayPath(eIdx, modeKind === "old" ? "hop-edge-old" : "hop-edge-new", offset) || info.pathEl;
          pathEl.style.opacity = "0";
          isOverlay = true;
        }

        let edgeTone = "healthy";
        if (modeKind === "old") edgeTone = "old";
        else if (modeKind === "new") edgeTone = "new";
        else if (e.kind === "breaks" || plan.bugNodes.has(e.to) || (singleBad && (e.kind === "emits" || plan.bugNodes.has(e.from)))) edgeTone = "bug";
        else if (e.kind === "checks" && plan.passNodes.has(e.to)) edgeTone = "pass";

        byStage.get(stg).push({ edgeIdx: eIdx, e, pathEl, isOverlay, edgeTone });
      }
      const keys = Array.from(byStage.keys()).sort((a, b) => a - b);
      return keys.map((k) => byStage.get(k));
    }

    function applyFinalStatic() {
      if (isSplitCompare && oldPlan && newPlan) {
        for (const nid of oldPlan.hlNodes) {
          const elNode = cardEl(nid);
          if (!elNode) continue;
          if (oldPlan.bugNodes.has(nid)) elNode.classList.add("hop-bug");
          else if (!newPlan.hlNodes.has(nid)) elNode.classList.add("hop-visited", "hop-old");
        }
        for (const nid of newPlan.hlNodes) {
          const elNode = cardEl(nid);
          if (!elNode) continue;
          if (newPlan.passNodes.has(nid)) elNode.classList.add("hop-active", "hop-pass");
          else elNode.classList.add("hop-visited", "hop-new");
        }
        for (const eIdx of oldPlan.hlEdges) {
          const info = ctx.edgePathMap.get(eIdx);
          if (!info) continue;
          const e = edges[eIdx];
          const inNew = newPlan.hlEdges.has(eIdx);
          const dstNode = nodeMap.get(e.to);
          const isSplit = inNew && dstNode && (dstNode.kind === "sink" || dstNode.kind === "invariant");
          if (!inNew) {
            info.pathEl.classList.add("hop-edge-old");
          } else if (isSplit) {
            addOverlayPath(eIdx, "hop-edge-old", 3.5);
          }
        }
        for (const eIdx of newPlan.hlEdges) {
          const info = ctx.edgePathMap.get(eIdx);
          if (!info) continue;
          const e = edges[eIdx];
          const inOld = oldPlan.hlEdges.has(eIdx);
          const dstNode = nodeMap.get(e.to);
          const isSplit = inOld && dstNode && (dstNode.kind === "sink" || dstNode.kind === "invariant");
          if (isSplit) {
            addOverlayPath(eIdx, "hop-edge-new", -3.5);
          } else {
            info.pathEl.classList.add("hop-edge-new");
          }
        }
      } else if (singlePlan) {
        const outIds = new Set();
        for (const eIdx of singlePlan.hlEdges) outIds.add(edges[eIdx].from);
        for (const nid of singlePlan.hlNodes) {
          const elNode = cardEl(nid);
          if (!elNode) continue;
          if (singlePlan.bugNodes.has(nid)) {
            elNode.classList.add("hop-bug");
          } else if (singlePlan.passNodes.has(nid)) {
            elNode.classList.add("hop-active", "hop-pass");
          } else if (!outIds.has(nid)) {
            elNode.classList.add("hop-active");
          } else {
            elNode.classList.add("hop-visited");
          }
        }
        for (const eIdx of singlePlan.hlEdges) {
          const info = ctx.edgePathMap.get(eIdx);
          if (!info) continue;
          const e = edges[eIdx];
          info.pathEl.classList.add("hop-edge-visited");
          if (e.kind === "breaks" || singlePlan.bugNodes.has(e.to) || (singleBad && e.kind === "emits")) {
            info.pathEl.classList.add("hop-bug");
          } else if (e.kind === "checks" && singlePlan.passNodes.has(e.to)) {
            info.pathEl.classList.add("hop-pass");
          }
        }
      }
    }

    if (!animatePackets || prefersReducedMotion()) {
      applyFinalStatic();
      if (onDone) onDone(myToken);
      return;
    }

    function animatePlanHops(plan, modeKind, useSplitOverlays, onPlanDone) {
      const stages = buildStages(plan, modeKind, useSplitOverlays);
      let activeNodes = new Set();

      // Hop 0: light up entry node(s)
      const entryIds = plan.inputs && plan.inputs.length ? plan.inputs : Array.from(plan.hlNodes).slice(0, 1);
      for (const nid of entryIds) {
        const elNode = cardEl(nid);
        if (!elNode) continue;
        elNode.classList.add("hop-active");
        if (modeKind === "new") elNode.classList.add("hop-pass");
        activeNodes.add(nid);
      }

      if (!stages.length) {
        for (const nid of plan.hlNodes) {
          const elNode = cardEl(nid);
          if (!elNode) continue;
          if (plan.bugNodes.has(nid)) {
            elNode.classList.add("hop-bug");
            spawnRipple(nid, true);
          } else if (plan.passNodes.has(nid)) {
            elNode.classList.add("hop-active", "hop-pass");
            spawnRipple(nid, false);
          } else {
            elNode.classList.add("hop-active");
          }
        }
        scheduleHopTimeout(myToken, onPlanDone, hopMs);
        return;
      }

      function runStage(sIdx) {
        if (replayRuntime.animToken !== myToken) return;
        if (sIdx >= stages.length) {
          onPlanDone();
          return;
        }

        // Transition previous stage active nodes to visited
        for (const nid of activeNodes) {
          const elNode = cardEl(nid);
          if (!elNode) continue;
          if (!elNode.classList.contains("hop-bug")) {
            elNode.classList.remove("hop-active");
            elNode.classList.add("hop-visited");
            if (modeKind === "old") elNode.classList.add("hop-old");
            else if (modeKind === "new") elNode.classList.add("hop-new");
          }
        }
        activeNodes = new Set();

        const stageEdges = stages[sIdx];
        const stageItems = stageEdges.map((item) => {
          if (item.isOverlay) {
            item.pathEl.style.opacity = "";
          } else {
            item.pathEl.classList.add("hop-edge-active");
            if (item.edgeTone === "bug" || item.edgeTone === "old") item.pathEl.classList.add("hop-bug");
            else if (item.edgeTone === "pass" || item.edgeTone === "new") item.pathEl.classList.add("hop-pass");
          }

          let totalLen = 0;
          try {
            totalLen = item.pathEl.getTotalLength();
          } catch (_) {}
          const fxCls =
            item.edgeTone === "bug" || item.edgeTone === "old"
              ? "fx-bug"
              : item.edgeTone === "pass" || item.edgeTone === "new"
                ? "fx-pass"
                : "fx-healthy";
          const trail = svgEl("path", { class: "flow-trail " + fxCls, d: item.pathEl.getAttribute("d") || "" });
          if (totalLen > 0) {
            trail.style.strokeDasharray = totalLen + " " + totalLen;
            trail.style.strokeDashoffset = String(totalLen);
          }
          const halo = svgEl("circle", { class: "flow-packet-halo " + fxCls, r: "8", cx: "-999", cy: "-999" });
          const pkt = svgEl("circle", { class: "flow-packet " + fxCls, r: "4", cx: "-999", cy: "-999" });
          fxLayer.appendChild(trail);
          fxLayer.appendChild(halo);
          fxLayer.appendChild(pkt);
          return { ...item, totalLen, trail, halo, pkt };
        });

        const t0 = performance.now();
        function frame(now) {
          if (replayRuntime.animToken !== myToken) return;
          const u = Math.min(1, (now - t0) / hopMs);

          for (const it of stageItems) {
            if (it.totalLen > 0) {
              const pt = it.pathEl.getPointAtLength(u * it.totalLen);
              it.pkt.setAttribute("cx", String(pt.x));
              it.pkt.setAttribute("cy", String(pt.y));
              it.halo.setAttribute("cx", String(pt.x));
              it.halo.setAttribute("cy", String(pt.y));
              const seg = Math.min(it.totalLen * 0.32, 46);
              const curLen = u * it.totalLen;
              it.trail.style.strokeDasharray = seg + " " + it.totalLen;
              it.trail.style.strokeDashoffset = String(-(curLen - seg));
            }
          }

          if (u < 1) {
            replayRuntime.animRaf = requestAnimationFrame(frame);
          } else {
            replayRuntime.animRaf = null;
            for (const it of stageItems) {
              if (it.trail.parentNode) it.trail.parentNode.removeChild(it.trail);
              if (it.halo.parentNode) it.halo.parentNode.removeChild(it.halo);
              if (it.pkt.parentNode) it.pkt.parentNode.removeChild(it.pkt);

              if (!it.isOverlay) {
                it.pathEl.classList.remove("hop-edge-active");
                if (modeKind === "old") it.pathEl.classList.add("hop-edge-old");
                else if (modeKind === "new") it.pathEl.classList.add("hop-edge-new");
                else it.pathEl.classList.add("hop-edge-visited");
              }

              const dst = it.e.to;
              const dstEl = cardEl(dst);
              if (dstEl) {
                if (plan.bugNodes.has(dst)) {
                  dstEl.classList.remove("hop-active", "hop-visited");
                  dstEl.classList.add("hop-bug");
                  spawnRipple(dst, true);
                } else if (plan.passNodes.has(dst)) {
                  dstEl.classList.remove("hop-visited");
                  dstEl.classList.add("hop-active", "hop-pass");
                  spawnRipple(dst, false);
                } else {
                  dstEl.classList.remove("hop-visited");
                  dstEl.classList.add("hop-active");
                  if (modeKind === "new") dstEl.classList.add("hop-new");
                  activeNodes.add(dst);
                }
              }
            }
            runStage(sIdx + 1);
          }
        }

        replayRuntime.animRaf = requestAnimationFrame(frame);
      }

      scheduleHopTimeout(myToken, () => runStage(0), Math.round(hopMs * 0.35));
    }

    if (isSplitCompare && oldPlan && newPlan) {
      animatePlanHops(oldPlan, "old", true, () => {
        const pauseMs = Math.max(240, Math.round(500 / speed));
        scheduleHopTimeout(myToken, () => {
          animatePlanHops(newPlan, "new", true, () => {
            if (onDone) onDone(myToken);
          });
        }, pauseMs);
      });
    } else if (singlePlan) {
      animatePlanHops(singlePlan, "single", false, () => {
        if (onDone) onDone(myToken);
      });
    } else if (onDone) {
      onDone(myToken);
    }
  }

  function switchReplayMode(targetMode) {
    const f = fixtureMap.get(state.replayFixtureId);
    if (!f) return;
    const { oldRun, newRun, hasCompare } = getCompareRuns(f);
    if (targetMode === "compare") {
      if (!hasCompare) return;
      state.compareMode = true;
    } else if (targetMode === "new") {
      if (!newRun) return;
      state.compareMode = false;
      state.replayRunId = newRun.id;
    } else {
      const unpatched = oldRun || (f.runs || []).find((r) => r.mode === "recorded") || (f.runs || [])[0];
      if (!unpatched) return;
      state.compareMode = false;
      state.replayRunId = unpatched.id;
    }
    stopReplayPlayback();
    replayRuntime.lastAnimatedKey = null;
    replayRuntime.needsCameraFocus = true;
    replayRuntime.userOverrodeCamera = false;
    ensureReplaySelection();
    syncHash();
    render();
  }

  function stepReplayBy(delta) {
    const f = fixtureMap.get(state.replayFixtureId);
    const seqs = getAlignedSeqs(f);
    if (!seqs.length) return;
    const idx = seqs.indexOf(state.replaySeq);
    const cur = idx < 0 ? 0 : idx;
    const nextIdx = Math.max(0, Math.min(seqs.length - 1, cur + delta));
    if (nextIdx !== cur) {
      state.replaySeq = seqs[nextIdx];
      replayRuntime.needsCameraFocus = true;
      replayRuntime.userOverrodeCamera = false;
      syncHash();
      render();
    }
  }

  function jumpToFirstDivergence() {
    const f = fixtureMap.get(state.replayFixtureId);
    if (!f) return;
    const r = (f.runs || []).find((x) => x.id === state.replayRunId) || defaultRunForFixture(f);
    const targetSeq = firstDivergenceSeqForFixture(f, r);
    if (targetSeq !== null && targetSeq !== undefined) {
      state.replaySeq = targetSeq;
      replayRuntime.lastAnimatedKey = null;
      replayRuntime.needsCameraFocus = true;
      replayRuntime.userOverrodeCamera = false;
      syncHash();
      render();
    }
  }

  function scheduleAdvanceAfterStep(token, hitBug) {
    if (!state.replayPlaying || state.tab !== "replay" || replayRuntime.animToken !== token) return;
    const f = fixtureMap.get(state.replayFixtureId);
    const seqs = getAlignedSeqs(f);
    if (!seqs.length) {
      stopReplayPlayback();
      return;
    }
    const idx = seqs.indexOf(state.replaySeq);
    const speed = state.replaySpeed || 1;
    const dwellMs = prefersReducedMotion()
      ? Math.round(900 / speed)
      : Math.round((hitBug ? 680 : 340) / speed);

    replayRuntime.playTimer = setTimeout(() => {
      replayRuntime.playTimer = null;
      if (!state.replayPlaying || state.tab !== "replay") return;
      if (idx >= seqs.length - 1) {
        state.replayPlaying = false;
        render();
        return;
      }
      state.replaySeq = seqs[Math.max(0, idx) + 1];
      replayRuntime.needsCameraFocus = true;
      replayRuntime.userOverrodeCamera = false;
      syncHash();
      render();
    }, dwellMs);
  }

  function toggleReplayPlayback() {
    if (state.tab !== "replay") return;
    if (state.replayPlaying) {
      stopReplayPlayback();
      render();
      return;
    }
    const f = fixtureMap.get(state.replayFixtureId);
    const seqs = getAlignedSeqs(f);
    if (!seqs.length) return;
    const idx = seqs.indexOf(state.replaySeq);
    if (idx === seqs.length - 1) {
      state.replaySeq = seqs[0];
    }
    state.replayPlaying = true;
    replayRuntime.lastAnimatedKey = null;
    replayRuntime.needsCameraFocus = true;
    replayRuntime.userOverrodeCamera = false;
    syncHash();
    render();
  }

  function renderReplayTab() {
    ensureReplaySelection();
    if (!fixtures.length) return el("div", { className: "empty-state", text: "No fixtures loaded." });

    const f = fixtureMap.get(state.replayFixtureId);
    const { oldRun, newRun, hasCompare } = getCompareRuns(f);
    const unpatchedRun = oldRun || (f.runs || []).find((r) => r.mode === "recorded") || (f.runs || [])[0] || null;
    const compareOn = isCompareActive(f);
    const activeRun = (f.runs || []).find((x) => x.id === state.replayRunId) || unpatchedRun;
    const seqs = getAlignedSeqs(f);
    if (seqs.length && !seqs.includes(state.replaySeq)) {
      state.replaySeq = seqs[0];
    }
    const activeSeq = state.replaySeq;
    const curIdx = Math.max(0, seqs.indexOf(activeSeq));
    const firstDivSeq = firstDivergenceSeqForFixture(f, activeRun);

    const oldStepBySeq = new Map(((oldRun && oldRun.steps) || []).map((st) => [st.seq, st]));
    const newStepBySeq = new Map(((newRun && newRun.steps) || []).map((st) => [st.seq, st]));
    const activeStepBySeq = new Map(((activeRun && activeRun.steps) || []).map((st) => [st.seq, st]));

    const oldStep = oldStepBySeq.get(activeSeq) || null;
    const newStep = newStepBySeq.get(activeSeq) || null;
    const singleStep = activeStepBySeq.get(activeSeq) || (activeRun && activeRun.steps && activeRun.steps[0]) || null;
    const primaryStep = compareOn ? newStep || oldStep : singleStep;

    const isDivStep = Boolean(
      (firstDivSeq !== null && activeSeq === firstDivSeq) ||
        (oldStep && (isBadOutcome(oldStep.outcome) || (oldStep.detected && oldStep.detected.length))) ||
        (oldStep && newStep && (oldStep.outcome !== newStep.outcome || outputsSig(oldStep) !== outputsSig(newStep)))
    );

    // Build causal highlight & sequential hop plan
    const hlNodes = new Set();
    const hlEdges = new Set();
    const hitRoles = new Map();
    let oldPlan = null;
    let newPlan = null;
    let singlePlan = null;
    const isSplitCompare = Boolean(compareOn && isDivStep && (oldStep || newStep));

    if (isSplitCompare) {
      oldPlan = buildCausalPlan(oldStep);
      newPlan = buildCausalPlan(newStep);
      for (const n of oldPlan.hlNodes) hlNodes.add(n);
      for (const n of newPlan.hlNodes) hlNodes.add(n);
      for (const e of oldPlan.hlEdges) hlEdges.add(e);
      for (const e of newPlan.hlEdges) hlEdges.add(e);
      for (const [n, r] of newPlan.hitRoles.entries()) hitRoles.set(n, r);
      for (const [n, r] of oldPlan.hitRoles.entries()) {
        if (r === "failure" || !hitRoles.has(n)) hitRoles.set(n, r);
      }
    } else if (primaryStep) {
      singlePlan = buildCausalPlan(primaryStep);
      for (const n of singlePlan.hlNodes) hlNodes.add(n);
      for (const e of singlePlan.hlEdges) hlEdges.add(e);
      for (const [n, r] of singlePlan.hitRoles.entries()) hitRoles.set(n, r);
    }

    const animKey = f.id + "|" + (compareOn ? "compare" : activeRun ? activeRun.id : "") + "|" + activeSeq;
    const shouldAnimatePackets = replayRuntime.lastAnimatedKey !== animKey || state.replayPlaying;
    replayRuntime.lastAnimatedKey = animKey;

    const singleBad = Boolean(
      primaryStep &&
        (isBadOutcome(primaryStep.outcome) ||
          (primaryStep.detected && primaryStep.detected.length) ||
          (singlePlan && singlePlan.bugNodes.size > 0))
    );

    // 1. Left Storyboard Header (.sb-header)
    const isUnpatchedActive = !compareOn && activeRun && unpatchedRun && activeRun.id === unpatchedRun.id;
    const isPatchedActive = !compareOn && activeRun && newRun && activeRun.id === newRun.id;

    const sbHeader = el(
      "div",
      { className: "sb-header" },
      el(
        "select",
        {
          className: "sb-fixture-select",
          attrs: { "aria-label": "Replay fixture" },
          on: {
            change: (e) => {
              stopReplayPlayback();
              state.replayFixtureId = e.target.value;
              state.replayRunId = null;
              state.replaySeq = null;
              state.compareMode = false;
              state.compExpanded = false;
              state.sourceLinksExpanded = false;
              replayRuntime.lastAnimatedKey = null;
              replayRuntime.needsCameraFocus = true;
              replayRuntime.userOverrodeCamera = false;
              replayMapViewState.initialized = false;
              ensureReplaySelection();
              syncHash();
              render();
            },
          },
        },
        fixtures.map((fx) =>
          el("option", {
            value: fx.id,
            text: fx.label + (fx.recorded ? " (" + fx.recorded.kind + ")" : ""),
            selected: fx.id === f.id,
          })
        )
      ),
      el(
        "div",
        { className: "sb-mode-switch" },
        el("button", {
          className: "sb-mode-btn mode-old" + (isUnpatchedActive ? " active" : ""),
          text: "Unpatched",
          title: "View unpatched run (1)",
          on: { click: () => switchReplayMode("old") },
        }),
        el("button", {
          className: "sb-mode-btn mode-new" + (isPatchedActive ? " active" : ""),
          text: "Patched",
          title: newRun ? "View patched run (2)" : "No patched run available",
          disabled: !newRun,
          on: { click: () => switchReplayMode("new") },
        }),
        el("button", {
          className: "sb-mode-btn mode-compare" + (compareOn ? " active" : ""),
          text: "Compare",
          title: hasCompare ? "Compare old vs new (3 / C)" : "Compare requires both old and new runs",
          disabled: !hasCompare,
          on: { click: () => switchReplayMode("compare") },
        })
      )
    );

    // 2. Playback Controls (.sb-controls)
    const pct = seqs.length ? Math.round(((curIdx + 1) / seqs.length) * 100) : 0;
    const progressFillCls = ["sb-progress-fill"];
    if (singleBad && !compareOn) progressFillCls.push("is-fail");
    else if (!singleBad && activeRun && activeRun.build === "new" && isDivStep) progressFillCls.push("is-pass");

    const sbControls = el(
      "div",
      { className: "sb-controls" },
      el(
        "div",
        { className: "sb-controls-row" },
        el("button", {
          className: "sb-btn",
          text: "◀",
          title: "Previous step (←)",
          disabled: curIdx <= 0,
          on: {
            click: () => {
              stopReplayPlayback();
              stepReplayBy(-1);
            },
          },
        }),
        el("button", {
          className: "sb-btn sb-play" + (state.replayPlaying ? " is-playing" : ""),
          text: state.replayPlaying ? "⏸ Pause" : "▶ Play",
          title: "Play / Pause (Space)",
          disabled: !seqs.length,
          on: { click: toggleReplayPlayback },
        }),
        el("button", {
          className: "sb-btn",
          text: "▶",
          title: "Next step (→)",
          disabled: curIdx >= seqs.length - 1,
          on: {
            click: () => {
              stopReplayPlayback();
              stepReplayBy(1);
            },
          },
        }),
        el("button", {
          className: "sb-btn sb-bug-btn",
          text: "⚡ Bug",
          title: "Jump to first failing / diverging step (F)",
          disabled: firstDivSeq === null || firstDivSeq === undefined,
          on: {
            click: () => {
              stopReplayPlayback();
              jumpToFirstDivergence();
            },
          },
        }),
        el(
          "div",
          { className: "sb-speed-group", title: "Playback speed" },
          [0.5, 1, 2].map((spd) =>
            el("button", {
              className: "sb-speed-btn" + (state.replaySpeed === spd ? " active" : ""),
              text: spd + "×",
              on: {
                click: () => {
                  state.replaySpeed = spd;
                  render();
                },
              },
            })
          )
        ),
        el("span", { className: "sb-step-counter", text: seqs.length ? "Step " + (curIdx + 1) + " / " + seqs.length : "0 / 0" })
      ),
      el("div", { className: "sb-progress" }, el("div", { className: progressFillCls.join(" "), style: "width:" + pct + "%" }))
    );

    // 3. Plain-English Numbered Step List (.sb-steps)
    const sbSteps = el("div", { className: "sb-steps" });
    let activeStepBtn = null;
    seqs.forEach((seq, idx) => {
      const os = oldStepBySeq.get(seq) || null;
      const ns = newStepBySeq.get(seq) || null;
      const st = compareOn ? ns || os : activeStepBySeq.get(seq) || null;
      const isAct = seq === activeSeq;
      const isPast = idx < curIdx;

      let outCls = "outcome-ok";
      let badgeText = "OK";
      let isFailSub = false;

      if (compareOn) {
        const osBad = Boolean(os && (isBadOutcome(os.outcome) || (os.detected && os.detected.length)));
        const nsBad = Boolean(ns && (isBadOutcome(ns.outcome) || (ns.detected && ns.detected.length)));
        const diffOut = Boolean(os && ns && (os.outcome !== ns.outcome || outputsSig(os) !== outputsSig(ns)));
        const divHere = (firstDivSeq !== null && seq === firstDivSeq) || osBad || diffOut || !os || !ns;
        if (nsBad) {
          outCls = "outcome-fail";
          badgeText = "Failed";
          isFailSub = true;
        } else if (divHere && osBad) {
          outCls = "outcome-warn";
          badgeText = "Fixed";
        } else if (divHere) {
          outCls = "outcome-warn";
          badgeText = "Diff";
        }
      } else if (!st) {
        outCls = "outcome-skip";
        badgeText = "Skipped";
      } else if (isBadOutcome(st.outcome) || (st.detected && st.detected.length)) {
        outCls = "outcome-fail";
        badgeText = "Failed";
        isFailSub = true;
      } else if (activeRun && activeRun.build === "new" && firstDivSeq !== null && seq === firstDivSeq) {
        outCls = "outcome-ok";
        badgeText = "Fixed";
      }

      const stepCls = ["sb-step", outCls];
      if (isAct) stepCls.push("is-active");
      else if (isPast) stepCls.push("is-past");

      const title = formatStepTitle(st);
      const sub = formatStepSubtitle(st, isFailSub);

      const btn = el(
        "button",
        {
          className: stepCls.join(" "),
          on: {
            click: () => {
              stopReplayPlayback();
              state.replaySeq = seq;
              replayRuntime.lastAnimatedKey = null;
              replayRuntime.needsCameraFocus = true;
              replayRuntime.userOverrodeCamera = false;
              syncHash();
              render();
            },
          },
        },
        el("span", { className: "sb-step-num", text: String(idx + 1) }),
        el(
          "div",
          { className: "sb-step-body" },
          el("div", { className: "sb-step-title", text: title, title: title }),
          sub ? el("div", { className: "sb-step-sub", text: sub, title: sub }) : null
        ),
        el("span", { className: "sb-step-status", text: badgeText })
      );
      if (isAct) activeStepBtn = btn;
      sbSteps.appendChild(btn);
    });

    if (activeStepBtn) {
      requestAnimationFrame(() => {
        try {
          activeStepBtn.scrollIntoView({ block: "nearest", behavior: "auto" });
        } catch (_) {}
      });
    }

    // 4. Docked Step Detail & Diff (.sb-detail)
    const sbDetail = el("div", { className: "sb-detail" });

    // 4a. What happened box
    const explain = buildStepExplanation(f, primaryStep, oldStep, newStep, compareOn, isDivStep, curIdx + 1, seqs.length, activeRun);
    sbDetail.appendChild(
      el(
        "div",
        { className: "sb-explain " + explain.tone },
        el("div", { className: "sb-explain-title", text: explain.title }),
        el("div", { className: "sb-explain-text", text: explain.text })
      )
    );

    // 4b. Invariants pills
    const invNames =
      (newRun && newRun.invariants && newRun.invariants.length && newRun.invariants) ||
      (oldRun && oldRun.invariants && oldRun.invariants.length && oldRun.invariants) ||
      (activeRun && activeRun.invariants) ||
      [];
    if (invNames.length && primaryStep) {
      const invPills = invNames.map((invName) => {
        let stVal = (primaryStep.invariants && primaryStep.invariants[invName]) || "unknown";
        let labelTxt;
        if (compareOn && oldStep && newStep && oldStep.invariants && newStep.invariants) {
          const ov = oldStep.invariants[invName] || "unknown";
          const nv = newStep.invariants[invName] || "unknown";
          stVal = nv;
          if (ov !== nv) {
            labelTxt = invName + ": " + ov + " → " + nv;
          } else {
            const sym = nv === "pass" ? "✓" : nv === "fail" ? "✗" : nv === "skipped" ? "–" : nv === "not_reached" ? "·" : "?";
            labelTxt = sym + " " + invName;
          }
        } else {
          const sym = stVal === "pass" ? "✓" : stVal === "fail" ? "✗" : stVal === "skipped" ? "–" : stVal === "not_reached" ? "·" : "?";
          labelTxt = sym + " " + invName;
        }
        return el("span", { className: "sb-inv-pill st-" + stVal, title: invName + ": " + stVal, text: labelTxt });
      });
      sbDetail.appendChild(el("div", { className: "sb-invariants" }, invPills));
    }

    // 4c. Old vs New Output Diff & Compensation Plan (when on failing/diverging step or in Compare mode)
    const divObj = newRun && newRun.divergence ? newRun.divergence : null;
    const compItems = findCompensateItems(f);
    const hasOldNewOutputs = Boolean(
      hasCompare &&
        ((divObj && (activeSeq === divObj.seq || activeSeq === firstDivSeq)) ||
          (isDivStep && oldStep && newStep))
    );
    const showDiffBox = Boolean(
      (compareOn && isDivStep && (hasOldNewOutputs || compItems.length)) ||
        ((isDivStep || singleBad) && (hasOldNewOutputs || compItems.length))
    );

    if (showDiffBox) {
      const diffChildren = [];
      diffChildren.push(
        el(
          "div",
          { className: "sb-diff-head" },
          el("span", { className: "sb-diff-title", text: hasOldNewOutputs ? "Old vs New Output Diff" : "Compensation Plan" }),
          oldRun && newRun
            ? el("span", {
                className: "pill pass mono",
                text: (oldRun.verdict || oldRun.status) + " → " + (newRun.verdict || newRun.status),
              })
            : null
        )
      );

      if (hasOldNewOutputs) {
        const oldDataText =
          divObj && divObj.old
            ? "(" + divObj.old.sink + ") " + decodeMaybeB64(divObj.old.data)
            : oldStep && oldStep.outputs && oldStep.outputs.length
              ? oldStep.outputs.map((o) => "(" + o.sink + ") " + decodeMaybeB64(o.data || "(empty)")).join("\n")
              : oldStep && oldStep.message
                ? oldStep.message
                : "(no output)";
        const newDataText =
          divObj && divObj.new
            ? "(" + divObj.new.sink + ") " + decodeMaybeB64(divObj.new.data)
            : newStep && newStep.outputs && newStep.outputs.length
              ? newStep.outputs.map((o) => "(" + o.sink + ") " + decodeMaybeB64(o.data || "(empty)")).join("\n")
              : "(no output)";

        diffChildren.push(
          el(
            "div",
            { className: "sb-diff-row old" },
            el("div", { className: "sb-diff-label", text: "Unpatched" }),
            el("div", { className: "sb-diff-code", text: oldDataText })
          ),
          el(
            "div",
            { className: "sb-diff-row new" },
            el("div", { className: "sb-diff-label", text: "Patched" }),
            el("div", { className: "sb-diff-code", text: newDataText })
          )
        );
      }

      if (compItems.length) {
        let visibleItems = compItems;
        if (!state.compExpanded && compItems.length > 2) {
          const firstUndo = compItems.find((it) => String(it.label).toLowerCase() === "undo");
          const firstApply = compItems.find((it) => String(it.label).toLowerCase() !== "undo");
          visibleItems = [firstUndo, firstApply].filter(Boolean);
          if (!visibleItems.length) visibleItems = compItems.slice(0, 2);
        }
        diffChildren.push(
          el(
            "div",
            { className: "sb-comp-list" },
            el(
              "div",
              { style: "display:flex;align-items:center;justify-content:space-between;gap:8px;" },
              el("span", { className: "sb-raw-label", text: "Compensation Plan (" + compItems.length + ")" }),
              compItems.length > 2
                ? el("button", {
                    className: "sb-comp-toggle",
                    text: state.compExpanded ? "Show less" : "Show all (" + compItems.length + ")",
                    on: {
                      click: () => {
                        state.compExpanded = !state.compExpanded;
                        render();
                      },
                    },
                  })
                : null
            ),
            visibleItems.map((it) =>
              el(
                "div",
                { className: "sb-comp-row" },
                el("span", {
                  className: "sb-comp-badge " + (String(it.label).toLowerCase() === "undo" ? "comp-undo" : "comp-apply"),
                  text: it.label || "step",
                }),
                el("div", {
                  className: "sb-comp-text",
                  text: (it.sink ? it.sink + ": " : "") + (it.text || ""),
                })
              )
            )
          )
        );
      }

      sbDetail.appendChild(el("div", { className: "sb-diff-box" }, diffChildren));
    }

    // 4d. Collapsible Inspect Raw JSON & PII X-Ray (collapsed by default!)
    const tokPair = findTokenizedPair(f);
    const rawBody = el("div", { className: "sb-raw-body" });
    if (primaryStep) {
      // 1. PII Scrub X-Ray (first when a .tokenized fixture exists)
      if (tokPair) {
        const rawStep = findStepInFixture(tokPair.rawFixture, activeSeq, activeRun ? activeRun.build : null);
        const tokStep = findStepInFixture(tokPair.tokenizedFixture, activeSeq, activeRun ? activeRun.build : null);
        const xraySection = el("div", { className: "sb-raw-section sb-xray-section" });
        const renderXraySection = () => {
          while (xraySection.firstChild) xraySection.removeChild(xraySection.firstChild);
          const view = state.piiXrayView || "stacked";
          xraySection.appendChild(
            el(
              "div",
              { className: "sb-raw-head" },
              el("span", { className: "sb-raw-label", text: "PII Scrub X-Ray" }),
              el(
                "div",
                { className: "sb-xray-tabs", attrs: { role: "tablist", "aria-label": "PII X-Ray view" } },
                [
                  { id: "stacked", label: "Stacked" },
                  { id: "raw", label: "Raw" },
                  { id: "tokenized", label: "Tokenized" },
                ].map((opt) =>
                  el("button", {
                    className: "sb-xray-tab" + (view === opt.id ? " active" : ""),
                    text: opt.label,
                    attrs: { type: "button", "aria-selected": view === opt.id ? "true" : "false" },
                    on: {
                      click: () => {
                        state.piiXrayView = opt.id;
                        renderXraySection();
                      },
                    },
                  })
                )
              )
            )
          );

          const cards = el("div", { className: "pii-xray-cards" + (view !== "stacked" ? " is-single" : "") });
          if (view !== "tokenized") {
            cards.appendChild(
              el(
                "div",
                { className: "pii-pane pii-raw" },
                el("span", { className: "pii-badge", text: "Raw PII" }),
                el("div", {
                  className: "code-block sb-raw-code",
                  text: rawStep && rawStep.input ? prettyJson(rawStep.input) : "(unavailable)",
                })
              )
            );
          }
          if (view !== "raw") {
            cards.appendChild(
              el(
                "div",
                { className: "pii-pane pii-tokenized" },
                el("span", { className: "pii-badge", text: "HMAC Tokenized" }),
                el("div", {
                  className: "code-block sb-raw-code",
                  text: tokStep && tokStep.input ? prettyJson(tokStep.input) : "(unavailable)",
                })
              )
            );
          }
          xraySection.appendChild(cards);
        };
        renderXraySection();
        rawBody.appendChild(xraySection);
      }

      // 2. Input & Outputs (pretty-printed JSON when parseable, compact 11px monospace)
      if (primaryStep.input) {
        rawBody.appendChild(
          el(
            "div",
            { className: "sb-raw-section" },
            el("div", {
              className: "sb-raw-label",
              text: "Input JSON (seq " + activeSeq + ")" + (primaryStep.omitted ? " (omitted)" : primaryStep.input_truncated ? " (truncated)" : ""),
            }),
            el("div", { className: "code-block sb-raw-code", text: prettyJson(primaryStep.input) })
          )
        );
      }

      if (primaryStep.reads && Object.keys(primaryStep.reads).length) {
        rawBody.appendChild(
          el(
            "div",
            { className: "sb-raw-section" },
            el("div", { className: "sb-raw-label", text: "Reads" }),
            el(
              "div",
              { className: "inv-pills" },
              Object.entries(primaryStep.reads).map(([k, v]) => el("span", { className: "pill mono", text: k + ": " + v }))
            )
          )
        );
      }

      if (compareOn && isDivStep && oldStep && newStep) {
        const oldOuts = (oldStep.outputs || []).map(formatStepOutputText).join("\n\n");
        const newOuts = (newStep.outputs || []).map(formatStepOutputText).join("\n\n");
        rawBody.appendChild(
          el(
            "div",
            { className: "sb-raw-section" },
            el("div", { className: "sb-raw-label", text: "Outputs (Unpatched vs Patched)" }),
            el(
              "div",
              { className: "pii-xray-cards" },
              el(
                "div",
                { className: "pii-pane pii-raw" },
                el("span", { className: "pii-badge", text: "Unpatched" }),
                el("div", { className: "code-block sb-raw-code", text: oldOuts || "(none)" })
              ),
              el(
                "div",
                { className: "pii-pane pii-tokenized" },
                el("span", { className: "pii-badge", text: "Patched" }),
                el("div", { className: "code-block sb-raw-code", text: newOuts || "(none)" })
              )
            )
          )
        );
      } else if (primaryStep.outputs && primaryStep.outputs.length) {
        const outsSection = el(
          "div",
          { className: "sb-raw-section" },
          el("div", { className: "sb-raw-label", text: "Outputs (" + primaryStep.outputs.length + ")" })
        );
        for (const o of primaryStep.outputs) {
          outsSection.appendChild(
            el(
              "div",
              { className: "sb-raw-output-item" },
              el("span", { className: "pill mono sb-raw-sink-pill", text: o.sink + (o.truncated ? " (truncated)" : "") }),
              el("div", { className: "code-block sb-raw-code", text: prettyJson(decodeMaybeB64(o.data || "(empty)")) })
            )
          );
        }
        rawBody.appendChild(outsSection);
      }

      // 3. Source Links (at bottom — deduplicated, prioritized external GitHub links first, compact single-line chips)
      const linkByKey = new Map();
      for (const nid of hlNodes) {
        const nd = nodeMap.get(nid);
        if (!nd || !nd.links) continue;
        for (const lk of nd.links) {
          const text = String(lk.text || "").trim();
          const url = String(lk.url || "").trim();
          if (!text && !url) continue;
          const dedupKey = text || url;
          const prev = linkByKey.get(dedupKey);
          if (!prev || (!/^https?:\/\//i.test(prev.url) && /^https?:\/\//i.test(url))) {
            linkByKey.set(dedupKey, { url, text });
          }
        }
      }
      const sourceLinks = Array.from(linkByKey.values()).sort((a, b) => {
        const aExt = /^https?:\/\//i.test(a.url) ? 0 : 1;
        const bExt = /^https?:\/\//i.test(b.url) ? 0 : 1;
        return aExt - bExt;
      });
      if (sourceLinks.length) {
        const srcSection = el("div", { className: "sb-raw-section sb-source-section" });
        const renderSourceLinksSection = () => {
          while (srcSection.firstChild) srcSection.removeChild(srcSection.firstChild);
          const visibleLinks =
            !state.sourceLinksExpanded && sourceLinks.length > 5
              ? sourceLinks.slice(0, 5)
              : sourceLinks;
          srcSection.appendChild(
            el(
              "div",
              { className: "sb-raw-head" },
              el("span", { className: "sb-raw-label", text: "Source Links" }),
              el("span", { className: "sb-raw-count", text: String(sourceLinks.length) })
            )
          );
          srcSection.appendChild(
            el("div", { className: "sb-source-list" }, visibleLinks.map(buildSourceLinkRow))
          );
          if (sourceLinks.length > 5) {
            srcSection.appendChild(
              el("button", {
                className: "sb-comp-toggle sb-source-toggle",
                attrs: { type: "button" },
                text: state.sourceLinksExpanded ? "Show fewer" : "Show all (" + sourceLinks.length + ")",
                on: {
                  click: () => {
                    state.sourceLinksExpanded = !state.sourceLinksExpanded;
                    renderSourceLinksSection();
                  },
                },
              })
            );
          }
        };
        renderSourceLinksSection();
        rawBody.appendChild(srcSection);
      }
    } else {
      rawBody.appendChild(el("div", { className: "sub-text", text: "No step data available." }));
    }

    const rawDetails = el(
      "details",
      {
        className: "sb-raw-details",
        on: {
          toggle: (e) => {
            const wasOpen = state.rawInspectorOpen;
            state.rawInspectorOpen = Boolean(e.target.open);
            if (state.rawInspectorOpen && !wasOpen) {
              requestAnimationFrame(() => {
                try {
                  e.target.scrollIntoView({ behavior: "smooth", block: "nearest" });
                } catch (_) {}
              });
            }
          },
        },
      },
      el("summary", { text: "Inspect Raw JSON" + (tokPair ? " & PII X-Ray" : "") }),
      rawBody
    );
    if (state.rawInspectorOpen) rawDetails.open = true;
    sbDetail.appendChild(rawDetails);

    const sidebarEl = el("aside", { className: "replay-sidebar" }, sbHeader, sbControls, sbSteps, sbDetail);

    // 5. Right Focused Canvas (.replay-canvas) — ZERO floating HUDs, ZERO diff cards, ZERO edge callouts!
    const wasViewInitialized = replayMapViewState.initialized;
    const canvas = buildMapCanvas({
      highlightNodes: hlNodes,
      highlightEdges: hlEdges,
      hitRoles: hitRoles,
      replayMode: true,
      skipInitialFit: true,
      suppressEdgeLabels: true,
      selectedNodeId: state.selectedNodeId,
      viewState: replayMapViewState,
      onUserInteract: () => {
        replayRuntime.userOverrodeCamera = true;
        replayRuntime.needsCameraFocus = false;
      },
      onSelectNode: (nid) => {
        state.selectedNodeId = state.selectedNodeId === nid ? null : nid;
        render();
      },
      onBackgroundClick: () => {
        if (state.selectedNodeId) {
          state.selectedNodeId = null;
          render();
        }
      },
      onAfterDraw: (ctx) => {
        replayRuntime.activeCanvasCtx = ctx;
        if (!wasViewInitialized || (replayRuntime.needsCameraFocus && !replayRuntime.userOverrodeCamera)) {
          replayRuntime.needsCameraFocus = false;
          focusNodesFitSmooth(ctx, hlNodes, !wasViewInitialized);
        }
        runSequentialHopAnimation(
          ctx,
          {
            isSplitCompare,
            oldPlan,
            newPlan,
            singlePlan,
            singleBad,
            animatePackets: shouldAnimatePackets,
          },
          (doneToken) => {
            if (state.replayPlaying) {
              scheduleAdvanceAfterStep(doneToken, singleBad || (isSplitCompare && Boolean(oldPlan && oldPlan.bugNodes.size)));
            }
          }
        );
      },
    });

    const replayCanvasEl = el("div", { className: "replay-canvas" }, canvas);
    if (state.selectedNodeId) {
      const insp = renderNodeInspector(state.selectedNodeId);
      if (insp) replayCanvasEl.appendChild(insp);
    }

    return el("div", { className: "replay-layout" }, sidebarEl, replayCanvasEl);
  }

  function renderInvariantsTab() {
    if (!fixtures.length) return el("div", { className: "empty-state", text: "No fixtures loaded." });
    const f = fixtureMap.get(state.invFixtureId) || fixtures[0];
    state.invFixtureId = f.id;

    const inner = el("div", { className: "page-inner" });
    const sel = el(
      "select",
      {
        className: "select-field",
        style: "max-width:440px;",
        on: {
          change: (e) => {
            state.invFixtureId = e.target.value;
            syncHash();
            render();
          },
        },
      },
      fixtures.map((fx) => el("option", { value: fx.id, text: fx.label, selected: fx.id === f.id }))
    );

    inner.appendChild(
      el(
        "div",
        { className: "panel" },
        el(
          "div",
          { style: "display:flex;align-items:center;justify-content:space-between;gap:12px;flex-wrap:wrap;margin-bottom:8px;" },
          sel,
          el(
            "div",
            { className: "legend-row" },
            el("span", { className: "pill pass", text: "✓ pass" }),
            el("span", { className: "pill fail", text: "✗ fail" }),
            el("span", { className: "pill warn", text: "– skipped" }),
            el("span", { className: "pill", text: "? unknown" }),
            el("span", { className: "pill skip", text: "· not_reached" })
          )
        ),
        el("div", { className: "sub-text", text: "Recorded runs cannot tell which invariants a build declares; replay with --bin to inspect declared checks." })
      )
    );

    for (const run of f.runs || []) {
      const panel = el("div", { className: "panel" });
      panel.appendChild(
        el(
          "div",
          { style: "display:flex;align-items:center;gap:10px;margin-bottom:10px;flex-wrap:wrap;" },
          el("h3", { style: "margin:0;", text: run.mode === "recorded" ? "Recorded run" : "Replay (" + (run.build || run.id) + (run.bin ? " · " + run.bin : "") + ")" }),
          el("span", { className: "pill " + statusPillClass(run.status) + " mono", text: run.verdict || run.status })
        )
      );
      if (run.error) {
        panel.appendChild(el("div", { className: "pill fail", text: run.error }));
      } else if (!run.invariants || !run.invariants.length) {
        panel.appendChild(el("div", { className: "sub-text", text: "No invariants declared or matched for this run." }));
      } else {
        const tbl = el("table", { className: "matrix" });
        const theadRow = el("tr", null, el("th", { className: "row-name", text: "Invariant" }));
        for (const st of run.steps || []) theadRow.appendChild(el("th", { text: "seq " + st.seq }));
        tbl.appendChild(el("thead", null, theadRow));

        const tbody = el("tbody");
        for (const invName of run.invariants) {
          const tr = el("tr", null, el("td", { className: "row-name", text: invName }));
          for (const st of run.steps || []) {
            const val = (st.invariants && st.invariants[invName]) || "unknown";
            const sym = val === "pass" ? "✓" : val === "fail" ? "✗" : val === "skipped" ? "–" : val === "not_reached" ? "·" : "?";
            tr.appendChild(
              el(
                "td",
                { className: "st-" + val, title: invName + " @ seq " + st.seq + ": " + val },
                el("button", { className: "cell-btn", text: sym, on: { click: () => jumpToReplay(f.id, run.id, st.seq) } })
              )
            );
          }
          tbody.appendChild(tr);
        }
        tbl.appendChild(tbody);
        panel.appendChild(el("div", { className: "matrix-wrap" }, tbl));
      }
      inner.appendChild(panel);
    }

    if (f.diff) {
      const df = f.diff;
      const dp = el("div", { className: "panel" });
      dp.appendChild(
        el(
          "div",
          { style: "display:flex;align-items:center;gap:10px;margin-bottom:10px;flex-wrap:wrap;" },
          el("h3", { style: "margin:0;", text: "Diff verification (" + df.source + ")" }),
          el("span", { className: "pill " + statusPillClass(df.verdict) + " mono", text: df.verdict }),
          el("span", { className: "pill mono", text: "old: " + df.old }),
          el("span", { className: "pill mono", text: "new: " + df.new })
        )
      );
      dp.appendChild(
        el(
          "div",
          { className: "inv-pills", style: "margin-bottom:12px;" },
          el("span", { className: "pill", text: "Variants: " + df.variants.passing + "/" + df.variants.reproducing + " reproducing (" + df.variants.candidates + " candidates, min " + df.variants.required + ")" }),
          el("span", { className: "pill", text: "Regressions: " + df.regressions.passing + "/" + df.regressions.counted + " counted (" + df.regressions.neighbours + " neighbours)" })
        )
      );

      if (df.by_kind && Object.keys(df.by_kind).length) {
        dp.appendChild(el("div", { className: "field-label", text: "By variant kind" }));
        const kt = el("table", { className: "data-table" });
        kt.appendChild(el("thead", null, el("tr", null, el("th", { text: "Kind" }), el("th", { text: "Passed" }), el("th", { text: "Reproducing" }))));
        const kb = el("tbody");
        for (const [k, v] of Object.entries(df.by_kind)) kb.appendChild(el("tr", null, el("td", { text: k }), el("td", { text: v.passed }), el("td", { text: v.total })));
        kt.appendChild(kb);
        dp.appendChild(kt);
      }

      if (df.by_invariant && Object.keys(df.by_invariant).length) {
        dp.appendChild(el("div", { className: "field-label", style: "margin-top:12px;", text: "By invariant (old vs new)" }));
        const it = el("table", { className: "data-table" });
        it.appendChild(el("thead", null, el("tr", null, el("th", { text: "Invariant" }), el("th", { text: "Old violations" }), el("th", { text: "New violations" }))));
        const ib = el("tbody");
        for (const [k, v] of Object.entries(df.by_invariant)) ib.appendChild(el("tr", null, el("td", { className: "mono", text: k }), el("td", { text: v.old }), el("td", { text: v.new })));
        it.appendChild(ib);
        dp.appendChild(it);
      }

      if (df.failures && df.failures.length) {
        dp.appendChild(el("div", { className: "field-label", style: "margin-top:12px;", text: "Failing variants and regressions" }));
        const ft = el("table", { className: "data-table" });
        ft.appendChild(el("thead", null, el("tr", null, el("th", { text: "Scope" }), el("th", { text: "ID" }), el("th", { text: "Mutation" }), el("th", { text: "Old" }), el("th", { text: "New" }), el("th", { text: "Detail" }))));
        const fb = el("tbody");
        for (const ch of df.failures) {
          fb.appendChild(el("tr", null, el("td", { text: ch.scope }), el("td", { text: ch.id }), el("td", { text: ch.mutation }), el("td", { className: "mono", text: ch.old }), el("td", { className: "mono", text: ch.new }), el("td", { text: ch.detail || "" })));
        }
        ft.appendChild(fb);
        dp.appendChild(ft);
      }

      inner.appendChild(dp);
    }

    return el("div", { className: "page-scroll" }, inner);
  }

  function renderOverlayTab(mode) {
    ensureOverlaySelection(mode);
    const ov = overlayMap.get(mode);
    if (!ov) return el("div", { className: "empty-state", text: "Overlay not found." });

    let activeLane = (ov.lanes || []).find((l) => l.id === state.overlayLaneId) || (ov.lanes || [])[0] || null;
    let activeStage = null;
    if (activeLane) {
      state.overlayLaneId = activeLane.id;
      activeStage = (activeLane.stages || []).find((st) => st.id === state.overlayStageId) || (activeLane.stages || [])[0] || null;
      if (activeStage) state.overlayStageId = activeStage.id;
    }

    const layout = el("div", { className: "overlay-layout" });
    const strip = el("div", { className: "lanes-strip" });

    for (const lane of ov.lanes || []) {
      const chain = el("div", { className: "stages-chain" });
      (lane.stages || []).forEach((st, idx) => {
        if (idx > 0) chain.appendChild(el("div", { className: "stage-arrow", text: "→" }));
        const isSel = activeLane && lane.id === activeLane.id && activeStage && st.id === activeStage.id;

        const metricChips = [];
        if (st.metrics) {
          for (const [mk, mv] of Object.entries(st.metrics).slice(0, 4)) {
            metricChips.push(el("span", { className: "metric-chip", text: mk + "=" + mv, title: mk + "=" + mv }));
          }
        }

        const runBadges = [];
        for (const fid of st.runs || []) {
          const fx = fixtureMap.get(fid);
          if (!fx) continue;
          const summaryRun = fx.runs.find((x) => x.build === "new") || fx.runs.find((x) => x.build === "old") || fx.runs[0];
          if (summaryRun) runBadges.push(el("span", { className: "pill " + statusPillClass(summaryRun.status) + " mono", text: summaryRun.verdict }));
        }

        const btn = el(
          "button",
          {
            className: "stage-card st-" + st.status + (isSel ? " selected" : ""),
            on: {
              click: () => {
                state.overlayLaneId = lane.id;
                state.overlayStageId = st.id;
                syncHash();
                render();
              },
            },
          },
          el("div", { className: "stage-head" }, el("span", { className: "stage-title", text: st.title || st.id }), el("span", { className: "pill " + st.status, text: st.status })),
          st.verdict ? el("div", { className: "stage-verdict", text: st.verdict, title: st.verdict }) : null,
          metricChips.length ? el("div", { className: "stage-metrics" }, metricChips) : null,
          runBadges.length ? el("div", { className: "stage-metrics" }, runBadges) : null
        );
        chain.appendChild(btn);
      });
      strip.appendChild(el("div", { className: "lane-row" }, el("div", { className: "lane-title", text: lane.title || lane.id }), chain));
    }
    layout.appendChild(strip);

    const hlNodes = new Set();
    const detailPane = el("div", { className: "stage-detail-pane" });

    if (activeStage) {
      for (const nid of activeStage.nodes || []) hlNodes.add(nid);
      for (const it of activeStage.items || []) {
        for (const nid of it.nodes || []) hlNodes.add(nid);
      }

      detailPane.appendChild(
        el(
          "div",
          { style: "display:flex;align-items:center;justify-content:space-between;gap:8px;flex-wrap:wrap;" },
          el("h3", { style: "margin:0;", text: activeStage.title || activeStage.id }),
          el("span", { className: "pill " + activeStage.status, text: activeStage.status })
        )
      );
      if (activeStage.verdict) detailPane.appendChild(el("span", { className: "pill mono", text: activeStage.verdict }));
      if (activeStage.detail) detailPane.appendChild(el("div", { className: "sub-text", text: activeStage.detail }));

      if (activeStage.runs && activeStage.runs.length) {
        const btns = el("div", { style: "display:flex;flex-wrap:wrap;gap:6px;" });
        for (const fid of activeStage.runs) {
          const fx = fixtureMap.get(fid);
          if (!fx) continue;
          for (const rn of fx.runs || []) {
            btns.appendChild(
              el("button", {
                className: "btn",
                title: "Replay " + fx.label + " (" + rn.id + ": " + rn.verdict + ")",
                text: "Open " + rn.id + " (" + rn.verdict + ")",
                on: { click: () => jumpToReplay(fx.id, rn.id, failingSeqForRun(fx, rn)) },
              })
            );
          }
        }
        detailPane.appendChild(btns);
      }

      if (activeStage.metrics && Object.keys(activeStage.metrics).length) {
        detailPane.appendChild(el("div", { className: "field-label", text: "Metrics" }));
        const mt = el("table", { className: "data-table" });
        const mb = el("tbody");
        for (const [k, v] of Object.entries(activeStage.metrics)) mb.appendChild(el("tr", null, el("td", { className: "mono", text: k }), el("td", { className: "mono", text: String(v) })));
        mt.appendChild(mb);
        detailPane.appendChild(mt);
      }

      if (activeStage.items && activeStage.items.length) {
        detailPane.appendChild(el("div", { className: "field-label", text: "Items (" + activeStage.items.length + ")" }));
        for (const it of activeStage.items) {
          detailPane.appendChild(
            el(
              "div",
              { className: "item-row" },
              el(
                "div",
                { style: "display:flex;align-items:center;gap:6px;flex-wrap:wrap;" },
                it.label ? el("span", { className: "pill " + statusPillClass(it.status), text: it.label }) : null,
                it.sink ? el("span", { className: "pill mono", text: it.sink }) : null
              ),
              it.text ? el("div", { className: "code-block", text: it.text }) : null
            )
          );
        }
      }
    }

    const canvas = buildMapCanvas({
      highlightNodes: hlNodes,
      selectedNodeId: state.selectedNodeId,
      viewState: overlayMapViewState,
      onSelectNode: (nid) => {
        state.selectedNodeId = state.selectedNodeId === nid ? null : nid;
        render();
      },
      onBackgroundClick: () => {
        if (state.selectedNodeId) {
          state.selectedNodeId = null;
          render();
        }
      },
    });
    if (state.selectedNodeId) {
      const insp = renderNodeInspector(state.selectedNodeId);
      if (insp) canvas.appendChild(insp);
    }

    layout.appendChild(el("div", { className: "overlay-bottom" }, detailPane, canvas));
    return layout;
  }

  function render() {
    stopReplayAnim();
    replayRuntime.activeCanvasCtx = null;
    while (appEl.firstChild) appEl.removeChild(appEl.firstChild);

    const shell = el("div", { className: "app-shell" });
    shell.appendChild(renderTopBar());

    if (state.showWarnings && warnings.length) {
      shell.appendChild(
        el(
          "div",
          { className: "warnings-bar" },
          el("strong", { text: "Map warnings" }),
          el("ul", null, warnings.map((w) => el("li", { text: w })))
        )
      );
    }

    const main = el("main", { className: "main-area" });
    if (state.tab === "map") main.appendChild(renderMapTab());
    else if (state.tab === "replay") main.appendChild(renderReplayTab());
    else if (state.tab === "invariants") main.appendChild(renderInvariantsTab());
    else if (state.tab.startsWith("mode:")) main.appendChild(renderOverlayTab(state.tab.slice(5)));

    shell.appendChild(main);
    appEl.appendChild(shell);
  }

  window.addEventListener("hashchange", () => {
    stopReplayPlayback();
    applyHash();
    render();
  });

  window.addEventListener("keydown", (e) => {
    const tag = document.activeElement ? document.activeElement.tagName : "";
    const inField = tag === "INPUT" || tag === "SELECT" || tag === "TEXTAREA";

    if (e.key === "/" && !inField && state.tab === "map") {
      const inp = document.querySelector('[data-kmap-search="1"]');
      if (inp) {
        e.preventDefault();
        inp.focus();
      }
      return;
    }

    if (e.key === "Escape") {
      let changed = false;
      if (state.replayPlaying) {
        stopReplayPlayback();
        changed = true;
      }
      if (state.selectedNodeId || state.showFlowsMenu) {
        state.selectedNodeId = null;
        state.showFlowsMenu = false;
        changed = true;
      }
      if (changed) render();
      return;
    }

    if (state.tab === "replay" && !inField) {
      if (e.code === "Space" || e.key === " ") {
        e.preventDefault();
        toggleReplayPlayback();
        return;
      }
      if (e.key === "f" || e.key === "F") {
        e.preventDefault();
        stopReplayPlayback();
        jumpToFirstDivergence();
        return;
      }
      if (e.key === "c" || e.key === "C") {
        const f = fixtureMap.get(state.replayFixtureId);
        const { hasCompare } = getCompareRuns(f);
        if (hasCompare) {
          e.preventDefault();
          switchReplayMode(isCompareActive(f) ? "old" : "compare");
        }
        return;
      }
      if (e.key === "1" || e.key === "2" || e.key === "3") {
        e.preventDefault();
        if (e.key === "1") switchReplayMode("old");
        else if (e.key === "2") switchReplayMode("new");
        else if (e.key === "3") switchReplayMode("compare");
        return;
      }
      if (e.key === "ArrowLeft" || e.key === "ArrowRight") {
        e.preventDefault();
        stopReplayPlayback();
        stepReplayBy(e.key === "ArrowLeft" ? -1 : 1);
      }
    }
  });

  const liveMeta = document.querySelector('meta[name="kavach-live"]');
  const isLiveMode =
    ((liveMeta && liveMeta.getAttribute("content") === "1") ||
      (document.body && document.body.dataset && document.body.dataset.live === "1")) &&
    window.location.protocol !== "file:";

  const liveState = {
    enabled: isLiveMode,
    status: "connecting",
    revision: 0,
    updatedAt: null,
  };

  function renderLivePill() {
    if (!liveState.enabled) return null;
    const isConnected = liveState.status === "live";
    const label = isConnected ? "Live" : liveState.status === "connecting" ? "Connecting…" : "Reconnecting…";
    const revText = liveState.revision > 0 ? "r" + liveState.revision : "";
    return el(
      "span",
      {
        className: "live-pill " + (isConnected ? "connected" : "reconnecting"),
        title: liveState.updatedAt ? "Updated " + liveState.updatedAt : "Live watch mode",
      },
      el("span", { className: "live-dot" }),
      el("span", { text: label }),
      revText ? el("span", { className: "live-rev", text: revText }) : null
    );
  }

  function renderPreservingSearchFocus() {
    const active = document.activeElement;
    const isSearch = active && active.getAttribute && active.getAttribute("data-kmap-search") === "1";
    const selStart = isSearch ? active.selectionStart : null;
    const selEnd = isSearch ? active.selectionEnd : null;
    render();
    if (isSearch) {
      const inp = document.querySelector('[data-kmap-search="1"]');
      if (inp) {
        inp.focus();
        if (selStart !== null && selEnd !== null) {
          try {
            inp.setSelectionRange(selStart, selEnd);
          } catch (_) {}
        }
      }
    }
  }

  function applyLiveModel(nextModel) {
    rebuildIndexes(nextModel);

    if (state.selectedNodeId && !nodeMap.has(state.selectedNodeId)) {
      state.selectedNodeId = null;
    }
    if (state.activeFlowId && !flows.some((f) => f.id === state.activeFlowId)) {
      state.activeFlowId = null;
      state.flowStepIdx = 0;
    }
    if (fixtures.length) {
      ensureReplaySelection();
      if (!fixtureMap.has(state.invFixtureId)) {
        state.invFixtureId = fixtures[0].id;
      }
    } else {
      state.replayFixtureId = null;
      state.replayRunId = null;
      state.replaySeq = null;
      state.invFixtureId = null;
    }
    if (overlays.length) {
      ensureOverlaySelection(state.overlayMode);
    } else {
      state.overlayMode = null;
      state.overlayLaneId = null;
      state.overlayStageId = null;
      if (state.tab.startsWith("mode:")) {
        state.tab = "map";
      }
    }

    renderPreservingSearchFocus();
  }

  function startLive() {
    if (!liveState.enabled || typeof EventSource === "undefined") return;

    let fetching = false;
    let pendingRev = 0;

    async function fetchLatestModel(targetRev) {
      if (fetching) {
        if (targetRev > pendingRev) pendingRev = targetRev;
        return;
      }
      fetching = true;
      try {
        const resp = await fetch("/api/model", { cache: "no-store" });
        if (!resp.ok) return;
        const revHdr = Number(resp.headers.get("X-Kavach-Revision") || targetRev || 0);
        const nextModel = await resp.json();
        if (revHdr > liveState.revision) liveState.revision = revHdr;
        else if (targetRev > liveState.revision) liveState.revision = targetRev;
        applyLiveModel(nextModel);
      } catch (_) {
        // Transient fetch errors will recover on subsequent events or reconnect.
      } finally {
        fetching = false;
        if (pendingRev > liveState.revision) {
          const next = pendingRev;
          pendingRev = 0;
          fetchLatestModel(next);
        }
      }
    }

    const es = new EventSource("/api/events");
    es.addEventListener("open", () => {
      liveState.status = "live";
      renderPreservingSearchFocus();
    });
    es.addEventListener("update", (ev) => {
      liveState.status = "live";
      let payload = null;
      try {
        payload = JSON.parse(ev.data || "{}");
      } catch (_) {
        return;
      }
      const rev = Number(payload.revision || 0);
      if (payload.updated_at) liveState.updatedAt = payload.updated_at;
      if (rev > liveState.revision) {
        fetchLatestModel(rev);
      } else {
        renderPreservingSearchFocus();
      }
    });
    es.addEventListener("error", () => {
      liveState.status = "reconnecting";
      renderPreservingSearchFocus();
    });
  }

  applyHash();
  render();
  startLive();
})();
