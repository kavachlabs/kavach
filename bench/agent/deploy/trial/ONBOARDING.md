# Running the trial: partners, tasks, new releases

Everything here runs from your laptop through `kavach-trial` (in this
directory; put it on your `PATH`). It runs the `bench trial` commands on the
VM over `ssh root@$KAVACH_TRIAL_HOST` (default 178.105.177.23), so you need
ssh as root there. The service reads `/etc/kavach/trial.json` and picks up
every edit within a request, with no restart, so **no visitor session is lost**.
Only `deploy` restarts the service.

Keys never go in `trial.json`: they stay in `/etc/kavach/trial.env`
(`trial.env.example`), which the service reads at start. Settings missing from
`trial.json` fall back to the `TRIAL_*` variables there; see "Trial mode" in
[API.md](../../console/API.md#trial-mode).

## Add a partner

One invite code per partner, each with a run budget of its own. Every run
bills the keys in `trial.env`.

```sh
kavach-trial invite add --label "Garry (YC)" --runs 5
```

It prints the code (`yc-xxxx-xxxx-xxxx-xxxx`, or your own with `--code`) and
the link to send, `https://try.kavachlabs.dev/#/trial/invite?code=...`, which
opens the invite form with the code filled in. The partner still presses the
button. `--note "intro from X"` keeps a reminder with the code.

```sh
kavach-trial invite list              # label, code, runs used/budget, state, last used
kavach-trial invite show garry        # that code's runs, by job id (a label works for a code)
kavach-trial invite revoke "Garry (YC)"   # by label or code
```

Revoking works at once, also for a visitor who redeemed the code earlier:
their next run is refused with "this invite code has been revoked". Runs
already going are not stopped. Change a budget or restore a revoked code by
editing `/etc/kavach/trial.json` on the VM (`runs`, `revoked`); a file that
fails its checks is logged and ignored, and the last good one stays in force.

Counts live in `/var/lib/kavach/jobs/.bench/invites-used.json` and survive
restarts. Codes in `TRIAL_INVITE_CODES` still work (with `TRIAL_INVITE_RUNS`
each) and are not listed; to revoke one, add the same code to the file with
`"revoked": true`.

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

   A base name adds its control and kavach arms (a folder name such as
   `redis-10068-kavach` adds just that one). It prints the arms found, adds
   them, and warns when an image in the `task.toml` is not published (checked
   with `docker manifest inspect` where the command runs: the VM; no docker
   there, no check). The first edit starts from `TRIAL_TASKS` in `trial.env`,
   so existing tasks stay. `task remove` and `task list` do the rest; removing
   the last task is refused, because an empty list means every task.

Models work the same way: `kavach-trial model add openrouter/z-ai/glm-5.3`.
A model's provider must be one with a key variable (`openrouter`,
`anthropic`, `openai`, `nvidia`, `zai`), and invite runs need that variable
in `trial.env`.

## Ship a console change

```sh
kavach-trial deploy [--force] [ref]    # ref defaults to main; push it first
```

It builds `bench` for linux/amd64 (`CGO_ENABLED=0`) from the ref's files,
fetches and checks out the same commit in `/opt/kavach` on the VM, copies the
binary as `bench.new` and renames it over `bench` (never in place: each run's
log helper executes it), restarts `bench-trial`, waits for
`/api/trial/session` on the VM, and checks the public URL.

It refuses while trial runs are live, because a restart forgets every visitor
session (the runs themselves keep going, but their owners can no longer stop
them); `--force` overrides. `kavach-trial status` shows the service, the
checkout, live runs and invite usage.
