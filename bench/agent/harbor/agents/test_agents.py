"""Tests for the OpenCode agent's pieces that need neither a model nor Harbor.

    python3 bench/agent/harbor/agents/test_agents.py

The isolation test brings up a cut-down copy of a task's Compose topology with
`docker compose` (a small image in place of the task's), so it is skipped
without Docker. It calls no model: the "upstream" is a local fake.
"""

import asyncio
import contextlib
import importlib.util
import ipaddress
import json
import subprocess
import sys
import tempfile
import threading
import types
import unittest
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path
from unittest import mock

HERE = Path(__file__).resolve().parent
HARBOR = HERE.parent
sys.path.insert(0, str(HERE))
import proxy  # noqa: E402

HARBOR_INSTALLED = importlib.util.find_spec("harbor") is not None
if HARBOR_INSTALLED:
    import opencode_live  # noqa: E402

KEY = "sk-test-upstream-key"
COMPOSE = """\
services:
  main:
    image: python:3.12-alpine
    command: ["tail", "-f", "/dev/null"]
    networks: [sealed]
    cap_drop: [NET_RAW]
  proxy:
    image: python:3.12-alpine
    init: true
    command: ["tail", "-f", "/dev/null"]
    volumes:
      - ./proxy.py:/proxy.py:ro
    extra_hosts:
      - host.docker.internal:host-gateway
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
"""


class TasksInSync(unittest.TestCase):
    def test_sync_tasks_check(self):
        result = subprocess.run(
            [sys.executable, str(HARBOR / "sync-tasks.py"), "--check"], capture_output=True, text=True
        )
        self.assertEqual(result.returncode, 0, result.stdout + "run bench/agent/harbor/sync-tasks.py")


class Peer:
    def __init__(self, ip):
        self.ip = ip

    def get_extra_info(self, _):
        return (self.ip, 1)


class Allowed(unittest.TestCase):
    def test_clients_and_denied(self):
        net = [ipaddress.ip_network("10.0.0.2/32")]
        self.assertTrue(proxy.allowed(Peer("10.0.0.2"), net, []))
        self.assertFalse(proxy.allowed(Peer("10.0.0.3"), net, []))
        self.assertTrue(proxy.allowed(Peer("10.0.0.3"), [], net))
        self.assertFalse(proxy.allowed(Peer("10.0.0.2"), [], net))

    def test_names_resolve(self):
        with mock.patch.object(proxy.socket, "getaddrinfo", return_value=[(2, 1, 6, "", ("10.1.2.3", 0))]):
            self.assertEqual(proxy.networks(["main", "10.0.0.0/8"]), [
                ipaddress.ip_network("10.1.2.3"), ipaddress.ip_network("10.0.0.0/8")])


class CleanTarget(unittest.TestCase):
    def test_accepted(self):
        for target, want in [
            ("/v1/chat/completions", "/chat/completions"),
            ("/v1/models?limit=5&x=a%20b", "/models?limit=5&x=a%20b"),
            ("/v1/models/", "/models/"),
        ]:
            self.assertEqual(proxy.clean_target(target), want, target)

    def test_refused(self):
        for target in [
            "/v1/a\r\nHost: evil", "/v1/a\nb", "/v1/a\x00b", "/v1/a\x1fb", "/v1/a\x7fb", "/v1/a b",
            "/v1/%0d%0aX", "/v1/%00", "/v1/../x", "/v1/./x", "/v1/a/../b", "/v1/%2e%2e/x", "/v1/%2E/x",
            "/v1//x", "/v1/a//b", "/v1/@evil/x", "/v1/a@b", "/v1/a\\b", "/v1/%2fx", "/v1/a?x=\r\n",
            "/v1/a?x=%0a", "/x/v1/a", "@h/v1/a", "http://h/v1/a",
        ]:
            self.assertIsNone(proxy.clean_target(target), repr(target))


def parse(raw):
    async def go():
        reader = asyncio.StreamReader()
        reader.feed_data(raw)
        reader.feed_eof()
        return await proxy.read_request(reader)

    return asyncio.run(go())


