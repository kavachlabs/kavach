# Bench console API

`bench api` serves this API on `http://127.0.0.1:4810` (`--port` to change it).
It only binds 127.0.0.1. It reads Harbor's job folders and starts and stops
`harbor run`. The terminal and web consoles are built on it.

The same server serves the web console at `/` (static files from `web/`).
`bench web` takes the same flags: it opens the browser at the server that
already answers on the port, or starts one and then opens it.

```sh
cd bench/agent/console && go run ./cmd/bench api [--jobs DIR] [--tasks DIR] [--root REPO] [--harbor CMD] [--port N]
```

| Flag | Default |
| --- | --- |
| `--root` | the repository, found by walking up from the current directory to the one holding `bench/agent` |
| `--jobs` | `<root>/bench/agent/results/harbor` (made absolute, because Harbor mounts trial folders into containers from it) |
| `--tasks` | `<root>/bench/agent/harbor/tasks` |
| `--harbor` | `harbor` |

`<root>/bench/agent/.env` is passed to Harbor as `--env-file` if it exists.
The console never reads it.

## Conventions

- Everything is JSON. Times are RFC 3339; a time that is not known is `null`.
- An error is a non-2xx status with `{"error": "message"}`.
- Requests must name a loopback host (`127.0.0.1`, `localhost`, `[::1]`).
  Otherwise the server answers 403, which blocks DNS rebinding. A POST that
  carries an `Origin` header must come from the same host and port, or it
  gets 403. `POST /api/jobs` needs `Content-Type: application/json`, or it
  gets 415. A web frontend served from the same origin meets all three.
- Status words:
  - Job: `running`, `done`, `stopped`, `pausing`, or `paused`. `stopped`
    means Harbor was stopped, or exited without finishing the job. See "Is
    Harbor still running?" below. `pausing` and `paused` are a job paused
    with `POST /api/jobs/{id}/pause`, before and after its Harbor exits.
  - Trial: `queued`, `running`, `pass`, `fail`, or `error`. A trial passes
    when its reward is 1. `error` means it has no reward: Harbor raised an
    exception, it was cancelled, or Harbor died while it ran
    (`"error": "interrupted"`).
- The arm comes from the end of the task name: `-control`, `-workflow`,
  `-kavach`, or `-kavach-implement`. Any other name has arm `""`.
- A job is identified by its folder name, and a trial by its folder name
  `<task>__<random>`. A queued trial has `"id": ""`.

## Objects

### Job

```json
{
  "id": "2026-10-11__11-20-33",
  "started": "2026-10-11T11:20:33.123+05:30",
  "finished": null,
  "status": "running",
  "tasks": ["redis-10068-control", "redis-10068-kavach"],
  "agent": "opencode",
  "models": ["openrouter/z-ai/glm-5.3"],
  "trials_done": 2,
  "trials_total": 4,
  "cost": 0.084,
  "stoppable": true,
  "resumable": false,
  "retryable": false,
  "error": "…",
  "trials": [ Trial, … ]
}
```

- `agent`: a Harbor agent name, or a custom agent's import path.
- `trials_done`: trials that passed, failed, or errored.
- `trials_total`: every trial the job will run.
- `cost`: the USD cost of all trials so far. It is 0 when the agent reports
  no cost.
- `tasks`, `agent`, `models`, `trials_total`: from the request until
  Harbor writes the job's `lock.json`, then from that file.
- `stoppable`: true when bench started the job (this server or an earlier
  one) and its Harbor is still running. Only such jobs can be stopped or
  paused.
- `resumable`: true when bench started the job and it is `paused` or
  `stopped`, not finished.
- `retryable`: true when bench started the job, its Harbor is not running,
  and Harbor wrote its folder: any of its trials can be run again with
  `POST /api/trials/{job}/{trial}/retry`.
- `error`: set on a `stopped` job when there is something to say:
  - for a job bench started whose Harbor exited without writing
    `result.json`, the end of Harbor's output, such as a bad model name;
  - for a job judged stopped from its files, why.
- `trials`: in `GET /api/jobs/{id}` and in the replies to POSTs, always an
  array, `[]` when there are none. The job list and `job` events leave it
  out. It includes a queued placeholder for each planned trial that has not
  started, while the job runs. It is sorted by task, model, then attempt.

#### Is Harbor still running?

Harbor writes `finished_at` only when it finishes. A job without it is
running unless one of these holds:

