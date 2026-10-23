# OpenCode for Harbor, watchable and without the key

`KavachOpenCode` is a Harbor agent: Harbor's OpenCode agent (same install,
same `opencode.txt` event stream, same ATIF trajectory and token and cost
accounting) with three changes:

- the API key stays out of the task container, which can reach nothing but
  the model, on local Docker and on remote Compose providers (Daytona, Modal,
  Vercel) alike;
- the session is copied to the trial directory every 15 seconds, so a run on
  a remote sandbox can be followed from the Mac, and reopened in OpenCode;
- on local Docker the attempt can also be watched live in OpenCode's TUI or
  web UI, read-only.

## Running

From the repository root (the import path is relative to it):

```sh
export PYTHONPATH=$PWD
harbor run -p <task-dir> -o "$PWD/bench/agent/harbor/jobs" \
  --agent bench.agent.harbor.agents:KavachOpenCode \
  -m openrouter/z-ai/glm-4.6
```

`--agent-import-path` also works but is deprecated in Harbor 0.24. The jobs
directory must be under `$HOME` with Colima.

The model is chosen as with Harbor's OpenCode agent: `-m provider/model`,
with the key in the provider's usual variable (`OPENROUTER_API_KEY` for
OpenRouter, read from the host environment or `--env-file`) and the endpoint
overridable with the provider's base-URL variable (`OPENROUTER_BASE_URL`).
OpenCode keeps its own provider for these, so it prices the run from
models.dev (filled during setup) and the trajectory has a cost.

The proxy forwards one call, `POST /v1/chat/completions` (`MODEL_ALLOWED` in
`proxy.py`), and answers every other method and path with 403, so the key
cannot reach the provider's account endpoints (OpenRouter's `/keys`,
`/credits`, `/auth`). The check is made on the target after `clean_target`,
so a query string is fine and a path trick is a 400 or a 403. OpenCode 1.1.8
makes only that call with the `openrouter` and openai-compatible providers
(the proxy log of the stub run shows only it); a provider with another API
(Anthropic's `/v1/messages`, OpenAI's `/v1/responses`) needs its entry added.

Any OpenAI-compatible endpoint (NIM, vLLM, Z.ai):

```sh
  -m endpoint/<model name at the endpoint> \
  --ak base_url=https://integrate.api.nvidia.com/v1 --ak api_key_env=NVIDIA_API_KEY
```

OpenCode has no prices for such a model; add them with
`--ak opencode_config='{"provider":{"kavach":{"models":{"<model>":{"cost":{"input":0.6,"output":2.2}}}}}}'`
(USD per million tokens) if the cost matters.

