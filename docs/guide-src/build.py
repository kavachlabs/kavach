"""Build the Kavach repository guide PDF.

    uv run --with markdown --with pygments --with pymupdf python build.py <repo> <out.pdf>

Chapters are Markdown files in chapters/, in name order, with these extra
block directives, each on a line of its own:

    @spec 3.1                 SPEC.md section 3.1 with its subsections, verbatim
    @spec-only 3              SPEC.md section 3 up to its first subsection
    @code path 10-80          lines 10..80 of a repo file
    @code path func:Name      a Go func (or method) Name, through its closing brace
    @code path type:Name      a Go type declaration
    @code path all            the whole file
    @flow A -> B -> C | caption        a box-and-arrow figure
    :::trap Title / :::note Title / :::try Title   ...   :::   callouts

Two passes: the second fills the table of contents with the page numbers the
first one produced, and adds PDF bookmarks.
"""

import html
import os
import re
import subprocess
import sys

import markdown
import graphs
import pymupdf
from pygments import highlight
from pygments.formatters import HtmlFormatter
from pygments.lexers import get_lexer_for_filename, TextLexer

HERE = os.path.dirname(os.path.abspath(__file__))
REPO, OUT = sys.argv[1], sys.argv[2]
CHROME = "/Applications/Google Chrome.app/Contents/MacOS/Google Chrome"

spec_lines = open(os.path.join(REPO, "SPEC.md")).read().split("\n")


def spec_section(num, only=False):
    start = level = None
    for i, l in enumerate(spec_lines):
        m = re.match(r"^(#{2,4}) (.*)$", l)
        if not m:
            continue
        if start is None:
            title = m.group(2)
            if title.startswith(num + " ") or title.startswith(num + ". "):
                start, level = i, len(m.group(1))
        elif only or len(m.group(1)) <= level:
            return title_of(start), "\n".join(spec_lines[start + 1:i])
    if start is None:
        raise SystemExit(f"no SPEC.md section {num}")
    return title_of(start), "\n".join(spec_lines[start + 1:])


def title_of(i):
    return re.sub(r"^#+ ", "", spec_lines[i])


def md(text):
    return markdown.markdown(text, extensions=["tables", "fenced_code", "codehilite"],
                             extension_configs={"codehilite": {"guess_lang": False, "css_class": "hl"}})


def demote(h, by):
    return re.sub(r"<(/?)h([1-6])", lambda m: f"<{m.group(1)}h{min(6, int(m.group(2)) + by)}", h)


def code_block(path, sel):
    src = open(os.path.join(REPO, path)).read().split("\n")
    if sel == "all":
        a, b = 1, len(src)
    elif sel.startswith(("func:", "type:")):
        kind, name = sel.split(":", 1)
        pat = (re.compile(rf"^func (\([^)]*\) )?{re.escape(name)}[\[(]") if kind == "func"
               else re.compile(rf"^type {re.escape(name)}\b"))
        a = next((i for i, l in enumerate(src) if pat.match(l)), None)
        if a is None:
            raise SystemExit(f"{path}: no {sel}")
        while a > 0 and src[a - 1].startswith("//"):
            a -= 1
        b = a
        while b < len(src) and src[b].startswith("//"):
            b += 1
        opener = src[b].rstrip()
        if opener.endswith("{") or opener.endswith("("):
            close = "}" if opener.endswith("{") else ")"
            while not src[b].startswith(close):
                b += 1
        a, b = a + 1, b + 1
    else:
        a, b = (int(x) for x in sel.split("-"))
    body = "\n".join(src[a - 1:b])
    try:
        lexer = get_lexer_for_filename(path)
    except Exception:
        lexer = TextLexer()
    fmt = HtmlFormatter(linenos="inline", linenostart=a, cssclass="hl", wrapcode=True)
    where = f"{path}" + ("" if sel == "all" else f" : {a}–{b}")
    return f'<div class="code"><div class="code-title">{html.escape(where)}</div>{highlight(body, lexer, fmt)}</div>'


def flow(spec):
    parts, _, caption = spec.partition("|")
    boxes = [p.strip() for p in parts.split("->")]
    inner = '<span class="arrow">→</span>'.join(f'<span class="box">{html.escape(b)}</span>' for b in boxes)
    cap = f'<div class="caption">{html.escape(caption.strip())}</div>' if caption.strip() else ""
    return f'<figure class="flow"><div class="boxes">{inner}</div>{cap}</figure>'


def render_body(lines):
    out, buf = [], []

    def flush():
        if buf:
            out.append(md("\n".join(buf)))
            buf.clear()

    i = 0
    while i < len(lines):
        l = lines[i]
        if l.startswith("@spec"):
            flush()
            only = l.startswith("@spec-only")
            num = l.split()[1]
            t, body = spec_section(num, only)
            out.append(f'<div class="spec"><div class="spec-title">SPEC.md §{html.escape(t)}</div>{demote(md(body), 2)}</div>')
        elif l.startswith("@code "):
            flush()
            _, path, sel = l.split(None, 2)
            out.append(code_block(path, sel.strip()))
        elif l.startswith(("@graph ", "@graphify ")):
            flush()
            out.append(graphs.directive(REPO, l))
        elif l.startswith("@flow "):
            flush()
            out.append(flow(l[6:]))
        elif l.startswith(":::") and l.strip() != ":::":
            flush()
            kind, _, head = l[3:].partition(" ")
            j = i + 1
            while lines[j].strip() != ":::":
                j += 1
            body = md("\n".join(lines[i + 1:j]))
            out.append(f'<div class="callout {kind}"><div class="callout-title">{html.escape(head)}</div><div class="callout-body">{body}</div></div>')
            i = j
        else:
            buf.append(l)
        i += 1
    flush()
    return "\n".join(out)


