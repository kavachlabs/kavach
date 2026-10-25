#!/usr/bin/env python3
"""Drafts a release's notes for the website's release notes page.

Reads a published GitHub release, finds the pull requests it lists, and writes
<out>/<version>.md in the form the website's content/releases/ expects: front
matter, then ## New / ## Fixed / ## Benchmark sections with one bullet per pull
request. The draft is marked `draft: true`, so the live site leaves it out until
someone rewrites it for readers and removes the mark.

Usage:
  scripts/draft-release-notes.py --tag v0.1.4 --out ../web/content/releases

It needs the gh CLI, signed in (GH_TOKEN in CI). It never overwrites an
existing file, since that may be notes someone has already rewritten.
"""

import argparse
import json
import re
import subprocess
import sys
from pathlib import Path

# Pull requests whose scope is one of these are about the agent benchmark and its console.
BENCHMARK_SCOPES = {"bench", "console", "trial", "agent", "bench-images", "bench images", "bench export"}
# Pull requests about the repository itself, not the release; they are listed in the
# pull request that carries the draft, not in the notes.
INTERNAL_SCOPES = {"ci", "test", "chore"}


def gh(*args):
    out = subprocess.run(["gh", *args], check=True, capture_output=True, text=True).stdout
    return json.loads(out)


def scopes(title):
    """The scopes before the title's colon: "bench console: x" is {bench, console, bench console}."""
    head, sep, _ = title.partition(":")
    if not sep:
        return set()
    head = re.sub(r"^(feat|fix|perf|docs)\((.+)\)$", r"\1, \2", head.strip())
    parts = [p.strip().lower() for p in head.split(",")]
    words = {w for p in parts for w in p.split()}
    return set(parts) | words


def section(pr):
    title = pr["title"]
    found = scopes(title)
    labels = {l["name"].lower() for l in pr.get("labels", [])}
    if found and found <= INTERNAL_SCOPES:
        return None
    if "fix" in found or "bug" in labels or re.match(r"(?i)fix(es|ed)?\b", title):
        return "Fixed"
    if "perf" in found or "performance" in labels:
        return "Faster"
    if found & BENCHMARK_SCOPES and not found & {"replay", "cmd/kavach", "kavach mcp", "sdk", "skills"}:
        return "Benchmark"
    return "New"


def lead(title):
    """The title without its scope, as a bullet's bold lead: "bench: add x" becomes "Add x"."""
    _, sep, rest = title.partition(":")
    text = (rest if sep else title).strip().rstrip(".")
    return text[:1].upper() + text[1:]


def main():
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("--tag", required=True, help="the release's tag, e.g. v0.1.4")
    ap.add_argument("--repo", default="kavachlabs/kavach")
    ap.add_argument("--out", required=True, help="the website's content/releases directory")
    args = ap.parse_args()

    version = args.tag.removeprefix("v")
    path = Path(args.out) / f"{version}.md"
    if path.exists():
        print(f"{path} exists; leaving it as it is", file=sys.stderr)
        return

    release = gh("release", "view", args.tag, "--repo", args.repo, "--json", "body,publishedAt,url")
    numbers = list(dict.fromkeys(int(n) for n in re.findall(r"/pull/(\d+)", release["body"] or "")))
    prs = [gh("pr", "view", str(n), "--repo", args.repo, "--json", "number,title,url,labels") for n in numbers]

    sections = {"New": [], "Faster": [], "Fixed": [], "Benchmark": []}
    left_out = []
    for pr in prs:
        name = section(pr)
        if name is None:
            left_out.append(pr)
            continue
        sections[name].append(f"- **{lead(pr['title'])}.** TODO: what this changes for someone using Kavach. ([#{pr['number']}]({pr['url']}))")

    date = (release["publishedAt"] or "")[:10]
    lines = [
        "---",
        f"version: {version}",
        f"date: {date}",
        "headline: \"TODO: one line on what this release lets people do.\"",
        "draft: true",
        "---",
        "",
    ]
    for name, bullets in sections.items():
        if bullets:
            lines += [f"## {name}", "", *bullets, ""]
    if not any(sections.values()):
        lines += ["## New", "", f"- **TODO.** The release lists no pull requests; see {release['url']}.", ""]

    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_text("\n".join(lines))
    print(path)
    for pr in left_out:
        print(f"left out: #{pr['number']} {pr['title']}", file=sys.stderr)


if __name__ == "__main__":
    main()
