# Trigger eval

Does a coding agent reach for Kavach when the user describes the problem in
their own words, and leave it alone when Kavach doesn't fit? An agent picks a
skill or an MCP tool from its description alone, so this measures the
descriptions in `skills/*/SKILL.md`, `.claude-plugin/` and `kavach mcp`'s
instructions.

`prompts.jsonl` holds 40 prompts. 24 are problems Kavach fits: bugs that won't
reproduce, record and replay, features that must preserve a stateful
service's behaviour, and invariants. 16 are ordinary tasks it doesn't fit. Each
runs in a fresh copy of its workspace under `workspaces/`, small repos that
never mention Kavach: a Go Kafka consumer, a Python Stripe webhook service, a
React dashboard and some Python scripts. The agent gets the Kavach plugin and
MCP server and nothing else from the user's setup. Edits and shell commands are
denied. A run stops at the agent's first decision: it used Kavach (a skill, an
MCP tool or the CLI), it started editing without it, or it ran out of turns.

`--distractors` also installs `distractors/`, generic debugging, testing and
verification skills that compete for the same prompts, as they do in a real
setup. Use it for every comparison.

```bash
go build -o /tmp/kavach ./cmd/kavach
python3 bench/trigger/run.py --plugin-dir . --kavach-bin /tmp/kavach \
  --distractors --repeat 3 --out /tmp/trigger.jsonl
```

To compare against the published descriptions, export them and run again:

```bash
mkdir /tmp/old && git archive HEAD .claude-plugin skills commands | tar -x -C /tmp/old
git stash && go build -o /tmp/kavach-old ./cmd/kavach && git stash pop
python3 bench/trigger/run.py --plugin-dir /tmp/old --kavach-bin /tmp/kavach-old \
  --distractors --repeat 3 --out /tmp/trigger-old.jsonl
```

It runs Claude Code (`claude -p`); pass `--model` to pick the model. The
prompts are ours, so a high score means the descriptions match how we think
users talk, not proof of how they do.