class StrictRequests(unittest.TestCase):
    GOOD = b"POST /v1/chat/completions HTTP/1.1\r\nHost: p\r\nContent-Length: 2\r\nX-A:\ta b \r\n\r\n{}"

    def test_accepted(self):
        req = parse(self.GOOD)
        self.assertEqual((req.method, req.target, req.body), ("POST", "/v1/chat/completions", b"{}"))
        self.assertEqual(req.header("x-a"), "a b")

    def test_refused(self):
        head = b"GET /v1/a HTTP/1.1\r\n"
        for name, raw in {
            "bare LF in value": head + b"X-A: a\nAuthorization: Bearer evil\r\n\r\n",
            "bare CR in value": head + b"X-A: a\rInjected: 1\r\n\r\n",
            "smuggled request": head + b"X-A: a\n\nGET /admin HTTP/1.1\nX-B: b\r\n\r\n",
            "space in name": head + b"X Bad: v\r\n\r\n",
            "NUL in name": head + b"X-Bad\x00: v\r\n\r\n",
            "NUL in value": head + b"X-A: a\x00b\r\n\r\n",
            "DEL in value": head + b"X-A: a\x7fb\r\n\r\n",
            "folded line": head + b"X-A: a\r\n b\r\n\r\n",
            "no colon": head + b"X-A\r\n\r\n",
            "two Content-Length": head + b"Content-Length: 1\r\nContent-Length: 2\r\n\r\n",
            "two Transfer-Encoding": head + b"Transfer-Encoding: chunked\r\ntransfer-encoding: chunked\r\n\r\n",
            "both": head + b"Content-Length: 1\r\nTransfer-Encoding: chunked\r\n\r\n",
            "bare LF request line": b"GET /v1/a HTTP/1.1\nHost: x\r\n\r\n",
            "control in target": b"GET /v1/a\x01b HTTP/1.1\r\n\r\n",
            "extra request line part": b"GET /v1/a b HTTP/1.1\r\n\r\n",
        }.items():
            with self.assertRaises(proxy.BadRequest, msg=name):
                parse(raw)

    def test_oversized_head(self):
        with self.assertRaises(asyncio.LimitOverrunError):
            parse(b"GET /v1/a HTTP/1.1\r\nX-A: " + b"a" * 200000 + b"\r\n\r\n")

    def test_view_targets(self):
        self.assertEqual(proxy.parse_target("/session?directory=%2Fapp"), (["session"], "directory=%2Fapp"))
        self.assertEqual(proxy.parse_target("/"), ([""], ""))
        for target in ["/a/../b", "//x", "x/y", "/a\r\nb", "/a@b", "/%2e%2e/x"]:
            self.assertIsNone(proxy.parse_target(target), target)

    def test_server_answers_400_and_sends_nothing_upstream(self):
        async def go(raw):
            sent = []

            async def upstream(r, w):
                sent.append(await r.read(100))
                w.close()

            up = await asyncio.start_server(upstream, "127.0.0.1", 0)
            route = proxy.model_route(f"http://127.0.0.1:{up.sockets[0].getsockname()[1]}/v1", "k")
            srv = await asyncio.start_server(lambda r, w: proxy.handle(r, w, route, [], [], "model"), "127.0.0.1", 0)
            reader, writer = await asyncio.open_connection("127.0.0.1", srv.sockets[0].getsockname()[1])
            writer.write(raw)
            try:
                reply = await asyncio.wait_for(reader.read(), 5)
            except ConnectionResetError:
                # The reply raced the client's unread megabytes; the close is the refusal.
                reply = b"HTTP/1.1 400 Bad Request (reset)"
            writer.close()
            srv.close()
            up.close()
            return reply, sent

        for raw in [
            b"GET /v1/a HTTP/1.1\r\nX-A: a\nAuthorization: Bearer evil\r\n\r\n",
            b"GET /v1/../a HTTP/1.1\r\n\r\n",
            b"GET /v1/a HTTP/1.1\r\nX-A: " + b"a" * 200000 + b"\r\n\r\n",
        ]:
            reply, sent = asyncio.run(go(raw))
            self.assertTrue(reply.startswith(b"HTTP/1.1 400 Bad Request"), reply[:40])
            self.assertEqual(sent, [])


