#!/usr/bin/env python3
"""The proxy container beside an attempt's environment. Standard library only;
it runs in a stock python image with this file mounted.

Two listeners:

  model (:8080)  The agent's only route to the model. Requests to /v1/<path>
                 go to <upstream>/<path> with the API key added; the key is
                 in this container's environment, never the agent's. The
                 agent sends a placeholder key, which is dropped.
  view (:8081)   A read-only window on the OpenCode server in the agent
                 container, published to the host on 127.0.0.1. GET, HEAD
                 and OPTIONS reach the server; any other method, websocket
                 upgrades and /pty (the web UI's terminal) are refused with
                 403, so a watcher cannot prompt, abort, revert or run
                 commands. Paths that are not part of OpenCode's API are the
                 web UI, fetched from app.opencode.ai as `opencode serve`
                 itself does.

Each listener can be limited to client addresses (--model-client,
--view-client), so the agent cannot use the view port or other containers
the model port.
"""

import argparse
import asyncio
import ipaddress
import json
import os
import ssl
import sys
import urllib.parse

# First path segments of OpenCode's server API (GET /doc in 1.1.8).
API = {
    "agent", "auth", "command", "config", "doc", "event", "experimental", "file",
    "find", "formatter", "global", "instance", "log", "lsp", "mcp", "path",
    "permission", "project", "provider", "pty", "question", "session", "tui", "vcs",
}
READ_METHODS = {"GET", "HEAD", "OPTIONS"}
HOP = {
    "connection", "keep-alive", "proxy-authorization", "proxy-connection", "te",
    "trailer", "transfer-encoding", "upgrade", "host", "content-length",
}


DEBUG = bool(os.environ.get("KAVACH_PROXY_DEBUG"))


def log(*parts):
    print(*parts, file=sys.stderr, flush=True)


class Request:
    def __init__(self, method, target, headers, body):
        self.method, self.target, self.headers, self.body = method, target, headers, body

    def header(self, name):
        name = name.lower()
        for k, v in self.headers:
            if k.lower() == name:
                return v
        return None


async def read_request(reader):
    head = await reader.readuntil(b"\r\n\r\n")
    lines = head.decode("latin-1").split("\r\n")
    method, target, _ = lines[0].split(" ", 2)
    headers = []
    for line in lines[1:]:
        if line:
            k, _, v = line.partition(":")
            headers.append((k.strip(), v.strip()))
    req = Request(method.upper(), target, headers, b"")
    if (req.header("transfer-encoding") or "").lower() == "chunked":
        body = bytearray()
        while True:
            size = int((await reader.readuntil(b"\r\n")).split(b";")[0], 16)
            if size == 0:
                await reader.readuntil(b"\r\n")
                break
            body += await reader.readexactly(size)
            await reader.readexactly(2)
        req.body = bytes(body)
    elif req.header("content-length"):
        req.body = await reader.readexactly(int(req.header("content-length")))
    return req


async def respond(writer, status, message):
    body = json.dumps({"error": message}).encode()
    writer.write(
        f"HTTP/1.1 {status} {'Forbidden' if status == 403 else 'Error'}\r\n"
        f"Content-Type: application/json\r\nContent-Length: {len(body)}\r\n"
        "Connection: close\r\n\r\n".encode() + body
    )
    await writer.drain()


async def forward(req, reader, writer, base, path, headers):
    """Send req to base (a parsed URL) at path, and copy the response back."""
    tls = base.scheme == "https"
    port = base.port or (443 if tls else 80)
    ctx = ssl.create_default_context() if tls else None
    up_reader, up_writer = await asyncio.open_connection(
        base.hostname, port, ssl=ctx, server_hostname=base.hostname if tls else None
    )
    host = base.hostname if port in (80, 443) else f"{base.hostname}:{port}"
    lines = [f"{req.method} {path} HTTP/1.1", f"Host: {host}", "Connection: close"]
    lines += [f"{k}: {v}" for k, v in headers if k.lower() not in HOP]
    if req.body or req.method in ("POST", "PUT", "PATCH"):
        lines.append(f"Content-Length: {len(req.body)}")
    up_writer.write(("\r\n".join(lines) + "\r\n\r\n").encode("latin-1") + req.body)
    await up_writer.drain()
    # A client that goes away (a watcher closing the TUI mid-stream) ends
    # the exchange, so streams such as /event do not outlive it.
    status = None
    gone = asyncio.ensure_future(reader.read(1))
    try:
        while True:
            chunk = asyncio.ensure_future(up_reader.read(65536))
            await asyncio.wait({chunk, gone}, return_when=asyncio.FIRST_COMPLETED)
            if gone.done() and gone.exception() is None and gone.result():
                # More bytes, not EOF: stop watching rather than misread them.
                gone = asyncio.get_running_loop().create_future()
            if not chunk.done() and gone.done():
                chunk.cancel()
                return status or "client closed"
            if not chunk.done():
                await chunk
            data = chunk.result()
            if not data:
                return status
            if status is None:
                status = data.split(b"\r\n", 1)[0].decode("latin-1", "replace")
            writer.write(data)
            await writer.drain()
    finally:
        gone.cancel()
        up_writer.close()