Options (`harbor agent schema` does not list import-path agents; see
`KavachOpenCodeOptions` in `opencode_live.py`): `version` (default 1.1.8;
installing is skipped when the image already has it), `base_url`,
`api_key_env`, `isolate` (default true), `require_isolation` (default true),
`live` (default true), `stall_sec` (default 600: with `live`, an attempt
whose event log gains nothing for that long ends with `AgentStalledError`,
which Harbor's `retries` run again; 0 never. Where the log is copied out of
the sandbox, the sandbox's own log is measured first, so a copy that stopped
never ends an attempt that is still working), plus
Harbor's OpenCode options (`opencode_config`, `disable_web_search`, ...).
MCP servers declared by the task (the kavach arm's `kavach mcp`) are
configured as Harbor's agent does.

### What a task must declare

The isolation is in the task's `environment/docker-compose.yaml`, which
`sync-tasks.py` writes (see Isolation below):

```yaml
services:
  main:
    # ...
    networks: [sealed]
    cap_drop: [NET_RAW]
  proxy:
    image: python:3.12-alpine
    init: true
    command: ["tail", "-f", "/dev/null"]
    volumes:
      - ./proxy.py:/proxy.py:ro
    sysctls:
      net.ipv4.ip_forward: "0"
      net.ipv6.conf.all.forwarding: "0"
    networks: [sealed, upstream]
networks:
  sealed:
    internal: true
    driver_opts:
      com.docker.network.bridge.inhibit_ipv4: "true"
  upstream: {}
```

and `environment/proxy.py`, a copy of `agents/proxy.py` (Compose binds it from
there; a remote provider uploads the environment directory, not `agents/`).
The task must not set `[agent] network_mode = "no-network"`. The task image
must ship OpenCode (the container has no network to install it), `bash` and
`node`; the task images do.

`inhibit_ipv4` leaves the host's side of the bridge without an address. An
`internal` network has no route out, but the bridge's gateway address is the
Docker host itself, and on Daytona, Modal and Vercel that is the sandbox VM,
which has full egress; without the option `main` can connect to it. The option
is honoured by Docker 29.5.2 (checked here); the DinD sandboxes install 28.3.3
(Daytona, Modal: `dind_image` default in `daytona/environment.py` 694,
`modal.py` 726) or the latest docker-ce (Vercel, `vercel/snapshots.py` 94), and
which release introduced it was not checked, so the agent tests it:

`cap_drop: [NET_RAW]` on `main` and the proxy's forwarding sysctls off close a
last way past the proxy: a root `main` with raw sockets could otherwise address
Ethernet frames to the proxy's MAC with the host as destination IP, and a
proxy that routes would pass them on (send-only, but a channel). No Harbor
overlay sets capabilities, `privileged` or sysctls on a task's services (the
only `cap_add` is the egress sidecar's own), and the namespaced `net.*`
sysctls need no privilege, so they apply on the DinD providers as on local
Docker; `docker compose config` merges them with Harbor's build overlay. The
tests check that `main` cannot open an `AF_PACKET` socket and that the proxy's
forwarding is off; they do not send a frame, since without `NET_RAW` none can
be sent.

Before the run the agent tries, from the task container, to connect to
`1.1.1.1:443` and to ports 22, 80, 443, 2375, 2376 and 8080 of the default
gateway and of the first address of each connected subnet (read from
`/proc/net/route`, leaving out the container's own address and the proxy's), and to
resolve `example.com`. If any connects or resolves, the task is not sealed and the agent
refuses to run (`require_isolation`); with `--ak require_isolation=false` it
runs with open egress, still without the key.

On Modal the default (non-VM) runtime adds `docker-compose-host-network.yaml`,
`network_mode: host` on every service (`modal.py` 594-595, 801-812), which
cancels all of this: `main` would share the host's network. Run with
`--ek modal_vm_runtime=true` (`modal.py` 985-986); the agent fails at the
start of `run()` on Modal without it.

### Testing without a model

`testdata/stub_model.py` is a scripted OpenAI-compatible endpoint: one bash
call (`ls -la && echo hello > hello.txt`), then a final message, streamed,
with token usage. `testdata/hello` is a task it passes.

```sh
python3 bench/agent/harbor/agents/testdata/stub_model.py --port 8399 --delay 60 --log /tmp/stub.jsonl &
STUB_KEY=sk-fake PYTHONPATH=$PWD harbor run -p bench/agent/harbor/agents/testdata/hello \
  -o "$PWD/bench/agent/harbor/jobs" --agent bench.agent.harbor.agents:KavachOpenCode \
  -m stub/scripted --ak base_url=http://host.docker.internal:8399/v1 --ak api_key_env=STUB_KEY
```

(`hello` gives its proxy service `host.docker.internal`; the benchmark tasks
do not.) `python3 bench/agent/harbor/agents/test_agents.py` checks the same
topology without Harbor or OpenCode: a cut-down copy of the Compose file with a
fake upstream. The main container gets the fake's answer through the proxy
with the key added, cannot connect to `1.1.1.1` or resolve a host name, and
has no key in its environment. It also checks that every task is in sync with
`sync-tasks.py`.

`--delay` holds the tool call so there is time to look at the mirrored
session. The stub logs the
`Authorization` header of each request: it carries the key from `STUB_KEY`,
which the task container never had. To exercise OpenCode's OpenRouter
provider instead, run with `-m openrouter/z-ai/glm-4.6`,
`OPENROUTER_API_KEY=sk-fake` and
`OPENROUTER_BASE_URL=http://host.docker.internal:8399/v1`.

## Watching

While `opencode run` works, the agent exports the session
(`opencode export`, run in the task container) every 15 seconds and replaces
`<trial-dir>/agent/opencode-session.json` atomically (a temporary file, then a
rename), so the file is always whole. `show.sh <trial-dir>` opens whatever
is there, mid-run too. This works wherever the trial runs: the export travels
through `environment.exec`, not a bind mount. `opencode-live.json`, which the
console reads, appears once the session exists:

```json
{"url": "", "session": "ses_...", "started": "2026-10-11T09:08:24Z", "dir": "/work"}
```

and gains `"finished": "<RFC3339>"` when the run ends.

On a sandbox that does not mount the logs directory (Daytona, Modal),
Harbor downloads `opencode.txt` only when the agent ends. Until then the
agent keeps a live copy of it in `<trial-dir>/agent/opencode-live.txt`, and
the console reads its steps from there (else from the session export):

- On Daytona and Modal it follows the log as it is written: `tail -F` on the
  sandbox's own machine, which holds the log at the same path (Harbor's DinD
  strategies bind `/logs` to itself there), streamed back through the
  provider's SDK (a Daytona session command's log websocket; Modal's
  `sandbox.exec` stdout). A follow ends after 2 minutes and starts again, and
  one whose stream goes quiet without ending (Daytona's does, some minutes
  in) is given up 30 seconds after that.
