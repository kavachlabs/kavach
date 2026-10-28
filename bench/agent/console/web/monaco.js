// Read-only Monaco editors for the invariant definitions (invariants.js).
// Monaco is loaded from the CDN the first time it is asked for; until it is
// ready, or when it can't load (offline), callers render plain <pre> blocks.
//
// The app rebuilds whole views on every render, so editors live in a cache
// keyed by the caller: an editor and the element it renders into. A render
// re-attaches the same element into the new tree, so the editor keeps its
// scroll, selection and focus. Editors whose element is no longer in the
// document after a render are disposed, their view state kept so they come
// back where they were.

const VS = "https://cdn.jsdelivr.net/npm/monaco-editor@0.52.2/min/vs";
const LOAD_TIMEOUT_MS = 15000;

let status = "idle"; // idle | loading | ready | failed
let monaco = null;
const listeners = new Set();

// monacoStatus says where loading Monaco is, starting it the first time it
// is called; onChange runs when it becomes ready or fails.
export function monacoStatus(onChange) {
  if (onChange) listeners.add(onChange);
  if (status === "idle") load();
  return status;
}

// A load that finishes after the timeout gave up still upgrades the views.
function settle(s) {
  if (status === s || status === "ready") return;
  status = s;
  for (const f of listeners) f();
}

function load() {
  status = "loading";
  const timer = setTimeout(() => settle("failed"), LOAD_TIMEOUT_MS);
  const fail = () => { clearTimeout(timer); settle("failed"); };
  // Monaco's workers must come from this origin: start them from a data URL
  // that imports the CDN's worker script.
  window.MonacoEnvironment = {
    getWorkerUrl: () => "data:text/javascript;charset=utf-8," + encodeURIComponent(
      `self.MonacoEnvironment = { baseUrl: ${JSON.stringify(VS.replace(/vs$/, ""))} };` +
      `importScripts(${JSON.stringify(VS + "/base/worker/workerMain.js")});`),
  };
  const script = document.createElement("script");
  script.src = `${VS}/loader.js`;
  script.async = true;
  script.onerror = fail;
  script.onload = () => {
    const req = window.require;
    if (typeof req !== "function" || !req.config) return fail();
    req.config({ paths: { vs: VS } });
    req(["vs/editor/editor.main"], () => {
      clearTimeout(timer);
      try {
        monaco = window.monaco;
        setup();
      } catch (e) {
        console.error("Monaco setup failed", e);
        monaco = null;
        return settle("failed");
      }
      settle("ready");
    }, fail);
  };
  document.head.append(script);
}

// The console's colour tokens (app.css :root), as Monaco wants them.
function token(name, fallback) {
  const v = getComputedStyle(document.documentElement).getPropertyValue(name).trim();
  return /^#[0-9a-f]{6}$/i.test(v) ? v : fallback;
}

export const THEME = "kavach";

function defineTheme() {
  const app = token("--app", "#191917");
  const panel = token("--panel", "#21211e");
  const text = token("--text", "#eeefe9");
  const dim = token("--dim", "#a7a89e");
  const faint = token("--faint", "#787970");
  const edge = token("--edge-strong", "#45453f");
  const pop = token("--pop", "#252522");
  const violet = token("--violet", "#b79cff");
  const peach = token("--peach", "#f7773a");
  const blue = token("--blue", "#8199ff");
  const ok = token("--ok", "#6abf6f");
  const bad = token("--bad", "#f26b4a");
  const bare = (c) => c.slice(1);
  const dark = getComputedStyle(document.documentElement).colorScheme === "dark";
  monaco.editor.defineTheme(THEME, {
    base: dark ? "vs-dark" : "vs",
    inherit: true,
    rules: [
      { token: "comment", foreground: bare(faint), fontStyle: "italic" },
      { token: "inserted", foreground: bare(ok) },
      { token: "deleted", foreground: bare(bad) },
      { token: "meta", foreground: bare(blue) },
      { token: "header", foreground: bare(dim), fontStyle: "bold" },
    ],
    colors: {
      "editor.background": panel,
      "editor.foreground": text,
      "editorGutter.background": panel,
      "editorLineNumber.foreground": faint,
      "editorLineNumber.activeForeground": dim,
      "editor.lineHighlightBackground": "#ffffff00",
      "editor.lineHighlightBorder": "#ffffff00",
      "editor.selectionBackground": blue + "40",
      "editor.inactiveSelectionBackground": blue + "26",
      "editorCursor.foreground": peach,
      "editorWidget.background": pop,
      "editorWidget.border": edge,
      "editorHoverWidget.background": pop,
      "editorHoverWidget.border": edge,
      "focusBorder": peach,
      "scrollbar.shadow": app + "00",
      "scrollbarSlider.background": edge + "99",
      "scrollbarSlider.hoverBackground": edge + "cc",
      "scrollbarSlider.activeBackground": dim + "66",
      "editorOverviewRuler.border": app + "00",
      "minimap.background": panel,
      "editor.findMatchHighlightBackground": violet + "40",
    },
  });
}

