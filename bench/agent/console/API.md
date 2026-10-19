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

## Backup to R2

Harbor writes every run to the jobs folder on this machine, whatever
`environment` the attempts use. The server copies each run to a Cloudflare R2
bucket once its Harbor has exited (finished, failed or stopped, and again
after a resume or a retry ends), and, when it starts and every ten minutes
after, any run whose files changed since its last copy. A paused run is not
copied: it is about to be resumed, so it waits for its eventual exit. A copy in
progress stops when a new Harbor starts for the run, and is redone when that
Harbor exits. One run is copied at a time, in the background.

It is off unless `R2_ACCOUNT_ID`, `R2_ACCESS_KEY_ID` and `R2_SECRET_ACCESS_KEY`
are set in `bench/agent/.env` or the server's environment (`R2_BUCKET`
defaults to `kavach-bench-runs`). Setup is in
[harbor/README.md](../harbor/README.md#backing-runs-up-to-r2).

- Objects are `harbor/<job id>/<path in the job folder>`, signed with SigV4
  against `https://<account>.r2.cloudflarestorage.com`.
- Only new and changed files are uploaded. `<jobs>/.bench/sync/<id>.json`
  keeps each file's size, modification time and SHA-256, outside the job
  folder. A file deleted locally stays in the bucket: it is a backup, not a
  mirror. Symlinks and anything that is not a regular file are skipped.
- A file that holds the value of any key in the env file or the environment
  (a name with `KEY`, `TOKEN`, `SECRET` or `PASSWORD`; values of eight
  characters or more), or matches `sk-or-v1-…` or `sk-ant-…`, is **not**
  uploaded. The job's `sync.skipped_files` names it and says which key or
  pattern, never a secret or any part of one. It is checked again when the
  file changes.

`bench sync [--job ID,ID]` copies the finished runs once, without a server
(to fill the bucket with runs that already exist), and prints one line per
run. A run that is running or paused is left alone.

## A read-only copy

The server starts paid runs and writes API keys, so it is never put on the
internet. To publish results, `bench export` writes the web console and the
answer to every GET it makes for some runs as files, for any static host:

```sh
cd bench/agent/console && go run ./cmd/bench export --out DIR [--job ID,ID] [--landing ID] [--jobs DIR]
```

`--job` defaults to every finished run, and `--landing`, the run the copy
opens on, to the newest of them. The copy has no new-run form, settings,
controls, live events or editor links. Paths under the repository and the
home directory, the host name and the user name are rewritten; anything
else the runs hold (agent transcripts, patches) is published as it is, so
read it first. `staticPath` in `cmd/bench/export.go` and `apiURL` in
`web/app.js` agree on where each response is kept.

## Trial mode

`bench api -trial [-addr 0.0.0.0:8080] [-origin https://try.kavachlabs.dev]
[-trust-cf]` serves the console publicly so visitors can try Kavach with
their own keys. `-addr` beyond loopback is refused without `-trial`. Nothing
else changes without the flag.

> **Deploy a public trial behind Cloudflare Tunnel.** Run `cloudflared` on the
> same machine, point it at the server's loopback address, and start the
> server with `-addr 127.0.0.1:PORT -trust-cf -origin https://your.host`. The
> server then never listens on a public interface: every request comes from
> the tunnel, which adds the real client address as `CF-Connecting-IP`, and
> the per-address rate limit, session limit and one-run limit depend on that
> address. Without the tunnel the header is whatever a client writes, so
> `-trust-cf` honours it only from a loopback connection, and refuses to
> start with a `-addr` beyond loopback. A server on a public `-addr` without
> `-trust-cf` sees only the proxy's address, if any, and limits everyone as
> one visitor.

Settings come from the environment (and the config file below):

| Variable | Default |
| --- | --- |
| `TRIAL_MODELS` | `openrouter/z-ai/glm-5.3`; comma-separated allowlist. All of a run's models must share a provider, whose usual key variable (`OPENROUTER_API_KEY`, `ANTHROPIC_API_KEY`, `OPENAI_API_KEY`, `NVIDIA_API_KEY`, `ZAI_API_KEY`) carries the visitor's key |
| `TRIAL_TASKS` | every task folder under `--tasks`; entries are folder names or base names |
| `TRIAL_MAX_ATTEMPTS` | 2; for invite sessions |
| `TRIAL_OWN_MAX_ATTEMPTS` | 10 attempts per task and model, for a session with its own keys |
| `TRIAL_OWN_MAX_CONCURRENT` | 4 attempts at once in one run, for a session with its own keys |
| `TRIAL_OWN_MAX_TRIALS` | 40 attempts in one run, retries included: (retries + 1) x tasks x models x attempts; for a session with its own keys |
| `TRIAL_OWN_MAX_RETRIES` | 2 Harbor retries of an erroring attempt, for a session with its own keys |
| `TRIAL_OWN_MAX_RETRY_REQUESTS` | 5 `retry` requests per run (kept in `<jobs>/.bench/<id>.retries`; more is 429), for a session with its own keys |
| `TRIAL_MAX_RUNNING` | 3 live runs, all visitors together; more gets 429 |
| `TRIAL_INVITE_CODES` | none; comma-separated |
| `TRIAL_INVITE_RUNS` | 5 runs per code |
| `TRIAL_SHOWCASE` | none; job ids anyone may list and read besides trial runs |
| `TRIAL_CONFIG` | `/etc/kavach/trial.json`; the config file below |
| `TURNSTILE_SECRET`, `TURNSTILE_SITE_KEY` | off; when the secret is set, `POST /api/jobs` from a session with its own keys needs a `turnstile_token` that Cloudflare's siteverify accepts (an invite session's runs are not checked) |