class ModelAllowlist(unittest.TestCase):
    def status(self, method, target):
        """(reply status line, requests the upstream saw)."""
        async def go():
            seen = []

            async def upstream(r, w):
                seen.append((await r.readuntil(b"\r\n")).decode().strip())
                w.write(b"HTTP/1.1 200 OK\r\nContent-Length: 2\r\n\r\n{}")
                await w.drain()
                w.close()

            up = await asyncio.start_server(upstream, "127.0.0.1", 0)
            route = proxy.model_route(f"http://127.0.0.1:{up.sockets[0].getsockname()[1]}/api/v1", "k")
            srv = await asyncio.start_server(lambda r, w: proxy.handle(r, w, route, [], [], "model"), "127.0.0.1", 0)
            reader, writer = await asyncio.open_connection("127.0.0.1", srv.sockets[0].getsockname()[1])
            writer.write(f"{method} {target} HTTP/1.1\r\nContent-Length: 2\r\n\r\n{{}}".encode())
            reply = await asyncio.wait_for(reader.read(), 5)
            writer.close()
            srv.close()
            up.close()
            return reply.split(b"\r\n")[0].decode(), seen

        return asyncio.run(go())

    def test_chat_completions_is_forwarded(self):
        for target in ["/v1/chat/completions", "/v1/chat/completions?x=1"]:
            status, seen = self.status("POST", target)
            self.assertEqual(status, "HTTP/1.1 200 OK", target)
            self.assertEqual(seen, [f"POST /api/v1{target[3:]} HTTP/1.1"])

    def test_everything_else_is_403_and_sends_nothing_upstream(self):
        for method, target in [
            ("GET", "/v1/chat/completions"), ("PUT", "/v1/chat/completions"),
            ("POST", "/v1/keys"), ("GET", "/v1/credits"), ("GET", "/v1/models"), ("POST", "/v1/auth/keys"),
            ("POST", "/v1/chat/completions/"), ("POST", "/v1/chat/completions/x"),
            ("POST", "/v1/Chat/completions"), ("POST", "/v1/chat/%63ompletions"),
            ("GET", "/v1/key?x=/chat/completions"),
        ]:
            status, seen = self.status(method, target)
            self.assertEqual(status, "HTTP/1.1 403 Forbidden", f"{method} {target}")
            self.assertEqual(seen, [], f"{method} {target}")


@unittest.skipUnless(HARBOR_INSTALLED, "needs harbor on the path")
class AgentChecks(unittest.TestCase):
    def test_gateway_candidates(self):
        routes = (
            "Iface\tDestination\tGateway\tFlags\tRefCnt\tUse\tMetric\tMask\tMTU\tWindow\tIRTT\n"
            "eth0\t00000000\t010017AC\t0003\t0\t0\t0\t00000000\t0\t0\t0\n"
            "eth0\t000017AC\t00000000\t0001\t0\t0\t0\x00\t0\t0\t0\n"
        ).replace("0\x00", "0000FFFF")
        self.assertEqual(opencode_live.gateway_candidates(routes), ["172.23.0.1"])
        self.assertEqual(opencode_live.gateway_candidates(""), [])
        self.assertEqual(opencode_live.gateway_candidates(routes, {"172.23.0.1"}), [])

    def test_modal_needs_vm_runtime(self):
        from types import SimpleNamespace

        from harbor.models.environment_type import EnvironmentType

        def env(kind, vm=None):
            e = SimpleNamespace(type=lambda: kind)
            if vm is not None:
                e._vm_runtime_enabled = vm
            return e

        with self.assertRaisesRegex(RuntimeError, "modal_vm_runtime=true"):
            opencode_live.check_modal_runtime(env(EnvironmentType.MODAL, False))
        opencode_live.check_modal_runtime(env(EnvironmentType.MODAL, True))
        opencode_live.check_modal_runtime(env(EnvironmentType.DOCKER))
        with self.assertRaisesRegex(RuntimeError, "no longer has"):
            opencode_live.check_modal_runtime(env(EnvironmentType.MODAL))


@unittest.skipUnless(HARBOR_INSTALLED, "harbor is not installed")
class PullBeforeUp(unittest.TestCase):
    def strategy(self, signature_ok=True):
        from types import SimpleNamespace

        calls = []

        class Strategy:
            _env = SimpleNamespace(task_env_config=SimpleNamespace(build_timeout_sec=1800.0))

            async def _compose_exec(self, subcommand, timeout_sec=None):
                calls.append((subcommand, timeout_sec))
                return "result"

        if not signature_ok:
            async def other(self, subcommand):
                return None

            Strategy._compose_exec = other
        return Strategy, calls

    def test_pulls_under_the_build_timeout_before_up(self):
        strategy, calls = self.strategy()
        self.assertTrue(opencode_live.pull_before_up(strategy))
        for sub, timeout in ([["build"], 1800], [["up", "-d"], 120], [["down"], 30]):
            self.assertEqual(asyncio.run(strategy()._compose_exec(sub, timeout)), "result")
        self.assertEqual(
            calls,
            [(["build"], 1800), (["pull", "--ignore-buildable"], 1800), (["up", "-d"], 120), (["down"], 30)],
        )

    def test_applies_once(self):
        strategy, calls = self.strategy()
        self.assertTrue(opencode_live.pull_before_up(strategy))
        with self.assertNoLogs("opencode_live", "INFO"):
            self.assertTrue(opencode_live.pull_before_up(strategy))
        asyncio.run(strategy()._compose_exec(["up", "-d"], 120))
        self.assertEqual(len(calls), 2)

    def test_logs_when_applied(self):
        strategy, _ = self.strategy()
        with self.assertLogs("opencode_live", "INFO") as logs:
            opencode_live.pull_before_up(strategy)
        self.assertIn("pull", logs.output[0])

    def test_does_nothing_if_the_method_changed(self):
        strategy, calls = self.strategy(signature_ok=False)
        before = strategy._compose_exec
        self.assertFalse(opencode_live.pull_before_up(strategy))
        self.assertIs(strategy._compose_exec, before)

    def test_does_nothing_on_another_harbor(self):
        with mock.patch("importlib.metadata.version", return_value="0.25.0"):
            self.assertFalse(opencode_live.pull_before_up())


