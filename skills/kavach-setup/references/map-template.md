# `.kavach/plan/map.json` Template

After writing `.kavach/plan/state-map.md` and `.kavach/plan/invariants.md`, author `.kavach/plan/map.json` (see [`examples/ledger/.kavach/plan/map.json`](../../../examples/ledger/.kavach/plan/map.json)) so `kavach map` can render the interactive call, state, and invariant flow map.

## Authoring Rules

- **One card per op (`kind: "entry"`):** Match by `source`, `position` (exact or trailing `*` glob), and/or JSON dot-path (`{"on": "input", "json": {"op": "..."}}`).
- **Upstream functions on the bug path (`kind: "function"`):** Include exact `file:line` or `file:start-end` entries in `code` (set `code_base` on the group to make them clickable links).
- **One state or sink card per store / emitted sink (`kind: "state"` or `"sink"`):** Match emitted outputs via `{"on": "output", "sink": "..."}`.
- **One invariant card per declared invariant (`kind: "invariant"`):** Set `"invariant"` to the exact `snake_case` name declared by the host. Add `match` rules only if an un-tagged host surfaces violations inside outputs or error messages.
- **One bug card per known defect (`kind: "bug"`):** Match recorded/replayed error or panic messages via `{"on": "failure", "kind": "...", "contains": "..."}`.
- **One flow per bug (`flows[]`):** Step through the entry op → functions → bug/state → broken invariant, setting `fixture` to the reproducing `.kavach` filename when available.

## Template

```json
{
  "kavach_map": 1,
  "title": "<Service or Subsystem Name>",
  "summary": "<One-sentence summary of the service flow and invariants>",
  "service": "<service-name>",
  "groups": [
    { "id": "entries", "title": "Inputs", "plain": "Entrypoints and operations" },
    { "id": "functions", "title": "Handlers", "plain": "Upstream functions along transition paths", "code_base": "" },
    { "id": "state", "title": "State & Sinks", "plain": "Mutable structures and emitted sinks" },
    { "id": "invariants", "title": "Invariants", "plain": "Laws checked after every step" },
    { "id": "bugs", "title": "Bugs", "plain": "Known defects and failure sites" }
  ],
  "nodes": [
    {
      "id": "in_op",
      "group": "entries",
      "kind": "entry",
      "title": "<op_name>",
      "plain": "<What this operation does>",
      "code": ["<path/to/handler.go:10-25>"],
      "match": [{ "on": "input", "json": { "op": "<op_name>" } }]
    },
    {
      "id": "fn_step",
      "group": "functions",
      "kind": "function",
      "title": "<FunctionName>",
      "plain": "<Plain-English summary>",
      "code": ["<path/to/service.go:40-80>"]
    },
    {
      "id": "sink_out",
      "group": "state",
      "kind": "sink",
      "title": "<sink_name>",
      "plain": "<What is emitted>",
      "code": ["<path/to/service.go:75>"],
      "match": [{ "on": "output", "sink": "<sink_name>" }]
    },
    {
      "id": "inv_law",
      "group": "invariants",
      "kind": "invariant",
      "title": "<invariant_name>",
      "plain": "<Exact invariant property>",
      "code": ["<path/to/invariants.go:15-30>"],
      "invariant": "<invariant_name>"
    },
    {
      "id": "bug_1",
      "group": "bugs",
      "kind": "bug",
      "title": "<Defect summary>",
      "plain": "<Root cause>",
      "code": ["<path/to/service.go:55>"],
      "match": [{ "on": "failure", "kind": "error", "contains": "<error substring>" }]
    }
  ],
  "edges": [
    { "from": "in_op", "to": "fn_step", "kind": "calls" },
    { "from": "fn_step", "to": "sink_out", "kind": "emits" },
    { "from": "sink_out", "to": "inv_law", "kind": "checks" },
    { "from": "fn_step", "to": "bug_1", "kind": "breaks" },
    { "from": "bug_1", "to": "inv_law", "kind": "breaks" }
  ],
  "flows": [
    {
      "id": "bug-1-flow",
      "title": "<Bug walkthrough>",
      "plain": "<What breaks>",
      "fixture": "<incident.kavach>",
      "steps": [
        { "node": "in_op", "text": "<Step 1 explanation>" },
        { "node": "fn_step", "text": "<Step 2 explanation>" },
        { "node": "bug_1", "text": "<Step 3 explanation>" },
        { "node": "inv_law", "text": "<Step 4 explanation>" }
      ]
    }
  ]
}
```