**Config file.** `-trial-config PATH` (or `TRIAL_CONFIG`; default
`/etc/kavach/trial.json`) names a JSON file the operator edits while the
server runs, so changes need no restart and forget no session. Its invite
codes are bearer credentials for the server's keys, so it is created 0640
`root:kavach` (0600 without a `kavach` group) and an edit keeps the mode; the
keys themselves stay in the environment:

```json
{
  "invites": [{"code": "yc-ab12-cd34-ef56-gh78", "label": "Garry (YC)", "runs": 5,
               "created": "2026-10-18T09:00:00Z", "revoked": false, "note": ""}],
  "tasks": ["redis-10068-control", "redis-10068-kavach"],
  "models": ["openrouter/z-ai/glm-5.3"],
  "max_attempts": 2,
  "own_max_attempts": 10,
  "own_max_concurrent": 4,
  "own_max_trials": 40,
  "own_max_retries": 2,
  "own_max_retry_requests": 5,
  "showcase": []
}
```

`tasks`, `models`, `max_attempts`, `own_max_attempts`, `own_max_concurrent`,
`own_max_trials`, `own_max_retries`, `own_max_retry_requests` and `showcase` replace the `TRIAL_*` value
of the same name when they are set, and the variable applies where they are
empty. Codes in `TRIAL_INVITE_CODES` stay valid beside the file's, each with
`TRIAL_INVITE_RUNS`; a code in both is the file's, so it can revoke one. With
no file the variables alone apply, so deleting the file brings back env codes
it had revoked: remove retired codes from `TRIAL_INVITE_CODES` as well. A file
that exists but does not load stops `bench api -trial` from starting (the
environment alone could allow more than the file did), and so does an
unreadable `invites-used.json`. The running server compares the file's mtime and
size on each use and reads it again when they change. A file that cannot be
parsed, has an unknown field or breaks a rule (a code under 8 characters, with
spaces or commas, or listed twice; a model that is not `provider/name`) is
logged and ignored: the last good configuration stays. `bench trial` edits the
file atomically, keeping its mode and owner (`bench trial -h`; see
[deploy/trial/ONBOARDING.md](../deploy/trial/ONBOARDING.md)).

