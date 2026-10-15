# Harbor tasks

The agent benchmark's tasks in the [Harbor](https://github.com/harbor-framework/harbor)
format, one task directory per experiment arm:

- **Track A (incidents):** `<task>-control` and `<task>-kavach`, checked with
  `kavach diff` (`redis-10068`, `redis-10764`, `redis-10858`, `redis-11734`,
  `redis-12082`).
- **Track B (features):** four arms per task (`go-genai-streamed-function-args`,
  `httpx-deterministic-cookie-store`, `kombu-single-active-consumer-priority`,
  `kombu-virtual-queue-dead-lettering`, `updo-policy-alerting`), checked with
  `kavach contract check`:
  - `<task>-control`: base commit and the feature specification;
  - `<task>-workflow`: base commit and the contract-first workflow using the
    project's own focused test suite;
  - `<task>-kavach`: base commit plus the Kavach integration, a locked
    behaviour contract (`/task/contract`), the `kavach` CLI and its MCP server;
  - `<task>-kavach-implement`: base commit plus the Kavach integration and
    `kavach` tools, with no contract supplied (the agent records traffic,
    declares/checks invariants, and initializes and locks the contract itself).

Harbor replaces `../harness/` for running agents.

## Layout

```
tasks/redis-10068-control/      Redis at the base commit, the bug report
tasks/redis-10068-kavach/       the same plus the Kavach integration, the
                                fixture, the kavach CLI and its MCP server
tasks/<track-b>-control/        base commit, the feature specification
tasks/<track-b>-workflow/       base commit, the contract-first test workflow
tasks/<track-b>-kavach/         base commit + Kavach integration, locked
                                /task/contract, kavach CLI and MCP server
tasks/<track-b>-kavach-implement/
                                base commit + Kavach integration and kavach
                                tools; agent builds and locks contract/
  task.toml
  instruction.md                ../tasks/<task>/task.md, with the arm's section
  environment/docker-compose.yaml   the agent's image
  tests/docker-compose.yaml     the verifier's image
  tests/test.sh                 runs the task's grade.sh, writes the reward
                                and kavach.json
  solution/                     applies the reference solution (oracle only)
check-candidates.sh             grades every candidate fix of redis-10068
                                through Harbor
```

The images are targets of each task's `../tasks/<task>/Dockerfile` (`control`,
`workflow`, `kavach` or `kavach-implement` for the agent; `grader` for the
verifier), built from the repository root because the image builds Kavach from
source. The compose files reach the root by a relative path; set `KAVACH_ROOT`
to the repository when running a copy of a task from somewhere else.

## Running

Needs Docker with Compose (Colima works) and `harbor` (0.24.0). Colima shares
only `$HOME` with its VM and Harbor mounts each trial's directory into the
containers, so the jobs directory must be under `$HOME`. Give it as an
absolute path: with a relative one Harbor's first upload to the verifier fails
and it falls back to a slower copy.

```sh
jobs=$PWD/bench/agent/results/harbor     # gitignored
harbor run -p bench/agent/harbor/tasks -a oracle -o "$jobs" -y   # reward 1
harbor run -p bench/agent/harbor/tasks -a nop    -o "$jobs" -y   # reward 0
harbor run -p bench/agent/harbor/tasks/redis-10068-kavach -a <agent> -m <model> -o "$jobs"
bench/agent/harbor/check-candidates.sh
```

The first run builds the images (a few minutes); afterwards a trial takes about
a minute plus the agent's time, and `check-candidates.sh` about four. `check-candidates.sh` runs the oracle on a copy
of each task for each patch in `../tasks/redis-10068/candidates/` and an empty
change, and prints the reward and Kavach's verdict for each:

| Candidate | Reward | Kavach |
| --- | --- | --- |
| `upstream`, `ms-delta-only`, `split-node-on-big-delta` | 1 | `fixed` (49 of 64 variants) |
| `stop-on-negative-delta`, empty | 0 | `invariant_violated(stream-trim-contract)@73` |

