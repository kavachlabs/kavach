# `kavach map` — Data, Call & Invariant Flow Maps

`kavach map` generates one self-contained interactive HTML file (with pan/zoom cards, edges, guided flow walkthroughs, step-by-step replay inspection, an invariant matrix, and optional stage overlay tabs) or prints the underlying JSON model via `--json`.

```bash
kavach map [map.json] [fixture.kavach]... [--bin H] [--new H] [--diff D.json]... [--overlay O.json]... [--watch DIR]... [--listen ADDR] [--interval D] [-o out.html] [--json] [--max-data N] [--title T]
```

With a spec (`map.json`), `kavach map` binds runtime fixture inputs, outputs, failures, and invariants onto code-level cards (`entry` → `function` → `state` / `sink` → `invariant` / `bug`). Without a spec, it synthesizes an `auto` map directly from the fixtures (`inputs`, `outputs`, `invariants`, `failures`).

## Modes

- **Recorded mode (no `--bin`):** Reads each `.kavach` fixture directly with no host binary required. Each `input` record starts a step; `clock`, `rand`, `gateway`, and `config` records are counted in `reads`; `output` records populate `outputs`; failure markers (`panic`, `error`, `invariant`, `crash`) set the step outcome and `recorded` failure.
- **Replay mode (`--bin`, optionally `--new`):** Replays each fixture through the `--bin` host command (`old` build) and optionally `--new` (`new` build) using the host protocol (SPEC §9). The `old` and `new` runs carry `declared` invariants, step outcomes, replayed outputs, and `divergence` between `old` and `new` (`replay.Compare`). Note: `--bin` has no `$KAVACH_BIN` default on `kavach map` so recorded-only invocations stay explicit.
- **Diff attachments (`--diff`):** Imports `kavach diff --json` reports, matching each report to the fixture with the same filename base name, and summarizes variant tallies by mutation kind (`by_kind`), per-invariant violations (`by_invariant`), outcomes (`by_outcome`), and failing checks.
- **Overlay mode (`--overlay`):** Adds one tab per distinct overlay `mode` (for example, `shadow`), rendering horizontal stage chains linked back to map nodes and fixtures. Overlays sharing the same `mode` merge into one tab.

## Live Recorder-Driven Map (`--watch`)

Watch a recorder or fixtures directory and serve an auto-refreshing map over HTTP:

```bash
kavach map [map.json] --watch <recorder-dir> [--listen 127.0.0.1:8090] [--interval 500ms]
```

- `--watch` may be repeated and combined with positional `map.json` / `*.kavach`, `--bin`/`--new`, `--diff`, `--overlay`, `--max-data`, and `--title` (cannot be combined with `--json` or `-o`/`--out`).
- Watches each directory recursively for `*.kavach` files (skipping hidden subdirectories unless explicitly passed as a `--watch` path), fingerprinting path, size, and modtime; truncated or unreadable files are skipped with a non-fatal warning until complete.
- **Endpoints:**
  - `/`: live HTML UI (`<meta name="kavach-live" content="1">`, `Cache-Control: no-store`) with a live status pill (`Live` / `Connecting…` / `Reconnecting…`) that auto-refreshes while preserving tab, selection, search, theme, and pan/zoom state.
  - `/api/model`: current `kavach_map_model: 1` JSON with `X-Kavach-Revision` and `Cache-Control: no-store`.
  - `/api/events`: Server-Sent Events (`text/event-stream`) emitting `event: update` payloads (`{ revision, updated_at, fixtures }`) and periodic keepalives.

## MCP Tool (`kavach_map`)

`kavach mcp` exposes `kavach_map` for AI agents to build and inspect flow maps without shelling out:

- **Parameters:** `spec`, `fixtures` (string array), `bin`, `new` (requires `bin`), `diffs` (string array), `overlays` (string array), `out`, `max_data` (integer `>= 0`, default `512`, `0` omits payloads), `title`, `include_model` (boolean, default `false`). At least one of `spec`, `fixtures`, or `overlays` is required.
- **Default summary vs full model:** Returns a lean structured summary (`title`, `nodes`, `edges`, `flows`, per-fixture `runs` with `verdict`/`status`/`steps`/`first_failure`, `overlays`, `warnings`, `stats`) by default; set `include_model: true` to include the full `kmap.Model` under `model`.
- **Optional HTML output:** Pass `out` to write the self-contained interactive HTML file and receive `out_path` and `html_bytes` in the tool response.

## Invariant Status Semantics

Each step tracks every invariant in `run.invariants` (ordered as: host-declared invariants first, then spec invariants not declared by the host, then any failing invariant in neither list):

| Step condition | Declared invariants | Undeclared / Recorded |
| --- | --- | --- |
| `ok` or `diverged` | `pass` (SPEC §6, §9: invariants run before output diffing) | `unknown` |
| `invariant` (failed at `X`) | Declared before `X`: `pass`; `X`: `fail`; after `X`: `unknown` (SPEC §9: stops at first failure) | `X`: `fail` if failed, else `unknown` |
| `panic`, `error`, `crash`, `timeout` | `skipped` (SPEC §9: host checks invariants only after an `ok` step) | `unknown` |
| `nondeterministic` | `unknown` | `unknown` |
| `not_reached` | `not_reached` | `not_reached` |

