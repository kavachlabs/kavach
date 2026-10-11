# Bench console API

`bench api` serves this API on `http://127.0.0.1:4810` (`--port` to change it).
It only binds 127.0.0.1. It reads Harbor's job folders and starts and stops
`harbor run`. The terminal and web consoles are built on it.

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
  - Job: `running`, `done`, or `stopped`. `stopped` means Harbor was stopped,
    or exited without finishing the job.
  - Trial: `queued`, `running`, `pass`, `fail`, or `error`. A trial passes
    when its reward is 1. `error` means it has no reward: Harbor raised an
    exception, it was cancelled, or Harbor died while it ran
    (`"error": "interrupted"`).
- The arm comes from the end of the task name: `-control` or `-kavach`.
  Any other name has arm `""`.
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
  "error": "…",
  "trials": [ Trial, … ]
}
```

- `agent`: a Harbor agent name, or a custom agent's import path.
- `trials_done`: trials that passed, failed, or errored.
- `trials_total`: every trial the job will run.
- `cost`: the USD cost of all trials so far. It is 0 when the agent reports
  no cost.
- `stoppable`: true when this server started the job and Harbor is still
  running. Only such jobs can be stopped.
- `error`: only on a job this server started whose Harbor exited before it
  wrote the job folder. It holds the end of Harbor's output, such as a bad
  model name.
- `trials`: only in `GET /api/jobs/{id}` and in the replies to POSTs. It
  includes a queued placeholder for each planned trial that has not started,
  while the job runs. It is sorted by task, model, then attempt.

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
  - `reasoning` is counted on its own, as OpenCode reports it.
- `cost`: USD, or `null` when the agent reports no cost.
- `error`: `"<ExceptionType>: <message>"` from Harbor, `"interrupted"`, or
  absent.
- `kavach`: the verifier's `kavach.json`, or `null`. It never affects the
  grade.
- `opencode`: the agent's `opencode-live.json`, or `null`.
  - `finished` is `null` while the attempt's OpenCode server is up.
  - `export` is the path of `opencode-session.json` (an `opencode export`),
    when that file exists. Open a finished attempt with
    `opencode import <export>`.

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
  "env_file": true
}
```

- When a check fails, it reads `{"ok": false, "error": "Cannot connect to the Docker daemon…"}`.
- `tasks`: the number of benchmark tasks (base names).
- `env_file`: whether `bench/agent/.env` exists.

### GET /api/tasks

Lists the Harbor task folders (those holding a `task.toml`) under the tasks
directory, grouped by base name. `title` is the first line of the
instruction.

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
  "name": "glm-vs-sonnet"
}
```

| Field | |
| --- | --- |
| `tasks` | required. Task folders: names under the tasks directory (from `/api/tasks`, `arms[].task`), or absolute paths. Give one arm or both |
| `models` | the agent's models. Leave it out for `oracle` and `nop` |
| `attempts` | per task and model, 1–100. Default 1 (Harbor's `n_attempts`) |
| `agent` | a Harbor agent name, or a custom agent's import path (`module.path:ClassName`). Default `opencode` |
| `timeout_sec` | overrides the tasks' agent timeout when it is above 0 |
| `concurrent` | trials at once, 1–32. Default 1 |
| `name` | the job's folder name: letters, digits, `.`, `_`, `-`. Default the start time, `2006-01-02__15-04-05`, as Harbor names jobs |

The trials are every task × model × attempt.

Replies:

- 202 and the Job (`status: "running"`, `stoppable: true`). Its folder may
  not exist yet, so `tasks` and `trials` may be empty until the next poll.
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

Reply: 202 and the Job. 409 when this server is not running that job: it
already ended, or another process started it.

Runs live in their own process group, so stopping `bench api` (Ctrl-C) does
not stop them. They keep running and still show as `running` from their
files. They cannot be stopped from a later `bench api`.

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