class Upstream(BaseHTTPRequestHandler):
    seen = []

    def do_POST(self):
        self.rfile.read(int(self.headers.get("Content-Length", 0)))
        Upstream.seen.append((self.path, self.headers.get("Authorization")))
        body = json.dumps({"from": "fake upstream"}).encode()
        self.send_response(200)
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def log_message(self, *args):
        pass


def docker_available():
    return subprocess.run(["docker", "info"], capture_output=True).returncode == 0


@unittest.skipUnless(docker_available(), "needs Docker")
class Isolation(unittest.TestCase):
    """main reaches only the proxy; the key is in the proxy alone."""

    @classmethod
    def setUpClass(cls):
        cls.dir = tempfile.TemporaryDirectory(dir=Path.home())
        root = Path(cls.dir.name)
        (root / "docker-compose.yaml").write_text(COMPOSE)
        (root / "proxy.py").write_text((HERE / "proxy.py").read_text())
        cls.project = "kavach-isolation-test"
        cls.server = ThreadingHTTPServer(("0.0.0.0", 0), Upstream)
        threading.Thread(target=cls.server.serve_forever, daemon=True).start()
        cls.compose("up", "-d")
        # Started as the agent starts it: the key is in this exec's environment.
        cls.compose(
            "exec", "-T", "-e", f"KAVACH_UPSTREAM_KEY={KEY}", "proxy", "sh", "-c",
            "nohup python3 -u /proxy.py --model-client main "
            f"--upstream http://host.docker.internal:{cls.server.server_port}/v1 "
            ">/tmp/proxy.log 2>&1 </dev/null &",
        )

    @classmethod
    def tearDownClass(cls):
        cls.compose("down", "--remove-orphans", "--timeout", "1", check=False)
        cls.server.shutdown()
        cls.dir.cleanup()

    @classmethod
    def compose(cls, *args, check=True):
        return subprocess.run(
            ["docker", "compose", "-p", cls.project, "--project-directory", cls.dir.name, *args],
            capture_output=True, text=True, check=check,
        )

    def main(self, script):
        return self.compose("exec", "-T", "main", "sh", "-c", script, check=False)

    def test_main_reaches_the_model_through_the_proxy(self):
        import time

        for _ in range(50):
            if self.main("python3 -c \"import socket; socket.create_connection(('proxy', 8080), 2)\"").returncode == 0:
                break
            time.sleep(0.2)
        result = self.main(
            "python3 - <<'EOF'\n"
            "import urllib.request\n"
            "r = urllib.request.Request('http://proxy:8080/v1/chat/completions', data=b'{}',"
            " headers={'Authorization': 'Bearer placeholder'})\n"
            "print(urllib.request.urlopen(r, timeout=10).read().decode())\n"
            "EOF"
        )
        self.assertIn("fake upstream", result.stdout, result.stderr)
        self.assertEqual(Upstream.seen[-1], ("/v1/chat/completions", f"Bearer {KEY}"))

    def test_main_cannot_reach_the_internet(self):
        for target in ("('1.1.1.1', 443)", "('example.com', 80)"):
            result = self.main(f"python3 -c \"import socket; socket.create_connection({target}, 3)\"")
            self.assertNotEqual(result.returncode, 0, target)

    def test_main_cannot_reach_the_docker_host(self):
        """A listener on the host network, reached through the bridge gateway."""
        port = "18765"
        subprocess.run(
            ["docker", "run", "-d", "--rm", "--name", self.project + "-host", "--network", "host",
             "python:3.12-alpine", "python3", "-m", "http.server", port],
            capture_output=True, check=True,
        )
        self.addCleanup(subprocess.run, ["docker", "rm", "-f", self.project + "-host"], capture_output=True)
        import time

        time.sleep(2)

        def gateway(network):
            out = subprocess.run(
                ["docker", "network", "inspect", f"{self.project}_{network}",
                 "--format", "{{range .IPAM.Config}}{{.Subnet}}{{end}}"],
                capture_output=True, text=True,
            ).stdout.strip()
            # The bridge's address is the subnet's first, whether or not it is
            # assigned (inhibit_ipv4 leaves it out of the network's gateway).
            return str(next(ipaddress.ip_network(out).hosts()))

        connect = "python3 -c \"import socket; socket.create_connection(('{}', {}), 3)\""
        # Control: the proxy sits on an ordinary bridge, whose gateway answers.
        control = self.compose("exec", "-T", "proxy", "sh", "-c", connect.format(gateway("upstream"), port), check=False)
        self.assertEqual(control.returncode, 0, control.stderr)
        result = self.main(connect.format(gateway("sealed"), port))
        self.assertNotEqual(result.returncode, 0, "main reached the Docker host")

    def test_main_cannot_send_raw_frames_and_proxy_does_not_forward(self):
        raw = self.main("python3 -c \"import socket; socket.socket(socket.AF_PACKET, socket.SOCK_RAW)\"")
        self.assertNotEqual(raw.returncode, 0)
        self.assertIn("ermitted", raw.stderr)
        for name in ("ipv4/ip_forward", "ipv6/conf/all/forwarding"):
            out = self.compose("exec", "-T", "proxy", "cat", f"/proc/sys/net/{name}", check=False)
            self.assertEqual(out.stdout.strip(), "0", name)

    def test_main_does_not_have_the_key(self):
        env = self.main("env; cat /proc/*/environ 2>/dev/null | tr '\\0' '\\n'")
        self.assertNotIn(KEY, env.stdout)


