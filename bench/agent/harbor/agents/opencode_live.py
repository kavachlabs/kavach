"""OpenCode for Harbor, watchable live and without the API key.

Harbor's OpenCode agent runs `opencode run --format json` with the provider
key in the agent's environment. This one keeps Harbor's install, trajectory
and token accounting (the same JSON event stream lands in opencode.txt) and
changes how the run is wired:

- A proxy service (proxy.py), declared in the task's docker-compose.yaml,
  holds the API key and is the agent's only route to the model; OpenCode in
  the task container is configured with the proxy's address and a
  placeholder key. The task container is on a Compose network with
  `internal: true`, shared with the proxy, which alone is also on a normal
  one, so every Compose provider (Docker, Daytona, Modal, Vercel) seals the
  task container the same way. The agent starts proxy.py inside its service
  with the key in that exec's environment only.
- `opencode serve` runs in the task container and the task is submitted to
  it with `opencode run --attach`. While it runs, the session is exported
  every few seconds to opencode-session.json in the trial's agent log
  directory, so progress can be read from wherever the trial runs. With
  KAVACH_VIEW_PORT set and the docker-compose-view.yaml overlay (local
  Docker only), the proxy also publishes a read-only view of the server on
  127.0.0.1, recorded in opencode-live.json.
- After the run the session is exported once more, which show.sh opens.

See README.md.
"""

import asyncio
import base64
import contextlib
import copy
import ipaddress
import importlib.metadata
import inspect
import json
import logging
import os
import shlex
import socket
import struct
import urllib.parse
import urllib.request
from datetime import datetime, timezone
from pathlib import Path
from typing import Any, override

from pydantic import Field

from harbor.agents.installed.base import NonZeroAgentExitCodeError, with_prompt_template
from harbor.agents.installed.opencode import OpenCode, OpenCodeOptions
from harbor.agents.model_connection import PROVIDERS
from harbor.environments.base import BaseEnvironment
from harbor.models.agent.context import AgentContext
from harbor.models.environment_type import EnvironmentType

LIVE_FILE = "opencode-live.json"
EXPORT_FILE = "opencode-session.json"
# The live copy of OpenCode's event log, for sandboxes that do not mount the
# logs directory (Daytona, Modal): Harbor downloads opencode.txt only when the
# agent ends.
LIVE_LOG = "opencode-live.txt"
INSTRUCTION_FILE = "instruction.md"
MODELS_URL = "https://models.dev/api.json"
PINNED_VERSION = "1.1.8"
SERVER_PORT = 4096
MODEL_PORT = 8080
PROXY_SERVICE = "proxy"
# Pause between copies of the running session to the trial directory.
MIRROR_SEC = 15
# Pause between copies of the event log's new bytes, and the most one exec
# returns: base64 of it stays well under the ~1 MB remote execs return.
TAIL_SEC = 3
TAIL_CHUNK = 256 << 10
# Where the provider can stream a command's output (Daytona, Modal), the log
# is followed with `tail -F` instead, on the sandbox's own machine, which
# holds it at the same path (Harbor's DinD strategies self-bind the logs
# directory). A follow ends after this long and starts again, so one whose
# reader went away (Modal cannot stop a command) does not outlive it long.
# A provider's stream can also go quiet without ending (Daytona's does, a few
# minutes in), so a follow is given up FOLLOW_GRACE_SEC after that; the poll
# before the next one copies what it missed.
FOLLOW_SEC = 120
FOLLOW_GRACE_SEC = 30
NO_LOG_HERE = 3
# An attempt whose event log gains nothing for STALL_SEC is ended as stalled:
# OpenCode is waiting on a model call that is not coming back. A long tool
# call (a full build, a test suite) also writes nothing until it ends, so
# the limit is well above the longest of those. Checked every
# STALL_CHECK_SEC. Where the log is copied rather than mounted, a copy that
# stopped is not taken for a log that did: the sandbox's own log is measured
# before an attempt is ended.
STALL_SEC = 600
STALL_CHECK_SEC = 15
EXPORT_WARN_EVERY = 20
# Outside every private range, then the Docker host's usual services: none may
# accept a connection from a sealed task container.
OUTSIDE = "1.1.1.1"
SEALED_PORTS = (443, 2375, 2376, 22, 80, 8080)
# A name that exists on the internet; a sealed container must not resolve it.
OUTSIDE_NAME = "example.com"
VIEW_OVERLAY = "docker-compose-view.yaml"
# bash is in every task image; this is a TCP connect.
PROBE = "timeout 3 bash -c 'exec 3<>/dev/tcp/{host}/{port}'"
NVM = "[ -f ~/.nvm/nvm.sh ] && . ~/.nvm/nvm.sh; "
# The provider id OpenCode is given for an endpoint named with base_url.
ENDPOINT_PROVIDER = "kavach"
PLACEHOLDER_KEY = "kavach-proxy-adds-the-key"