- **bench started it.** For each run, `<jobs>/.bench/` keeps:
  - `<id>.json`: the JobConfig given to Harbor;
  - `<id>.log`: Harbor's output;
  - `<id>.pid`: Harbor's pid.

  The job is running while that pid is alive and `ps` shows it running
  `harbor` on that `<id>.json`. This check also guards against pid reuse.
  The check is exact and survives restarts of `bench api`.
- **Harbor cancelled trials** (`n_cancelled_trials` > 0 in `result.json`).
  This is what Ctrl-C or a stop leaves behind.
- **Every trial has finished** and nothing in the job folder has changed
  for 2 minutes. Harbor writes `finished_at` within seconds of its last
  trial.
- **Nothing in the job folder has changed for 45 minutes.** This rule is
  for jobs started outside bench, which have no pid to check. Harbor and
  the agents write as they go; the longest quiet stretch is building a
  task's image, which redis-10068 allows 30 minutes for.

Otherwise the job counts as `stopped`, and its unfinished trials become
`error` with `"interrupted"`.

### Trial

```json
{
  "id": "redis-10068-kavach__Pass001",
  "job": "opencode-glm",
  "task": "redis-10068-kavach",
  "arm": "kavach",
  "model": "openrouter/z-ai/glm-5.3",
  "attempt": 1,
  "status": "pass",
  "reward": 1,
  "started": "2026-10-11T04:45:30Z",
  "finished": "2026-10-11T04:52:40Z",
  "seconds": 249,
  "steps": 41,
  "tokens": {"input": 136666, "cached": 1831936, "output": 34290, "reasoning": 31997, "fresh": 170956},
  "cost": 0.082,
  "error": "",
  "kavach": {
    "verdict": "fixed",
    "variants": "49 of 64 reproduce the incident on the old build · 49 of those pass on the new build",
    "old": "invariant_violated(stream-trim-contract)@73"
  },
  "opencode": {
    "url": "http://127.0.0.1:41234",
    "session": "ses_ed6b6dacfffe25MOZ8B7cm5kqa",
    "started": "2026-10-11T04:47:01Z",
    "finished": "2026-10-11T04:51:10Z",
    "export": "/…/redis-10068-kavach__Pass001/agent/opencode-session.json"
  }
}
```

- `attempt`: numbered from 1 within one task and model, in the order the
  attempts started.
- `started`: while the trial runs, this is when Harbor created its folder.
  Afterwards it is Harbor's own start time.
- `seconds`: the agent's run time, set once the trial has finished.
  `null` until then; compute elapsed time from `started`.
- `steps`: model calls so far. It counts while the trial runs, if the agent
  writes OpenCode's event log.
- `tokens`:
  - `input` excludes `cached`. Harbor's own input count includes the cached
    tokens; the console subtracts them.
  - `fresh` is input + output: the new tokens of the attempt. Show it as
    the attempt's token count. The cached input re-reads the conversation
    on every model call, so it dwarfs the rest: about 1.8M for GLM's
    attempt, against 171k fresh.
  - `reasoning` is part of `output`, not added to it. OpenCode's per-call
    `output` already includes the reasoning tokens: in the GLM attempt,
    reasoning ≤ output on every call (10850 of 10917, for example). Harbor's
    `n_output_tokens` is the sum of those. `reasoning` is a breakdown; never
    add it to `output` or to `fresh`.
- `cost`: USD, or `null` when the agent reports no cost.
- `error`: `"<ExceptionType>: <message>"` from Harbor, `"interrupted"`, or
  absent.
- `kavach`: the verifier's `kavach.json`, or `null`. It never affects the
  grade.
- `opencode`: the agent's `opencode-live.json`, or `null`.
  - `finished` is `null` while the attempt's OpenCode server is up.
  - `web` is the session's page in OpenCode's web UI, while the server is
    up. The bare `url` opens on a project list without it.
  - `export` is the path of `opencode-session.json` (an `opencode export`),
    when that file exists. Open a finished attempt with
    `<root>/bench/agent/harbor/show.sh <trial folder>`, which imports it into
    a separate OpenCode data folder.

### Step

One line of an attempt, in order:

```json
{"t_ms": 274, "kind": "shell", "title": "ls /work && git -C /work log --oneline -5",
 "detail": "00-RELEASENOTES\nBUGS\n…", "tool": "bash", "exit": 0}
```

