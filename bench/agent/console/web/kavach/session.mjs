// Session is a person's session with the console as a Kavach handler: what
// they do (load the page, toggle the panes, fold a section, open the model
// picker) and what the console's status poll returns, one input each. It
// runs the console's own code for these, uistate.js and index.html's
// pre-paint script, and stands in only for the page: localStorage, which
// survives a load, and what is drawn.
//
// Its invariants say what the person expects:
//
//   saved_panes_restore   what a toggle saves, restored on the same screen,
//                         draws the panes as they are
//   panes_survive_reload  a load draws the panes as they were left
//   first_paint_matches   the first paint shows the left sidebar as the
//                         first render does, so it does not show and then
//                         slide shut
//   form_kept             a poll that returned what the last one did does
//                         not rebuild the new-run form: a rebuild loses its
//                         focus and scroll, and an open model picker
//
// An input is a JSON object:
//
//   {"do": "load", "narrow": false, "view": "new"}
//   {"do": "toggle_left"} {"do": "toggle_right"} {"do": "fold", "key": "runs"}
//   {"do": "open_picker"} {"do": "close_picker"}
//   {"do": "poll", "status": {...}}   (or "error": "...")

import { readFileSync } from "node:fs";
import { HandlerError } from "../../../../../sdk/typescript/dist/index.js";
import * as ui from "../uistate.js";

const utf8 = (v) => new TextEncoder().encode(JSON.stringify(v));

// bootScripts are index.html's inline scripts, which run before the first
// paint.
const bootScripts = (() => {
  const html = readFileSync(new URL("../index.html", import.meta.url), "utf8");
  const head = html.slice(0, html.indexOf("</head>"));
  return [...head.matchAll(/<script>([\s\S]*?)<\/script>/g)].map((m) => m[1]);
})();

// boot runs the pre-paint scripts against a stand-in for the page and says
// whether the first paint shows the left sidebar. It shows unless a script
// marks it closed (the boot-noside class; .main.noside comes from app.js).
function boot(storage, narrow) {
  const classes = new Set();
  const root = { dataset: {}, style: { setProperty() {} }, classList: { add: (c) => classes.add(c), remove: (c) => classes.delete(c) } };
  const page = {
    document: { documentElement: root },
    localStorage: { getItem: (k) => (k in storage ? storage[k] : null), setItem() {} },
    matchMedia: (q) => ({ matches: q === ui.NARROW && narrow }),
  };
  for (const src of bootScripts) new Function(...Object.keys(page), src)(...Object.values(page));
  return !classes.has("boot-noside");
}

export class Session {
  storage = {}; // localStorage: survives a load
  page = null; // the loaded page: { narrow, view, panes, picker, drawn, lastPoll }
  left = null; // the panes as the person last left them, on a wide screen
  step = {}; // what this input did, for the invariants

  handle(env, input) {
    let ev;
    try {
      ev = JSON.parse(new TextDecoder().decode(input.data));
    } catch (e) {
      throw new HandlerError(`decode input at ${input.position}: ${e.message}`);
    }
    this.step = {};
    if (ev.do === "load") return this.load(env, ev);
    const p = this.page;
    if (!p) throw new HandlerError(`${ev.do} before the page loaded`);
    switch (ev.do) {
      case "toggle_left":
        ui.toggleSide(p.panes, p.narrow);
        return this.save(env);
      case "toggle_right":
        ui.toggleRight(p.panes);
        return this.save(env);
      case "fold":
        ui.toggleFold(p.panes, ev.key);
        return this.save(env);
      case "open_picker":
      case "close_picker":
        if (p.view !== "new") throw new HandlerError(`the model picker is on the new-run page, not ${p.view}`);
        p.picker = ev.do === "open_picker";
        return env.emit("page.picker", utf8({ open: p.picker }));
      case "poll":
        return this.poll(env, ev);
    }
    throw new HandlerError(`unknown input ${JSON.stringify(ev.do)}`);
  }

  // load is a page load: the pre-paint scripts paint, then app.js reads the
  // saved prefs and renders.
  load(env, ev) {
    const narrow = Boolean(ev.narrow);
    const painted = boot(this.storage, narrow);
    const panes = ui.initialPanes(ui.readPrefs(this.storage[ui.UI_PREFS_KEY] ?? null), narrow);
    if (narrow) panes.side = false; // app.js closes it on a narrow screen
    this.page = { narrow, view: ev.view || "job", panes, picker: false, drawn: undefined, lastPoll: undefined };
    const drawn = this.drawn();
    env.emit("page.paint", utf8({ left: painted }));
    env.emit("page.render", utf8(drawn));
    this.step = { load: true, painted, drawn, expected: narrow ? null : this.left };
  }

  // save is what app.js does after a toggle: save the prefs and redraw.
  save(env) {
    const p = this.page;
    this.storage[ui.UI_PREFS_KEY] = ui.prefsJSON(p.panes);
    const drawn = this.drawn();
    env.emit("page.storage", utf8({ [ui.UI_PREFS_KEY]: this.storage[ui.UI_PREFS_KEY] }));
    env.emit("page.render", utf8(drawn));
    if (p.narrow) return; // a narrow screen starts closed whatever is saved
    this.left = drawn;
    const restored = ui.initialPanes(ui.readPrefs(this.storage[ui.UI_PREFS_KEY]), false);
    this.step = { saved: true, drawn, restored: this.drawnOf(restored, false) };
  }

  // poll is a status poll returning: the new-run form is redrawn when
  // statusRedraw says so.
  poll(env, ev) {
    const p = this.page;
    const status = ev.status ?? null;
    const err = ev.error ?? "";
    const same = p.lastPoll !== undefined && p.lastPoll === JSON.stringify([status, err]);
    p.lastPoll = JSON.stringify([status, err]);
    const draw = ui.statusRedraw(p.drawn, status, err);
    if (!draw.redraw) return;
    p.drawn = draw.key;
    if (p.view !== "new") return;
    env.emit("page.form", utf8({ rebuilt: true, picker: p.picker }));
    this.step = { rebuilt: same, picker: p.picker };
  }

  drawn() {
    return this.drawnOf(this.page.panes, this.page.narrow);
  }

  drawnOf(panes, narrow) {
    return { left: ui.sideOpen(panes.side, narrow), right: panes.rightSide, folded: [...panes.folded].sort() };
  }

  invariants() {
    const s = () => this.step;
    return [
      {
        name: "saved_panes_restore",
        check() {
          const { saved, drawn, restored } = s();
          if (saved && JSON.stringify(drawn) !== JSON.stringify(restored)) {
            throw new Error(`drawn as ${JSON.stringify(drawn)}, saved as what a load draws as ${JSON.stringify(restored)}`);
          }
        },
      },
      {
        name: "panes_survive_reload",
        check() {
          const { load, drawn, expected } = s();
          if (!load || !expected) return;
          if (JSON.stringify(drawn) !== JSON.stringify(expected)) {
            throw new Error(`left as ${JSON.stringify(expected)}, drawn after the load as ${JSON.stringify(drawn)}`);
          }
        },
      },
      {
        name: "first_paint_matches",
        check() {
          const { load, painted, drawn } = s();
          if (load && painted !== drawn.left) {
            throw new Error(`the first paint ${painted ? "shows" : "hides"} the left sidebar, the first render ${drawn.left ? "shows" : "hides"} it`);
          }
        },
      },
      {
        name: "form_kept",
        check() {
          const { rebuilt, picker } = s();
          if (rebuilt) throw new Error(`a poll that returned what the last one did rebuilt the new-run form${picker ? " and the open model picker" : ""}`);
        },
      },
    ];
  }
}