- Before each follow, and on any other provider, or when the log is not on
  the sandbox's machine, it polls: the log's new bytes every 3 seconds,
  through `environment.exec` in base64 chunks of 256 KiB.

On local Docker the log is in the trial directory already, and nothing is
copied.

### A live view on local Docker

For OpenCode's own TUI or web UI on a local Docker run, publish the proxy's
read-only view with the overlay and say which host port:

```sh
export KAVACH_VIEW_PORT=48081
harbor run ... --extra-docker-compose bench/agent/harbor/agents/docker-compose-view.yaml
```

`url` in `opencode-live.json` is then `http://127.0.0.1:48081`, on the host's
loopback only (it is written only when the overlay is among the run's extra
Compose files; otherwise `url` is `""`), and

```sh
bench/agent/harbor/attach.sh <trial-dir>         # opencode attach <url> --session <session>
bench/agent/harbor/attach.sh <trial-dir> --web   # prints the URL for a browser
```

work as before. Do not pass `--dir` to `opencode attach`: it changes to that
directory on the host, and with a directory that exists only in the container
the TUI starts and never draws. One port means one attempt at a time. Do not
use the overlay on Daytona, Modal or Vercel: it would publish inside the
sandbox VM, which nobody can reach. Without it the proxy has no view listener.

## Reopening

The last export, after the run, leaves the session in
`<trial-dir>/agent/opencode-session.json`.

```sh
bench/agent/harbor/show.sh <trial-dir>
```

imports it into a separate OpenCode data and config directory
(`KAVACH_SHOW_DATA`, default `~/.local/share/kavach-bench/opencode`) and
opens it in the TUI. A separate config is needed: plugins in a personal
OpenCode config made `opencode import` fail. The session is filed under
OpenCode's "global" project so it opens outside the task's repository.