Each code has its own `runs` budget. The runs used, the time of the last one
and the job ids it started are kept per SHA-256 of the code in
`<jobs>/.bench/invites-used.json` (written atomically, and read again at
start; counts from the earlier `trial-invites.json` carry over). A revoked
code cannot be redeemed, and a session that redeemed it before gets 403
(`this invite code has been revoked`) from its next `POST /api/jobs`;
`invite_runs_left` shows 0. Runs already started continue.

**Sessions.** A visitor has no account. `POST /api/trial/keys` or
`/api/trial/invite` starts a session and sets its cookie (32 random bytes in
hex; `HttpOnly`, `SameSite=Lax`). Over https, or with `X-Forwarded-Proto:
https`, the cookie is `__Host-kv_session` and `Secure` (`Path=/`, no
`Domain`); over plain http, as in local development, `kv_session`. Both calls
always mint a new id and drop the session the request came with. Only the
list of the visitor's runs carries over (the runs' owner records are rewritten
to the new id), so they keep Stop on a live run; keys, invite and everything
else are the new ones. Each request that uses a session refreshes the cookie's `Max-Age`;
a session expires after 24 hours idle. One network address holds at most 10
sessions (a new one replaces its oldest), and a full table (10000) drops the
one idle longest.

Sessions live in memory: **a restart forgets every session and its keys**.
Visitors enter their keys again, and can no longer stop runs that began
before the restart (the runs themselves continue, and stay readable).

**Keys** are held in the session's memory only. They are never written to
disk (not in a job folder, `.bench/<id>.json`, the log or the owner file),
never logged and never returned; each is 20-512 visible ASCII characters. A
run's Harbor gets them in its process environment: the minimal base (`PATH`,
`USER`, `LANG`, `LC_ALL`, `TMPDIR`, `PYTHONPATH`) plus the visitor's
variables, without `--env-file`, so nothing else of the console's
environment reaches it. Its `HOME` is a folder of its own
(`<jobs>/.bench/home/<id>`, mode 0700, removed when Harbor exits), so the
operator's `~/.modal.toml` and other credential files are out of reach;
`UV_CACHE_DIR`, `UV_TOOL_DIR` and `UV_PYTHON_INSTALL_DIR` keep pointing at
the operator's, so a uv-installed Harbor still starts. Invite runs get the
same, with the server's keys.

A visitor's or an invite's Harbor is started by a helper, `bench _logpipe`
(hidden), which runs Harbor with the keys in its own environment, never in
argv, and writes Harbor's output to `<id>.log` with the run's key values (also
split across writes) replaced by `[redacted]`, so a provider's error that
quotes a key never lands on disk. The helper is detached like any Harbor, in a
process group of its own, so a console that exits or restarts leaves the run
going. The pid recorded for the run is the helper's: it forwards `SIGINT` and
`SIGTERM` to Harbor alone, which is what Stop sends, then writes what it held
back, replaces the run's key values (also JSON-escaped) with `[redacted]` in
every file under the job's folder, in place, a file at a time through a
temporary file and a rename with the old mode, removes the run's home folder
and exits with Harbor's code. At start the server removes the home folders
of runs that are not running. If the helper is killed outright (`kill -9`,
the OOM killer) Harbor lives on in the helper's process group: the run still
counts as live, and Stop interrupts the processes of that group whose command
line names the run's config. Nothing scrubs the job folder then, and the
home folder stays until the sweep; the console's per-run scrub below
covers served files for as long as it runs.