class KavachOpenCodeOptions(OpenCodeOptions):
    version: str | None = Field(default=None, description=f"opencode-ai version to install (default {PINNED_VERSION}).")
    base_url: str | None = Field(
        default=None,
        description="OpenAI-compatible endpoint (NIM, vLLM, Z.ai, a local stub). "
        "-m is then <anything>/<model name at that endpoint>.",
    )
    api_key_env: str | None = Field(
        default=None,
        description="Host variable holding the endpoint's key. Default: the "
        "provider's usual variable, e.g. OPENROUTER_API_KEY.",
    )
    isolate: bool = Field(
        default=True,
        description="Check before the run that the task container cannot "
        "reach the internet. The task's docker-compose.yaml seals it, on an "
        "internal network shared with the proxy service.",
    )
    require_isolation: bool = Field(
        default=True,
        description="Fail when isolate is set and the task container can "
        "reach the internet, instead of running with open egress.",
    )
    live: bool = Field(default=True, description="Run opencode serve and mirror the session while it runs.")
    stall_sec: float = Field(
        default=STALL_SEC,
        description="With live, end the attempt with AgentStalledError, which "
        "Harbor's retries run again, when OpenCode's event log gains nothing "
        "for this many seconds; 0 never.",
    )


class AgentStalledError(RuntimeError):
    """OpenCode's event log stopped growing while it ran. Harbor's retry
    config does not exclude it, so a run with retries runs the attempt
    again."""


def check_modal_runtime(environment: BaseEnvironment) -> None:
    """Modal's default runtime puts every Compose service on the host's network
    (docker-compose-host-network.yaml), which defeats the sealed network.
    Fails closed: if Harbor stops exposing the flag, so does this."""
    if environment.type() != EnvironmentType.MODAL:
        return
    enabled = getattr(environment, "_vm_runtime_enabled", None)
    if enabled is None:
        raise RuntimeError(
            "cannot tell whether Modal's VM runtime is on: Harbor no longer has "
            "ModalEnvironment._vm_runtime_enabled; update check_modal_runtime"
        )
    if not enabled:
        raise RuntimeError(
            "on Modal the task container would share the host's network and reach the proxy's "
            "upstream directly; run with --ek modal_vm_runtime=true"
        )


def gateway_candidates(routes: str, exclude: set[str] = frozenset()) -> list[str]:
    """Addresses of the Docker host as the task container sees it, from the
    contents of /proc/net/route: the default gateway, and the first address of
    each directly connected subnet (the bridge's). Without an address on the
    bridge that first address can belong to a container, so `exclude` (the
    task container's own, and the proxy's) is left out."""
    found: list[str] = []
    for line in routes.splitlines()[1:]:
        fields = line.split()
        if len(fields) < 8:
            continue
        dest, gateway = (socket.inet_ntoa(struct.pack("<L", int(fields[i], 16))) for i in (1, 2))
        address = gateway if dest == "0.0.0.0" else str(ipaddress.ip_address(dest) + 1)
        if address not in found and address != "0.0.0.0" and address not in exclude:
            found.append(address)
    return found


def _now() -> str:
    return datetime.now(timezone.utc).isoformat(timespec="seconds").replace("+00:00", "Z")