Other files in `agent/`: `opencode.txt` (the JSON events, as with Harbor's
agent), `trajectory.json`, `opencode-serve.log`, `proxy.log` (every request
through the proxy: model calls, view requests, refusals) and `opencode/`
(OpenCode's data directory).

## How it works

`run()` (in `opencode_live.py`), on the host, using only Harbor's environment
API (`exec`, `service_exec`, `upload_file`); it does not call `docker`:

1. Fails on Modal without the VM runtime. Checks that the task container cannot connect to `1.1.1.1` or the Docker host (see above).
2. Starts `proxy.py` in the `proxy` service with `service_exec`. The upstream
   and the key are known only now (they follow from `-m` and the host
   environment), so they are not in the Compose file: the key is in that
   exec's environment, so only the proxy's process has it. Waits until the
   task container can connect to `proxy:8080`.
3. Writes `~/.config/opencode/opencode.json` in the container: the provider's
   `baseURL` is `http://proxy:8080/v1`, its `apiKey` a placeholder;
   permissions as in the pre-Harbor harness (nothing asks, web fetch denied);
   `small_model` is the model under test; autoupdate and sharing off.
4. Starts `opencode serve --hostname 0.0.0.0 --port 4096` and submits the
   task with `opencode run --attach http://127.0.0.1:4096 --format json`, so
   `opencode.txt` and Harbor's trajectory conversion are unchanged.
5. Finds the session (`GET /session` from inside the container), writes
   `opencode-live.json`, and mirrors the session every 15 seconds (an export that is still running holds back the next).
   Without a bind mount, also keeps `opencode-live.txt`, a live copy of the
   event log: followed through the provider's streaming exec on Daytona and
   Modal, polled every 3 seconds elsewhere.
6. Afterwards, also on failure or timeout: marks the file finished, stops the
   server, exports the session once more and saves the proxy's log
   (`proxy.log`). Harbor's `compose down` removes the proxy.

During setup the agent also downloads `https://models.dev/api.json` on the
host and uploads it as OpenCode's `~/.cache/opencode/models.json`, because the
sealed container cannot fetch it and the cache gives the trajectory its cost.

### Isolation

Harbor's `[agent] network_mode = "no-network"` is not used, because it is not
the same thing on every provider (Harbor 0.24.0, `harbor/environments`):

- Docker: it turns on an egress-control sidecar and moves `main` into the
  sidecar's network namespace (`docker/docker.py` 480-515), unless the task
  gives the service its own `networks`, which is respected (442-478). An
  agent phase that differs from the environment's baseline needs
  `capabilities.dynamic_network_policy` (`trial/trial.py` 283-312).
- Daytona and Modal run Compose tasks inside a Docker-in-Docker sandbox and
  report no dynamic network policy and no allowlists in Compose mode
  (`daytona/environment.py` 1075-1082, `modal.py` 1007-1014), so a task that
  sets `no-network` for the agent phase alone is rejected when the trial
  starts. A `no-network` baseline instead appends
  `docker-compose-no-network.yaml` (`main: network_mode: none`) after the
  task's file (`daytona/environment.py` 552, `modal.py` 589,
  `vercel/compose.py` ~409), which cuts `main` off from the proxy as well and
  cannot be combined with `networks:`.
- Harbor 0.24's Daytona strategy runs `docker compose up -d` with a 120 s limit
  (`daytona/environment.py` 773), and the image pull happens inside it, so a
  large prebuilt image dies mid-extract. `opencode_live.pull_before_up` runs
  `docker compose pull --ignore-buildable` first under `build_timeout_sec`; it
  applies only on Harbor 0.24.x with `_DaytonaDinD._compose_exec` as it is
  there. Modal's strategy already gives `up` that limit.

So the task's Compose file does the sealing, with the same Compose features
everywhere: `main` is only on `sealed`, an `internal: true` network with no
IPv4 address on the host's side (no route out; Docker's DNS does not resolve outside
names on it), shared with
`proxy`, which is also on `upstream`, an ordinary network. `main` can reach the
proxy by name and nothing else. The proxy forwards only `POST /v1/chat/completions` to the one
upstream and, with `--model-client main`, only `main` may use that port. That
is what the egress sidecar gave on local Docker (reach the proxy only), and it
holds from the first second of the trial instead of only during `run()`.

Why the key does not go through Compose's `${VAR}`: a DinD provider runs
`docker compose` with only the task's `[environment].env` and the trial's
persistent env as interpolation variables, not the host's environment
(`daytona/environment.py` 523-535, `modal.py` 559, `vercel/compose.py`
385-395; only local Docker adds `os.environ`, `docker/docker.py` 669-683).
The same variables are injected into `main` (`docker/__init__.py` 20-25,
`environments/base.py` 314-321), so interpolation would put the key in the
container it must stay out of. `service_exec` on the proxy service
(`compose_service_ops.py` 91-116, `dind_compose.py` 261, `docker/docker.py`
1369) works the same on every provider.

