#!/usr/bin/env python3
"""Gives every task the model proxy service and seals its main container.

    sync-tasks.py          apply to bench/agent/harbor/tasks/* and agents/testdata/hello
    sync-tasks.py --check  change nothing; exit 1 if any task differs

Per task this (idempotently):
  - appends the `proxy` service and the `sealed` (internal) and `upstream`
    networks to environment/docker-compose.yaml, and puts main on `sealed`;
  - copies agents/proxy.py to environment/proxy.py, because Compose binds it
    from there and a remote provider uploads only the environment directory;
  - drops `[agent] network_mode = "no-network"` from task.toml, which Daytona
    and Modal cannot apply to a Compose task (README.md, "Isolation");
  - points task.toml at the prebuilt images (image-tag.py): `docker_image` for
    the agent's image and `[verifier.environment] docker_image` for the
    grader's, and keeps `build:` out of both compose files, because a remote
    provider uploads only environment/ and tests/ and has no repository to
    build from (README.md, "Images"). The tags are content keys, so this
    check fails after a change to a Dockerfile or a file it copies; run
    sync-tasks.py to update them;
  - gives task.toml a second `[[verifier.collect]]` hook that copies the final
    version of every changed source file to /logs/artifacts/source, for the
    bench console. It is derived from the patch hook, so it starts from the
    same commit.

The hello smoke-test task also gets `host.docker.internal` on the proxy, for
the local stub model.
"""

import importlib.util
import re
import sys
from pathlib import Path

HERE = Path(__file__).resolve().parent
PROXY = HERE / "agents" / "proxy.py"
HELLO = HERE / "agents" / "testdata" / "hello"
_spec = importlib.util.spec_from_file_location("image_tag", HERE / "image-tag.py")
image_tag = importlib.util.module_from_spec(_spec)
_spec.loader.exec_module(image_tag)
TASKS = sorted(p for p in (HERE / "tasks").iterdir() if p.is_dir()) + [HELLO]

MAIN_NETWORKS = """    # No route out: main shares only an internal network with the proxy.
    networks: [sealed]
    # No raw sockets, which could address frames past the proxy to the host.
    cap_drop: [NET_RAW]
"""
PROXY_SYSCTLS = """    # Not a router: frames sent to the proxy's MAC are not forwarded.
    sysctls:
      net.ipv4.ip_forward: "0"
      net.ipv6.conf.all.forwarding: "0"
"""
PROXY_SERVICE = """
  # The OpenCode agent (bench/agent/harbor/agents) starts proxy.py here with
  # the API key in that command's environment. main never has the key and
  # reaches the model only through this service.
  proxy:
    image: python:3.12-alpine
    init: true
    command: ["tail", "-f", "/dev/null"]
    volumes:
      - ./proxy.py:/proxy.py:ro
{extra}{sysctls}    networks: [sealed, upstream]

networks:
  sealed:
    internal: true
    # No address on the host's side of the bridge: main cannot reach the Docker
    # host, which on a remote provider is the sandbox VM and has full egress.
    driver_opts:
      com.docker.network.bridge.inhibit_ipv4: "true"
  upstream: {{}}
"""
HELLO_COMPOSE = """# Smoke-test task: main is built from the Dockerfile beside this file.
services:
  main:
"""
HOST_GATEWAY = """    extra_hosts:
      - host.docker.internal:host-gateway
"""
NEW_COMMENT = """# No network while the agent works: main's Compose network is internal and
# shared only with the model proxy service, which holds the API key
# (environment/docker-compose.yaml; bench/agent/harbor/agents/README.md).
"""
ENV_HEADER = """# main is the prebuilt image named by docker_image in ../task.toml, which
# sync-tasks.py keeps current; build it with bench/agent/harbor/image-tag.py.
"""
LEADING_COMMENT = re.compile(r"\A(?:#[^\n]*\n)+(?=services:)")
BUILD_BLOCK = re.compile(
    r"    build:\n      context: [^\n]*\n      dockerfile: [^\n]*\n      target: [^\n]*\n"
)
IMAGE_COMMENT = "# Prebuilt, so a remote provider pulls it and never builds (README.md, \"Images\").\n"
ENV_IMAGE = re.compile(r"(?:#[^\n]*\n)?docker_image = [^\n]*\n")
VERIFIER_ENV = re.compile(r"\[verifier\.environment\]\n(?:[^\n\[][^\n]*\n)*\n")
AGENT_NETWORK = re.compile(
    r"(?:#[^\n]*\n)*network_mode = \"no-network\"\n"
)
PATCH_HOOK = re.compile(
    r'(\[\[verifier\.collect\]\]\nuser = "root"\n'
    r'command = "cd (?P<dir>\S+) && git -c safe\.directory=\S+ add -A && '
    r'git -c safe\.directory=\S+ diff --cached --binary (?P<base>.+?) > /logs/artifacts/patch\.diff"\n)'
)
SOURCE_HOOK = re.compile(r"\n# Also the final version of every file[^\n]*\n(?:#[^\n]*\n)*\[\[verifier\.collect\]\]\n[^\n]*\ncommand = [^\n]*/logs/artifacts/source[^\n]*\n")
SOURCE_COMMENT = """
# Also the final version of every file changed since the commit the patch
# starts from (for the Kavach arms, the parent of the integration commit, so
# its files are included): whole, regular files only, not deleted or binary,
# up to 1 MB each and 32 MB in all, so the console can show the agent's
# source without the task's image.
"""
SOURCE_COMMAND = (
    'cd {dir} && g=\\"git -c safe.directory={dir} -c core.quotepath=false\\" && $g add -A && '
    "b={base} && mkdir -p /logs/artifacts/source && t=0 && "
    '$g diff --cached --numstat --no-renames --diff-filter=d $b | while IFS=$(printf \\"\\\\t\\") read -r a r f; do '
    '[ \\"$a\\" != - ] && [ -f \\"$f\\" ] && [ ! -L \\"$f\\" ] && s=$(wc -c < \\"$f\\") && '
    "[ $s -le 1048576 ] && [ $((t+s)) -le 33554432 ] && t=$((t+s)) && "
    'mkdir -p \\"/logs/artifacts/source/$(dirname \\"$f\\")\\" && cp \\"$f\\" \\"/logs/artifacts/source/$f\\" && '
    'chmod 644 \\"/logs/artifacts/source/$f\\"; done; true'
)


