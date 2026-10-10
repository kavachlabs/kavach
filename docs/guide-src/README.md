# Repository guide source

Builds `docs/Kavach_Repository_Guide.pdf`. Chapters are Markdown in `chapters/`;
`@spec` and `@code` directives pull SPEC.md sections and source lines in
verbatim, so rebuild after changing either. Directives are listed in the
`build.py` docstring.

    uv run --with markdown --with pygments --with pymupdf python build.py ../.. ../Kavach_Repository_Guide.pdf

Run it from this directory. The arguments are the repo root and the output
path. It needs Google Chrome to print the PDF; the macOS path is `CHROME` in
`build.py`.

Figures and `@graphify` listings also need Graphviz `dot`, the `graphify` CLI
on PATH, and `graphify-out-core/graph.json` at the repo root. That directory is
not in the repo. `graphify update .` should write `graphify-out/graph.json`
(its default); copy that directory to `graphify-out-core/`. Untested.
