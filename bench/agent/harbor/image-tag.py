#!/usr/bin/env python3
"""Names and builds the bench task images.

    image-tag.py ref   TASK TARGET   print ghcr.io/kavachlabs/bench-TASK:TARGET-KEY
    image-tag.py list                print every task x target as JSON (for CI)
    image-tag.py build TASK [TARGET...]   docker build them from the working tree

KEY is a hash of the task's Dockerfile, of every repository file its COPY
lines pull in (path, executable bit and content, from the working tree with
`git hash-object`) and of the build context's .dockerignore files, so it needs
no build and no network. The Dockerfile text is in it, so editing a FROM tag
changes the key; an upstream re-push of the same tag does not, and a rebuild
after the key changes picks up the newer content.
Task images are ../tasks/TASK/Dockerfile; targets are its stages.
`build` refuses when a file those COPY lines pull in is untracked or modified,
because the image would carry a key CI computes from the committed tree;
--allow-dirty overrides.
"""

import hashlib
import json
import os
import re
import subprocess
import sys
from pathlib import Path

HERE = Path(__file__).resolve().parent
ROOT = HERE.parents[2]
TASKS = ROOT / "bench" / "agent" / "tasks"
REGISTRY = "ghcr.io/kavachlabs"
SOURCE = "https://github.com/kavachlabs/kavach"
KEY_LEN = 12

_STAGE = re.compile(r"^FROM\s.*\bAS\s+(\S+)\s*$", re.M | re.I)
_VAR = re.compile(r"\$\{(\w+)\}|\$(\w+)")


def logical_lines(text: str) -> list[str]:
    return re.sub(r"\\\n", " ", text).splitlines()


def copy_sources(dockerfile: Path) -> list[str]:
    """Repo-relative paths named by COPY/ADD lines, other than --from copies.

    ARG follows Dockerfile scope: before the first FROM it is global; in a
    stage a redeclaration applies from that line on, and without a default it
    takes the global one if there is one.
    """
    glob: dict[str, str] = {}
    local: dict[str, str] | None = None
    sources = []

    def subst(word: str) -> str:
        def one(m):
            name = m.group(1) or m.group(2)
            if name not in local:
                sys.exit(f"image-tag: {dockerfile}: ${name} is not an ARG in scope")
            return local[name]
        return _VAR.sub(one, word)

    for line in logical_lines(dockerfile.read_text()):
        words = line.split()
        if not words:
            continue
        op = words[0].upper()
        if op == "FROM":
            local = {}
        elif op == "ARG":
            for w in words[1:]:
                name, has_default, default = w.partition("=")
                if local is None:
                    glob[name] = default.strip("\"'")
                elif has_default:
                    local[name] = default.strip("\"'")
                elif name in glob:
                    local[name] = glob[name]
        elif op in ("COPY", "ADD") and local is not None:
            flags = [w for w in words[1:] if w.startswith("--")]
            if any(f.startswith("--from") for f in flags):
                continue
            paths = [w for w in words[1:] if not w.startswith("--")]
            for src in paths[:-1]:
                sources.append(Path(subst(src)).as_posix().rstrip("/"))
    return sorted(set(sources))


def files_under(rel: str) -> list[str]:
    out = subprocess.run(
        ["git", "ls-files", "-z", "--", rel], cwd=ROOT, check=True, capture_output=True, text=True
    ).stdout
    files = [f for f in out.split("\0") if f and (ROOT / f).is_file()]
    if not files:
        sys.exit(f"image-tag: {rel} is copied by a Dockerfile but no tracked file is under it")
    return files


def ignore_files(task: str) -> list[str]:
    """The .dockerignore files Docker applies to this task's build context."""
    names = [".dockerignore", (TASKS / task / "Dockerfile").relative_to(ROOT).as_posix() + ".dockerignore"]
    return [n for n in names if (ROOT / n).is_file()]


