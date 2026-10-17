"""Tests for the OpenCode agent's pieces that need neither a model nor Harbor.

    python3 bench/agent/harbor/agents/test_agents.py

The isolation test brings up a cut-down copy of a task's Compose topology with
`docker compose` (a small image in place of the task's), so it is skipped
without Docker. It calls no model: the "upstream" is a local fake.
"""

import asyncio
import importlib.util
import ipaddress
import json
import subprocess
import sys
import tempfile
import threading
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


if __name__ == "__main__":
    unittest.main()
