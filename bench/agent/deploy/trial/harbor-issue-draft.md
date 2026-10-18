<!--
Draft of an upstream issue for Harbor (https://github.com/laude-institute/harbor).
Not filed. Checked against harbor 0.24.0 from PyPI and against main on
2026-10-18, which still has the 120 s limit. Our workaround is pull_before_up
in bench/agent/harbor/agents/opencode_live.py.
-->

# Daytona (DinD): `docker compose up -d` has a fixed 120 s timeout that includes the image pull

## Summary

In the Daytona environment's Docker-in-Docker strategy (`_DaytonaDinD`),
`start()` runs `docker compose up -d` with a hard-coded `timeout_sec=120`.
For a task with a prebuilt image (`docker_image` in `task.toml`), nothing
pulls that image before `up`: `docker compose build` skips services that
have only `image:`. So the pull runs inside `up` and has to finish in 120
seconds. A large image does not: in our case, a 520 MB layer was still
extracting when the limit hit, and the trial failed with
`RuntimeError: docker compose up failed: ...`. The task's own
`build_timeout_sec` (default 600) does not apply.

## Where

harbor 0.24.0, `src/harbor/environments/daytona/environment.py`:

- line 763-765: `docker compose build` runs under
  `timeout_sec=round(env.task_env_config.build_timeout_sec)`.
- line 773: `result = await self._compose_exec(["up", "-d"], timeout_sec=120)`.
- line 644: `_DaytonaDinD._compose_exec(subcommand, timeout_sec)`, which hands
  the limit to `_vm_exec`.

Other environments already handle this:

- `src/harbor/environments/modal.py`, line 825-828: `up -d` runs under
  `build_timeout_sec`.
- `src/harbor/environments/prime.py`, line 246-258: runs `build`, then
  `pull --ignore-buildable` under `build_timeout_sec`, then
  `up --detach --wait --no-build --pull never`.

`src/harbor/environments/blaxel.py` line 1208 has the same fixed
`timeout_sec=120` and probably the same problem; we have not tried it.

## To reproduce

1. Make a task in compose mode (an `environment/docker-compose.yaml`, which
   puts Daytona in the DinD strategy) whose service uses an image of about
   1 GB or more (compressed) that the sandbox has not cached, either as
   `image:` in the compose file or as `docker_image` in `task.toml`.
2. `harbor run` it with `--env daytona`.
3. `docker compose up -d` is cut off at 120 s while the image is still
   downloading or extracting, and the trial fails before the agent starts.
   The same task passes on Modal and on local Docker.

## Suggested fix

Pull before `up`, under the build limit, the way `prime.py` does:

```python
result = await self._compose_exec(
    ["pull", "--ignore-buildable"],
    timeout_sec=round(env.task_env_config.build_timeout_sec),
)
# then, as now, or with --pull never:
result = await self._compose_exec(["up", "-d"], timeout_sec=120)
```

or give `up -d` `build_timeout_sec`, as `modal.py` does. Pulling first
keeps a short limit on `up` itself, so a container that hangs at start
still fails fast.

## Workaround

We wrap `_DaytonaDinD._compose_exec` so that before `["up", "-d"]` with
timeout 120 it runs `["pull", "--ignore-buildable"]` with
`build_timeout_sec` (a failed pull is left to `up` to report). It is
guarded on harbor 0.24.x and on the method's signature.
