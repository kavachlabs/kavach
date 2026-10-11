# Harbor tasks

The agent benchmark's tasks in the [Harbor](https://github.com/harbor-framework/harbor)
format, one task per experiment arm. A task name ends in `-control` or
`-kavach`, and the console reads the arm from that suffix. So far there is one
pair: `redis-10068`, the pilot in `../tasks/redis-10068/` (see its README).
Harbor replaces `../harness/` for running agents.

## Layout

```
tasks/redis-10068-control/      Redis at the base commit, the bug report
tasks/redis-10068-kavach/       the same plus the Kavach integration, the
                                fixture, the kavach CLI and its MCP server
  task.toml
  instruction.md                ../tasks/redis-10068/task.md, without its
                                "Kavach arm only" section for control
  environment/docker-compose.yaml   the agent's image
  tests/docker-compose.yaml     the verifier's image
  tests/test.sh                 runs the pilot's grade.sh, writes the reward
                                and kavach.json
  solution/                     applies the upstream fix (oracle only)
check-candidates.sh             grades every candidate fix through Harbor
```

Both images are targets of the pilot's `../tasks/redis-10068/Dockerfile`
(`control` or `kavach` for the agent, `grader` for the verifier), built from
the repository root because the image builds Kavach from source. The compose
files reach the root by a relative path; set `KAVACH_ROOT` to the repository
when running a copy of a task from somewhere else.

## Running

Needs Docker with Compose (Colima works) and `harbor` (0.24.0). Colima shares
only `$HOME` with its VM and Harbor mounts each trial's directory into the
containers, so the jobs directory must be under `$HOME`. Give it as an
absolute path: with a relative one Harbor's first upload to the verifier fails
and it falls back to a slower copy.

```sh
jobs=$PWD/bench/agent/results/harbor     # gitignored
harbor run -p bench/agent/harbor/tasks -a oracle -o "$jobs" -y   # reward 1 on both
harbor run -p bench/agent/harbor/tasks -a nop    -o "$jobs" -y   # reward 0 on both
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
   In the Kavach arm, task.toml declares `kavach mcp` as a stdio MCP server.
2. When the agent stops, a collect hook writes its change, everything since
   the commit it started from (including any commits it made), to
   `/logs/artifacts/patch.diff`. Then the agent's container is stopped.
3. The verifier runs in a new container from the grader image and gets only
   that patch. `test.sh` runs `grade.sh`: it applies the patch without changes
   under `tests/` to the tree the agent started from, builds it, applies the
   hidden upstream test and runs the XTRIM tests. The reward is 1 if they
   print `All tests passed`, else 0. Then it runs `kavach diff` of the fixture,
   old build against new; the control arm's tree gets the integration first.

In the trial directory (`<jobs>/<job>/<task>__<id>/`):

| Path | |
| --- | --- |
| `result.json` | Harbor's result; the reward is `verifier_result.rewards.reward` |
| `agent/` | the agent's logs, and `trajectory.json` for agents that write one |
| `artifacts/logs/artifacts/patch.diff` | the agent's change |
| `verifier/reward.txt` | the reward |
| `verifier/kavach.json` | Kavach's verdict (below) |
| `verifier/grade.json`, `grade.log` | `grade.sh`'s result, and its build, test and diff output |
| `verifier/test-stdout.txt` | `test.sh`'s output |

## kavach.json

Written by the verifier in both arms. It never affects the reward.

```json
{"verdict": "fixed",
 "variants": "49 of 64 reproduce the incident on the old build · 49 of those pass on the new build",
 "old": "invariant_violated(stream-trim-contract)@73"}
```

- `verdict`: `kavach diff`'s verdict for the agent's build (`fixed`,
  `invariant_violated(<name>)@<seq>`, `variant_failed(<k>)@<seq>`, ...), or
  `n/a` when it could not run: the patch did not apply or did not build, or,
  in the control arm, the integration does not apply on top of the agent's
  change.
- `variants`: the `variants` line of `kavach diff`, or empty.
- `old`: the old build's verdict on the fixture, or empty.

## What the agent can and cannot see

Checked with a stub agent that listed the container during the agent phase.

- It sees the instruction, `/work` (a fresh git repository with one commit, or
  two in the Kavach arm, so `git log` holds nothing after the base commit),
  `/task/TASK.md` and, in the Kavach arm, `/task/incident.kavach`,
  `/opt/redis-old/redis-server` and the `kavach` CLI. `/logs/agent`,
  `/logs/artifacts` and an empty `/logs/verifier` are mounted from the trial
  directory.
- It cannot see the hidden test, `test.sh`, the candidates or the READMEs.
  They are only in the grader image or the task directory, and Harbor copies
  `solution/` into the container only for the oracle. Because the verifier has
  its own container, `/tests` never exists in the agent's, and nothing the
  agent leaves in its container reaches the grade except the patch.
  The verifier empties `/logs/verifier` before it runs.

## Network

task.toml sets no network policy, so the agent, the verifier and both
containers have Harbor's default, `public`: the agent must reach its model
API. The agent's shell can therefore download the upstream fix. Limiting
egress to the model endpoint (Harbor's `network_mode = "allowlist"`) is left
to the agent configuration, which knows the endpoint.