the same on both tasks.

## A trial

1. The agent works in `/work` in the agent image, as the image's user `agent`.
   In the `kavach` and `kavach-implement` arms, `task.toml` declares
   `kavach mcp` as a stdio MCP server.
2. When the agent stops, a collect hook writes its change, everything since
   the commit it started from (including any commits it made), to
   `/logs/artifacts/patch.diff`. In the arms with a contract, a second hook
   copies its `contract.json`, `rounds.jsonl`, `rounds/`, `lock.json` and
   `traffic.kavach` to `/logs/artifacts/contract/`. Then the agent's container
   is stopped.
3. The verifier runs in a new container from the grader image and gets only
   that patch. `test.sh` runs `grade.sh`: it applies the patch without changes
   to the hidden test files to the tree the agent started from, builds it,
   applies the hidden tests and runs them. The reward is 1 if they pass, else
   0. Then it runs `kavach diff` (Track A) or `kavach contract check` against
   `/task/contract` (Track B), old build against new; the `control` and
   `workflow` arms' trees get the integration first.

In the trial directory (`<jobs>/<job>/<task>__<id>/`):

| Path | |
| --- | --- |
| `result.json` | Harbor's result; the reward is `verifier_result.rewards.reward` |
| `agent/` | the agent's logs, and `trajectory.json` for agents that write one |
| `artifacts/logs/artifacts/patch.diff` | the agent's change |
| `verifier/reward.txt` | the reward |
| `verifier/kavach.json` | Kavach's verdict (below) |
| `verifier/grade.json`, `grade.log` | `grade.sh`'s result, and its build, test and Kavach output |
| `verifier/test-stdout.txt` | `test.sh`'s output |

## kavach.json

Written by the verifier in every arm. It never affects the reward.

```json
{"verdict": "fixed",
 "variants": "49 of 64 reproduce the incident on the old build · 49 of those pass on the new build",
 "old": "invariant_violated(stream-trim-contract)@73"}
```

- `verdict`: `kavach diff`'s verdict on Track A (`fixed`,
  `invariant_violated(<name>)@<seq>`, `variant_failed(<k>)@<seq>`, ...) or
  `kavach contract check`'s verdict on Track B (`pass`, `unreached@<seq>`,
  `regression@<seq>`, `invariant_violated(<name>)@<seq>`,
  `variant_failed(<k>)@<seq>`, ...), or `n/a` when it could not run.
- `features` (Track B): the `features` summary of `kavach contract check` (for
  example, `13 of 13 feature steps changed`).
- `variants`: the `variants` line of `kavach diff` or `kavach contract check`,
  or empty.
- `old`: the old build's verdict on the fixture or baseline traffic, or empty.

## What the agent can and cannot see

Checked with a stub agent that listed the container during the agent phase.

- It sees the instruction, `/work` (a fresh git repository with one commit, or
  two in the `kavach` and `kavach-implement` arms, so `git log` holds nothing
  after the base commit), `/task/TASK.md` and, in the `kavach` arm, the locked
  `/task/contract` (or `/task/incident.kavach` on Track A), the old build in
  `/opt/...` and the `kavach` CLI. `/logs/agent`, `/logs/artifacts` and an
  empty `/logs/verifier` are mounted from the trial directory.
- It cannot see the hidden test, `test.sh`, the candidates or the READMEs.
  They are only in the grader image or the task directory, and Harbor copies
  `solution/` into the container only for the oracle. Because the verifier has
  its own container, `/tests` never exists in the agent's, and nothing the
  agent leaves in its container reaches the grade except the patch.
  The verifier empties `/logs/verifier` before it runs.

## Network

Track A `task.toml` files set no network policy (`public` by default); Track B
`task.toml` files set `network_mode = "no-network"` (the OpenCode agent wrapper
in `bench/agent/harbor/agents` lets the container reach only its local model
proxy).