def source_hook(text: str) -> str:
    text = SOURCE_HOOK.sub("", text)
    m = PATCH_HOOK.search(text)
    base = m["base"].replace("$(git -c safe.directory=" + m["dir"] + " ", "$($g ")
    if "--grep" in base:
        base += "^"
    cmd = SOURCE_COMMAND.format(dir=m["dir"], base=base)
    return text[: m.end()] + SOURCE_COMMENT + '[[verifier.collect]]\nuser = "root"\ncommand = "' + cmd + '"\n' + text[m.end() :]


SEALED = """  sealed:
    internal: true
"""
SEALED_NO_HOST = SEALED + """    # No address on the host's side of the bridge: main cannot reach the Docker
    # host, which on a remote provider is the sandbox VM and has full egress.
    driver_opts:
      com.docker.network.bridge.inhibit_ipv4: "true"
"""


def compose_for(task: Path, text: str) -> str:
    if task != HELLO:
        text = BUILD_BLOCK.sub("", text)
        text = LEADING_COMMENT.sub(ENV_HEADER, text, count=1)
    if "networks: [sealed]" in text:
        text = text.replace(SEALED + "  upstream", SEALED_NO_HOST + "  upstream")
        if "cap_drop" not in text:
            text = text.replace("    networks: [sealed]\n", MAIN_NETWORKS.split("\n", 1)[1])
            text = text.replace("    networks: [sealed, upstream]\n", PROXY_SYSCTLS + "    networks: [sealed, upstream]\n")
        return text
    if task == HELLO:
        text = HELLO_COMPOSE
    return text + MAIN_NETWORKS + PROXY_SERVICE.format(extra=HOST_GATEWAY if task == HELLO else "", sysctls=PROXY_SYSCTLS)


def toml_for(task: Path, text: str) -> str:
    text = AGENT_NETWORK.sub(NEW_COMMENT, text)
    if task == HELLO:
        return text
    arm = re.search(r'^arm = "([^"]+)"', text, re.M).group(1)
    name = task.name.removesuffix("-" + arm)
    text = VERIFIER_ENV.sub("", ENV_IMAGE.sub("", text))
    env = re.search(r"^\[environment\]\n((?:[a-z_]+ = [^\n]*\n)+)", text, re.M)
    fields = env.group(1)
    text = text[: env.start(1)] + f'{IMAGE_COMMENT}docker_image = "{image_tag.ref(name, arm)}"\n' + text[env.start(1) :]
    block = f'[verifier.environment]\n{IMAGE_COMMENT}docker_image = "{image_tag.ref(name, "grader")}"\n{fields}\n'
    return source_hook(text.replace("[[verifier.collect]]", block + "[[verifier.collect]]", 1))


def main() -> int:
    check = "--check" in sys.argv[1:]
    stale = []
    for task in TASKS:
        env = task / "environment"
        compose = env / "docker-compose.yaml"
        toml = task / "task.toml"
        want = {
            compose: compose_for(task, compose.read_text() if compose.exists() else ""),
            toml: toml_for(task, toml.read_text()),
            env / "proxy.py": PROXY.read_text(),
        }
        tests = task / "tests" / "docker-compose.yaml"
        if task != HELLO:
            want[tests] = BUILD_BLOCK.sub("", tests.read_text())
        for path, text in want.items():
            if not path.exists() or path.read_text() != text:
                stale.append(path)
                if not check:
                    path.write_text(text)
    for path in stale:
        print(("differs: " if check else "wrote: ") + str(path.relative_to(HERE)))
    if check and stale:
        print("run bench/agent/harbor/sync-tasks.py to update them", file=sys.stderr)
    return 1 if check and stale else 0


if __name__ == "__main__":
    sys.exit(main())