// retheme redefines the editors' theme from the console's colour tokens,
// after the console switched between light and dark.
export function retheme() {
  if (!monaco) return;
  defineTheme();
  monaco.editor.setTheme(THEME);
}

function setup() {
  defineTheme();
  // Snippets are fragments of files: language services would flag them.
  const ts = monaco.languages.typescript;
  if (ts) for (const d of [ts.typescriptDefaults, ts.javascriptDefaults]) d.setDiagnosticsOptions({ noSemanticValidation: true, noSyntaxValidation: true });
  if (monaco.languages.json) monaco.languages.json.jsonDefaults.setDiagnosticsOptions({ validate: false });
  for (const l of ["css", "scss", "less"]) {
    const d = monaco.languages.css && monaco.languages.css[`${l}Defaults`];
    if (d) d.setOptions({ validate: false });
  }
  // Monaco has no diff language: a unified diff's lines by their first
  // character.
  if (!monaco.languages.getLanguages().some((l) => l.id === "diff")) {
    monaco.languages.register({ id: "diff", extensions: [".diff", ".patch"] });
    monaco.languages.setMonarchTokensProvider("diff", {
      tokenizer: {
        root: [
          [/^(diff --git|index|---|\+\+\+|new file|deleted file|similarity|rename) .*$/, "header"],
          [/^@@.*$/, "meta"],
          [/^\+.*$/, "inserted"],
          [/^-.*$/, "deleted"],
          [/^.*$/, ""],
        ],
      },
    });
  }
  // Geist Mono loads after the page; measure again once it has.
  if (document.fonts && document.fonts.ready) document.fonts.ready.then(() => monaco.editor.remeasureFonts());
}

const LANG = {
  c: "c", h: "c", cc: "cpp", cpp: "cpp", cxx: "cpp", hh: "cpp", hpp: "cpp", hxx: "cpp",
  py: "python", pyi: "python", go: "go", rs: "rust", ts: "typescript", tsx: "typescript", mts: "typescript",
  js: "javascript", jsx: "javascript", mjs: "javascript", cjs: "javascript", java: "java", kt: "kotlin", kts: "kotlin",
  scala: "scala", rb: "ruby", php: "php", cs: "csharp", swift: "swift", lua: "lua", pl: "perl", pm: "perl",
  r: "r", ex: "elixir", exs: "elixir", clj: "clojure", dart: "dart", sh: "shell", bash: "shell", zsh: "shell",
  tcl: "tcl", sql: "sql", yaml: "yaml", yml: "yaml", json: "json", toml: "ini", ini: "ini", cfg: "ini",
  md: "markdown", html: "html", css: "css", xml: "xml", proto: "protobuf", diff: "diff", patch: "diff",
};

// languageOf names the Monaco language of a file, from its extension.
export function languageOf(path) {
  const base = String(path || "").split("/").pop();
  if (/^Dockerfile/i.test(base)) return "dockerfile";
  if (/^Makefile$/i.test(base)) return "plaintext";
  const ext = base.includes(".") ? base.split(".").pop().toLowerCase() : "";
  return LANG[ext] || "plaintext";
}

const LINE_H = 18;
const PAD = 6;
const HSCROLL = 8;

// The editors on screen: key -> { ed, host, trial, text, model }.
const cache = new Map();
// The view state of editors disposed because their view was left.
const viewStates = new Map();
// Shared models of whole patches, per trial and source: `${trial}|${i}`.
const models = new Map();
let currentTrial = "";

// useTrial disposes every editor and model that belongs to another trial.
export function useTrial(trial) {
  if (trial === currentTrial) return;
  currentTrial = trial;
  for (const [key, e] of cache) if (e.trial !== trial) drop(key, e, false);
  for (const key of viewStates.keys()) if (!key.startsWith(trial + "|")) viewStates.delete(key);
  for (const [key, m] of models) if (!key.startsWith(trial + "|")) { m.dispose(); models.delete(key); }
}

function drop(key, e, keepView = true) {
  if (e.ed) {
    if (keepView) {
      const vs = e.ed.saveViewState();
      if (vs) viewStates.set(key, vs);
    }
    e.ed.dispose(); // disposes the model it made; shared models stay
  }
  cache.delete(key);
}

// After a render, dispose the editors it didn't re-attach.
let sweepQueued = false;
function sweep() {
  sweepQueued = false;
  for (const [key, e] of cache) if (!e.host.isConnected) drop(key, e);
}
const observer = new MutationObserver((records) => {
  if (sweepQueued) return;
  for (const r of records) {
    if (!r.removedNodes.length) continue;
    // Monaco's own rendering removes nodes inside its editors all the time.
    if (r.target.closest && r.target.closest(".mon-host")) continue;
    sweepQueued = true;
    requestAnimationFrame(sweep);
    return;
  }
});
observer.observe(document.body, { childList: true, subtree: true });

// reattach returns a cached editor's element for the new tree, keeping
// focus on it when it had it.
function reattach(e) {
  const had = e.ed && e.host.contains(document.activeElement);
  if (had) queueMicrotask(() => { if (e.host.isConnected) e.ed.focus(); });
  return e.host;
}