| Field | |
| --- | --- |
| `t_ms` | milliseconds since the attempt's first event |
| `kind` | `text`, `shell`, `read`, `search`, `edit`, `mcp`, `error`, or `usage` |
| `title` | `text`: what the model said. `shell`: the command. `read`: the path, plus ` lines 780–839` when the read had a range. `search`: the pattern. `edit`: the path. `mcp`: the tool's name. `error`: `"<name>: <message>"`. `usage`: `"step N"` |
| `detail` | tool output; for `edit`, the diff (`-`/`+` lines). May be absent |
| `tool` | the tool's name as the agent called it |
| `exit` | the shell command's exit status, when known |
| `tokens`, `cost` | on `usage` steps only: that model call's tokens (same shape as the trial's, with `fresh`) and USD cost |
| `kavach` | true for Kavach's MCP tools (names starting `kavach_`). Their title is `kavach <verb> <binary>`, e.g. `kavach diff ./src/redis-server` |
| `verdict`, `passed`, `variants` | on Kavach steps whose output is JSON: the verdict (`fixed`, `invariant_violated(…)@73`, …), whether it passed, and for `diff`, `"49 of 64 variants reproduce on the old build · 49 of those pass"` |
| `contract` | on contract checks (`kavach_contract_check`, or `kavach contract check --json` in a shell): the report's `round`, `streak`, `verdict`, `passed`, `detail`; `feature_steps`, the seqs of the `changed` ones and the `unreached` ones; the `invariants` the contract requires, those the build `declared` (in check order) and `missing_invariants`; `variant_candidates`, `variants_passed`, `variants_preexisting`; `variant_kinds`, by class of mutation (`field`, `pair`, `move`, `drop`, `duplicate`, `clock`), each `{candidates, passed, preexisting}`; and `variant_failures`, each `{id, kind, mutation, feature_seq, verdict, seq, detail}` with the mutation and detail cut to 600 bytes. Parsed from the whole output, before `detail` is cut |

Title and detail are cut to 4000 bytes, ending in `…`. `mcp` covers MCP
tools and any tool not listed above, such as `todowrite`.

Where steps come from:

- Once the trial has finished: Harbor's ATIF trajectory
  (`agent/trajectory.json`). ATIF has one timestamp per model call, so all
  tools of one call share a `t_ms`, and it has no `exit`.
- While the trial runs: OpenCode's event log (`agent/opencode.txt`). Harbor's
  OpenCode agent writes it line by line into the trial folder, which is
  mounted into the container. Steps therefore grow live.
- An agent with neither file, such as oracle or nop, has no steps.

## Endpoints

### GET /api/status

Cached for 10 seconds.

```json
{
  "docker": {"ok": true, "version": "29.5.2"},
  "harbor": {"ok": true, "version": "0.24.0"},
  "jobs_dir": "/Users/me/kavach/bench/agent/results/harbor",
  "tasks_dir": "/Users/me/kavach/bench/agent/harbor/tasks",
  "tasks": 1,
  "env_file": true,
  "root": "/Users/me/kavach"
}
```

- When a check fails, it reads `{"ok": false, "error": "Cannot connect to the Docker daemon…"}`.
- `tasks`: the number of benchmark tasks (base names).
- `env_file`: whether `bench/agent/.env` exists.
- `root`: the kavach repository.
- `jobs_warning`: present when attempts in `jobs_dir` can't be graded. On a
  Mac, Colima shares only the home folder with Docker, so a jobs folder
  outside it reaches the verifier empty. The consoles show the warning and
  start no runs.

### GET /api/tasks

Lists the Harbor task folders (those holding a `task.toml`) under the tasks
directory, grouped by base name. `title` is the first line of the
instruction, without its `#` marks or a leading `Task:` label.

```json
[
  {
    "name": "redis-10068",
    "title": "XTRIM MINID deletes entries above the threshold",
    "arms": [
      {"arm": "control", "task": "redis-10068-control", "path": "/…/tasks/redis-10068-control"},
      {"arm": "kavach",  "task": "redis-10068-kavach",  "path": "/…/tasks/redis-10068-kavach"}
    ]
  }
]
```

### GET /api/tasks/{task}/environment

What one Harbor task (`redis-10068-kavach`, not the base name) runs in, for
reading before a run. `files` holds, in order: the agent's compose file
(`role: "agent"`), the verifier's (`"verifier"`), each Dockerfile they build
(`"dockerfile"`, listed once even when both build it) and `task.toml`
(`"task"`). A compose file's `dockerfile` is the absolute path it builds, with
`${VAR:-default}` in its context expanded from the server's environment as a
run would, and `target` its build target. A file that does not exist has
`missing: true`; one over 256 KiB is cut short with `truncated: true`. The
other fields come from `task.toml`. 404 when there is no such task.

