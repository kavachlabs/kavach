# Kavach Agent Skills

Agent skills that teach AI coding agents ([Claude Code](https://code.claude.com/docs), [OpenAI Codex](https://github.com/openai/codex), [OpenCode](https://opencode.ai), Cursor, and Antigravity) how to onboard services with zero-`memrepo` driver capture, reproduce production crashes in microseconds, verify bug fixes across mutated retry and gateway-fault variants, declare and falsify invariants, build features against locked behavioural contracts, and verify other agents' patches with Kavach.

> **[Install](#install)** · **[Skills overview](#skills-overview)** · **[Starter prompts](#starter-prompts)** · **[MCP server](#mcp-server)** · **[Antigravity](#antigravity)**

## Install

### Single-command install (CLI + Agent Skills)

Install the `kavach` CLI, the `kavach-recorder` flight recorder, and all five Kavach agent skills in one command:

```bash
curl -fsSL https://www.thekavachlabs.com/install.sh | sh
```

### `npx skills` installer

Or install the skills into your coding agents via the standard `npx skills` CLI:

```bash
npx skills@latest add kavachlabs/kavach
# Or pick individual skills:
npx skills@latest add kavachlabs/kavach --skill kavach-setup --skill kavach-invariants
```

### Register the MCP server

Give your agent structured JSON verdicts via `kavach mcp`:

```bash
claude mcp add kavach -- kavach mcp
```

---

## Skills overview

| Skill | When to use | Primary tools used |
| --- | --- | --- |
| [`kavach-setup`](./kavach-setup/SKILL.md) | Onboard a service with v0.2 zero-`memrepo` driver capture (`kavach init`, `kavach doctor`, `github.com/kavachlabs/kavach-go`, `@kavachlabs/*`) in Go or TypeScript/Node 22, or wire a scoped `kavach-host` harness across 13 languages. | `kavach init`, `kavach doctor`, `kavach dev`, `kavach map`, `kavach replay`, `kavach diff` |
| [`kavach-invariants`](./kavach-invariants/SKILL.md) | Declare zero-boilerplate rules in `.kavach/invariants.yaml` (`at_most_once_emit`, `anti_lobotomy`, `non_negative`, `conservation`) and optional code invariants (`kv.Invariant` / `kv.invariant`), then falsify each one in milliseconds against 1-line mutants. | `.kavach/invariants.yaml`, `.kavach/plan/invariants.md`, `kavach replay`, `kavach diff` |
| [`kavach-fix`](./kavach-fix/SKILL.md) | Fix a production incident recorded in a `*.kavach` fixture and prove the patch survives 10+ incident variants and 48 neighbour regression variants (`verdict: "fixed"`). | `kavach inspect`, `kavach replay`, `kavach diff` (`kavach_list_incidents`, `kavach_replay`, `kavach_diff`) |
| [`kavach-implement`](./kavach-implement/SKILL.md) | Build a new feature contract-first: record baseline + feature traffic, declare spec invariants, pause for human `kavach contract approve`, and iterate until `kavach contract check` returns `pass`. | `kavach contract init`, `kavach contract show`, `kavach contract check` (`kavach_contract_show`, `kavach_contract_check`) |
| [`kavach-verify`](./kavach-verify/SKILL.md) | Check another agent's fix or feature without touching the code: replay the incident on the old and new builds, then post one fixed-format result naming the author (or `@coordinator [done]` when it holds). Built for the verifier role in an Antigravity `/swarm` or any agent pipeline. | `kavach replay`, `kavach diff`, `kavach contract check`, `kavach map` (`kavach_replay`, `kavach_diff`, `kavach_contract_check`) |

See [`examples/ledger/.kavach/plan/`](../examples/ledger/.kavach/plan/) ([`state-map.md`](../examples/ledger/.kavach/plan/state-map.md) and [`invariants.md`](../examples/ledger/.kavach/plan/invariants.md)) for a complete example of the human-reviewable planning artifacts generated before code changes.

---

## Starter prompts

### `kavach-setup`

```text
/kavach-setup Run kavach init and kavach doctor on this service, wrap our existing Postgres, Redis, Kafka, and HTTP drivers at initialization with zero business-logic edits and zero memrepo mocks, record a local capsule with kavach dev, and verify offline replay.
```

### `kavach-invariants`

```text
/kavach-invariants Write declarative rules in .kavach/invariants.yaml for our billing pipeline, record the blueprint in .kavach/plan/invariants.md, and falsify each invariant in milliseconds against 1-line mutants with kavach diff.
```

### `kavach-fix`

```text
/kavach-fix Reproduce the crash in testdata/null-amount.kavach and fix the handler until kavach diff reports verdict: fixed.
```

### `kavach-implement`

```text
/kavach-implement Implement the feature described in docs/spec.md contract-first with Kavach. Stop after kavach contract show so I can approve the contract before you write the implementation.
```

### `kavach-verify`

```text
/kavach-verify Check the ledger worker's fix in #12 against testdata/null-amount.kavach and post the verdict to the board.
```

In an Antigravity `/swarm`, ask the coordinator for a Kavach verifier:

```text
/swarm Fix the incident in testdata/null-amount.kavach. Add a worker named verifier whose brief is: load the kavach-verify skill and check every [done] post from the fixer.
```

---

## MCP server

All five skills work with both the `kavach` CLI and the `kavach mcp` Model Context Protocol server (`claude mcp add kavach -- kavach mcp`):

- `kavach_list_incidents` — discover `*.kavach` fixtures and inspect how each failed
- `kavach_replay` — reproduce an incident or variant in microseconds (`still_failing@N`, `ok`)
- `kavach_time_travel` — pause replay at `--until <seq>` and probe live state
- `kavach_diff` — compare `--old` and `--new` across incident perturbations and 48 regression variants (`fixed`, `variant_failed(K)@N`, `regressed(K)@N`)
- `kavach_contract_show` — inspect a feature contract and lock status before human approval
- `kavach_contract_check` — verify a candidate feature build against an approved contract (`pass`, `regression@N`, `unreached@N`, `variant_failed(K)@N`)
- `kavach_map` — build or inspect the interactive flow map

---

## Antigravity

Antigravity reads MCP servers from `~/.gemini/config/mcp_config.json` and global skills from `~/.gemini/config/skills/<name>/SKILL.md` (workspace skills go in `.agents/skills/<name>/SKILL.md`).

1. Register the MCP server. Antigravity starts it without your shell's `PATH`, so give the full path that `command -v kavach` prints:

   ```json
   {
     "mcpServers": {
       "kavach": { "command": "/Users/you/.local/bin/kavach", "args": ["mcp"] }
     }
   }
   ```

   Every agent, including each member of a `/swarm`, then has the `kavach_*` tools.

2. Install the skills. `install.sh` copies them to `~/.gemini/config/skills` when `~/.gemini/config` exists; to copy them by hand:

   ```bash
   cp -R skills/kavach-* ~/.gemini/config/skills/
   ```

3. In a swarm, give the verifier role to `kavach-verify` (see [Starter prompts](#kavach-verify)). It posts one result per check, for example:

   ```text
   @coordinator [done] [kavach] fixed · 01_split_repeated_identifier_bulk.kavach · re #12
   old still_failing@15 → new fixed · variants 56/56 · regressions 42/42
   ```
