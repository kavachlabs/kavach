# Kavach Agent Skills

Agent skills that teach AI coding agents ([Claude Code](https://code.claude.com/docs), [OpenAI Codex](https://github.com/openai/codex), [OpenCode](https://opencode.ai), Cursor, and Antigravity) how to reproduce production crashes in microseconds, verify bug fixes across 56 fault-neighborhood variants, build features against locked behavioural contracts, and falsify domain invariants with Kavach.

> **[Install](#install)** · **[Skills overview](#skills-overview)** · **[Starter prompts](#starter-prompts)** · **[MCP server](#mcp-server)**

## Install

### Single-command install (CLI + Agent Skills)

Install the `kavach` CLI, the `kavach-recorder` flight recorder, and all four Kavach agent skills in one command:

```bash
curl -fsSL https://www.thekavachlabs.com/install.sh | sh
```

### `npx skills` installer

Or install the skills into your coding agents via the standard `npx skills` CLI:

```bash
npx skills add kavachlabs/kavach
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
| [`kavach-fix`](./kavach-fix/SKILL.md) | Fix a production incident recorded in a `*.kavach` fixture and prove the patch survives 10+ incident variants and 48 neighbour regression variants (`verdict: "fixed"`). | `kavach inspect`, `kavach replay`, `kavach diff` (`kavach_list_incidents`, `kavach_replay`, `kavach_diff`) |
| [`kavach-implement`](./kavach-implement/SKILL.md) | Build a new feature contract-first: record baseline + feature traffic, declare spec invariants, pause for human `kavach contract approve`, and iterate until `kavach contract check` returns `pass`. | `kavach contract init`, `kavach contract show`, `kavach contract check` (`kavach_contract_show`, `kavach_contract_check`) |
| [`kavach-setup`](./kavach-setup/SKILL.md) | Map state boundaries into `.kavach/plan/state-map.md`, then instrument a handler or build a `kavach-host` harness in Go, Rust, Python, TypeScript, Java, C, C++, Ruby, PHP, Elixir, OCaml, Haskell, or Julia. | `.kavach/plan/state-map.md`, Language SDKs (`sdk/*`), `kavach-recorder`, `kavach replay` |
| [`kavach-invariants`](./kavach-invariants/SKILL.md) | Propose domain invariants and planned mutants in `.kavach/plan/invariants.md`, declare them via the SDK, and falsify (mutation-test) each one in milliseconds to confirm every invariant catches real bugs. | `.kavach/plan/invariants.md`, `env.Invariant`, `kavach replay`, `kavach diff` |

See [`examples/ledger/.kavach/plan/`](../examples/ledger/.kavach/plan/) ([`state-map.md`](../examples/ledger/.kavach/plan/state-map.md) and [`invariants.md`](../examples/ledger/.kavach/plan/invariants.md)) for a complete example of the human-reviewable planning artifacts generated before code changes.

---

## Starter prompts

### `kavach-fix`

```text
/kavach-fix Reproduce the crash in testdata/null-amount.kavach and fix the handler until kavach diff reports verdict: fixed.
```

### `kavach-implement`

```text
/kavach-implement Implement the feature described in docs/spec.md contract-first with Kavach. Stop after kavach contract show so I can approve the contract before you write the implementation.
```

### `kavach-setup`

```text
/kavach-setup Map the order-matching handler in internal/matcher/ into .kavach/plan/state-map.md and instrument it with the Kavach Go SDK so it records production failures via kavach-recorder and runs as a kavach-host for deterministic replay.
```

### `kavach-invariants`

```text
/kavach-invariants Write .kavach/plan/invariants.md for the ledger service, declare the invariants on the handler, and falsify each invariant by mutation-testing it against a recorded journal.
```

---

## MCP server

All four skills work with both the `kavach` CLI and the `kavach mcp` Model Context Protocol server (`claude mcp add kavach -- kavach mcp`):

- `kavach_list_incidents` — discover `*.kavach` fixtures and inspect how each failed
- `kavach_replay` — reproduce an incident or variant in microseconds (`still_failing@N`, `ok`)
- `kavach_diff` — compare `--old` and `--new` across incident perturbations and 48 regression variants (`fixed`, `variant_failed(K)@N`, `regressed(K)@N`)
- `kavach_contract_show` — inspect a feature contract and lock status before human approval
- `kavach_contract_check` — verify a candidate feature build against an approved contract (`pass`, `regression@N`, `unreached@N`, `variant_failed(K)@N`)