```json
{
  "task": "axum-1934-kavach",
  "network": "no-network",
  "workdir": "/work",
  "build_timeout_sec": 1800,
  "verifier_mode": "separate",
  "mcp_servers": ["kavach"],
  "files": [
    {"role": "agent", "path": "environment/docker-compose.yaml", "content": "…",
     "dockerfile": "/…/bench/agent/tasks/axum-1934/Dockerfile", "target": "kavach"},
    {"role": "verifier", "path": "tests/docker-compose.yaml", "content": "…",
     "dockerfile": "/…/bench/agent/tasks/axum-1934/Dockerfile", "target": "grader"},
    {"role": "dockerfile", "path": "/…/bench/agent/tasks/axum-1934/Dockerfile", "content": "…"},
    {"role": "task", "path": "task.toml", "content": "…"}
  ]
}
```

### GET /api/models?q=nemotron

OpenRouter's public model list, fetched at most once an hour. `q` is
optional; it is a case-insensitive substring of the id or name. Prices are
USD per million tokens, and `null` when variable. `tools` says whether the
model supports tool calls; the agent needs them. The reply keeps
OpenRouter's order.

```json
[
  {"id": "nvidia/nemotron-3.5-lightning", "name": "NVIDIA: Nemotron 3.5 Lightning",
   "context_length": 262144, "price_in": 0.07, "price_out": 0.2, "tools": true}
]
```

For OpenCode, the model to run is `openrouter/<id>`.

### GET /api/jobs

Every job, newest first, without `trials`.

```json
[
  {"id": "api-oracle", "started": "2026-10-11T11:39:18.581474+05:30",
   "finished": "2026-10-11T11:40:51.763137+05:30", "status": "done",
   "tasks": ["hello-world-control"], "agent": "oracle", "models": [],
   "trials_done": 1, "trials_total": 1, "cost": 0, "stoppable": false}
]
```

### GET /api/jobs/{id}

One Job with `trials`, or 404.

### GET /api/jobs/{id}/compare

One row per task (base name), model, and arm, sorted in that order:

```json
[
  {"task": "redis-10068", "model": "openrouter/z-ai/glm-5.3", "arm": "control",
   "passes": 2, "attempts": 3, "errors": 0, "pending": 0,
   "median_seconds": 352, "median_fresh_tokens": 240112, "median_cached_tokens": 2310400, "mean_cost": 0.26},
  {"task": "redis-10068", "model": "openrouter/z-ai/glm-5.3", "arm": "kavach",
   "passes": 1, "attempts": 1, "errors": 0, "pending": 2,
   "median_seconds": 249, "median_fresh_tokens": 170956, "median_cached_tokens": 1831936, "mean_cost": 0.082}
]
```

- `attempts`: graded attempts only (pass or fail).
- The pass rate is `passes / attempts`.
- `errors` and `pending` (queued or running) are counted separately and are
  left out of the medians and the mean.
- Medians and the mean are `null` when no attempt has a value.
- `median_fresh_tokens` is the median of `tokens.fresh`, and
  `median_cached_tokens` the median of `tokens.cached`.

### GET /api/trials/{job}/{trial}

One Trial, or 404.

### GET /api/trials/{job}/{trial}/steps

The trial's steps so far, as an array of Step. It is `[]` when there are
none. Refetch it on that trial's `trial` event.

### GET /api/trials/{job}/{trial}/patch

The agent's change: `artifacts/logs/artifacts/patch.diff`, which the task's
collect hook writes when the agent stops.

- 200, `text/plain; charset=utf-8`, the diff as is. It is empty when the
  agent changed nothing.
- 404 when the file does not exist: the trial is still running, or its task
  has no collect hook.

### GET /api/trials/{job}/{trial}/grade

The verifier's `grade.json`, plus the tests the verifier's output lists.
404 when there is no `grade.json` (a running trial, or a task whose
verifier does not write one).

```json
{
  "grade": {"applied": true, "built": true, "hidden": "fail",
            "kavach_diff": "invariant_violated(stream-trim-contract)@73", "variants": ""},
  "passed": 7,
  "failed": 1,
  "tests": [
    {"name": "XTRIM with MINID option", "ok": true},
    {"name": "XTRIM with MINID option, big delta from master record", "ok": false}
  ]
}
```