def pull_before_up(strategy: type | None = None) -> bool:
    """Harbor 0.24's Daytona DinD strategy runs `docker compose up -d` with a
    120 s limit, and the image pull happens inside it, so a large prebuilt image
    dies mid-extract. Run `docker compose pull` first, under the task's
    build_timeout_sec. Modal's strategy already gives `up` that limit.
    Returns whether the patch is in place; does nothing on another Harbor or if
    the method is not what it was in 0.24.0."""
    if strategy is None:
        try:
            if not importlib.metadata.version("harbor").startswith("0.24."):
                return False
            from harbor.environments.daytona.environment import _DaytonaDinD as strategy
        except (ImportError, importlib.metadata.PackageNotFoundError):
            return False
    original = getattr(strategy, "_compose_exec", None)
    if getattr(original, "_kavach_pulls", False):
        return True
    if not inspect.iscoroutinefunction(original) or list(inspect.signature(original).parameters) != [
        "self", "subcommand", "timeout_sec"
    ]:
        return False

    async def compose_exec(self, subcommand, timeout_sec=None):
        if subcommand == ["up", "-d"] and timeout_sec == 120:
            # A failed pull is left to `up`, which reports it.
            await original(
                self, ["pull", "--ignore-buildable"], round(self._env.task_env_config.build_timeout_sec)
            )
        return await original(self, subcommand, timeout_sec)

    compose_exec._kavach_pulls = True
    strategy._compose_exec = compose_exec
    logging.getLogger(__name__).info("docker compose pull runs before up on Daytona, under build_timeout_sec")
    return True


pull_before_up()


