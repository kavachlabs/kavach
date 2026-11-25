---
name: kavach-verify
description: >
  Check another agent's fix or feature with Kavach and report the verdict;
  never change the code yourself. Use when you are the verifier in a team of
  agents (an Antigravity /swarm, a sub-agent pipeline, a review step) and an
  author says a change is done, or when asked to confirm that someone else's
  patch to a stateful service really fixes a recorded `.kavach` incident or
  meets an approved `contract/`. Replays the incident on the old and new
  builds with `kavach diff` (or `kavach contract check` for a feature) and
  posts one fixed-format result naming the author, so the author knows exactly
  what to fix next. Not for writing the fix (use kavach-fix) or the feature
  (use kavach-implement).
compatibility: Requires the `kavach` CLI on PATH (or `kavach mcp` registered).
metadata:
  version: "0.2.0"
---

# Verify a Change with Kavach (`kavach-verify`)

You are the check, not the fix. An author changed a service and says it is done; you run Kavach against their build and report what Kavach says, in a form the author and the coordinator can act on without asking you anything.

## Hard Rules

1. **Never edit the author's code, the fixtures, or the contract.** A verifier that fixes things puts two agents in the same files. Report and let the author fix.
2. **Never edit, regenerate, or delete a `.kavach` fixture**, and never run `kavach contract approve`.
3. **Report the verdict exactly as Kavach prints it.** `unverified` is not `fixed`; `variant_failed(3)@15` is not "mostly works".
4. **Build what the author built.** Verify the author's own working copy or branch, not yours. If you cannot tell which one, ask the author once, by name, then wait.
5. **One result post per check**, in the format below. No running commentary on the board.

## Workflow

### Step 1: Find what to check

Read the author's completion post (in a swarm, `read_board` and find their `[done]` post; note its `#seq`). You need:

- the **old** build: the code before the change (the base commit, or the build the incident was recorded on);
- the **new** build: the author's change, from their workspace or branch;
- the **fixture** (`*.kavach`) for a fix, or the **contract directory** (default `contract/`) for a feature.

`kavach_list_incidents` (or `find . -name "*.kavach"`) finds fixtures. A host command is whatever runs the service as a Kavach host, e.g. `./bin/ledger-old` or `"python -m ledger"`.

### Step 2: Run Kavach

**A fix** (an incident fixture exists):

```bash
kavach replay <fixture> --bin <old>          # must fail: still_failing@N or invariant_violated(X)@N
kavach diff <fixture> --old <old> --new <new> --json > /tmp/kavach-diff.json
```

Or the MCP tools: `kavach_replay` with `{fixture, bin: old}`, then `kavach_diff` with `{fixture, old, new}`.

If the old build does **not** fail on the fixture, stop: post that to the author and coordinator; the check cannot say anything.

**A feature** (a `contract/` directory exists):

```bash
kavach contract check --old <old> --new <new> --json
```

Or the MCP tool `kavach_contract_check`.

### Step 3: Optional map link

If the repo has a map spec (`map.json`, usually next to a `build.sh`), build a map so the post can link straight to the failing step:

```bash
kavach map <map.json> <fixture> --bin <old> --new <new> --diff /tmp/kavach-diff.json -o <dir>/verify.map.html
```

Link `<dir>/verify.map.html#replay/<fixture name without .kavach>/compare/<N>`, where `N` is the failing seq. Skip this step if there is no spec; never write one just to verify.

### Step 4: Post the result

First line, always:

```
@<who acts next> [kavach] <verdict> · <fixture or contract> · re #<seq of the author's post>
```

Then, depending on the verdict:

| Verdict | Who acts next | Second line |
| --- | --- | --- |
| `fixed` | `@coordinator`, and start the text after the mention with `[done]` | `old <old> → new fixed · variants <variants.passing>/<variants.reproducing> · regressions <regressions.passing>/<regressions.counted>` |
| `pass` (contract) | `@coordinator`, with `[done]` | `contract pass · feature steps <feature_steps>` |
| `variant_failed(K)@N` | the author | `fix is too narrow: variant K (<mutation>) fails at seq N. Replay: kavach replay <variants.checks[id=K].file> --bin <new>` |
| `regressed(K)@N` | the author | `fix changed healthy traffic: regression variant K (<mutation>) differs at seq N. Compare kavach replay <regressions.checks[id=K].file> --bin <old> --json and --bin <new> --json` |
| `still_failing@N`, `invariant_violated(X)@N` | the author | `the recorded incident still fails at seq N: <new_detail>` |
| `diverged@N`, `nondeterministic@N` | the author | `steps before the failure changed at seq N: <new_detail>` |
| `regression@N`, `unreached@N` (contract) | the author | `<detail>` |
| `unverified` | the author and `@coordinator` | `only <variants.reproducing> of <variants.required> variants reproduce the incident; the fix cannot be confirmed` |

Add the map link as a last line when you built one. Read counts from the `--json` report (`kavach diff`: `verdict`, `old`, `detail`, `variants.{required,reproducing,passing,checks[]}`, `regressions.{counted,passing,checks[]}`, where each failing check carries `mutation` and the saved `file`; `kavach contract check`: `verdict`, `feature_steps`); do not estimate them.

Example after a fix that held:

```
@coordinator [done] [kavach] fixed · 01_split_repeated_identifier_bulk.kavach · re #12
old still_failing@15 → new fixed · variants 56/56 · regressions 42/42
map: showcase/blnk/map/verify.map.html#replay/01_split_repeated_identifier_bulk/compare/15
```

Example after one that did not:

```
@ledger [kavach] variant_failed(3)@15 · 01_split_repeated_identifier_bulk.kavach · re #12
fix is too narrow: variant 3 (duplicate payee, reordered) fails at seq 15. Replay: kavach replay /tmp/kv/variant-3.kavach --bin ./bin/ledger-new
```

### Step 5: Wait

Then wait for the author's next `[done]` (in a swarm, `swarm_wait`) and check again from Step 1. Mention only the agent who acts next: the author while it fails, the coordinator once it passes.

## In an Antigravity swarm

- Members talk with `post_message`, `read_board` and `swarm_wait`. Refer to earlier posts by `#seq`; do not paste other members' reports.
- Authors with their own workspace (`workspace:"share"` or `"branch"`) build there; `swarm{action:"status"}` shows where each one works. Build the new binary from that path.
- Kavach's tools are available to every member once `kavach mcp` is in `~/.gemini/config/mcp_config.json` (see the [skills README](../README.md#antigravity)).
