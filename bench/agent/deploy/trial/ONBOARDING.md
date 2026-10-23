# Running the trial: partners, tasks, new releases

Everything here runs from your laptop through `kavach-trial` (in this
directory; put it on your `PATH`). It runs the `bench trial` commands on the
VM over `ssh root@$KAVACH_TRIAL_HOST` (default 178.105.177.23), so you need
ssh as root there. The service reads `/etc/kavach/trial.json` and picks up
every edit within a request, with no restart, so **no visitor session is lost**.
Only `deploy` restarts the service.

Keys never go in `trial.json`: they stay in `/etc/kavach/trial.env`
(`trial.env.example`), which the service reads at start. `trial.json` does hold
the invite codes, which are as good as the keys, so it is `root:kavach`, mode
0640; keep it that way. A file created before this (or by hand) needs the
fix once: `chgrp kavach /etc/kavach/trial.json && chmod 0640 /etc/kavach/trial.json`. Settings missing from
`trial.json` fall back to the `TRIAL_*` variables there; see "Trial mode" in
[API.md](../../console/API.md#trial-mode).

## Add a partner

One invite code per partner, each with a run budget of its own. Every run
bills the keys in `trial.env`.

```sh
kavach-trial invite add --label "Garry (YC)" --runs 5
```

It prints the code (`kv-xxxx-xxxx-xxxx-xxxx`, or your own with `--code`) and
the link to send, `https://try.kavachlabs.dev/#/trial/invite?code=...`, which
opens the invite form with the code filled in. The partner still presses the
button. `--note "intro from X"` keeps a reminder with the code.

```sh
kavach-trial invite list              # label, code, runs used/budget, state, last used
kavach-trial invite show garry        # that code's runs, by job id (a code, a label, or a part of one label)
kavach-trial invite revoke "Garry (YC)"   # by the full label or the code
```

Revoking works at once, also for a visitor who redeemed the code earlier:
their next run is refused with "this invite code has been revoked". Runs
already going are not stopped. Change a budget or restore a revoked code by
editing `/etc/kavach/trial.json` on the VM (`runs`, `revoked`); a file that
fails its checks is logged and ignored, and the last good one stays in force.

Counts live in `/var/lib/kavach/jobs/.bench/invites-used.json` and survive
restarts. Codes in `TRIAL_INVITE_CODES` still work (with `TRIAL_INVITE_RUNS`
each) and are not listed; to revoke one, add the same code to the file with
`"revoked": true` and remove it from `trial.env` at the next chance: deleting
`trial.json` brings every env code back. If `trial.json` or `invites-used.json`
is corrupt, the service refuses to start (a running one keeps its last good
file) rather than run with weaker limits.

## Put a new task in the trial

A visitor can run a task only when all of this holds:

1. **The task exists** under `bench/agent/harbor/tasks/` with a `-control` and
   a `-kavach` arm (see `bench/agent/harbor/README.md`), merged to the ref the
   VM checks out. `task add` looks for it in the VM's `/opt/kavach` checkout.
2. **Its images are published.** Sandboxes at Daytona and Modal pull the
   prebuilt images named in each `task.toml`; they cannot build. After a
   change to a Dockerfile or the files it copies, run
   `bench/agent/harbor/sync-tasks.py` and commit; then, from Actions, run the
   `bench-images` workflow on main. A new image's package starts private:
   make it public once (harbor/README.md, "Making the packages public"),
   because sandboxes pull anonymously.
3. **A test run passes.** Locally, as in harbor/README.md "Running":
   `harbor run -p bench/agent/harbor/tasks/<task>-kavach -a oracle ...`
   gives reward 1 and `-a nop` gives 0. Then, after step 4, start a run on the
   trial itself with an invite of your own, and revoke it afterwards.
4. **It is on the allow-list:**

   ```sh
   kavach-trial deploy                  # the VM needs the task files (skip if already there)
   kavach-trial task add redis-10068
   ```

   A base name adds every arm the task has, which must include control and
   kavach: an incident's two, a feature's four (control, workflow, kavach,
   kavach-implement). A folder name such as `redis-10068-kavach` adds just
   that one, and `task remove` with a base name removes all its arms. It prints the arms found, adds
   them, and warns when an image in the `task.toml` is not published (checked
   with `docker manifest inspect` where the command runs: the VM; no docker
   there, no check). The first edit starts from `TRIAL_TASKS` in `trial.env`,
   so existing tasks stay. `task remove` and `task list` do the rest; removing
   the last task is refused, because an empty list means every task.

Models work the same way: `kavach-trial model add openrouter/z-ai/glm-5.3`.
A model's provider must be one with a key variable (`openrouter`,
`anthropic`, `openai`, `nvidia`, `zai`), and invite runs need that variable
in `trial.env`.

## Visitors with their own keys

A visitor who enters their own OpenRouter and Daytona or Modal keys gets the
full run form: any task, any OpenRouter model, attempts, attempts at once,
retries, timeout, and Pause, Resume and Retry on their own runs. They bill
their own accounts, but each run is still a Harbor process on this VM, so the
server caps them: 10 attempts per task and model, 4 at once, 2 Harbor retries
per erroring attempt, 40 attempts in a run counting retries (retries + 1 x
tasks x models x attempts), 5 retry requests per run, a 2-hour timeout
override. Invite sessions keep the allow-lists above and cannot pause or
resume; they can retry a failed or errored attempt of a run they started on
the invite, on the server's keys, which uses none of the invite's runs (the
5 retry requests per run apply). To change the caps, set `own_max_attempts`,
`own_max_concurrent`, `own_max_trials`, `own_max_retries` and
`own_max_retry_requests` in `/etc/kavach/trial.json` (taking
effect within a request), or `TRIAL_OWN_MAX_*` (`..._RETRY_REQUESTS` for the last) in `trial.env`. The one-live-run
limits and `TRIAL_MAX_RUNNING` apply to them as before; lower
`own_max_concurrent` first if the VM is short of memory.

## Ship a console change

```sh
kavach-trial deploy [--force] [ref]    # ref defaults to main; push it first
```

It builds `bench` for linux/amd64 (`CGO_ENABLED=0`) from the whole tree at the
ref (`--build-only` stops there), copies the binary to the VM as `bench.new`,
fetches and checks out the same commit in `/opt/kavach`, renames the binary
over `bench` (never in place: each run's log helper executes it), first
running `bench.new api -trial -check` on the VM against the real `trial.json`
and usage file (a failure leaves the VM untouched) and keeping the old binary
as `bench.prev`; if the restart or the health check then fails it puts back
`bench.prev` and the old checkout, restarts, and says what state the VM is in.
It restarts
`bench-trial`, waits for
`/api/trial/session` on the VM, and checks the public URL.

It refuses while trial runs are live, because a restart forgets every
visitor's keys (sessions, their invites and their runs survive it, but a
visitor on their own keys enters them again); `--force` overrides. `kavach-trial status` shows the service, the
checkout, live runs and invite usage.