@unittest.skipUnless(HARBOR_INSTALLED, "harbor is not installed")
class ExportThroughFile(unittest.TestCase):
    """The session export travels as a file, not as exec stdout, which remote
    exec APIs truncate once a session passes about 1 MB."""

    def setUp(self):
        from types import SimpleNamespace

        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        self.logs = Path(self.tmp.name)
        self.agent = object.__new__(opencode_live.KavachOpenCode)
        self.agent.logs_dir = self.logs
        self.agent._session = "ses_1"
        self.agent._export_failures = 0
        self.agent.logger = mock.Mock()
        self.ns = SimpleNamespace

    def environment(self, content, return_code=0, download_error=None):
        ns = self.ns

        class Env:
            execs = []

            async def exec(self, command, **kwargs):
                self.execs.append(command)
                # What a capped exec would return: the start of the output only.
                return ns(stdout=content[:1000], stderr="", return_code=return_code)

            async def download_file(self, source, target):
                if download_error:
                    raise download_error
                Path(target).write_text(content)

        return Env()

    def export(self, env):
        asyncio.run(self.agent._export(env, {}))

    def test_session_over_a_megabyte_is_exported_whole(self):
        big = json.dumps({"info": {}, "messages": ["x" * 100] * 20000}, indent=2)
        self.assertGreater(len(big), 1_000_000)
        env = self.environment(big)
        self.export(env)
        self.assertEqual((self.logs / opencode_live.EXPORT_FILE).read_text(), big)
        self.assertIn("> /tmp/kavach-export.json.tmp", env.execs[0])
        self.assertEqual([p.name for p in self.logs.iterdir()], [opencode_live.EXPORT_FILE])

    def test_truncated_file_keeps_the_previous_copy(self):
        previous = self.logs / opencode_live.EXPORT_FILE
        previous.write_text('{"old": true}')
        self.export(self.environment('{"info": {"a": '))
        self.assertEqual(previous.read_text(), '{"old": true}')
        self.assertEqual([p.name for p in self.logs.iterdir()], [opencode_live.EXPORT_FILE])

    def test_download_failure_keeps_the_previous_copy(self):
        previous = self.logs / opencode_live.EXPORT_FILE
        previous.write_text('{"old": true}')
        self.export(self.environment("{}", download_error=RuntimeError("boom")))
        self.assertEqual(previous.read_text(), '{"old": true}')
        self.agent.logger.warning.assert_called_once()

    def test_failed_export_command_is_not_downloaded(self):
        self.export(self.environment("{}", return_code=1))
        self.assertFalse((self.logs / opencode_live.EXPORT_FILE).exists())

    def test_warnings_are_rate_limited(self):
        env = self.environment("{bad")
        for _ in range(opencode_live.EXPORT_WARN_EVERY + 1):
            self.export(env)
        self.assertEqual(self.agent.logger.warning.call_count, 2)
        self.assertIn("4 bytes", self.agent.logger.warning.call_args.args[0])

    def test_success_resets_the_failure_count(self):
        self.export(self.environment("{bad"))
        self.export(self.environment("{}"))
        self.assertEqual(self.agent._export_failures, 0)


