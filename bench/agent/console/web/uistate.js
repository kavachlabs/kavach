// The console's pane and poll logic, apart from the page: what is saved of
// the panes, what a load restores, how the panes toggle, and whether a status
// poll redraws. app.js draws what these decide; kavach/session.mjs runs them
// as a Kavach service, so that their bugs become fixtures.
//
// Nothing here touches the page, storage or the clock.

export const UI_PREFS_KEY = "kavach-bench.ui-prefs";

// NARROW is the screen width at which the left sidebar covers the view.
export const NARROW = "(max-width: 900px)";

// readPrefs reads the saved prefs (a JSON string, or null when none are
// saved) over the defaults. side is null until the left sidebar is toggled
// by hand.
export function readPrefs(raw) {
  const prefs = { leftW: 256, rightW: 400, side: null, rightSide: true, folded: [] };
  try {
    Object.assign(prefs, JSON.parse(raw || "{}"));
  } catch {}
  return prefs;
}

// prefsJSON is what is saved of the panes: their widths, whether each is
// open, and which sidebar sections are folded.
export function prefsJSON(ui) {
  return JSON.stringify({ leftW: ui.leftW, rightW: ui.rightW, side: ui.side, rightSide: ui.rightSide, folded: [...ui.folded] });
}

// initialPanes is the panes' state on load. side is null to follow the
// screen, or true or false once toggled; a narrow screen starts closed
// whatever was saved, since there the sidebar covers the view.
export function initialPanes(prefs, narrow) {
  return {
    side: typeof prefs.side === "boolean" && !narrow ? prefs.side : null,
    rightSide: prefs.rightSide !== false,
    folded: new Set(Array.isArray(prefs.folded) ? prefs.folded : []),
    leftW: prefs.leftW || 256,
    rightW: prefs.rightW || 400,
  };
}

// sideOpen says whether the left sidebar shows.
export function sideOpen(side, narrow) {
  return side != null ? side : !narrow;
}

// toggleSide, toggleRight and toggleFold are the panes' toggles: the menu
// button and ⌘B, the right pane's button and ⌘J, and a section's heading.
export function toggleSide(ui, narrow) {
  ui.side = !sideOpen(ui.side, narrow);
}

export function toggleRight(ui) {
  ui.rightSide = !ui.rightSide;
}

export function toggleFold(ui, key) {
  if (ui.folded.has(key)) ui.folded.delete(key);
  else ui.folded.add(key);
}

// statusRedraw decides whether a status poll redraws, given the key of the
// status last drawn (undefined before the first). A redraw rebuilds the
// new-run form, and an open model picker with it.
export function statusRedraw(drawn, status, err) {
  const key = JSON.stringify([status, err]);
  return { key, redraw: key !== drawn };
}