def render_chapter(n, text):
    lines = text.split("\n")
    title = lines[0].lstrip("# ").strip()
    subtitle = ""
    i = 1
    if i < len(lines) and lines[i].startswith("> "):
        subtitle = lines[i][2:].strip()
        i += 1
    body = render_body(lines[i:])
    # Number the chapter's sections: ## -> h2 "n.k".
    k = [0]

    def num_h2(m):
        k[0] += 1
        return f'<h2 id="s{n}-{k[0]}"><span class="secnum">{n}.{k[0]}</span>{m.group(1)}</h2>'
    body = re.sub(r"<h2>(.*?)</h2>", num_h2, body)
    sections = re.findall(rf'<h2 id="s{n}-(\d+)"><span class="secnum">[^<]*</span>(.*?)</h2>', body)
    head = (f'<section class="chapter" id="ch{n}"><div class="chapnum">Chapter {n}</div>'
            f'<h1>{html.escape(title)}</h1>' + (f'<p class="chapsub">{html.escape(subtitle)}</p>' if subtitle else ""))
    return title, sections, head + body + "</section>"


def build(pages=None):
    files = sorted(f for f in os.listdir(os.path.join(HERE, "chapters")) if f.endswith(".md"))
    pre = open(os.path.join(HERE, "chapters", files[0])).read() if files[0].startswith("00") else ""
    chapters = []
    for f in files:
        if f.startswith("00"):
            continue
        chapters.append(render_chapter(len(chapters) + 1, open(os.path.join(HERE, "chapters", f)).read()))
    toc = ['<section class="toc"><h1 class="toch">Contents</h1>']
    for n, (t, secs, _) in enumerate(chapters, 1):
        p = (pages or {}).get(f"ch{n}", "")
        toc.append(f'<div class="toc-ch"><span class="n">{n}</span><span class="t">{html.escape(t)}</span><span class="p">{p}</span></div>')
        for k, st in secs:
            p = (pages or {}).get(f"s{n}-{k}", "")
            toc.append(f'<div class="toc-s"><span class="n">{n}.{k}</span><span class="t">{re.sub("<[^>]+>", "", st)}</span><span class="dots"></span><span class="p">{p}</span></div>')
    toc.append("</section>")
    css = open(os.path.join(HERE, "style.css")).read() + HtmlFormatter(style="friendly").get_style_defs(".hl")
    cover = open(os.path.join(HERE, "cover.html")).read()
    doc = (f'<!doctype html><html><head><meta charset="utf-8"><title>Kavach Repository Guide</title>'
           f'<style>{css}</style></head><body>{cover}{"".join(toc)}'
           f'<section class="preface">{render_body(pre.split(chr(10)))}</section>' + "".join(c[2] for c in chapters) + "</body></html>")
    path = os.path.join(HERE, "guide.html")
    open(path, "w").write(doc)
    subprocess.run([CHROME, "--headless=new", "--disable-gpu", "--no-pdf-header-footer",
                    f"--print-to-pdf={OUT}", "--virtual-time-budget=5000", "file://" + path],
                   check=True, capture_output=True)
    return chapters


def locate(chapters):
    """Page of each chapter and section heading, found by its text."""
    d = pymupdf.open(OUT)
    pages, toc = {}, []
    texts = [d[i].get_text() for i in range(d.page_count)]
    start = next(i for i, t in enumerate(texts) if "\nCHAPTER 1\n" in t)
    first = start
    for n, (t, secs, _) in enumerate(chapters, 1):
        while f"\nCHAPTER {n}\n" not in texts[first]:
            first += 1
        pages[f"ch{n}"] = first + 1
        toc.append([1, f"{n}  {t}", first + 1])
        cur = first
        for k, st in secs:
            plain = html.unescape(re.sub("<[^>]+>", "", st))
            label = f"{n}.{k}"
            while not re.search(rf"(^|\n){re.escape(label)}\s*\n?\s*{re.escape(plain[:20])}", texts[cur]):
                cur += 1
                if cur >= len(texts):
                    raise SystemExit(f"cannot find {label} {plain}")
            pages[f"s{n}-{k}"] = cur + 1
            toc.append([2, f"{label}  {plain}", cur + 1])
    return pages, toc, start


chapters = build()
pages, toc, start = locate(chapters)
build(pages)
d = pymupdf.open(OUT)
d.set_toc(toc)
d.set_metadata({"title": "Kavach: A Guided Tour of the Repository", "author": "Kavach Labs"})
d.save(OUT + ".tmp")
os.replace(OUT + ".tmp", OUT)
print(OUT, pymupdf.open(OUT).page_count, "pages")