How `proxy.py` reaches the proxy service: Daytona, Modal and Vercel upload
the task's `environment/` directory to the sandbox and run Compose with it as
the project directory (`daytona/environment.py` 739, `modal.py` 778,
`vercel/compose.py` 470-482), so `./proxy.py` is bound from there; a bind of
`agents/proxy.py` could not work, as that path is not in the sandbox. Each
task therefore holds a copy; `sync-tasks.py` writes them and `--check` fails
when one differs. A build from `environment/` would need the same file there
too, plus a Dockerfile for one script.

The proxy (`proxy.py`, standard library only) has two ports:

- 8080, the model: `/v1/<path>` goes to `<upstream>/<path>` with the key in
  place of the placeholder (in `Authorization`, or `x-api-key` if the client
  used that). Only `main` may connect. Requests are parsed strictly (CRLF line endings only; header names are tokens; no control characters in values; no folded lines; no repeated or combined `Content-Length`/`Transfer-Encoding`), so `main` cannot inject headers or a second request into what is sent upstream; a violation or an oversized head is a 400. Request targets with control characters, `.`/`..`
  or empty path segments, or `@` in the path get 400; the upstream request line is
  rebuilt from the validated parts.
- 8081, the view (only with `--opencode`, i.e. with `KAVACH_VIEW_PORT` set):
  forwards `GET`, `HEAD` and `OPTIONS` of OpenCode's API to the server;
  refuses every other method, websocket upgrades and `/pty` (the web UI's
  terminal) with 403. Other paths are the web UI, fetched from
  `app.opencode.ai` as `opencode serve` itself does. `main` is refused;
  connections published to the host arrive from the network's gateway.

Harbor internals this relies on (Harbor 0.24.0):
`harbor.agents.installed.opencode.OpenCode` (subclassed; `_parse_stdout`,
`populate_context_post_run`, `_build_register_skills_command`,
`_deep_merge`, `_OUTPUT_FILENAME`), `BaseInstalledAgent.install` /
`exec_as_agent` / `with_prompt_template`, `harbor.agents.model_connection`
(`PROVIDERS`, the resolved `api_key`) and `BaseEnvironment.service_exec`.

## Security properties

Checked with `test_agents.py` (a cut-down topology) and by running the stub
end to end:

- The key is not in the task container's environment, in OpenCode's config,
  in any process's environment, or in the server's `GET /config` (which shows
  the placeholder). It exists only in the proxy service's process and on the
  host.
- From the task container, `1.1.1.1` and the Docker host (the bridge gateway) are unreachable and host names do not
  resolve; only `proxy` is. The proxy forwards only `/v1/` to the one
  upstream.
- From the host, prompts (`POST /session/<id>/message`, `prompt_async`),
  `abort`, `shell`, new sessions, config changes and terminals are refused
  with 403 at the view.

What remains:

- **The model endpoint through the proxy.** The agent can make its own
  requests to the model with the key (not read it). It could use the
  model's output as a channel, or spend money; the run's timeout bounds it.
- **The key in the exec's command line.** `service_exec` passes it as
  `docker compose exec -e KAVACH_UPSTREAM_KEY=...`, visible in the process list
  of the host (local Docker) or of the sandbox VM (remote) for an instant;
  the task container cannot see either.
- **The web UI's assets come from app.opencode.ai**, via the proxy, as with
  `opencode serve`; the UI may be newer than the 1.1.8 server. The UI's
  HTML loads through the view; it has not been exercised in a browser.
- **Watchers are not authenticated.** Anything on the host that can reach
  127.0.0.1 can read the session while the local view is published (it is
  read-only). The view forwards `GET /config` and `GET /provider`, which show
  the placeholder key, not the real one.
- **The read-only filter is method-based.** It relies on OpenCode 1.1.8's
  API changing state only through non-GET methods; every GET route in
  `GET /doc` was checked except `/tui/control/next`, which pops a TUI
  control request (nothing uses it in a headless run). A later OpenCode
  version should be rechecked.
- **Not run on a remote provider.** Remote runs pull the task's prebuilt
  images (`docker_image` in `task.toml`; bench/agent/harbor/README.md,
  "Images"), which must be public, and additionally need `python:3.12-alpine`
  pulled in the sandbox.
