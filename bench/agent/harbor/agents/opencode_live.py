"""OpenCode for Harbor, watchable live and without the API key.

Harbor's OpenCode agent runs `opencode run --format json` with the provider
key in the agent's environment. This one keeps Harbor's install, trajectory
and token accounting (the same JSON event stream lands in opencode.txt) and
changes how the run is wired:

- A proxy container (proxy.py) joins the environment's Docker network. It
  holds the API key and is the agent's only route to the model; OpenCode in
  the task container is configured with the proxy's address and a
  placeholder key.
- During the run the task container can reach only the proxy: the agent
  narrows Harbor's egress policy (the egress-control sidecar) to the proxy's
  address, and restores it afterwards.
- `opencode serve` runs in the task container and the task is submitted to
  it with `opencode run --attach`. The proxy publishes a read-only view of
  that server on 127.0.0.1 of the host, recorded in opencode-live.json in the
  trial's agent log directory.
- After the run the session is exported to opencode-session.json, which
  show.sh opens on the host.

See README.md.
"""

import asyncio
import copy
import json
import os
import shlex
import subprocess
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
from harbor.models.task.config import NetworkMode, NetworkPolicy

PROXY_SCRIPT = Path(__file__).with_name("proxy.py")
LIVE_FILE = "opencode-live.json"
EXPORT_FILE = "opencode-session.json"
PINNED_VERSION = "1.1.8"
SERVER_PORT = 4096
MODEL_PORT = 8080
VIEW_PORT = 8081
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
        description="Restrict the task container's egress to the model proxy "
        "during the run. Needs Harbor's egress control, which a task enables "
        'with [agent] network_mode = "no-network".',
    )
    require_isolation: bool = Field(
        default=True,
        description="Fail when isolate is set but the environment cannot "
        "restrict egress, instead of running with open egress.",
    )
    live: bool = Field(default=True, description="Run opencode serve and publish the read-only view.")
    proxy_image: str = Field(default="python:3.12-alpine", description="Image the proxy runs in.")


def _now() -> str:
    return datetime.now(timezone.utc).isoformat(timespec="seconds").replace("+00:00", "Z")


def _docker(*args: str, env: dict[str, str] | None = None, check: bool = True) -> str:
    result = subprocess.run(
        ["docker", *args], capture_output=True, text=True, env=env, check=False
    )
    if check and result.returncode != 0:
        raise RuntimeError(f"docker {args[0]} failed: {result.stderr.strip()}")
    return result.stdout.strip()