def content_key(task: str) -> str:
    dockerfile = TASKS / task / "Dockerfile"
    files = {dockerfile.relative_to(ROOT).as_posix(), *ignore_files(task)}
    for src in copy_sources(dockerfile):
        files.update(files_under(src))
    files = sorted(files)
    blobs = subprocess.run(
        ["git", "hash-object", "--stdin-paths"],
        cwd=ROOT, check=True, capture_output=True, text=True, input="\n".join(files) + "\n",
    ).stdout.split()
    digest = hashlib.sha256()
    for name, blob in zip(files, blobs, strict=True):
        mode = "100755" if os.access(ROOT / name, os.X_OK) else "100644"
        digest.update(f"{name}\0{mode}\0{blob}\n".encode())
    return digest.hexdigest()[:KEY_LEN]


def ignored(path: str, patterns: list[str]) -> bool:
    """Whether .dockerignore patterns exclude path (last match wins, ! re-includes)."""
    result = False
    for pat in patterns:
        neg = pat.startswith("!")
        pat = pat.lstrip("!").strip("/")
        rx = re.escape(pat).replace(r"\*\*/", "(.*/)?").replace(r"\*\*", ".*").replace(r"\*", "[^/]*").replace(r"\?", "[^/]")
        if re.fullmatch(rx + "(/.*)?", path):
            result = not neg
    return result


def dirty_files(task: str) -> list[str]:
    """Untracked or modified files under the task's COPY sources that Docker would send."""
    ignore_file = (ROOT / ".dockerignore")
    specific = TASKS / task / "Dockerfile.dockerignore"
    if specific.is_file():
        ignore_file = specific
    patterns = []
    if ignore_file.is_file():
        patterns = [l.strip() for l in ignore_file.read_text().splitlines() if l.strip() and not l.startswith("#")]
    sources = copy_sources(TASKS / task / "Dockerfile") + [(TASKS / task / "Dockerfile").relative_to(ROOT).as_posix()]
    out = subprocess.run(
        ["git", "status", "--porcelain", "-z", "--untracked-files=all", "--", *sources],
        cwd=ROOT, check=True, capture_output=True, text=True,
    ).stdout
    entries = [e for e in out.split("\0") if e]
    return [e for e in entries if not ignored(e[3:], patterns)]


def targets(task: str) -> list[str]:
    return _STAGE.findall((TASKS / task / "Dockerfile").read_text())


def ref(task: str, target: str) -> str:
    if target not in targets(task):
        sys.exit(f"image-tag: {task} has no target {target}")
    return f"{REGISTRY}/bench-{task}:{target}-{content_key(task)}"


def platforms(task: str) -> list[str]:
    """arm64 is skipped when the Dockerfile pins its base to amd64."""
    pinned = "--platform=linux/amd64" in (TASKS / task / "Dockerfile").read_text()
    return ["linux/amd64"] if pinned else ["linux/amd64", "linux/arm64"]


def task_names() -> list[str]:
    return sorted(p.name for p in TASKS.iterdir() if (p / "Dockerfile").is_file())


# The targets Harbor tasks use; the rest of a Dockerfile's stages are build steps.
def harbor_targets(task: str) -> list[str]:
    return [t for t in targets(task) if t in ("control", "workflow", "kavach", "kavach-implement", "grader")]


def main(argv: list[str]) -> int:
    if len(argv) == 3 and argv[0] == "ref":
        print(ref(argv[1], argv[2]))
    elif argv == ["list"]:
        print(json.dumps([
            {"task": t, "target": g, "ref": ref(t, g), "platforms": ",".join(platforms(t))}
            for t in task_names() for g in harbor_targets(t)
        ], indent=1))
    elif len(argv) >= 2 and argv[0] == "build":
        task = argv[1]
        dirty = dirty_files(task)
        if dirty and "--allow-dirty" not in argv:
            sys.exit("image-tag: refusing to build; the image would carry CI's key but not the committed files:\n  "
                     + "\n  ".join(dirty) + "\ncommit or stash them, or pass --allow-dirty")
        argv = [a for a in argv if a != "--allow-dirty"]
        for target in argv[2:] or harbor_targets(task):
            r = ref(task, target)
            subprocess.run(
                ["docker", "build", "-f", str((TASKS / task / "Dockerfile").relative_to(ROOT)),
                 "--target", target, "--label", f"org.opencontainers.image.source={SOURCE}",
                 "-t", r, "."],
                cwd=ROOT, check=True,
            )
            print(r)
    else:
        print(__doc__, file=sys.stderr)
        return 2
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