class KavachOpenCode(OpenCode):
    options_model = KavachOpenCodeOptions
    options: KavachOpenCodeOptions

    @staticmethod
    def name() -> str:
        return "opencode-live"

    def __init__(self, *args, **kwargs):
        super().__init__(*args, **kwargs)
        # Harbor passes version=None when -ak version is not given.
        self._version = self._version or PINNED_VERSION
        self._proxy_started = False
        self._session: str | None = None
        self._export_failures = 0

    @override
    async def install(self, environment: BaseEnvironment) -> None:
        # Ensure ripgrep (rg) is present for OpenCode's grep/glob tools and bash,
        # even if the task container was started from an older cached image layer.
        await environment.exec(
            "command -v rg >/dev/null 2>&1 || "
            "(apt-get update >/dev/null 2>&1 && apt-get install -y --no-install-recommends ripgrep >/dev/null 2>&1 && rm -rf /var/lib/apt/lists/*)",
            user="root",
        )
        # Task images ship OpenCode already, for a non-root user that cannot
        # install packages, and the task container has no network to install it.
        result = await environment.exec(f"{NVM}opencode --version")
        lines = (result.stdout or "").strip().splitlines()
        if result.return_code == 0 and lines and lines[-1].strip() == self._version:
            self.logger.debug(f"opencode {self._version} is already installed")
        else:
            await super().install(environment)
        await self._fill_models_cache(environment)

    async def _fill_models_cache(self, environment: BaseEnvironment) -> None:
        """OpenCode cannot fetch models.dev from the sealed task container, so
        the host does. The copy built into 1.1.8 lacks newer models, whose
        prices give the trajectory its cost."""
        cache = self.logs_dir / "models.json"
        try:
            # models.dev refuses urllib's default user agent.
            request = urllib.request.Request(MODELS_URL, headers={"User-Agent": "curl/8"})
            with urllib.request.urlopen(request, timeout=30) as response:
                cache.write_bytes(response.read())
        except OSError as e:
            self.logger.warning(f"models.dev cache not filled, the trajectory will have no cost: {e}")
            return
        try:
            await environment.upload_file(cache, "/tmp/models.json")
            # cp, not mv: the upload is root's, in a sticky directory.
            result = await environment.exec("mkdir -p ~/.cache/opencode && cp /tmp/models.json ~/.cache/opencode/models.json")
            if result.return_code != 0:
                self.logger.warning(f"models.dev cache not installed: {result.stdout} {result.stderr}")
        finally:
            cache.unlink()

    def _upstream(self) -> tuple[str, str, str, str]:
        """(upstream base URL, API key, OpenCode provider id, OpenCode model)."""
        if not self.model_name or "/" not in self.model_name:
            raise ValueError("Model name must be in the format provider/model_name")
        provider, model_id = self.model_name.split("/", 1)
        if self.options.base_url:
            base_url = self.options.base_url
            opencode_provider = ENDPOINT_PROVIDER
        else:
            spec = PROVIDERS.get(provider)
            base_url = spec and ((spec.base_url_envs and self._get_env(*spec.base_url_envs)) or spec.base_url)
            opencode_provider = provider
            if not base_url:
                raise ValueError(
                    f"No endpoint known for provider {provider!r}; pass --ak base_url=..."
                )
        if self.options.api_key_env:
            key = self._get_env(self.options.api_key_env)
            if key is None:
                raise ValueError(f"{self.options.api_key_env} is not set")
        else:
            key = super(OpenCode, self).model_connection.api_key or ""
        return base_url, key, opencode_provider, f"{opencode_provider}/{model_id}"

    def _build_config(self, proxy_url: str, opencode_provider: str, model: str) -> dict[str, Any]:
        model_id = model.split("/", 1)[1]
        provider: dict[str, Any] = {
            "options": {"baseURL": proxy_url, "apiKey": PLACEHOLDER_KEY},
            "models": {model_id: {}},
        }
        if opencode_provider == ENDPOINT_PROVIDER:
            provider.update(npm="@ai-sdk/openai-compatible", name="endpoint")
            provider["models"] = {model_id: {"name": model_id}}
        config: dict[str, Any] = {
            "$schema": "https://opencode.ai/config.json",
            "autoupdate": False,
            "share": "disabled",
            # A headless run has nobody to answer "ask", so nothing may ask.
            "permission": {
                "edit": "allow",
                "bash": "allow",
                "webfetch": "deny",
                "external_directory": "allow",
                "doom_loop": "deny",
            },
            # Titles and summaries use the small model; without this OpenCode
            # sends them to its own hosted model.
            "small_model": model,
            "provider": {opencode_provider: provider},
        }
        if self.mcp_servers:
            mcp: dict[str, dict[str, Any]] = {}
            for server in self.mcp_servers:
                if server.transport == "stdio":
                    command = [server.command, *server.args] if server.command else []
                    mcp[server.name] = {"type": "local", "command": command}
                else:
                    mcp[server.name] = {"type": "remote", "url": server.url, "oauth": False}
            config["mcp"] = mcp
        config = self._deep_merge(config, copy.deepcopy(self._opencode_config))
        if self.options.disable_web_search:
            config["permission"].update(websearch="deny", webfetch="deny")
        return config

    def _view_port(self, environment: BaseEnvironment) -> str | None:
        """The host port of the local view, only when its overlay is in use."""
        overlays = getattr(environment, "extra_docker_compose_paths", [])
        if self.options.live and any(Path(p).name == VIEW_OVERLAY for p in overlays):
            return os.environ.get("KAVACH_VIEW_PORT")
        return None

    async def _start_proxy(self, environment: BaseEnvironment, upstream: str | None, key: str) -> str:
        """Start proxy.py in the proxy service and wait until the task
        container reaches it. Returns the host URL of the read-only view, or ''."""
        command = ["python3", "-u", "/proxy.py", "--model-client", "main"]
        if upstream:
            command += ["--upstream", upstream]
        view_port = self._view_port(environment)
        if view_port:
            command += ["--opencode", f"http://main:{SERVER_PORT}", "--view-deny", "main"]
        # The key is in this exec's environment, so only the proxy's process
        # has it; the task container never does.
        result = await environment.service_exec(
            f"nohup {shlex.join(command)} >/tmp/proxy.log 2>&1 </dev/null &",
            service=PROXY_SERVICE,
            env={"KAVACH_UPSTREAM_KEY": key},
        )
        if result.return_code != 0:
            raise RuntimeError(f"the model proxy did not start: {result.stdout} {result.stderr}")
        self._proxy_started = True
        for _ in range(100):
            if (await environment.exec(PROBE.format(host=PROXY_SERVICE, port=MODEL_PORT))).return_code == 0:
                return f"http://127.0.0.1:{view_port}" if view_port else ""
            await asyncio.sleep(0.2)
        raise RuntimeError("the model proxy did not start; see proxy.log")

    async def _save_proxy_log(self, environment: BaseEnvironment) -> None:
        if self._proxy_started:
            log = await environment.service_exec("cat /tmp/proxy.log", service=PROXY_SERVICE)
            (self.logs_dir / "proxy.log").write_text(log.stdout or "")

    async def _check_sealed(self, environment: BaseEnvironment) -> None:
        """Refuse to run when the task container reaches the internet or the
        Docker host (its network's gateway, and the first address of each
        directly connected subnet)."""
        # /etc/hosts holds the container's own address; the proxy's is by name.
        info = (await environment.exec(
            f"cat /proc/net/route; echo '#'; cat /etc/hosts; echo '#'; getent hosts {PROXY_SERVICE}"
        )).stdout or ""
        routes, _, rest = info.partition("\n#\n")
        own = {w for line in rest.splitlines() for w in line.split()[:1] if w.count(".") == 3}
        hosts = [OUTSIDE, *gateway_candidates(routes, own)]
        probes = " ".join(
            f"(timeout 1 bash -c 'exec 3<>/dev/tcp/{h}/{p}' 2>/dev/null && echo {h}:{p}) &"
            for h in hosts for p in SEALED_PORTS
        ) + f" (timeout 3 getent hosts {OUTSIDE_NAME} >/dev/null && echo dns:{OUTSIDE_NAME}) &"
        reached = ((await environment.exec(f"{probes} wait")).stdout or "").split()
        if not reached:
            return
        message = (
            f"the task container can reach {', '.join(reached)} (host:port, or a name it resolves): its docker-compose.yaml must put it on an "
            "internal network, without a bridge address, shared with the proxy service (see README.md)"
        )
        if self.options.require_isolation:
            raise RuntimeError(message + ", or pass --ak require_isolation=false")
        self.logger.warning(message + "; running with open egress")

    def _write_atomic(self, name: str, text: str) -> None:
        path = self.logs_dir / name
        tmp = path.with_suffix(".tmp")
        tmp.write_text(text)
        tmp.replace(path)

    def _write_live(self, data: dict[str, Any]) -> None:
        self._write_atomic(LIVE_FILE, json.dumps(data, indent=2) + "\n")

    async def _find_session(self, environment: BaseEnvironment, directory: str, since_ms: int) -> str | None:
        """The run's session: from the server, or else from the run's output."""
        query = urllib.parse.urlencode({"directory": directory})
        url = f"http://127.0.0.1:{SERVER_PORT}/session?{query}"
        result = await environment.exec(
            f'{NVM}node -e "fetch(process.argv[1]).then(r => r.text()).then(console.log)" {shlex.quote(url)}'
        )
        try:
            roots = [
                s for s in json.loads(result.stdout)
                if not s.get("parentID") and s.get("time", {}).get("created", 0) >= since_ms
            ]
            if roots:
                return min(roots, key=lambda s: s["time"]["created"])["id"]
        except (ValueError, TypeError, AttributeError):
            pass
        for event in self._parse_stdout():
            if event.get("sessionID"):
                return event["sessionID"]
        return None

    async def _mirror(self, environment: BaseEnvironment, env: dict[str, str], directory: str, view_url: str) -> None:
        """Write the live file as soon as the run's session exists, then keep
        opencode-session.json current while it runs. One export at a time: the
        next waits for the last, so a slow one skips ticks."""
        started = _now()
        since_ms = int(datetime.now(timezone.utc).timestamp() * 1000) - 5000
        session = None
        while not session:
            try:
                session = await self._find_session(environment, directory, since_ms)
                if session:
                    self._session = session
                    self._write_live({"url": view_url, "session": session, "started": started, "dir": directory})
            except Exception:
                self.logger.exception("looking for the session failed; retrying")
            if not session:
                await asyncio.sleep(0.5)
        while True:
            await self._quietly(self._export(environment, env))
            await asyncio.sleep(MIRROR_SEC)

    def _append_live(self, data: bytes) -> None:
        with (self.logs_dir / LIVE_LOG).open("ab") as f:
            f.write(data)
        self._live_offset += len(data)

    def _follow_command(self) -> str:
        # Exits NO_LOG_HERE when the log is not on this machine: following
        # stops, and polling goes on.
        remote = shlex.quote((self.environment_logs_dir / self._OUTPUT_FILENAME).as_posix())
        return (
            f"[ -f {remote} ] || exit {NO_LOG_HERE}; "
            f"exec timeout {FOLLOW_SEC} tail -c +{self._live_offset + 1} -F {remote} 2>/dev/null"
        )

    def _follower(self, environment: BaseEnvironment):
        """The provider's streaming exec, as a function that follows the log
        from the live copy's end until the stream ends and returns the exit
        status, or None where the provider has none (then the log is
        polled)."""
        sandbox = getattr(environment, "_sandbox", None)
        if sandbox is None:
            return None
        kind = environment.type()
        if kind == EnvironmentType.DAYTONA:
            return lambda: self._follow_daytona(sandbox)
        if kind == EnvironmentType.MODAL:
            return lambda: self._follow_modal(sandbox)
        return None

    async def _follow_daytona(self, sandbox) -> int | None:
        from daytona import SessionExecuteRequest

        session = f"kavach-live-{os.urandom(4).hex()}"
        await sandbox.process.create_session(session)
        try:
            cmd = await sandbox.process.execute_session_command(
                session, SessionExecuteRequest(command=self._follow_command(), run_async=True)
            )
            # The SDK decodes the stream incrementally as UTF-8, so a character
            # split across messages arrives whole.
            append = self._appender()
            await sandbox.process.get_session_command_logs_async(
                session, cmd.cmd_id, lambda text: append(text.encode()), lambda _: None
            )
            done = await sandbox.process.get_session_command(session, cmd.cmd_id)
            return done.exit_code
        finally:
            # Deleting the session stops its tail.
            with contextlib.suppress(Exception):
                await asyncio.shield(sandbox.process.delete_session(session))

    async def _follow_modal(self, sandbox) -> int | None:
        append = self._appender()
        process = await sandbox.exec.aio("sh", "-c", self._follow_command(), text=False)
        async for chunk in process.stdout:
            append(chunk)
        return await process.wait.aio()

    async def _follow_for(self, follow) -> int | None:
        """Run one follow, giving up on a stream that outlives its command.
        A follow given up is cancelled but not waited for: its cleanup (the
        SDK closing a dead websocket, deleting the provider's session) can
        hang too, and the log is polled meanwhile. Whatever it still
        delivers is dropped, so nothing is copied twice."""
        self._follows = getattr(self, "_follows", 0) + 1
        task = asyncio.ensure_future(follow())
        done, _ = await asyncio.wait({task}, timeout=FOLLOW_SEC + FOLLOW_GRACE_SEC)
        if task in done:
            return task.result()
        self._follows += 1
        task.cancel()
        # Kept until it ends, so it is not collected mid-cleanup; its outcome
        # is retrieved so asyncio does not warn about it.
        self._abandoned = getattr(self, "_abandoned", set())
        self._abandoned.add(task)
        task.add_done_callback(lambda t: (self._abandoned.discard(t), t.cancelled() or t.exception()))
        return None

    def _appender(self):
        """_append_live for the current follow only."""
        mine = getattr(self, "_follows", 0)
        return lambda data: self._append_live(data) if getattr(self, "_follows", 0) == mine else None

    async def _tail_once(self, environment: BaseEnvironment) -> bool:
        """Append the bytes the sandbox's event log gained since the last
        call to the trial's live copy. Returns False once the log is in the
        trial directory itself (a bind mount): there is nothing to copy."""
        if (self.logs_dir / self._OUTPUT_FILENAME).exists():
            return False
        remote = shlex.quote((self.environment_logs_dir / self._OUTPUT_FILENAME).as_posix())
        while True:
            result = await environment.exec(
                f"tail -c +{self._live_offset + 1} {remote} 2>/dev/null | head -c {TAIL_CHUNK} | base64 | tr -d '\\n'",
                timeout_sec=60,
            )
            if result.return_code:
                return True
            data = base64.b64decode(result.stdout or "")
            if not data:
                return True
            self._append_live(data)
            if len(data) < TAIL_CHUNK:
                return True

    async def _tail(self, environment: BaseEnvironment) -> None:
        """Keep the live copy of the event log current while the agent runs:
        follow it where the provider streams, and poll it between follows
        (which also copies what was written before one started) and where
        it does not."""
        self._live_offset = 0
        follow = self._follower(environment)
        failures = 0
        while True:
            try:
                if not await self._tail_once(environment):
                    return
                # Once the log exists: until then there is nothing to follow.
                if follow and self._live_offset and await self._follow_for(follow) == NO_LOG_HERE:
                    self.logger.warning("the event log is not on the sandbox's machine; polling it instead")
                    follow = None
                failures = 0
            except Exception as exc:
                failures += 1
                if failures % EXPORT_WARN_EVERY == 1:
                    self.logger.warning(f"copying the event log failed ({failures} in a row): {exc!r:.200}")
            await asyncio.sleep(TAIL_SEC)

    @override
    @with_prompt_template
    async def run(self, instruction: str, environment: BaseEnvironment, context: AgentContext) -> None:
        self._instruction = instruction
        upstream, key, opencode_provider, model = self._upstream()
        logs = self.environment_logs_dir
        env = {
            "OPENCODE_FAKE_VCS": "git",
            "XDG_DATA_HOME": (logs / "opencode" / "xdg-data").as_posix(),
            "XDG_STATE_HOME": (logs / "opencode" / "xdg-state").as_posix(),
        }
        serving = False
        mirror: asyncio.Task | None = None
        tail: asyncio.Task | None = None
        try:
            check_modal_runtime(environment)
            if self.options.isolate:
                await self._check_sealed(environment)
            view_url = await self._start_proxy(environment, upstream, key)

            if skills := self._build_register_skills_command():
                await self.exec_as_agent(environment, command=skills, env=env)
            config = json.dumps(self._build_config(f"http://{PROXY_SERVICE}:{MODEL_PORT}/v1", opencode_provider, model), indent=2)
            await self.exec_as_agent(
                environment,
                command=f"mkdir -p ~/.config/opencode && printf '%s' {shlex.quote(config)} > ~/.config/opencode/opencode.json",
            )

            attach = ""
            if self.options.live:
                directory = (await self.exec_as_agent(environment, command="pwd")).stdout.strip()
                await self.exec_as_agent(
                    environment,
                    env=env,
                    command=(
                        f"{NVM}nohup opencode serve --hostname 0.0.0.0 --port {SERVER_PORT} "
                        f"> {shlex.quote((logs / 'opencode-serve.log').as_posix())} 2>&1 </dev/null & "
                        "echo $! > /tmp/opencode-serve.pid; "
                        f"for i in $(seq 150); do node -e \"fetch('http://127.0.0.1:{SERVER_PORT}/global/health')"
                        ".then(r => process.exit(r.ok ? 0 : 1), () => process.exit(1))\" && exit 0; "
                        "sleep 0.2; done; exit 1"
                    ),
                )
                serving = True
                attach = f"--attach http://127.0.0.1:{SERVER_PORT} "
                mirror = asyncio.create_task(self._mirror(environment, env, directory, view_url))
                tail = asyncio.create_task(self._tail(environment))

            # The instruction goes in on stdin, which `opencode run` appends to
            # the message. On the command line, the agent's own `pkill -f
            # redis-server` matches it whenever the task names that binary.
            prompt = self.logs_dir / INSTRUCTION_FILE
            prompt.write_text(instruction)
            remote_prompt = f"/tmp/{INSTRUCTION_FILE}"
            await environment.upload_file(prompt, remote_prompt)
            await environment.exec(f"chmod 644 {remote_prompt}", user="root")
            flags = self.build_cli_flags()
            await self._unless_stalled(
                self.exec_as_agent(
                    environment,
                    env=env,
                    command=(
                        f"{NVM}opencode run {attach}--format json --model {shlex.quote(model)} "
                        f"{flags if flags else ''} "
                        f"2>&1 <{remote_prompt} | stdbuf -oL tee "
                        f"{shlex.quote((logs / self._OUTPUT_FILENAME).as_posix())}"
                    ),
                ),
                self.options.stall_sec if self.options.live else 0,
                environment,
            )
        finally:
            # Each step runs even if an earlier one fails.
            for task in (mirror, tail):
                if task:
                    task.cancel()
                    with contextlib.suppress(asyncio.CancelledError, Exception):
                        await task
            await self._quietly(self._mark_finished())
            if serving:
                await self._quietly(environment.exec("kill $(cat /tmp/opencode-serve.pid) 2>/dev/null; true", user="root"))
            await self._quietly(self._export(environment, env))
            await self._quietly(self._save_proxy_log(environment))

        if messages := self._error_messages():
            raise NonZeroAgentExitCodeError("OpenCode emitted error event(s): " + "; ".join(messages[:3]))

    def _progress(self) -> int:
        """How far the event log has got: its size where the trial directory
        holds it (a bind mount), else what _tail has copied of it."""
        local = self.logs_dir / self._OUTPUT_FILENAME
        if local.exists():
            return local.stat().st_size
        return self._live_offset

    async def _remote_size(self, environment: BaseEnvironment | None) -> int | None:
        """The size of the event log in the sandbox, or None if it can't be had."""
        if environment is None:
            return None
        remote = shlex.quote((self.environment_logs_dir / self._OUTPUT_FILENAME).as_posix())
        try:
            result = await environment.exec(f"stat -c %s {remote}", timeout_sec=60)
            return int(result.stdout.strip()) if not result.return_code else None
        except Exception:
            return None

    async def _stalled(self, limit: float, environment: BaseEnvironment | None = None) -> None:
        """Return once the event log has gained nothing for limit seconds.
        Where it is copied, not mounted, the sandbox's own log must also have
        gained nothing since the last look: a copy can stop while the log
        grows. One that can't be measured is not taken to have stalled."""
        loop = asyncio.get_running_loop()
        last, since, remote = self._progress(), loop.time(), None
        while True:
            await asyncio.sleep(min(STALL_CHECK_SEC, limit))
            now, got = loop.time(), self._progress()
            if got != last:
                last, since = got, now
                continue
            if now - since < limit:
                continue
            if (self.logs_dir / self._OUTPUT_FILENAME).exists():
                return
            size = await self._remote_size(environment)
            # No bigger than the copy: the copy is current, so the log too
            # gained nothing. The same as last look: the log stopped there.
            if size is not None and (size <= got or size == remote):
                return
            if size is not None:
                remote = size
            since = loop.time()

    async def _unless_stalled(self, awaitable, limit: float, environment: BaseEnvironment | None = None):
        """Await awaitable, or raise AgentStalledError once the event log has
        gained nothing for limit seconds (none when limit is 0). The caller's
        cleanup kills the OpenCode server, which ends the run inside the
        sandbox too."""
        if limit <= 0:
            return await awaitable
        run = asyncio.ensure_future(awaitable)
        watch = asyncio.create_task(self._stalled(limit, environment))
        try:
            await asyncio.wait({run, watch}, return_when=asyncio.FIRST_COMPLETED)
        except BaseException:
            # Cancelled from outside, as by Harbor's agent timeout.
            run.cancel()
            watch.cancel()
            raise
        if run.done():
            watch.cancel()
            with contextlib.suppress(asyncio.CancelledError):
                await watch
            return run.result()
        run.cancel()
        with contextlib.suppress(asyncio.CancelledError, Exception):
            await run
        raise AgentStalledError(f"OpenCode's event log gained nothing for {limit:g} s: its model call is taken to have hung")

    async def _quietly(self, awaitable) -> None:
        try:
            await awaitable
        except Exception:
            self.logger.exception("cleanup step failed")

    async def _mark_finished(self) -> None:
        path = self.logs_dir / LIVE_FILE
        if path.exists():
            self._write_live({**json.loads(path.read_text()), "finished": _now()})

    async def _export(self, environment: BaseEnvironment, env: dict[str, str]) -> None:
        """Copy the session to the trial directory; a failed or partial export
        leaves the previous copy in place."""
        if not self._session:
            for event in self._parse_stdout():
                if event.get("sessionID"):
                    self._session = event["sessionID"]
                    break
        if not self._session:
            return
        # Not through exec stdout: remote exec APIs truncate large output, and
        # sessions outgrow that. A file transfer has no such cap.
        remote = "/tmp/kavach-export.json"
        local = self.logs_dir / (EXPORT_FILE + ".download")
        try:
            result = await environment.exec(
                f"{NVM}opencode export {shlex.quote(self._session)} > {remote}.tmp 2>/dev/null && mv {remote}.tmp {remote}",
                env=env,
                timeout_sec=120,
            )
            if result.return_code:
                raise RuntimeError(f"opencode export exited {result.return_code}")
            await environment.download_file(remote, local)
            json.loads(local.read_text())
            local.replace(self.logs_dir / EXPORT_FILE)
        except Exception as exc:
            self._export_failures += 1
            if self._export_failures % EXPORT_WARN_EVERY == 1:
                size = local.stat().st_size if local.exists() else None
                self.logger.warning(
                    f"opencode export failed ({self._export_failures} in a row, downloaded {size} bytes): {exc!r:.200}"
                )
            return
        finally:
            local.unlink(missing_ok=True)
        self._export_failures = 0
