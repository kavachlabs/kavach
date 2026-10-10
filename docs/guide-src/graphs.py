"""Figures and listings from the Graphify code graph (graphify-out-core/graph.json).

    @graph files <prefix> [<prefix>...] | caption   files of these paths and the calls between them
    @graph calls <node-id> [depth] | caption        what a function calls and what calls it
    @graphify explain <node-id>                     the CLI's explanation of a node
    @graphify path <node-id> <node-id>              the CLI's shortest path between two nodes
"""

import collections
import html
import json
import os
import re
import subprocess

_g = None


def graph(repo):
    global _g
    if _g is None:
        _g = json.load(open(os.path.join(repo, "graphify-out-core", "graph.json")))
        _g["byid"] = {n["id"]: n for n in _g["nodes"]}
    return _g


def dot_svg(src):
    svg = subprocess.run(["dot", "-Tsvg"], input=src, capture_output=True, text=True, check=True).stdout
    svg = svg[svg.index("<svg"):]
    # Natural size, shrunk by CSS to the column when wider.
    return svg


DOT_HEAD = ('digraph G {\n graph [fontname="Charter", fontsize=10, nodesep=0.25, ranksep=0.45, pad=0.1, bgcolor="transparent"];\n'
            ' node [fontname="Menlo", fontsize=9, shape=box, style="rounded,filled", fillcolor="#eef3fa", color="#667788", penwidth=0.8, height=0.3];\n'
            ' edge [color="#556677", arrowsize=0.6, penwidth=0.7, fontname="Charter", fontsize=8];\n')


def q(s):
    return '"' + s.replace('"', '\\"') + '"'


def files_figure(repo, prefixes):
    g = graph(repo)
    inside = lambda f: f and any(f.startswith(p) for p in prefixes) and not f.endswith("_test.go")
    w = collections.Counter()
    seen = set()
    for l in g["links"]:
        if l["relation"] not in ("calls", "references", "indirect_call"):
            continue
        a, b = g["byid"].get(l["source"]), g["byid"].get(l["target"])
        if not a or not b:
            continue
        fa, fb = a.get("source_file"), b.get("source_file")
        if inside(fa):
            seen.add(fa)
        if inside(fb):
            seen.add(fb)
        if fa != fb and inside(fa) and inside(fb):
            w[(fa, fb)] += 1
    for n in g["nodes"]:
        f = n.get("source_file")
        if inside(f) and f.endswith((".go", ".py")):
            seen.add(f)
    groups = collections.defaultdict(list)
    for f in sorted(seen):
        groups[os.path.dirname(f)].append(f)
    out = [DOT_HEAD, ' rankdir=LR;\n']
    for i, (d, fs) in enumerate(sorted(groups.items())):
        out.append(f' subgraph cluster_{i} {{ label={q(d + "/")}; style="rounded"; color="#c5cfdb"; fontcolor="#1f3a5f";\n')
        for f in fs:
            out.append(f'  {q(f)} [label={q(os.path.basename(f))}];\n')
        out.append(" }\n")
    for (a, b), n in w.items():
        out.append(f' {q(a)} -> {q(b)} [penwidth={min(0.6 + n / 6, 3):.2f}, tooltip="{n} calls"];\n')
    out.append("}\n")
    return dot_svg("".join(out))


def calls_figure(repo, center, depth=1, limit=28):
    g = graph(repo)
    if center not in g["byid"]:
        raise SystemExit(f"graph has no node {center}")
    out_e = collections.defaultdict(list)
    in_e = collections.defaultdict(list)
    for l in g["links"]:
        if l["relation"] in ("calls", "indirect_call") and l["source"] in g["byid"] and l["target"] in g["byid"]:
            out_e[l["source"]].append(l["target"])
            in_e[l["target"]].append(l["source"])
    nodes, edges = {center}, set()
    frontier = [center]
    for _ in range(depth):
        nxt = []
        for n in frontier:
            for t in out_e[n]:
                if len(nodes) < limit or t in nodes:
                    edges.add((n, t))
                    if t not in nodes:
                        nodes.add(t)
                        nxt.append(t)
        frontier = nxt
    for s in in_e[center]:
        if len(nodes) < limit + 6:
            nodes.add(s)
            edges.add((s, center))

    def label(i):
        n = g["byid"][i]
        f = n.get("source_file", "")
        loc = n.get("source_location", "").lstrip("L")
        return f'{n["label"]}\n{f}{":" + loc if loc else ""}'
    out = [DOT_HEAD, ' rankdir=LR;\n']
    for i in nodes:
        style = ' fillcolor="#1f3a5f", fontcolor="white", color="#1f3a5f"' if i == center else (
            ' fillcolor="#fdf3e4"' if (i, center) in edges else "")
        out.append(f' {q(i)} [label={q(label(i))}{style}];\n')
    for a, b in sorted(edges):
        out.append(f' {q(a)} -> {q(b)};\n')
    out.append("}\n")
    return dot_svg("".join(out))


def cli(repo, args):
    r = subprocess.run(["graphify", *args, "--graph", os.path.join(repo, "graphify-out-core", "graph.json")],
                       capture_output=True, text=True, cwd=repo)
    text = re.sub(r"\x1b\[[0-9;]*m", "", r.stdout or r.stderr).rstrip()
    if r.returncode != 0 or not text:
        raise SystemExit(f"graphify {' '.join(args)} failed: {text}")
    return text


def directive(repo, line):
    """HTML for one @graph or @graphify line."""
    body, _, caption = line.partition("|")
    words = body.split()
    cap = f'<p class="caption">{html.escape(caption.strip())}</p>' if caption.strip() else ""
    if words[0] == "@graph":
        if words[1] == "files":
            svg = files_figure(repo, words[2:])
        else:
            svg = calls_figure(repo, words[2], int(words[3]) if len(words) > 3 else 1)
        return f'<figure class="diagram graph">{svg}{cap}</figure>'
    text = cli(repo, words[1:])
    title = "graphify " + " ".join(words[1:])
    return (f'<div class="code graphify"><div class="code-title">$ {html.escape(title)}</div>'
            f'<div class="hl"><pre>{html.escape(text)}</pre></div></div>')
