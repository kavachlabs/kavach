# OpenCode for Harbor, watchable and without the key

`KavachOpenCode` is a Harbor agent: Harbor's OpenCode agent (same install,
same `opencode.txt` event stream, same ATIF trajectory and token and cost
accounting) with three changes:

- the API key stays out of the task container, and during the run the
  container can reach nothing but the model;
- the attempt can be watched live in OpenCode's TUI or web UI, read-only;
- the finished session is exported and can be reopened in OpenCode.

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
`live` (default true), `proxy_image` (default `python:3.12-alpine`), plus
Harbor's OpenCode options (`opencode_config`, `disable_web_search`, ...).
MCP servers declared by the task (the kavach arm's `kavach mcp`) are
configured as Harbor's agent does.

### What a task must declare

```toml
[agent]
network_mode = "no-network"
```

This turns on Harbor's egress control for the agent phase; the agent then
opens exactly one address, its proxy. Setup (installing OpenCode, filling the
models.dev cache) keeps the environment's network, and the verifier is
unaffected. Without it the agent refuses to run (`require_isolation`); with
`--ak require_isolation=false` it runs with open egress, still without the
key.

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

`--delay` holds the tool call so there is time to attach. The stub logs the
`Authorization` header of each request: it carries the key from `STUB_KEY`,
which the task container never had. To exercise OpenCode's OpenRouter
provider instead, run with `-m openrouter/z-ai/glm-4.6`,
`OPENROUTER_API_KEY=sk-fake` and
`OPENROUTER_BASE_URL=http://host.docker.internal:8399/v1`.

## Watching

While an attempt runs, `<trial-dir>/agent/opencode-live.json` holds:

```json
{"url": "http://127.0.0.1:32774", "session": "ses_...", "started": "2026-10-11T09:08:24Z", "dir": "/work"}
```

`url` is the read-only view of the OpenCode server in the task container,
on the host's loopback only; `session` is the attempt's session; `dir` its
working directory in the container. The file appears once the session exists
(about when the first model call starts) and, when the run ends, gains
`"finished": "<RFC3339>"`; the URL then stops answering.

```sh
bench/agent/harbor/attach.sh <trial-dir>         # opencode attach <url> --session <session>
bench/agent/harbor/attach.sh <trial-dir> --web   # prints the URL for a browser
```

Do not pass `--dir` to `opencode attach`: it changes to that directory on
the host, and with a directory that exists only in the container the TUI
starts and never draws.

## Reopening

After the run the session is exported (`opencode export`) to
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

`run()` (in `opencode_live.py`), on the host:

1. Starts the proxy container (`proxy.py` in `python:3.12-alpine`, labelled
   `kavach.bench=proxy`) on the trial's compose network with `docker run`.
   The key is passed in its environment. It publishes the view port on
   `127.0.0.1` with a random host port.
2. Narrows the agent-phase network policy to the proxy's address
   (`environment.set_network_policy`, an allowlist of one IP), restored when
   the run ends.
3. Writes `~/.config/opencode/opencode.json` in the container: the provider's
   `baseURL` is the proxy, its `apiKey` a placeholder; permissions as in the
   pre-Harbor harness (nothing asks, web fetch denied); `small_model` is the
   model under test; autoupdate and sharing off.
4. Starts `opencode serve --hostname 0.0.0.0 --port 4096` and submits the
   task with `opencode run --attach http://127.0.0.1:4096 --format json`, so
   `opencode.txt` and Harbor's trajectory conversion are unchanged.
5. Polls the view's `GET /session` for the session and writes
   `opencode-live.json`.
6. Afterwards, also on failure or timeout: marks the file finished, stops the
   server, exports the session, saves the proxy's log and removes it (a
   container left on the network would stop Harbor's `compose down`), and
   restores the network policy.

The proxy (`proxy.py`, standard library only) has two ports:

- 8080, the model: `/v1/<path>` goes to `<upstream>/<path>` with the key in
  place of the placeholder (in `Authorization`, or `x-api-key` if the client
  used that). Only the task container's address may connect.
- 8081, the view: forwards `GET`, `HEAD` and `OPTIONS` of OpenCode's API to
  the server; refuses every other method, websocket upgrades and `/pty`
  (the web UI's terminal) with 403. Other paths are the web UI, fetched from
  `app.opencode.ai` as `opencode serve` itself does. Only the network
  gateway, where published-port connections arrive, may connect; the task
  container is refused.

Harbor internals this relies on (Harbor 0.24.0):
`harbor.agents.installed.opencode.OpenCode` (subclassed; `_parse_stdout`,
`populate_context_post_run`, `_build_register_skills_command`,
`_deep_merge`, `_OUTPUT_FILENAME`), `BaseInstalledAgent.install` /
`exec_as_agent` / `with_prompt_template`, `harbor.agents.model_connection`
(`PROVIDERS`, the resolved `api_key`), the docker environment's compose
project naming (`_sanitize_docker_compose_project_name(session_id)`, a private
function) and labels `com.docker.compose.project` / `.service`, its egress
control (`harbor-docker-egress-control-sidecar`, whose network namespace the
task container shares; `capabilities.dynamic_network_policy`;
`set_network_policy`), and the agent log directory being bind-mounted, so
files written on the host appear in the trial directory at once.

## Security properties

Checked from inside the task container during a run (the evidence is in the
commit that added this):

- The key is not in the container's environment, in OpenCode's config, in
  any process's environment, or in the server's `GET /config` (which shows
  the placeholder). It exists only in the proxy container and on the host.
- `https://github.com`, `http://example.com`, `https://openrouter.ai` and the
  model endpoint itself are unreachable; only the proxy's address is. A
  forged `Host` header or a connection to another address with the proxy's
  name as TLS SNI is refused too, since the allowlist entry is an IP.
- The proxy forwards only `/v1/` to the one upstream, and refuses the task
  container on the view port.
- From the host, prompts (`POST /session/<id>/message`, `prompt_async`),
  `abort`, `shell`, new sessions, config changes and terminals are refused
  with 403 at the view.

What remains:

- **DNS.** Harbor's egress control lets DNS through to Docker's resolver,
  so the agent can resolve any name, and could leak data in DNS queries. It
  cannot connect to what it resolves.
- **The model endpoint through the proxy.** The agent can make its own
  requests to the model with the key (not read it). It could use the
  model's output as a channel, or spend money; the run's timeout bounds it.
- **15-second response-header limit.** Traffic from the task container to
  the proxy passes Harbor's egress proxy (gost), which drops an HTTP
  exchange whose response headers take more than 15 s. OpenCode streams and
  real endpoints send headers at once (verified with a stub that waits 40 s
  after the first chunk), so runs are unaffected; a non-streaming call to a
  slow endpoint would fail.
- **The web UI's assets come from app.opencode.ai**, via the proxy, as with
  `opencode serve`; the UI may be newer than the 1.1.8 server. The UI's
  HTML loads through the view; it has not been exercised in a browser.
- **Watchers are not authenticated.** Anything on the host that can reach
  127.0.0.1 can read the session while it runs (it is read-only). The view
  forwards `GET /config` and `GET /provider`, which show the placeholder
  key, not the real one.
- **The read-only filter is method-based.** It relies on OpenCode 1.1.8's
  API changing state only through non-GET methods; every GET route in
  `GET /doc` was checked except `/tui/control/next`, which pops a TUI
  control request (nothing uses it in a headless run). A later OpenCode
  version should be rechecked.
- The agent runs `docker` on the host (the proxy container, `docker port`,
  `docker logs`), outside Harbor's environment abstraction; it works only
  with Harbor's local docker environment.