def allowed(writer, clients):
    if not clients:
        return True
    peer = ipaddress.ip_address(writer.get_extra_info("peername")[0])
    return any(peer in net for net in clients)


HANDLERS = set()


async def handle(reader, writer, route, clients, name):
    # asyncio holds connection handlers weakly; keep them until they finish.
    task = asyncio.current_task()
    HANDLERS.add(task)
    task.add_done_callback(HANDLERS.discard)
    try:
        if not allowed(writer, clients):
            log(name, "refused client", writer.get_extra_info("peername")[0])
            await respond(writer, 403, "client not allowed")
            return
        if DEBUG:
            log(name, "accepted", writer.get_extra_info("peername"))
        req = await read_request(reader)
        if DEBUG:
            log(name, "request", req.method, req.target, req.headers)
        result = await route(req, reader, writer)
        log(name, req.method, req.target.split("?")[0], "->", result)
    except (asyncio.IncompleteReadError, ConnectionError, ValueError) as e:
        log(name, "error", type(e).__name__, e)
    except OSError as e:
        log(name, "upstream error", e)
        try:
            await respond(writer, 502, f"upstream: {e}")
        except OSError:
            pass
    finally:
        writer.close()


def model_route(upstream, key):
    base = urllib.parse.urlsplit(upstream)
    prefix = base.path.rstrip("/")

    async def route(req, reader, writer):
        if not req.target.startswith("/v1/"):
            await respond(writer, 404, "only /v1/ is forwarded")
            return "404"
        headers = [(k, v) for k, v in req.headers if k.lower() not in ("authorization", "x-api-key")]
        # The key goes where the client put its placeholder: Authorization
        # for OpenAI-style APIs, x-api-key for Anthropic's.
        if key and req.header("x-api-key") is not None:
            headers.append(("x-api-key", key))
        elif key:
            headers.append(("Authorization", f"Bearer {key}"))
        return await forward(req, reader, writer, base, prefix + req.target[3:], headers)

    return route


def view_route(server, ui):
    server, ui = urllib.parse.urlsplit(server), urllib.parse.urlsplit(ui)

    async def route(req, reader, writer):
        path = urllib.parse.urlsplit(req.target).path
        segment = path.lstrip("/").split("/", 1)[0]
        if req.header("upgrade") or segment == "pty":
            await respond(writer, 403, "read-only view: terminals are not available")
            return "403"
        if req.method not in READ_METHODS:
            await respond(writer, 403, f"read-only view: {req.method} is not allowed")
            return "403"
        if segment in API:
            return await forward(req, reader, writer, server, req.target, req.headers)
        headers = [(k, v) for k, v in req.headers if k.lower() not in ("cookie", "authorization")]
        return await forward(req, reader, writer, ui, req.target, headers)

    return route


def networks(values):
    return [ipaddress.ip_network(v, strict=False) for v in values or []]


async def main():
    p = argparse.ArgumentParser()
    p.add_argument("--upstream", help="model endpoint base URL, e.g. https://openrouter.ai/api/v1")
    p.add_argument("--model-port", type=int, default=8080)
    p.add_argument("--model-client", action="append", help="address or CIDR allowed on the model port")
    p.add_argument("--opencode", help="OpenCode server URL, e.g. http://172.18.0.2:4096")
    p.add_argument("--view-port", type=int, default=8081)
    p.add_argument("--view-client", action="append", help="address or CIDR allowed on the view port")
    p.add_argument("--ui", default="https://app.opencode.ai")
    a = p.parse_args()
    key = os.environ.pop("KAVACH_UPSTREAM_KEY", "")
    servers = []
    if a.upstream:
        route, clients = model_route(a.upstream, key), networks(a.model_client)
        servers.append(await asyncio.start_server(
            lambda r, w: handle(r, w, route, clients, "model"), "0.0.0.0", a.model_port))
        log(f"model :{a.model_port} -> {a.upstream} (key {'set' if key else 'not set'})")
    if a.opencode:
        vroute, vclients = view_route(a.opencode, a.ui), networks(a.view_client)
        servers.append(await asyncio.start_server(
            lambda r, w: handle(r, w, vroute, vclients, "view"), "0.0.0.0", a.view_port))
        log(f"view :{a.view_port} -> {a.opencode} (read-only)")
    open("/tmp/ready", "w").close()
    await asyncio.gather(*(s.serve_forever() for s in servers))


if __name__ == "__main__":
    asyncio.run(main())