- `grade`: `grade.json` as written. For redis-10068 it has these fields:
  - `hidden`: `pass` or `fail`, the grade.
  - `applied`, `built`: whether the patch applied and built.
  - `kavach_diff`, `variants`: Kavach's verdict.
- `tests`: found best-effort in `verifier/grade.log` and
  `verifier/test-stdout.txt`, in order. Two formats are recognised: Redis's
  test runner (`[ok]: name (1 ms)`, `[err]: name in file`, `[exception]`)
  and pytest's `-rA` summary (`PASSED file::test`, `FAILED file::test - why`).
  Skipped tests are left out. When a test appears twice, the later result
  wins.
- `tests` is `[]` when neither file lists any test; then `passed` and
  `failed` are 0. "1 of 8 XTRIM tests failed" is
  `failed` of `passed + failed`.

### GET /api/trials/{job}/{trial}/invariants?names=a,b

What the trial's service declares as invariants. Kavach knows an invariant
only by its name, so this reads the source that declares them: the task's
Kavach integration patch (the `*kavach*.patch` next to the Dockerfile its
compose file builds) and the agent's patch on top. `names`, optional, are
invariants the trial's contract checks named; they are looked for along with
any the source declares.

```json
{"task": "httpx-deterministic-cookie-store-kavach-implement",
 "sources": ["/…/tasks/httpx-deterministic-cookie-store/httpx-kavach.patch", "/…/jobs/…/artifacts/logs/artifacts/patch.diff"],
 "invariants": [
   {"name": "cookie-limits",
    "doc": "the store never holds more than max_cookies cookies.",
    "doc_at": {"role": "doc", "path": "kavach_driver.py", "line": 173, "text": "",
               "file": "/…/httpx-kavach.patch", "file_line": 190, "file_hit": 190},
    "code": [
      {"role": "declaration", "path": "kavach_driver.py", "line": 427, "text": "…",
       "file": "/…/httpx-kavach.patch", "file_line": 444, "file_hit": 445},
      {"role": "check", "path": "kavach_driver.py", "line": 437, "text": "def _limits(self) -> None:\n…",
       "file": "/…/httpx-kavach.patch", "file_line": 454, "file_hit": 454}]}]}
```

- `doc`: best effort, the docstring or comment that defines the name: a
  definition list entry (`cookie-scope   every cookie…`, or the name alone
  with its definition indented below) or a comment that starts with it
  (`// alert-cooldown: …`). Absent when the source has none.
- `code`: the `declaration`, the `check` function the declaration passes
  (up to 40 lines), and up to four lines where it `fails`; `line` is the
  first line of `text` in the patched file.
- `sources`: the absolute paths of the patches read, the task's first. Both
  are on the host; the patched source tree exists only in the task's image.
- `file`, `file_line`, `file_hit`: on a snippet, the patch it was read from
  (one of `sources`), the line of that patch that shows `text`'s first line,
  and the line that shows the first of `text`'s lines naming the invariant
  (`file_line` when none does). Lines the patch removes can sit between a
  snippet's lines, so `file_hit` is not always `file_line` plus an offset.
- `agent`: on an invariant or a snippet, the agent's patch adds or changes
  lines that name or describe it.