def _inspect(container: str) -> dict[str, Any]:
    return json.loads(_docker("inspect", container))[0]


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
        self._proxy: str | None = None
        self._session: str | None = None

    @override
    async def install(self, environment: BaseEnvironment) -> None:
        # Ensure ripgrep (rg) is present for OpenCode's grep/glob tools and bash,
        # even if the task container was started from an older cached image layer.
        await environment.exec(
            "command -v rg >/dev/null 2>&1 || "
            "(apt-get update >/dev/null 2>&1 && apt-get install -y --no-install-recommends ripgrep >/dev/null 2>&1 && rm -rf /var/lib/apt/lists/*)",
            user="root",
        )
        # Task images may ship OpenCode already, for a non-root user that
        # cannot install packages.
        nvm = "[ -f ~/.nvm/nvm.sh ] && . ~/.nvm/nvm.sh; "
        result = await environment.exec(f"{nvm}opencode --version")
        lines = (result.stdout or "").strip().splitlines()
        if result.return_code == 0 and lines and lines[-1].strip() == self._version:
            self.logger.debug(f"opencode {self._version} is already installed")
        else:
            await super().install(environment)
        # Fill OpenCode's models.dev cache while setup still has the network:
        # during the run it cannot fetch it, and the copy built into 1.1.8
        # lacks newer models, whose prices give the trajectory its cost.
        await environment.exec(f"{nvm}opencode models >/dev/null 2>&1 || true")

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

    def _netns_owner(self, environment: BaseEnvironment) -> dict[str, Any]:
        """The container whose network namespace the task container uses: the
        egress sidecar when egress control is on, else the task container."""
        from harbor.environments.docker.docker import (
            DockerEnvironment,
            _sanitize_docker_compose_project_name,
        )

        if not isinstance(environment, DockerEnvironment):
            raise RuntimeError("kavach OpenCode needs Harbor's docker environment")
        project = _sanitize_docker_compose_project_name(environment.session_id)
        main = _docker(
            "ps", "-q",
            "--filter", f"label=com.docker.compose.project={project}",
            "--filter", "label=com.docker.compose.service=main",
        ).splitlines()
        if len(main) != 1:
            raise RuntimeError(f"expected one main container in {project}, found {main}")
        info = _inspect(main[0])
        mode = info["HostConfig"]["NetworkMode"]
        if mode.startswith("container:"):
            info = _inspect(mode.split(":", 1)[1])
        return info

    def _start_proxy(self, environment: BaseEnvironment, upstream: str | None, key: str) -> tuple[str, str]:
        """Start the proxy; return (its address, host URL of the view or '')."""
        owner = self._netns_owner(environment)
        net_name, net = next(iter(owner["NetworkSettings"]["Networks"].items()))
        agent_ip = net["IPAddress"]
        name = f"kavach-proxy-{owner['Name'].strip('/')}"
        self._proxy = name
        args = [
            "run", "-d", "--name", name, "--label", "kavach.bench=proxy",
            "--network", net_name, "--add-host", "host.docker.internal:host-gateway",
            "-v", f"{PROXY_SCRIPT}:/proxy.py:ro",
        ]
        command = ["python3", "-u", "/proxy.py"]
        if upstream:
            args += ["-e", "KAVACH_UPSTREAM_KEY"]
            command += ["--upstream", upstream, "--model-client", agent_ip]
        if self.options.live:
            # Published connections arrive from the network's gateway; the
            # task container is refused.
            args += ["-p", f"127.0.0.1::{VIEW_PORT}"]
            command += [
                "--opencode", f"http://{agent_ip}:{SERVER_PORT}",
                "--view-client", net["Gateway"],
            ]
        _docker("rm", "-f", name, check=False)
        _docker(*args, self.options.proxy_image, *command, env={**os.environ, "KAVACH_UPSTREAM_KEY": key})
        ip = _inspect(name)["NetworkSettings"]["Networks"][net_name]["IPAddress"]
        view = ""
        if self.options.live:
            host = _docker("port", name, f"{VIEW_PORT}/tcp").splitlines()[0]
            view = "http://127.0.0.1:" + host.rsplit(":", 1)[1]
        return ip, view

    def _stop_proxy(self) -> None:
        if not self._proxy:
            return
        logs = subprocess.run(["docker", "logs", self._proxy], capture_output=True, text=True)
        (self.logs_dir / "proxy.log").write_text(logs.stdout + logs.stderr)
        _docker("rm", "-f", self._proxy, check=False)
        self._proxy = None

    async def _wait_proxy(self) -> None:
        for _ in range(100):
            if subprocess.run(["docker", "exec", self._proxy, "test", "-f", "/tmp/ready"],
                              capture_output=True).returncode == 0:
                return
            await asyncio.sleep(0.2)
        raise RuntimeError("the model proxy did not start; see proxy.log")

    def _write_live(self, data: dict[str, Any]) -> None:
        path = self.logs_dir / LIVE_FILE
        tmp = path.with_suffix(".tmp")
        tmp.write_text(json.dumps(data, indent=2) + "\n")
        tmp.replace(path)

    def _find_session(self, url: str, directory: str, since_ms: int) -> str | None:
        """The run's session: from the server, or else from the run's output."""
        query = urllib.parse.urlencode({"directory": directory})
        try:
            with urllib.request.urlopen(f"{url}/session?{query}", timeout=5) as response:
                sessions = json.load(response)
            roots = [
                s for s in sessions
                if not s.get("parentID") and s.get("time", {}).get("created", 0) >= since_ms
            ]
            if roots:
                return min(roots, key=lambda s: s["time"]["created"])["id"]
        except (OSError, ValueError):
            pass
        for event in self._parse_stdout():
            if event.get("sessionID"):
                return event["sessionID"]
        return None

    async def _publish_live(self, url: str, directory: str) -> None:
        """Write the live file as soon as the run's session exists."""
        started = _now()
        since_ms = int(datetime.now(timezone.utc).timestamp() * 1000) - 5000
        while not (session := await asyncio.to_thread(self._find_session, url, directory, since_ms)):
            await asyncio.sleep(0.5)
        self._session = session
        self._write_live({"url": url, "session": session, "started": started, "dir": directory})

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
        nvm = "[ -f ~/.nvm/nvm.sh ] && . ~/.nvm/nvm.sh; "
        previous_policy = environment.network_policy
        policy_changed = False
        serving = False
        publisher: asyncio.Task | None = None
        try:
            if self.options.isolate and not environment.capabilities.dynamic_network_policy:
                message = (
                    "egress cannot be restricted: the task does not enable Harbor's "
                    'egress control (add [agent] network_mode = "no-network" to task.toml)'
                )
                if self.options.require_isolation:
                    raise RuntimeError(message + ", or pass --ak require_isolation=false")
                self.logger.warning(message + "; running with open egress")

            proxy_ip, view_url = self._start_proxy(environment, upstream, key)
            await self._wait_proxy()
            if self.options.isolate and environment.capabilities.dynamic_network_policy:
                hosts = list(previous_policy.allowed_hosts) if previous_policy.network_mode == NetworkMode.ALLOWLIST else []
                await environment.set_network_policy(
                    NetworkPolicy(network_mode=NetworkMode.ALLOWLIST, allowed_hosts=[*hosts, proxy_ip])
                )
                policy_changed = True

            if skills := self._build_register_skills_command():
                await self.exec_as_agent(environment, command=skills, env=env)
            config = json.dumps(self._build_config(f"http://{proxy_ip}:{MODEL_PORT}/v1", opencode_provider, model), indent=2)
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
                        f"{nvm}nohup opencode serve --hostname 0.0.0.0 --port {SERVER_PORT} "
                        f"> {shlex.quote((logs / 'opencode-serve.log').as_posix())} 2>&1 </dev/null & "
                        "echo $! > /tmp/opencode-serve.pid; "
                        f"for i in $(seq 150); do node -e \"fetch('http://127.0.0.1:{SERVER_PORT}/global/health')"
                        ".then(r => process.exit(r.ok ? 0 : 1), () => process.exit(1))\" && exit 0; "
                        "sleep 0.2; done; exit 1"
                    ),
                )
                serving = True
                attach = f"--attach http://127.0.0.1:{SERVER_PORT} "
                publisher = asyncio.create_task(self._publish_live(view_url, directory))

            flags = self.build_cli_flags()
            await self.exec_as_agent(
                environment,
                env=env,
                command=(
                    f"{nvm}opencode run {attach}--format json --model {shlex.quote(model)} "
                    f"{flags + ' ' if flags else ''}-- {shlex.quote(instruction)} "
                    f"2>&1 </dev/null | stdbuf -oL tee "
                    f"{shlex.quote((logs / self._OUTPUT_FILENAME).as_posix())}"
                ),
            )
        finally:
            # Each step runs even if an earlier one fails: a proxy left on the
            # network would stop Harbor from removing it.
            if publisher:
                publisher.cancel()
            await self._quietly(self._mark_finished())
            if serving:
                await self._quietly(environment.exec("kill $(cat /tmp/opencode-serve.pid) 2>/dev/null; true", user="root"))
            await self._quietly(self._export(environment, env, nvm))
            await self._quietly(asyncio.to_thread(self._stop_proxy))
            if policy_changed:
                await self._quietly(environment.set_network_policy(previous_policy))

        if messages := self._error_messages():
            raise NonZeroAgentExitCodeError("OpenCode emitted error event(s): " + "; ".join(messages[:3]))

    async def _quietly(self, awaitable) -> None:
        try:
            await awaitable
        except Exception:
            self.logger.exception("cleanup step failed")

    async def _mark_finished(self) -> None:
        path = self.logs_dir / LIVE_FILE
        if path.exists():
            self._write_live({**json.loads(path.read_text()), "finished": _now()})

    async def _export(self, environment: BaseEnvironment, env: dict[str, str], nvm: str) -> None:
        if not self._session:
            for event in self._parse_stdout():
                if event.get("sessionID"):
                    self._session = event["sessionID"]
                    break
        if not self._session:
            return
        target = (self.environment_logs_dir / EXPORT_FILE).as_posix()
        result = await environment.exec(
            f"{nvm}opencode export {shlex.quote(self._session)} > {shlex.quote(target)}",
            env=env,
        )
        if result.return_code != 0:
            self.logger.warning(f"opencode export failed: {result.stderr}")