**Detection override:** If an `invariant` node defines `match` rules and matches a step's input, output, or failure (for example, an upstream host reporting an invariant violation inside a JSON output or `error` message), that invariant is marked `fail` for that step and recorded in `step.detected`.

## `map.json` Schema (`kavach_map: 1`)

Decoded strictly (`DisallowUnknownFields`).

```json
{
  "kavach_map": 1,
  "title": "Service Name",
  "summary": "Optional summary",
  "service": "service-id",
  "groups": [
    { "id": "entries", "title": "Inputs", "plain": "Entrypoints", "code_base": "https://github.com/org/repo/blob/<sha>/" }
  ],
  "nodes": [
    {
      "id": "in_deposit",
      "group": "entries",
      "kind": "entry",
      "title": "deposit",
      "plain": "Plain-English summary",
      "detail": "Optional longer note",
      "code": ["path/to/file.go:42-60"],
      "invariant": "required when kind == invariant",
      "match": [
        { "on": "input", "source": "http", "position": "POST /tx/*", "json": { "op": "deposit" }, "contains": "...", "regex": "..." }
      ]
    }
  ],
  "edges": [
    { "from": "in_deposit", "to": "fn_handle", "kind": "calls", "label": "optional" }
  ],
  "flows": [
    {
      "id": "bug-1",
      "title": "Bug walkthrough",
      "plain": "Short flow description",
      "fixture": "incident.kavach",
      "steps": [
        { "node": "in_deposit", "text": "Step explanation." }
      ]
    }
  ]
}
```

- **Node kinds:** `entry`, `function`, `state`, `sink`, `invariant`, `bug`, `external`, `note`.
- **Edge kinds:** `calls`, `reads`, `writes`, `emits`, `checks`, `breaks`, `flows` (empty defaults to `flows`).
- **Code links:** Entries in `code` can be `"path"`, `"path:12"`, or `"path:12-30"`. When the node's group sets `code_base`, URLs expand to `<code_base>/<path>#L12` or `#L12-L30`.
- **Rule matching:** Fields inside one rule are ANDed; multiple rules on a node are ORed.
  - `on`: `"input"`, `"output"`, or `"failure"`. When omitted, inferred from `sink` → `output`, `source`/`position` → `input`, `kind` → `failure`.
  - `position`: exact match, or prefix match when ending in `*`.
  - `json`: dot-path → scalar matched against parsed JSON object payloads (numbers normalized as `float64`).
  - `contains` / `regex`: tested against raw input/output bytes or failure `message`.

## Overlay Schema (`kavach_map_overlay: 1`)

```json
{
  "kavach_map_overlay": 1,
  "mode": "shadow",
  "title": "Shadow Healer",
  "summary": "Optional description",
  "lanes": [
    {
      "id": "incident-01",
      "title": "01_incident",
      "stages": [
        {
          "id": "harvest",
          "title": "Harvest ring → capsule",
          "status": "pass",
          "verdict": "scanned=2",
          "detail": "Optional detail",
          "nodes": ["in_bulk"],
          "fixture": "../capsules/01_incident.kavach",
          "metrics": { "scanned": 2 },
          "items": [
            { "label": "undo", "text": "...", "status": "fail", "sink": "ledger" }
          ]
        }
      ]
    }
  ]
}
```

- **Stage statuses:** `pass`, `fail`, `warn`, `info` (default), `skip`.
- **Stage `fixture`:** Resolved relative to the overlay file's directory, deduplicated into `fixtures`, and linked via `stage.runs`.
- **Node resolution:** Unknown `stage.nodes` entries are dropped with a warning (non-fatal). If `stage.nodes` is omitted and `fixture` is present, it defaults to the nodes hit at the fixture's failing (or last) step. `item.sink` resolves to nodes whose output rules match that sink.

## `--json` Model (`kavach_map_model: 1`)

Top-level fields emitted by `kavach map --json`:

- `kavach_map_model`, `generator`, `title`, `summary`
- `groups`, `nodes` (with computed `links` and `auto`), `edges`, `flows`
- `fixtures[]`: `{ id, label, path, service, origin, recorded, runs[], diff }`
  - `runs[]`: `{ id, mode, build, bin, verdict, status, detail, seq, invariants, declared, steps[], divergence, error }`
  - `steps[]`: `{ seq, source, position, input, input_truncated, omitted, outputs[], reads, outcome, message, hits[], invariants, detected }`
- `overlays[]`, `stats` (per-node `hits`, `violations`, `variants_old`, `variants_new`), `warnings`

## Privacy & Path Safety

- Pass `--max-data 0` to omit all step input/output payloads from the generated HTML and JSON (`"omitted": true` is set while rule matching still evaluates in-memory before serialization).
- All file paths (`fixture.path`, `overlay.source`, `stage.fixture`, `diff.source`) are stored relative to `BaseDir` (the output file's directory or current working directory), and host binary labels keep only `filepath.Base`.
- Embedded JSON is serialized with Go's HTML-safe `json.Marshal`, and the UI constructs all DOM elements via `textContent` with no `innerHTML` or external network requests.