- `note`: why `invariants` is empty, when it is.
- `source`: where the real source files come from: `container` (the trial's
  own container, `<trial>__env-main-1`, while it runs), `image` (a throwaway
  container, with no network, from `kavach-bench/<task>:<arm>` with the
  trial's `patch.diff` applied on `/work`) or `patch` (neither; the snippets
  are only the patches' lines). `source_note` says why it is `patch`, or warns
  that the image was built after the trial started and its files may differ.
  When the image is missing the note has the `docker build` command that
  makes it. The first request for a trial does the work, one at a time per
  trial; the files are cached under `~/.cache/kavach-bench/src/<job>/<trial>/`
  (`image/…`, or `container/…` re-read every time). The patch is applied on
  the image's `HEAD`, as the collect hook diffs against the Kavach
  integration commit in the Kavach arms, and on the first commit otherwise.
- `files`: the repo-relative paths of the real files the snippets are in.
- `src_file`, `src_line`, `src_hit`: on a snippet when `source` is not
  `patch`, the absolute path of its cached real file, the line `text` starts
  at in it and the line naming the invariant (`src_line` when none does). The
  patch's line is moved by the agent's hunks and checked against the file,
  searching 200 lines either way; a snippet that isn't found has no
  `src_file` and keeps to the patch.

### GET /api/trials/{job}/{trial}/invariants/source?i=N

The text of `sources[N]` from the invariants response, as `text/plain`. Only
those patches can be read: `i` is an index, never a path. 400 when `i` is not
a non-negative integer; 404 when the trial is unknown or has fewer than
`N+1` sources.

### GET /api/trials/{job}/{trial}/invariants/file?path=P

The text of a real source file, `P` one of the repo-relative `files` of the
invariants response (fetch that first), as `text/plain`. Only files a patch of
the trial touches and the response fetched can be read. 400 when `path` is
missing; 404 for an unknown trial, a path no patch touches or that wasn't
fetched, and any path with `..` or a leading `/`.

### GET /api/trials/{job}/{trial}/incident

What the trial was checked against, decoded into steps: the recorded
incident with the verifier's `kavach diff` of it and the old and new builds'
replays, or, for a feature task, the contract's recorded traffic. The web
console draws its headline, probe strip and incident replay from it. 404
when the task has neither.

```json
{"kind": "incident", "fixture": "verifier/incident.kavach", "service": "ledger", "start": "genesis", "records": 34,
 "steps": [{"seq": 32, "source": "file:events.jsonl", "position": "8", "input": "{…}", "…": "…",
            "marker": {"kind": "panic", "message": "runtime error: invalid memory address or nil pointer dereference"}}],
 "failure": {"seq": 32, "kind": "panic", "message": "runtime error: …"},
 "old": {"verdict": "still_failing@32", "status": "still_failing", "seq": 32, "steps": [{"seq": 1, "outputs": [], "panic": "", "error": ""}]},
 "new": {"verdict": "fixed", "status": "fixed", "steps": […]},
 "diff": {"source": "json", "verdict": "variant_failed(7)@23", "old": "still_failing@32", "new": "fixed",
          "detail": "variant 7 (…): still_failing@23: panic: …",
          "divergence": {"seq": 32, "output": 0, "detail": "output 0 of step 32 differs"},
          "variants": {"candidates": 56, "reproducing": 47, "passing": 39, "required": 10,
                       "checks": [{"id": 7, "incident_seq": 23, "mutation": "failing input (seq 32): \"type\" \"deposit\" → \"transfer\"",
                                   "old": "still_failing@23", "new": "still_failing@23", "passed": false, "reproduces": true}]},
          "regressions": {"candidates": 48, "counted": 46, "passing": 46, "neighbours": 4,
                          "checks": [{"id": 1, "input_seq": 4, "mutation": "input (seq 5): …", "old": "ok", "new": "ok", "passed": true, "counted": true}]}}}
```

- The fixture is the grader's copy (`verifier/incident.kavach`), else the
  `incident.kavach` beside the Dockerfile the task's verifier builds; for a
  feature task, the collected contract's `traffic.kavach` (`kind:
  "traffic"`, with no `old`, `new` or `diff`).
- `steps` are as in a variant's replay below; `recorded` holds production's
  outputs. `failure` is the first step with a failure marker, `null` when
  there is none.
- `old` and `new`: `kavach replay --json` of the incident on each build
  (`verifier/replay-old.json`, `replay-new.json`), `null` when not saved.
- `diff`: `verifier/kavach-diff.json` (`source: "json"`), with every variant
  check (at most 512 of each). Runs graded before the graders saved it have
  the report from `grade.log` (`source: "log"`): the verdicts, the first
  divergence, and `variants_line` and `regressions_line` in place of the
  checks. `null` before grading.

### GET /api/trials/{job}/{trial}/variants

The rounds of the trial's contract checks and the failing variants they
saved, read from the contract its collect hook copied out of the container
(`artifacts/logs/artifacts/contract/`): `rounds.jsonl`, and
`rounds/<round>/variant-NN.kavach` with the new build's replay of it in
`variant-NN.json`.

```json
{"rounds": [
   {"round": 3, "at": "2026-10-12T15:17:05Z", "build": "python -m kavach_driver", "verdict": "variant_failed(12)@103", "changed": 59}],
 "variants": [
   {"round": 4, "id": 12, "has_result": true, "verdict": "invariant_violated(cookie-limits)@103", "service": "httpx-cookies",
    "variant": {"id": 12, "mutation": "input seq 148 delivered twice", "incident": 103, "failure": "invariant: cookie-limits"}}]}
```

- `rounds`: one per line of `rounds.jsonl`, in file order; lines that are
  not JSON are skipped. `build` is cut to 600 bytes.
- `variants`: by `round`, then `id`. `variant` is the journal header's
  (SPEC.md §3.2): `mutation` names seqs of the contract's traffic,
  `incident` is the seq of the varied feature input in the variant itself.
  `has_result` is set when `variant-NN.json` exists, and `verdict` is its
  verdict. `error` says why the journal or result could not be read.
- A trial without a collected contract answers 200 with `[]` for both and a
  `note`; so does one whose checks saved no variants.

### GET /api/trials/{job}/{trial}/variants/{round}/{id}

One saved variant, decoded into steps for stepping through it, next to what
the new build did with it. `round` and `id` are integers (400 otherwise);
404 when the variant is not there.

```json
{"round": 4, "id": 12, "service": "httpx-cookies", "start": "genesis",
 "variant": {"id": 12, "mutation": "input seq 148 delivered twice", "incident": 103, "failure": "invariant: cookie-limits"},
 "records": 105,
 "steps": [
   {"seq": 101, "source": "traffic", "position": "61",
    "input": "{\"client\": \"shop\", …}", "input_base64": false,
    "clock": [1791815821349495553], "random": 0,
    "gateways": [{"gateway": "db", "request": "…", "response": "…", "error": ""}],
    "config": [{"key": "MAX_COOKIES", "value": "50", "present": true, "source": "env"}],
    "recorded": [],
    "traffic": [{"sink": "client:shop", "data": "{\"error\":\"no client shop\"}"}], "traffic_seq": 148}],
 "new": {"verdict": "invariant_violated(cookie-limits)@103", "status": "invariant_violated", "seq": 103,
         "invariant": "cookie-limits", "detail": "client shop: holds 51 cookies, max_cookies=50",
         "steps": [{"seq": 101, "outputs": [{"sink": "client:shop", "data": "{…}"}], "panic": "", "error": ""}]}}
```

- `steps`: one per input record, in journal order. The clock, random,
  gateway, config, output and failure-marker records after an input belong
  to its step. `clock` is each clock read's Unix nanoseconds; `random` counts
  the random reads.
- `recorded`: the step's output records. Kavach writes a variant without
  outputs (it never happened), so this is normally `[]`. `traffic` and
  `traffic_seq` are the outputs and seq of the same input (same source and
  position) in the contract's `traffic.kavach`, when that was collected:
  what the old build did with it in the recorded traffic, not in the
  variant. `input_changed` is set when the variant's input data differs from
  the traffic's (a field mutation). An input whose source and position
  repeat in the traffic gets neither.
- `marker`: `{kind, message}`, on the step whose record holds a failure
  marker (`panic`, `error`, `invariant`, `crash`); absent otherwise.
- `new`: the new build's replay (`variant-NN.json`), `null` when it was not
  saved; `new_error` says why it could not be read. `verdict` is Kavach's
  verdict string. Steps may also carry `crash`, `timeout` and
  `synthesized_reads`.
- Text: bytes that are valid UTF-8 are a string cut to 4000 bytes (with
  `…`); others are the first 3000 bytes in base64 with a `_base64` flag set
  (`input_base64`, `data_base64`, `request_base64`, `response_base64`,
  `value_base64`). Output and gateway objects carry `local: true` for
  local scope.
- At most 2000 steps of the variant and of `new`; `truncated` is set when
  there were more. `journal_truncated` is set when the file ends inside a
  block (only its complete blocks are shown).

### POST /api/jobs

Starts `harbor run`. Content type `application/json`. Unknown fields are
rejected.

```json
{
  "tasks": ["redis-10068-control", "redis-10068-kavach"],
  "models": ["openrouter/z-ai/glm-5.3", "openrouter/anthropic/claude-sonnet-5.5"],
  "attempts": 3,
  "agent": "opencode",
  "timeout_sec": 3600,
  "concurrent": 1,
  "retries": 1,
  "name": "glm-vs-sonnet"
}
```

| Field | |
| --- | --- |
| `tasks` | required. Task folders: names under the tasks directory (from `/api/tasks`, `arms[].task`), or absolute paths. Give one arm or both |
| `models` | the agent's models. Leave it out for `oracle` and `nop` |
| `attempts` | per task and model, 1–100. Default 1 (Harbor's `n_attempts`) |
| `agent` | a Harbor agent name, or a custom agent's import path (`module.path:ClassName`). Default `opencode`, which runs the repository's OpenCode agent (`bench.agent.harbor.agents:KavachOpenCode`) rather than Harbor's: the model key stays out of the task container, and each attempt can be watched in OpenCode. Jobs list it as `opencode` |
| `timeout_sec` | overrides the tasks' agent timeout when it is above 0 |
| `concurrent` | trials at once, 1–32. Default 1 |
| `retries` | how many times an attempt that ends with an error (a provider or container failure) runs again, 0–5. Harbor deletes the failed attempt's folder first. It does not retry timeouts, usage limits or refusals, nor an attempt whose tests ran and failed. Default 0 |
| `name` | the job's folder name: letters, digits, `.`, `_`, `-`. Default adjective-colour-animal and seven random letters and digits (`brisk-cobalt-heron-x7k2p9q`) |

The trials are every task × model × attempt.

Replies:

- 202 and the Job (`status: "running"`, `stoppable: true`). Its folder may
  not exist yet. Until it does, `tasks`, `agent`, `models` and
  `trials_total` come from the request, and `trials` is `[]`.
- 400 for an invalid request.
- 409 when the name is taken.
- 415 for the wrong content type.

If Harbor fails at once, for example on a bad model, the job turns
`stopped` with `error` set.

### POST /api/jobs/{id}/stop

Sends SIGINT to the job's Harbor process (not its process group). Harbor
treats it like Ctrl-C:

- It cancels the running trials. They end as `error` with
  `"CancelledError"`.
- It removes their containers.
- It exits. The job becomes `stopped`, usually within about 15 seconds.

Reply: 202 and the Job. 409 when the job is not running, or bench did not
start it (it has no `<jobs>/.bench/<id>.pid`).

Runs outlive `bench api`:

- Each run is its own process group, so Ctrl-C on `bench api` does not
  reach Harbor.
- Harbor's output goes to `<jobs>/.bench/<id>.log`, not to a pipe, which
  would kill Harbor on its next write once the server is gone.

A later `bench api` sees such a run as `running` and `stoppable`, and can
stop it.

### POST /api/jobs/{id}/pause

Stops the job as `stop` does and marks it paused
(`<jobs>/.bench/<id>.paused`). Harbor has no pause of its own, and freezing
the containers would leave the agent's timeout running and its model
connections to time out. The job is `pausing` until Harbor exits, then
`paused`. A paused job leaves out its cancelled trials, and lists them as
`queued` again: resuming runs them from the start. What they cost is lost.

Reply: 202 and the Job. 409 as for `stop`.

### POST /api/jobs/{id}/resume

Continues a `paused` or `stopped` job bench started, with
`harbor jobs resume -p <jobs>/<id>`. Harbor keeps the finished trials,
deletes the cancelled ones (`CancelledError`) and runs the rest, in a new
Harbor process tracked as `start` tracks one. `harbor jobs resume` has no
`--env-file`; the env file is loaded by `/bin/sh` and never read by bench.
A job stopped before Harbor wrote its folder is started again from its
`<jobs>/.bench/<id>.json`.

Reply: 202 and the Job, now `running`. 409 when the job is running,
finished, or bench did not start it.

### POST /api/trials/{job}/{trial}/retry

Runs one trial of a `retryable` job again, finished or not, whatever its
outcome. The trial's folder moves to
`<jobs>/.bench/retried/<job>/<trial>.<yyyymmdd-hhmmss>`, where it is kept,
and `harbor jobs resume` runs the trial it no longer finds under a new id,
together with any others the job had not finished. A finished job is
finished again once it does.

Reply: 202 and the Job, now `running`. 404 when bench did not start the job
or it has no such trial; 409 while its Harbor is running.

### GET /api/events

Server-Sent Events. The server polls the jobs folder once a second while
anyone listens, and sends what changed.

On connect, it sends one `job` event per existing job (no trials). That
closes the gap between a GET and the stream. After that:

```
event: job
data: {"id":"api-oracle","started":"…","finished":null,"status":"running","tasks":["hello-world-control"],"agent":"oracle","models":[],"trials_done":0,"trials_total":1,"cost":0,"stoppable":true}

event: trial
data: {"id":"hello-world-control__pgVGJUW","job":"api-oracle","task":"hello-world-control","arm":"control","model":"","attempt":1,"status":"pass","reward":1,…}

: ping
```

- `job`: a Job without `trials`, whenever anything in it changed.
- `trial`: a Trial, whenever it changed: started, a new model step,
  finished. Queued placeholders send no events; their job's event covers
  them.
- `: ping`: a comment every 15 seconds.

A job whose folder is deleted sends nothing. A client that falls more than
1024 events behind is disconnected and should reconnect. `EventSource`
reconnects on its own.