const baseOptions = () => ({
  theme: THEME,
  readOnly: true,
  domReadOnly: true,
  automaticLayout: true,
  minimap: { enabled: false },
  scrollBeyondLastLine: false,
  fontFamily: getComputedStyle(document.documentElement).getPropertyValue("--font-code").trim() || "monospace",
  fontSize: 12,
  lineHeight: LINE_H,
  padding: { top: PAD, bottom: PAD },
  renderLineHighlight: "none",
  folding: false,
  glyphMargin: false,
  lineDecorationsWidth: 12,
  overviewRulerLanes: 0,
  overviewRulerBorder: false,
  hideCursorInOverviewRuler: true,
  stickyScroll: { enabled: false },
  renderValidationDecorations: "off",
  occurrencesHighlight: "off",
  selectionHighlight: false,
  links: false,
  matchBrackets: "never",
  guides: { indentation: false },
  unicodeHighlight: { ambiguousCharacters: false, invisibleCharacters: false },
  scrollbar: { alwaysConsumeMouseWheel: false, useShadows: false, verticalScrollbarSize: 8, horizontalScrollbarSize: HSCROLL },
  tabSize: 8,
  contextmenu: false,
});

const hitDecoration = (line) => ({
  range: new monaco.Range(line, 1, line, 1),
  options: { isWholeLine: true, className: "mon-hit", linesDecorationsClassName: "mon-hit-gutter" },
});

// snippetEditor is a read-only editor of a snippet: text whose first line is
// firstLine of its file, with the lines at hits (0-based) marked. It is as
// tall as its text, up to maxLines, then it scrolls.
export function snippetEditor(key, trial, { text, language, firstLine, hits, label, maxLines = 22 }) {
  const got = cache.get(key);
  if (got && got.text === text) return reattach(got);
  if (got) drop(key, got, false);
  const host = document.createElement("div");
  host.className = "mon-host";
  const lines = text.split("\n").length;
  const last = firstLine + lines - 1;
  const ed = monaco.editor.create(host, {
    ...baseOptions(),
    value: text,
    language,
    ariaLabel: label,
    lineNumbers: (n) => String(n + firstLine - 1),
    lineNumbersMinChars: Math.max(3, String(last).length),
  });
  ed.createDecorationsCollection(hits.map((i) => hitDecoration(i + 1)));
  const fit = () => {
    const wide = ed.getScrollWidth() > ed.getLayoutInfo().contentWidth;
    host.style.height = `${Math.min(ed.getContentHeight(), maxLines * LINE_H + 2 * PAD) + (wide ? HSCROLL : 0)}px`;
  };
  host.style.height = `${Math.min(lines, maxLines) * LINE_H + 2 * PAD}px`;
  ed.onDidContentSizeChange(fit);
  ed.onDidLayoutChange(fit);
  const vs = viewStates.get(key);
  if (vs) { ed.restoreViewState(vs); viewStates.delete(key); }
  cache.set(key, { ed, host, trial, text });
  return host;
}

// patchEditor is a read-only editor of a whole patch, the source'th of the
// trial's sources (or of a whole real file, "file|path", in its language),
// scrolled to and marking lines from..to with hit marked more strongly. The
// model is shared by every editor of it.
export function patchEditor(key, trial, source, { text, from, to, hit, label, language = "diff" }) {
  const mk = `${trial}|${source}`;
  let model = models.get(mk);
  if (!model || model.isDisposed()) {
    model = monaco.editor.createModel(text, language);
    models.set(mk, model);
  }
  const got = cache.get(key);
  if (got && got.model === model) return reattach(got);
  if (got) drop(key, got, false);
  const host = document.createElement("div");
  host.className = "mon-host mon-patch";
  const ed = monaco.editor.create(host, {
    ...baseOptions(),
    model,
    ariaLabel: label,
    lineNumbersMinChars: Math.max(3, String(model.getLineCount()).length),
  });
  const marks = [];
  for (let l = from; l <= to; l++) if (l !== hit) marks.push({ range: new monaco.Range(l, 1, l, 1), options: { isWholeLine: true, className: "mon-range" } });
  marks.push(hitDecoration(hit));
  ed.createDecorationsCollection(marks);
  const vs = viewStates.get(key);
  if (vs) { ed.restoreViewState(vs); viewStates.delete(key); }
  else {
    ed.setPosition({ lineNumber: hit, column: 1 });
    // Its size is known once it is in the document.
    requestAnimationFrame(() => ed.revealLineInCenter(hit, monaco.editor.ScrollType.Immediate));
  }
  cache.set(key, { ed, host, trial, model });
  return host;
}

// cachedElement keeps a plain element (a fallback <pre>) across renders the
// way the editors are kept, so its scroll survives.
export function cachedElement(key, trial, make) {
  const got = cache.get(key);
  if (got && !got.ed) return got.host;
  if (got) drop(key, got, false);
  const host = make();
  cache.set(key, { ed: null, host, trial });
  return host;
}