**Restart the console only through its own process** (for systemd,
`KillMode=process`). The helpers are detached so a restart leaves the runs
going, but `pkill bench`, which matches the helpers too, or a signal to the
whole control group, sends them SIGTERM, and that stops every visitor run. The log is not served: a stopped
job's `error` is a short message chosen from it: `the model provider
rejected the key`, `the sandbox provider rejected the key`, or `Harbor
exited with an error (code N)`. A trial's `error` is cut to its kind
(`AgentTimeoutError`). A trial's `opencode` block has no `url`, `web` or
`export`.

As a second line of defence, what is served is also scrubbed: each trial run's
own key values, while it is live in this process, are replaced by `[redacted]` in that
run's responses and events, and the server's own keys (the env file's and the
environment's model and provider keys and the R2 credentials) in all of them.
Only values of 20 characters or more are replaced. The server's folders in
every `/api/*` response are replaced by `<jobs>`, `<tasks>`, `<root>` and `~`
for the home folder, only where one is a whole path prefix, so `/rootfs` is
left alone when the home folder is `/root`.

`GET /api/trial/session` (no cookie needed; reads never create a session):

```json
{
  "has_keys": true, "provider": "daytona", "invite": false,
  "runs": ["brisk-cobalt-heron-x7k2p9q"],
  "limits": {"models": ["openrouter/z-ai/glm-5.3"], "tasks": ["redis-10068-control", "redis-10068-kavach"], "max_attempts": 2,
             "own": {"max_attempts": 10, "max_concurrent": 4, "max_trials": 40, "max_retries": 2, "max_retry_requests": 5, "max_timeout_min": 120}},
  "turnstile_site_key": "",
  "invite_runs_left": 4
}
```

`limits.own` appears only for a session with its own keys, and then `tasks`
lists every task. `invite_runs_left` appears only for an invite session. `provider` of an invite
session is the first of `daytona`, `modal` the server has keys for.
`turnstile_site_key` is empty for an invite session without keys, whose runs
are not checked.

`POST /api/trial/keys` `{"model_key", "provider": "daytona"|"modal",
"daytona_api_key", "modal_token_id", "modal_token_secret"}` stores the keys
the provider needs (each 8-512 visible ASCII characters, no spaces or
newlines; unknown fields are rejected) and replies like GET. 400 otherwise.
`DELETE /api/trial/keys` forgets keys and invite; 204.

`POST /api/trial/invite` `{"code"}` makes the session an invite session, which
runs on the server's keys (the env file, else the process environment),
passed to Harbor the same way as a visitor's: only the model key and the
provider's. The provider is the first of `daytona`, `modal` whose keys are
all variables; a `~/.modal.toml` does not count. 403 for an unknown code (compared in constant time), a revoked one, or one
without runs left. A code's runs started are kept in
`<jobs>/.bench/invites-used.json` under the SHA-256 of the code.

**Starting a run.** `POST /api/jobs` needs a session with keys or an invite
(401 otherwise). A session that brings its **own keys** gets the same run
form and controls as the local console, for its own runs; an **invite**
session, whose runs bill the operator, keeps the limited form below. Unknown
fields are rejected with 400, as everywhere (`env` is not a request field).
`turnstile_token` is read only from a session with its own keys: an invite
already says who runs, and caps their runs.

*Invite session.* Takes `{"tasks", "models", "attempts"}` (a `turnstile_token` is accepted and ignored);
the other fields of the ordinary request (`agent`, `environment`, `name`,
`concurrent`, `retries`, `timeout_sec`) are known and overridden.
`models` defaults to the first allowed one. The server forces the rest:
`environment` is the session's provider, `agent` the repository's OpenCode
agent, `concurrent` 2, `retries` 0, no timeout override, a generated `name`;
400 for a model, task or attempts count outside `TRIAL_MODELS`,
`TRIAL_TASKS` and `TRIAL_MAX_ATTEMPTS`.

*Session with its own keys.* Takes every field of the ordinary request,
checked on the server: `tasks` any task arm in `--tasks` (the invite
`TRIAL_TASKS` do not apply; an absolute path is not a task); `agent`
`opencode` (default), `oracle` or `nop`, never an import path; `models` for
`opencode`, none for the others, each `openrouter/<vendor>/<name>` and in
OpenRouter's model list (502 if the list cannot be fetched; the key form takes
an OpenRouter key, so no other provider's models); `attempts` 1 to
`TRIAL_OWN_MAX_ATTEMPTS`, `concurrent` 1 to `TRIAL_OWN_MAX_CONCURRENT`,
(retries + 1) x tasks x models x attempts at most `TRIAL_OWN_MAX_TRIALS`, `retries` 0 to `TRIAL_OWN_MAX_RETRIES`,
`timeout_sec` 0 (the task's own) to 7200; `environment` omitted or the
session's own provider (`docker` and the other providers are 400: docker
would run on this server). `name` is ignored and generated. Harbor gets the
session's sandbox keys, and its model key when the agent calls a model, as
above and nothing of the server's.

Own keys: 400/403 for a missing/failed Turnstile token. Both: 429 when
the session, or another session from the same network address, already has a
live run, or `TRIAL_MAX_RUNNING` is reached; 403 when the invite is spent or revoked. A
bad key is an ordinary failed run: the job turns `stopped` or its trials
`error`, with the classified message above in `error`.

**Ownership and visibility.** Each run records the SHA-256 of its session's id
(in memory and `<jobs>/.bench/<id>.owner`, so it survives a restart). GETs of
jobs and trials stay public, as share links (`#/jobs/<id>`), but only for
trial runs and `TRIAL_SHOWCASE`: `GET /api/jobs` lists only those and any
other job or trial is 404. `stoppable` is true only for the owner, `sync` is always absent, and
`resumable` and `retryable` are true only for the owner when the session is
not an invite's. `POST /api/jobs/{id}/stop` is for the owner (else 403).

**Controls for runs on own keys.** `POST /api/jobs/{id}/pause`, `POST
/api/jobs/{id}/resume` and `POST /api/trials/{job}/{trial}/retry` work for the
owner (by the hashed session id) of a session that is not an invite's; anyone
else, an invite session included, gets the 404 they always had. Resume and
retry start Harbor again, so they go through the same path as starting: the
session's own keys in the isolated environment and home folder, the
`_logpipe` helper, never `--env-file`, never the server's keys; the live-run
limits (one per session, one per address, `TRIAL_MAX_RUNNING`) apply, so
resuming a run beside a live one is 429. They are refused with 409 when the
server no longer holds the session's keys (forgotten, expired, a restart):
`enter your keys again to resume`; nothing is started, and the server's
environment is never the fallback. They are also 409 when the run used a
sandbox other than the session's provider, or a model other than OpenRouter's
(a run another session began, carried over when a session was replaced).
Turnstile is checked when a run starts, not on resume or retry: a resumed run
was started through it, and a fresh token per click would need the widget on
every run page; the address's write rate limit applies to both. A run takes at most
`TRIAL_OWN_MAX_RETRY_REQUESTS` retry requests (429 after), counted across
restarts. Each relaunch removes the run's old home folder and starts in a fresh one.

**Locked for everyone**, own keys included, and 404 whoever asks:
`/api/settings` (it reads and writes the server's env file and keys),
`POST /api/jobs/{id}/sync` (the server's R2 backup), and
`/api/tasks/{task}/environment` (it builds with the server's Docker and names
local paths). A run on `docker` is refused for the same reason: runs go to
Daytona or Modal. `/api/status` and `/api/tasks` carry no local paths; the
invariants view shows patches rather than starting containers. The event stream
is filtered the same way; it reflects the cookie it was opened with.

**Requests.** The host check is off. A write with an `Origin` must match the
request's host or `-origin`. Each address may make 20 writes (POST, DELETE) a
minute (429 and `Retry-After`). The address is the connection's. With
`-trust-cf` it is the `CF-Connecting-IP` header, but only when the connection
is from loopback (a tunnel such as cloudflared on this machine); `-trust-cf`
with a `-addr` beyond loopback is a startup error, since anyone could then
forge the header.

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
  "sync": {"state": "synced", "at": "2026-10-11T11:41:02+05:30", "files": 214, "skipped": 0},
  "trials": [ Trial, … ]
}
```

- `agent`: a Harbor agent name, or a custom agent's import path.
- `environment`: where the attempts run (`daytona`, `modal`, `vercel`), from
  the job's config. Absent for local Docker runs.
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
- `sync`: the job's backup to R2 (see above). `state` is `off` (no R2 keys),
  `pending` (not copied yet), `syncing`, `synced` or `failed`. `at` is when the
  last copy finished and `files` how many of the job's files are in the bucket.
  `error` is the short reason for `failed`. `skipped` counts files the secret
  guard held back, and `skipped_files` lists up to 20 as
  `{"path": "agent/env.log", "reason": "contains the value of OPENROUTER_API_KEY"}`.
  `bench export` leaves it out.
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
  "root": "/Users/me/kavach",
  "environments": [
    {"name": "daytona", "ready": false, "keys": [{"name": "DAYTONA_API_KEY", "set": false}]},
    {"name": "modal", "ready": true, "keys": [{"name": "MODAL_TOKEN_ID", "set": true}, {"name": "MODAL_TOKEN_SECRET", "set": true}]},
    {"name": "vercel", "ready": false, "keys": [{"name": "VERCEL_TOKEN", "set": false}]}
  ],
  "sync": {
    "enabled": true,
    "bucket": "kavach-bench-runs",
    "keys": [
      {"name": "R2_ACCOUNT_ID", "set": true, "masked": "••••9f3a"},
      {"name": "R2_ACCESS_KEY_ID", "set": true, "masked": "••••c2d1"},
      {"name": "R2_SECRET_ACCESS_KEY", "set": true, "masked": "••••77be"}
    ]
  }
}
```

- When a check fails, it reads `{"ok": false, "error": "Cannot connect to the Docker daemon…"}`.
- `tasks`: the number of benchmark tasks (base names).
- `env_file`: whether `bench/agent/.env` exists.
- `root`: the kavach repository.
- `environments`: for each cloud provider a run can use, whether a run on it
  can start: `ready` is true when it has credentials and the Harbor install
  has its extra (`harbor[daytona]`, `harbor[modal]`, `harbor[vercel]`, checked
  by importing the SDK with Harbor's own Python, and cached for 30 seconds
  after a failure). `need` describes the credentials accepted: Daytona
  `DAYTONA_API_KEY` or `DAYTONA_JWT_TOKEN` and `DAYTONA_ORGANIZATION_ID`;
  Modal `MODAL_TOKEN_ID` and `MODAL_TOKEN_SECRET` or a `~/.modal.toml`
  (`via` says when that file is what authenticates); Vercel `VERCEL_TOKEN`
  (Harbor's `vercel link` fallback is not honoured). `problem` says why
  `ready` is false, including the `uv tool install --with 'harbor[…]' harbor`
  command when the extra is missing. `keys` are names and booleans only, never
  a value; a key left empty in `.env` is unset even if the server's
  environment has it, as Harbor loads `.env` over it. Unlike the rest, it is
  read on every request: saving a key in `/api/settings` shows at once.
- `sync`: whether runs are backed up to R2. When `enabled` is false,
  `reason` says which variables are missing. The keys are masked to their
  last four characters, as in `/api/settings`. Like `environments`, it is read
  on every request.
- `jobs_warning`: present when attempts in `jobs_dir` can't be graded. On a
  Mac, Colima shares only the home folder with Docker, so a jobs folder
  outside it reaches the verifier empty. The consoles show the warning and
  start no local Docker runs; runs on a cloud provider are not affected.

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
(`role: "agent"`), the verifier's (`"verifier"`), each Dockerfile their images
are built from (`"dockerfile"`, listed once even when both use it) and
`task.toml` (`"task"`). The compose files build nothing: a compose file's
`image` is the prebuilt image `task.toml` names for it (`[environment]` or
`[verifier.environment]` `docker_image`,
`ghcr.io/kavachlabs/bench-<task>:<target>-<key>`), `target` the Dockerfile
stage in that ref, and `dockerfile` the absolute path of
`bench/agent/tasks/<task>/Dockerfile`, found from the tasks directory. A file that does not exist has
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
     "image": "ghcr.io/kavachlabs/bench-axum-1934:kavach-145a3830e64f",
     "dockerfile": "/…/bench/agent/tasks/axum-1934/Dockerfile", "target": "kavach"},
    {"role": "verifier", "path": "tests/docker-compose.yaml", "content": "…",
     "image": "ghcr.io/kavachlabs/bench-axum-1934:grader-145a3830e64f",
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

### GET /api/trials/{job}/{trial}/events

A piece of the attempt's OpenCode event log, the JSON lines OpenCode wrote,
for the web UI's Events tab. It reads `agent/opencode.txt` once Harbor has
it, else the live copy `agent/opencode-live.txt` the agent mirrors while it
runs.

```json
{"source": "agent/opencode-live.txt", "from": 0, "next": 18342, "size": 18342,
 "lines": ["{\"type\":\"step_start\",...}", "..."], "skipped": false}
```

- `?from=N` reads from byte `N` of `source`; without it, or past the end, the
  response is the log's last 256 KiB, starting at a line, with `skipped`
  true when that is not the whole log.
- At most 256 KiB per call. A line still being written is left out until it
  ends, so `next` is always at a line start; pass it as the next `from`.
  `next < size` means there is more to read now.
- `source` is `""` and `lines` `[]` when there is no log yet. When `source`
  changes (the agent ended and Harbor's copy arrived), start again without
  `from`.
- In trial mode, as everywhere, the response is redacted of keys and the
  server's paths.

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
image is built from) and the agent's patch on top. `names`, optional, are
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
- `source`: where the real source files come from: `artifacts` (tried first:
  the whole files the task's second collect hook copied to
  `/logs/artifacts/source/<repo-relative path>`, so under the trial's
  `artifacts/logs/artifacts/source/`; every regular file changed since the
  commit the arm's `patch.diff` starts from, which in the Kavach arms is the
  parent of the integration commit, so its files are included; not deleted,
  not binary, not a symlink, up to 1 MB each; read in place, refusing
  symlinks, with no Docker and no cache, so it works for a trial that ran on
  another machine; trials that ran before the hook have none. If some of the
  snippets' files are missing from it and Docker is configured, the
  container/image path is tried; without Docker, or if that fails, the partial
  set is used and the other snippets keep to the patches), `container` (the trial's
  own container, `<trial>__env-main-1`, while it runs), `image` (a throwaway
  container, with no network, from the task's prebuilt image (`docker_image` in its
  `task.toml`) with the
  trial's `patch.diff` applied on `/work`) or `patch` (neither; the snippets
  are only the patches' lines). `source_note` says why it is `patch`, or warns
  that the image was built after the trial started and its files may differ.
  When the image is missing the note has the `image-tag.py build` command (or
  `docker pull`) that gets it. The first request for a trial does the work, one at a time per
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
  `incident.kavach` beside the Dockerfile the task's verifier image is built from; for a
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
| `environment` | where the attempts run: `docker` (local, the default), `daytona`, `modal` or `vercel`. A cloud provider is written to the job config as `environment.type` and is refused with 400, naming the missing variable, unless its keys are present (see `environments` in `/api/status`). `modal` also sets `environment.kwargs.modal_vm_runtime: true`: Harbor's default Modal runtime puts every Compose service on the host network, which breaks tasks with networks of their own, so the VM runtime (an alpha Modal feature) is used. `docker` adds nothing to the config |
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

### POST /api/jobs/{id}/sync

Queues a copy of the job to R2 now, after any already running. It is for a
run whose files changed outside the server, or whose last copy `failed`.

Reply: 202 and the Job. 404 for an unknown job. 409 when R2 sync is off, or
the job's Harbor is running or paused (it is copied once it has finished).

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
