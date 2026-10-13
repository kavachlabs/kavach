# Agent commands

`kavach-implement.md` is the `/kavach-implement` command: contract-first
feature work with `kavach contract` (AGENTS.md, "Building a feature"). It is
plain Markdown with `$ARGUMENTS`, so it works as a command in agents that
read Markdown commands:

```bash
# Claude Code, for one project or for every project
cp commands/kavach-implement.md .claude/commands/
cp commands/kavach-implement.md ~/.claude/commands/

# OpenCode
mkdir -p .opencode/command && cp commands/kavach-implement.md .opencode/command/
```

Then `/kavach-implement docs/hsetex.md`. The command needs the `kavach` CLI on
`PATH`, or the MCP server (`kavach mcp`) registered.
