---
name: kavach-setup
description: >
  Add deterministic record and replay to a service, so that production
  incidents replay exactly on a laptop and fixes can be verified instead of
  guessed from logs. Use when the user wants to capture production traffic or
  crashes for replay, make intermittent or prod-only bugs reproducible the next
  time they happen, turn production incidents into regression tests, or set up
  Kavach in a codebase. Fits services that handle events or requests one at a
  time and keep state (queue consumers, webhook handlers, ledgers, state
  machines), in Go, Rust, Python, TypeScript, Java, C, C++, Ruby, PHP, Elixir,
  OCaml, Haskell or Julia. Maps the service's state boundaries, then wires the
  Kavach SDK into its handler or builds a small replay harness.
compatibility: Requires `kavach` and `kavach-recorder` on PATH.
metadata:
  version: "0.1.5"
---

# Instrument a Service or Harness with Kavach (`kavach-setup`)

Onboard a service or subsystem to Kavach so it can record `.kavach` flight-recorder fixtures over the 8 MiB shared-memory ring (`SPEC.md §10.7`) and act as a deterministic replay host (`kavach-host`, `SPEC.md §9`).

## Two Integration Modes

Choose the lightest integration mode that fits the user's goal:

1. **Direct Handler Instrumentation (Production + Replay):**
   Wire the service's core event/command handler directly to the Kavach SDK for its language, and add the `MaybeReplay` / `maybe_host` entrypoint at the top of `main`. In production, `Recorder.Step` writes into `kavach-recorder` over shared memory (`81.2 ns/event`); when invoked with `kavach-host`, the same binary runs as a deterministic replay host.
2. **Scoped Subsystem Harness (`cmd/kavach-harness`):**
   When working inside a large existing repository (like Redis, Valkey, Kombu, HTTPX, or Axum) to fix an incident or verify a feature contract without refactoring the entire server entrypoint, create a thin `kavach-host` harness binary/script that drives the target subsystem's functions through `env` and checks state invariants after each step.

---

## Step-by-Step Setup Workflow

### 1. Write `.kavach/plan/state-map.md` (Before Touching Code)

Inspect the target handler or subsystem and write **`.kavach/plan/state-map.md`** following [`references/state-map-template.md`](./references/state-map-template.md) (see [`examples/ledger/.kavach/plan/state-map.md`](../../examples/ledger/.kavach/plan/state-map.md) for a complete example):

- **State Root & Mutable Structures:** Primary structs, maps, caches, or secondary indices that hold domain state, with `file:line` locations.
- **Transition Entrypoints (`Input`):** Commands, events, requests, or messages that trigger a state transition.
- **Determinism Boundary (`env` Reads):**
  - Wall clock / timestamps (`env.Now()` / `env.now()`)
  - Random bytes / UUIDs / jitter (`env.Read()` / `env.RandBytes()` / `env.random()`)
  - Synchronous outbound queries to external services or DBs (`env.Query()` / `env.query()`)
  - Dynamic config or feature flags (`env.Config()` / `env.config()`)
- **Captured Side Effects (`env.Emit` / `env.emit`):** Outbound messages, DB writes, or notifications that should be captured and compared during replay rather than executed against live systems.
- **Snapshots & Host Entrypoint:** `Snapshot()` / `Restore()` hooks and where `kavach-host` is intercepted.

 Writing `.kavach/plan/state-map.md` first gives the human engineer a concise artifact to review and feeds directly into `/kavach-invariants` (`.kavach/plan/invariants.md`).

### 2. Wire the Language SDK

Consult [`references/languages.md`](./references/languages.md) for the exact SDK API and host entrypoint for Go, Rust, Python, TypeScript/Node, Java, C, C++, Ruby, PHP, Elixir, OCaml, Haskell, and Julia.

Ensure the binary or script checks for `kavach-host` as its **very first action in `main`** before opening network sockets, worker pools, or database connections.

### 3. Verify Host Mode & Deterministic Replay

1. Build the host command (or prepare the script invocation).
2. Record a short smoke journal (either by driving a few steps through `Recorder` with `kavach-recorder` on `PATH` and flushing a fixture, or using an existing `.kavach` fixture).
3. Replay the journal against the host command:
   ```bash
   kavach replay <journal.kavach> --bin "<host-command>"
   ```
4. Confirm:
   - The verdict is `ok` (for a healthy recording) or `still_failing@N` (for a crash recording).
   - Running `kavach replay` twice in a row produces the exact same verdict with zero `nondeterministic@N` or `diverged@N` errors.