@unittest.skipUnless(HARBOR_INSTALLED, "needs harbor")
class LiveLog(unittest.TestCase):
    """The event log's new bytes reach the trial directory while the agent
    runs, on a sandbox that does not mount it."""

    def setUp(self):
        from types import SimpleNamespace

        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        self.logs = Path(self.tmp.name)
        self.agent = object.__new__(opencode_live.KavachOpenCode)
        self.agent.logs_dir = self.logs
        self.agent._live_offset = 0
        self.agent.logger = mock.Mock()
        self.ns = SimpleNamespace
        self.remote = b""
        self.execs = []

    def environment(self, return_code=0, missing=False):
        test = self

        class Env:
            async def exec(self, command, **kwargs):
                test.execs.append(command)
                # Run the command's pipeline on the fake log, as the shell would.
                import re
                import base64

                m = re.match(r"tail -c \+(\d+) (\S+) 2>/dev/null \| head -c (\d+) \| base64 \| tr -d '\\n'$", command)
                test.assertIsNotNone(m, command)
                test.assertEqual(m.group(2), "/logs/agent/opencode.txt")
                start, n = int(m.group(1)) - 1, int(m.group(3))
                out = b"" if missing else test.remote[start : start + n]
                return test.ns(stdout=base64.b64encode(out).decode(), stderr="", return_code=return_code)

        return Env()

    def tail(self, env):
        with mock.patch.object(type(self.agent), "environment_logs_dir", Path("/logs/agent"), create=True):
            return asyncio.run(self.agent._tail_once(env))

    def live(self):
        return (self.logs / opencode_live.LIVE_LOG).read_bytes()

    def test_copies_what_the_log_gains(self):
        env = self.environment()
        self.remote = b'{"type":"text"}\n{"type":"tool_'
        self.assertTrue(self.tail(env))
        self.remote += 'use","part":{"tool":"bash","state":{"output":"é"}}}\n'.encode()
        self.assertTrue(self.tail(env))
        self.assertTrue(self.tail(env))  # nothing new
        self.assertEqual(self.live(), self.remote)
        self.assertEqual(self.agent._live_offset, len(self.remote))

    def test_a_burst_over_one_chunk_is_copied_whole(self):
        self.remote = bytes(range(256)) * ((3 * opencode_live.TAIL_CHUNK) // 256 + 7)
        self.assertTrue(self.tail(self.environment()))
        self.assertEqual(self.live(), self.remote)
        self.assertEqual(len(self.execs), 4)

    def test_no_log_yet_writes_nothing(self):
        self.assertTrue(self.tail(self.environment(missing=True)))
        self.assertFalse((self.logs / opencode_live.LIVE_LOG).exists())

    def test_a_failed_exec_copies_nothing(self):
        self.remote = b'{"type":"text"}\n'
        self.assertTrue(self.tail(self.environment(return_code=1)))
        self.assertFalse((self.logs / opencode_live.LIVE_LOG).exists())
        self.assertEqual(self.agent._live_offset, 0)

    def test_a_mounted_log_is_not_copied(self):
        (self.logs / "opencode.txt").write_text('{"type":"text"}\n')
        self.assertFalse(self.tail(self.environment()))
        self.assertEqual(self.execs, [])
        self.assertFalse((self.logs / opencode_live.LIVE_LOG).exists())


@unittest.skipUnless(HARBOR_INSTALLED, "needs harbor")
class Stall(unittest.TestCase):
    """An attempt whose event log stops growing ends as stalled, which
    Harbor's retries run again; one whose log grows runs to its end."""

    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        self.log = Path(self.tmp.name) / "opencode.txt"
        self.agent = object.__new__(opencode_live.KavachOpenCode)
        self.agent.logs_dir = Path(self.tmp.name)
        self.agent.environment_logs_dir = Path("/logs/agent")
        self.agent._live_offset = 0
        patch = mock.patch.object(opencode_live, "STALL_CHECK_SEC", 0.01)
        patch.start()
        self.addCleanup(patch.stop)
        # The sandbox's own log, as stat reports it; None fails the exec.
        self.remote = [0]
        self.stats = 0

    def sandbox(self):
        test = self

        class Env:
            async def exec(self, command, timeout_sec=None):
                assert command.startswith("stat -c %s ") and "/logs/agent/opencode.txt" in command
                test.stats += 1
                size = test.remote[min(test.stats, len(test.remote)) - 1]
                if size is None:
                    return types.SimpleNamespace(return_code=1, stdout="")
                return types.SimpleNamespace(return_code=0, stdout=f"{size}\n")

        return Env()

    def run_for(self, awaitable, limit):
        return asyncio.run(self.agent._unless_stalled(awaitable, limit, self.sandbox()))

    @staticmethod
    async def hung():
        await asyncio.Event().wait()

    def test_a_silent_run_is_ended(self):
        cancelled = []

        async def hung():
            try:
                await asyncio.Event().wait()
            except asyncio.CancelledError:
                cancelled.append(True)
                raise

        with self.assertRaises(opencode_live.AgentStalledError):
            self.run_for(hung(), 0.1)
        self.assertEqual(cancelled, [True])

    def test_a_growing_log_keeps_it_going(self):
        async def busy():
            for _ in range(15):
                with self.log.open("a") as f:
                    f.write('{"type":"text"}\n')
                await asyncio.sleep(0.02)
            return "done"

        self.assertEqual(self.run_for(busy(), 0.1), "done")

    def test_the_copied_log_counts_where_none_is_mounted(self):
        async def busy():
            for _ in range(15):
                self.agent._live_offset += 10
                await asyncio.sleep(0.02)
            return "done"

        self.assertEqual(self.run_for(busy(), 0.1), "done")

    def test_a_stopped_copy_of_a_growing_log_keeps_it_going(self):
        # The copy froze at 100 bytes; the sandbox's log goes on growing.
        self.agent._live_offset = 100
        self.remote = list(range(500, 100_000, 400))

        async def busy():
            await asyncio.sleep(0.5)
            return "done"

        self.assertEqual(self.run_for(busy(), 0.05), "done")
        self.assertGreater(self.stats, 2)

    def test_a_stopped_copy_of_a_stopped_log_is_ended(self):
        self.agent._live_offset = 100
        self.remote = [500, 500]
        with self.assertRaises(opencode_live.AgentStalledError):
            self.run_for(self.hung(), 0.05)
        self.assertEqual(self.stats, 2)

    def test_a_log_that_cannot_be_measured_is_not_ended(self):
        self.remote = [None]

        async def slow():
            await asyncio.sleep(0.3)
            return "done"

        self.assertEqual(self.run_for(slow(), 0.05), "done")
        self.assertGreater(self.stats, 1)

    def test_a_mounted_log_needs_no_look_in_the_sandbox(self):
        self.log.write_text("x")
        with self.assertRaises(opencode_live.AgentStalledError):
            self.run_for(self.hung(), 0.05)
        self.assertEqual(self.stats, 0)

    def test_a_follow_whose_stream_never_ends_is_given_up(self):
        with mock.patch.object(opencode_live, "FOLLOW_SEC", 0.01), mock.patch.object(opencode_live, "FOLLOW_GRACE_SEC", 0.01):
            self.assertIsNone(asyncio.run(self.agent._follow_for(self.hung)))

    def test_the_run_s_own_error_is_raised(self):
        async def fails():
            raise RuntimeError("boom")

        with self.assertRaisesRegex(RuntimeError, "boom"):
            self.run_for(fails(), 0.1)

    def test_zero_never_ends_it(self):
        async def slow():
            await asyncio.sleep(0.15)
            return "done"

        self.assertEqual(self.run_for(slow(), 0), "done")

    def test_harbor_retries_it(self):
        from harbor.models.job.config import RetryConfig

        self.assertNotIn("AgentStalledError", RetryConfig().exclude_exceptions)


@unittest.skipUnless(HARBOR_INSTALLED, "harbor is not installed")
class FollowLog(LiveLog):
    """Where the provider streams (Daytona, Modal), the log is followed with
    tail -F on the sandbox's machine; it is polled before each follow and
    where it cannot be followed."""

    def setUp(self):
        super().setUp()
        # The Daytona SDK is there when Harbor runs on Daytona; the agent
        # takes only the request type from it.
        sdk = self.ns(SessionExecuteRequest=lambda **kw: self.ns(**kw))
        patcher = mock.patch.dict(sys.modules, {"daytona": sdk})
        patcher.start()
        self.addCleanup(patcher.stop)

    def daytona(self, chunks, exit_code=0, hang=False):
        test = self

        class Process:
            sessions, deleted, commands = [], [], []

            async def create_session(self, sid):
                self.sessions.append(sid)

            async def execute_session_command(self, sid, req):
                test.assertTrue(req.run_async)
                self.commands.append(req.command)
                return test.ns(cmd_id="c1")

            async def get_session_command_logs_async(self, sid, cid, on_stdout, on_stderr):
                for c in chunks:
                    on_stdout(c)
                if hang:
                    await asyncio.Event().wait()

            async def get_session_command(self, sid, cid):
                return test.ns(exit_code=exit_code)

            async def delete_session(self, sid):
                self.deleted.append(sid)

        return test.ns(process=Process())

    def modal(self, chunks, exit_code=0):
        test = self

        class Stream:
            def __aiter__(self):
                async def gen():
                    for c in chunks:
                        yield c

                return gen()

        class Wait:
            async def aio(self):
                return exit_code

        class Exec:
            calls = []

            async def aio(self, *args, **kwargs):
                self.calls.append((args, kwargs))
                return test.ns(stdout=Stream(), wait=Wait())

        return test.ns(exec=Exec())

    def env_of(self, kind, sandbox):
        env = self.environment()
        env.type = lambda: kind
        env._sandbox = sandbox
        return env

    def run_tail(self, env, rounds=2):
        """Run _tail until its rounds-th pause."""
        sleeps = 0

        async def sleep(_):
            nonlocal sleeps
            sleeps += 1
            if sleeps >= rounds:
                raise asyncio.CancelledError

        with mock.patch.object(type(self.agent), "environment_logs_dir", Path("/logs/agent"), create=True), \
                mock.patch.object(opencode_live.asyncio, "sleep", sleep):
            with self.assertRaises(asyncio.CancelledError):
                asyncio.run(self.agent._tail(env))

    def test_daytona_follows_from_the_end_of_the_copy(self):
        self.remote = b'{"type":"text"}\n'
        sandbox = self.daytona(['{"type":"tool_use","part":{"tool":"bash"}}\n', '{"type":"step_fi', 'nish"}\n'])
        self.run_tail(self.env_of(opencode_live.EnvironmentType.DAYTONA, sandbox), rounds=1)
        self.assertEqual(self.live(), b'{"type":"text"}\n{"type":"tool_use","part":{"tool":"bash"}}\n{"type":"step_finish"}\n')
        cmd = sandbox.process.commands[0]
        self.assertIn("tail -c +17 -F /logs/agent/opencode.txt", cmd)
        self.assertIn(f"timeout {opencode_live.FOLLOW_SEC} ", cmd)
        self.assertEqual(sandbox.process.deleted, sandbox.process.sessions)

    def test_a_cancelled_follow_deletes_its_session(self):
        self.remote = b'{"type":"text"}\n'
        sandbox = self.daytona(['{"type":"text"}\n'], hang=True)
        env = self.env_of(opencode_live.EnvironmentType.DAYTONA, sandbox)

        async def run():
            with mock.patch.object(type(self.agent), "environment_logs_dir", Path("/logs/agent"), create=True):
                task = asyncio.create_task(self.agent._tail(env))
                for _ in range(20):
                    await asyncio.sleep(0)
                task.cancel()
                with contextlib.suppress(asyncio.CancelledError):
                    await task

        asyncio.run(run())
        self.assertEqual(len(sandbox.process.sessions), 1)
        self.assertEqual(sandbox.process.deleted, sandbox.process.sessions)

    def test_modal_follows_in_bytes(self):
        self.remote = b'{"type":"text"}\n'
        sandbox = self.modal(['{"type":"text","part":{"text":"é"}}\n'.encode()[:20], '{"type":"text","part":{"text":"é"}}\n'.encode()[20:]])
        self.run_tail(self.env_of(opencode_live.EnvironmentType.MODAL, sandbox), rounds=1)
        self.assertEqual(self.live(), b'{"type":"text"}\n' + '{"type":"text","part":{"text":"é"}}\n'.encode())
        args, kwargs = sandbox.exec.calls[0]
        self.assertEqual(args[:2], ("sh", "-c"))
        self.assertFalse(kwargs["text"])

    def test_no_follow_before_the_log_exists(self):
        sandbox = self.daytona(["x"])
        self.run_tail(self.env_of(opencode_live.EnvironmentType.DAYTONA, sandbox), rounds=2)
        self.assertEqual(sandbox.process.commands, [])
        self.assertFalse((self.logs / opencode_live.LIVE_LOG).exists())

    def test_a_log_not_on_the_machine_is_polled(self):
        self.remote = b'{"type":"text"}\n'
        sandbox = self.daytona([], exit_code=opencode_live.NO_LOG_HERE)
        self.run_tail(self.env_of(opencode_live.EnvironmentType.DAYTONA, sandbox), rounds=3)
        self.assertEqual(len(sandbox.process.commands), 1)
        self.agent.logger.warning.assert_called_once()
        self.assertEqual(self.live(), self.remote)

    def test_other_providers_are_polled(self):
        env = self.env_of(opencode_live.EnvironmentType.DOCKER, object())
        self.assertIsNone(self.agent._follower(env))
        self.assertIsNone(self.agent._follower(self.environment()))


if __name__ == "__main__":
    unittest.main()
