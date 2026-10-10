# Problem sourcing for the Kavach agent benchmark

This pass screened 613 public bug-fix problems against the problem-sourcer rubric in `.claude/agents/problem-sourcer.md`. Each problem is one row in `ledger.csv`.

`sample.py` reproduces both random draws. Accessed 2026-10-10. The ledger was revised after three verifier rounds and a fourth round that added the gap roadmap and the DeepSWE feature track; see "Changes after verification" at the end.

The benchmark includes every problem, not only the fits. For the outs, the `kavach_gap` column says which capabilities Kavach lacks for each one (see "Gaps").

**Headline**
- 157 of 613 problems fit (26%), but only 11 fit as-is. All 11 are Redis or Valkey (10 Redis + 1 Valkey).
- 146 need a wrapper. Of those, 119 are document or source transforms.
- 30 fits (4.9% of all problems screened) are service-shaped: handlers that fold state across requests, commands or gateway calls.
- 145 of the 157 fits are caught live only through an invariant.
- Kavach's variants can plausibly reach the trigger in only 3 fits: redis-10068, redis-10764 and laravel-48573.
- For 4 of the 30 service fits, the correct upstream fix changes outputs on steps before the failing step, so Kavach would report `diverged` rather than `fixed`. For another 5 this is unclear (see "Fix changes pre-failure outputs").
- **Gaps.** 2 of 613 problems have no identified capability gap: redis-10068 and redis-10764. That is not a claim that Kavach solves them.
  - Nothing has been run, and no Redis integration exists yet (mstime, the LRU clock and so on must be routed).
  - A `fixed` verdict needs at least 10 reproducing variants (SPEC §6.1), which nobody has shown.
  - Among the 157 fits, the gaps are invariants that would give the bug away (143) and variants that cannot reach the trigger (154).
  - Among the outs, the commonest first gaps are features that need diff acceptance (222), triggers that are not input-driven (87), and out-of-model problems (73).
- **Feature track (DeepSWE).** With rubric item 1 waived, 51 of the 113 DeepSWE tasks fit with a wrapper (45%). Of those, 42 are transforms and 9 are services; 13 of the 51 are soft calls. None fits as-is.

## Ledger columns

| Column | Meaning |
|---|---|
| source, id, repo, language, created, license | Provenance |
| verdict | fit / fit-with-wrapper / out |
| failing_item | The rubric item an out fails first |
| reason | Why |
| verified_or_inferred | `verified`, `verified (audit)`, `verified (class re-screen)` or `inferred` (an unaudited first-pass out) |
| wrapper | One sentence; fits only |
| narrow_fix_possible | y / n / ? |
| failure_class | See the class definitions below. Blank for inferred outs. |
| call | Blank, `soft` (a judgement call that could go either way) or `tie` (decided by the less-favourable rule) |
| swe_bench_verified | y if the id is in SWE-bench Verified |
| shape | Fits only: `service`, `transform` or `other` (see "Yield by shape") |
| variants_reach_trigger | Fits only: y / n / ? — whether Kavach's variants can perturb the bug's trigger |
| fix_changes_prefailure_outputs | Service fits only: y / n / ? — whether the correct upstream fix changes any output emitted on a step before the failing step, given the wrapper's input sequence. If y, replaying the correct fix reports `diverged` (SPEC §6), and §6.1 rule 3 fails. |
| needs_invariant | Fits only: y for wrong-output and error-reply |
| f2p_present | Fits only: `n` where the dataset lists no fail-to-pass tests, only none-to-pass ones |
| kavach_gap | Every row: the capabilities Kavach lacks for this problem, from a fixed vocabulary (see "Gaps"). Several values are separated by `;`. `out-of-model` comes first whenever present, then `licence`, then the rest, most fundamental first. |
| feature_track | DeepSWE only: the verdict with rubric item 1 waived, `fit` / `fit-with-wrapper` / `out@N`. The original `verdict` column is unchanged. |
| feature_shape | DeepSWE only: `service` / `transform` / `other` |
| hidden_verifier_independent | DeepSWE only: y if the task's hidden tests grade the feature through the interface the instruction names, so grading needs nothing from Kavach and an independent implementation of the spec can pass |
| traffic_needed | DeepSWE feature-track fits: the synthetic traffic Kavach would need, in one line. `n/a` for outs. |
| feature_reason | DeepSWE only: the wrapper (fits) or the reason (outs), plus any `soft` mark |

The `failure_class` values are:
- `crash`: a panic, uncaught exception, assertion or segfault.
- `error-reply`: an error the program legitimately returns for some inputs, such as a Redis `-ERR` reply, a compiler or adapter diagnostic, or a validation error.
- `wrong-output`: the program completes but its output is wrong.

## Sources and versions

| Source | Version | Size | Screened | License |
|---|---|---|---|---|
| DeepSWE (Datacurve), github.com/datacurve-ai/deep-swe | commit 0b9fabbb63b9 (2026-08-26), dataset `deep-swe-1-1` | 113 tasks | all 113 | Apache-2.0 for the tasks; upstream licenses are in PROVENANCE.md, all permissive |
| SWE-bench Multilingual, huggingface.co/datasets/SWE-bench/SWE-bench_Multilingual | sha 846e647b9f33 (2026-08-17) | 300 tasks, 41 repos | all 300 | MIT |
| Multi-SWE-bench (including SWE-bench Verified for Python), huggingface.co/datasets/ByteDance-Seed/Multi-SWE-bench | sha 56ff018c04a3 (2026-07-08), 48 jsonl files | 2,237 instances | random 200 | Card: "CC0, subject to ByteDance IP" |

- Multi-SWE-bench's Python file contains exactly the same 500 ids as SWE-bench Verified. I checked this against princeton-nlp/SWE-bench_Verified at sha c104f840 (2025-02-18).
- 37 of the 200 sampled rows come from that Python file, including all 6 Python fits.
- "DeepSWE" here means Datacurve's benchmark, not Agentica's DeepSWE model.

## Method

**Passes**
- First pass: every problem, judged on its metadata and problem statement.
- Verified pass: the issue, fix diff and tests, for every problem the first pass did not rule out.
- Audit: a verified pass on a random 10% (rounded up) of the first-pass outs, using `python3 -I sample.py audit <tsv> <seed>`. Seeds: DeepSWE 1, SWE-bench Multilingual 2, Multi-SWE-bench 3.
- Multi-SWE-bench sample: `random.Random(20261010).sample` of 200 ids from the sorted, de-duplicated id list (`python3 -I sample.py msb <dir>`).
- Class re-screen: the audit showed the site-generator rule was too broad, so I re-screened all 8 remaining rows in that class. They are kept out of the error rate.

**Dates**
- SWE-bench Multilingual: its `created_at` field.
- Multi-SWE-bench: the GitHub PR creation date, assuming the instance number is the PR number.
- DeepSWE: no per-task date exists. The `created` column gives the task's base commit date from the GitHub API (12 base commits are not on GitHub), the image tag month (2026-05) and the dataset commit (2026-08-26).

**Licenses**
- Taken from the GitHub API.
- Where the API says NOASSERTION, I read the repo's LICENSE file, and the row says so.

### Operational rules

- **out@1**
  - Features, enhancements and requested behaviour changes.
  - All of DeepSWE. 106 of its 113 tasks are feature requests, its "bugfix" tasks are multi-part specs, and its reference solutions are not upstream fixes.
- **out@2**
  - Library APIs triggered only by calling code.
  - Compile-time or macro behaviour.
  - DOM rendering.
  - Whole-site builds with no per-document entry point.
  - IDE plugins.
  - CLI argument or config parsing at startup.
- **fit-with-wrapper**: data fed to a stable entry point. Examples: source text to linters and compilers, documents to parsers, requests to routers, gateway responses to clients.
- **fit**: handler-shaped as-is.
- **out@4**
  - Filesystem, process or threads.
  - Host time zone.
  - Uninitialised or memory-dependent reads (redis-11631, micropython-13039).
  - Fixes that must issue a new gateway call (SPEC §6).
- **out@7**: redis-13338. It fits on items 1–6, but its fix landed after Redis relicensed on 2024-03-20.

**Known inconsistency (not re-screened)**
- CLI argv or config parsed at startup is out@2 (clap, pylint, bat options, coreutils `tr`). Source text fed to a compiler or linter is fit-with-wrapper.
- Both are a single text fed to a pure-ish entry point.
- I recommend applying the shape tier to both: keep transforms, and any argv-driven CLI problems, out of the primary benchmark, because Kavach's variants cannot perturb raw text (see Threats).
- Promoting the startup-argv and startup-config problems would touch up to 18 out@2 rows, and they would become transforms.

## Yield

Yield = fit + fit-with-wrapper. Outs are counted by the first rubric item they fail.

| Source | Screened | fit | fit-with-wrapper | Yield | out@1 | out@2 | out@3 | out@4 | out@7 |
|---|---|---|---|---|---|---|---|---|---|
| SWE-bench Multilingual | 300 | 11 | 109 | 120 (40%) | 48 | 87 | 7 | 37 | 1 |
| Multi-SWE-bench (incl. SWE-bench Verified for Python), sample | 200 | 0 | 37 | 37 (18%) | 79 | 68 | 3 | 13 | 0 |
| DeepSWE | 113 | 0 | 0 | 0 (0%) | 113 | 0 | 0 | 0 | 0 |
| **All** | 613 | 11 | 146 | 157 (26%) | 240 | 155 | 10 | 50 | 1 |

| Language (SDK) | Screened | fit | fit-with-wrapper | Yield | out@1 | out@2 | out@3 | out@4 | out@7 |
|---|---|---|---|---|---|---|---|---|---|
| c (incl. C++) | 66 | 11 | 13 | 24 (36%) | 17 | 18 | 1 | 5 | 1 |
| typescript (incl. JS) | 135 | 0 | 26 | 26 (19%) | 58 | 43 | 5 | 3 | 0 |
| java | 54 | 0 | 25 | 25 (46%) | 14 | 13 | 0 | 2 | 0 |
| kotlin (via Java SDK) | 8 | 0 | 4 | 4 (50%) | 1 | 2 | 0 | 1 | 0 |
| php | 43 | 0 | 22 | 22 (51%) | 2 | 19 | 0 | 0 | 0 |
| go | 115 | 0 | 19 | 19 (17%) | 75 | 7 | 1 | 13 | 0 |
| ruby | 44 | 0 | 16 | 16 (36%) | 7 | 9 | 1 | 11 | 0 |
| rust | 77 | 0 | 15 | 15 (19%) | 29 | 18 | 2 | 13 | 0 |
| python | 71 | 0 | 6 | 6 (8%) | 37 | 26 | 0 | 2 | 0 |

No source contains Elixir, Haskell, Julia or OCaml. Three DeepSWE tasks carry the wrong `language` in task.toml; the ledger corrects them from the files their solution patches touch:
- httpx-deterministic-cookie-store: typescript → python.
- koota-entity-snapshot-rollback: python → typescript.
- prometheus-transactional-reload-status: typescript → go.

### Yield by shape

The verdicts are unchanged by this split; the `shape` column records it. A fit is labelled `service` only if its own wrapper puts it behind a handler that folds state across requests, commands or gateway calls.
- `transform`: one text or record in, one result out. Linters, formatters, compilers and parsers, plus serialiser round trips and helpers driven directly.
- `other`: library queries over prepared data, and validator APIs.

| Source | Fits | service | transform | other | needs invariant | crash | error-reply | wrong-output | variants reach trigger y / ? / n |
|---|---|---|---|---|---|---|---|---|---|
| SWE-bench Multilingual | 120 | 26 | 86 | 8 | 112 | 8 | 23 | 89 | 3 / 31 / 86 |
| Multi-SWE-bench | 37 | 4 | 33 | 0 | 33 | 4 | 8 | 25 | 0 / 4 / 33 |
| DeepSWE | 0 | 0 | 0 | 0 | 0 | 0 | 0 | 0 | — |
| **All** | 157 | 30 | 119 | 8 | 145 | 12 | 31 | 114 | 3 / 35 / 119 |

- **Service yield:** 30 of 613 (4.9%).
- **Service fits by language:** C 11 (all Redis/Valkey), Go 9, Rust 3, Python 3 (all in SWE-bench Verified), TypeScript/JS 2, Ruby 1, PHP 1, Java 0.
- **Soft service rows:** 7 of the 30 are marked `soft` in `call`:
  - Two are library objects fed operations: laravel-48573 (an ArrayStore) and fluentd-3640 (a RetryState).
  - prometheus-12874 is a head appender that also writes mmapped chunks.
  - axios-5085 and axios-5892 are stateless, one call per input.
  - caddy-5870's tests call an unexported traversal directly.
  - gin-3227's fail-to-pass test does not exercise the issue's scenario.
- **Concentration:** 6 repos supply 66 of the 157 fits: lombok 14, rubocop 13, php-cs-fixer 10, redis 10, svelte 10 and caddy 9.

### Fix changes pre-failure outputs (service fits)

I read each upstream fix against the wrapper's input sequence. A wrong-output bug caught by an invariant at its first occurrence usually leaves earlier steps untouched. The exceptions are fixes that change state or output on paths before the trigger.

Of the 30 service fits: **4 y, 5 ?, 21 n.**

**y (the correct fix diverges before the failure):**
- **caddy-5870:** the fix also turns "key already exists" and "key does not exist" errors on other admin paths into 409 and 404 responses. Any earlier PUT/POST that hit those paths changes status.
- **fluentd-3640:** the retry timeout is computed when the retry state is set up. If the wrapper emits `secondary_transition_at` on every step, every step's output changes. Avoidable only if the wrapper emits it on the failing step alone.
- **redis-11279:** the fix sets the sanitize-payload flag in `ACLCreateUser`. Any earlier ACL GETUSER or ACL LIST of a user created earlier changes. Avoidable only if the input sequence has no such call before the failing step.
- **django-13551:** the fix adds the email to the token hash, so every earlier step that emits a reset token changes. Not avoidable, because the bug needs a token issued before the email change.

**? (unclear without running):**
- **laravel-48573:** the fix changes how `put` reads the clock (a precise timestamp instead of `currentTime()`). Outputs are probably unchanged, and the count of clock reads probably is too.
- **prometheus-12874:** the fix moves the chunk-size check. Chunk-cut timing for non-duplicate samples may change.
- **prometheus-13845:** earlier range queries may have had corrupted label slices without producing visible duplicates.
- **redis-12472:** the rule corruption happens at ACL SETUSER. Permission replies on steps between that and the failing ACL LIST may differ.
- **redis-13115:** an earlier EVAL that stores a Lua number (SET via `redis.call`) stores "2e+08" rather than "200000000", so later GETs before the failing step would differ.

**n (the other 21):** axios-5085, axios-5892, gin-1805, gin-3227, gin-3820, gin-4003, prometheus-10633, redis-10068, redis-10095, redis-10764, redis-11510, redis-11734, redis-12272, redis-9733, axum-1934, axum-2096, axum-691, valkey-1499, cli-3517, django-16255, django-16612.

Tasks built from the y and ? rows must use input sequences that avoid those earlier steps where they can. django-13551 cannot avoid them and should not be used to measure verification.

## Gaps

Every row has a `kavach_gap`: the capabilities Kavach lacks for that problem. The column turns the rejection reasons into a roadmap. The vocabulary is fixed:

| Gap | Meaning, and the capability that would close it |
|---|---|
| `none` | No capability gap identified. This does not mean Kavach solves the problem: nothing has been run, the application's integration (routing its clock, randomness and so on) does not exist yet, and a `fixed` verdict still needs at least 10 reproducing variants (SPEC §6.1). |
| `invariant-leaks-bug` | The failure is a wrong output or an error reply. It is captured only through an invariant, and any invariant precise enough to flag it states the expected behaviour, so writing it hands the agent the bug. |
| `variants-cannot-reach` | §6.1's variants (JSON-field mutations, reordering, clock shifts) cannot perturb the trigger, because it lives in raw text, raw bytes or numeric config. Generator-based variants (Hypothesis-style, with shrinking) over the input format would close this. |
| `feature-needs-diff-acceptance` | The correct change alters outputs on traffic the old build handled: every feature, plus the 4 fits whose fix changes pre-failure outputs. Kavach would need a way to accept intended diffs. |
| `filesystem-state` | The trigger or the state lives on disk |
| `threads-concurrency` | Real concurrency inside a step (SPEC §11) |
| `timing-hang-leak` | Hangs, leaks, performance or wall-clock timing |
| `child-process` | Spawns or talks to child processes |
| `memory-dependent` | Outcome depends on uninitialised memory or allocation layout, or the feature bounds memory |
| `argv-startup-config` | The trigger is process startup state: argv, config files, environment variables or host settings such as the time zone. Recording these as startup inputs (SPEC already has `environment` facts) would close it. |
| `not-input-driven` | The trigger is program code calling an API (library calls, declarations, builders, object protocols). It would need an API-call-sequence harness, where an input is a call script. |
| `licence` | One rule, applied to every row: named whenever the row's licence at its date is source-available rather than open source, i.e. BUSL-1.1 (Terraform after 2023-08-10) or RSALv2/SSPLv1 (Redis after 2024-03-20). It is added to whatever other gaps the row has. Logstash's Elastic-licensed x-pack is not involved in its rows, so they are not marked. |
| `out-of-model(reason)` | No plausible capability within the handler model |

**How the values were assigned**
- **Fits:**
  - `invariant-leaks-bug` for every wrong-output or error-reply fit, except two: redis-10068 and redis-10764.
  - For those two, the invariant is the command's documented contract: XTRIM MINID keeps entries at or above the threshold, and BZMPOP wakes only on its own keys.
  - laravel-48573 is not an exception, by the tie rule. Laravel documents TTLs in seconds, and whole-second expiry was long-standing behaviour. An invariant that fires must assume millisecond precision, which is the semantics the fix chose, so it encodes the fix.
  - For the other 143 the label is an upper bound. I did not look for metamorphic invariants, such as round-trip or idempotence, row by row.
  - `variants-cannot-reach` wherever `variants_reach_trigger` is n or ?.
  - `feature-needs-diff-acceptance` where `fix_changes_prefailure_outputs` = y.
- **out@1:** `feature-needs-diff-acceptance`, plus a second gap where one is evident.
  - DeepSWE takes the second gap from the feature-track re-screen.
  - Elsewhere it comes from keywords: CLI flags → argv, API additions → not-input-driven, React props → DOM/UI.
  - Three out@1 rows are performance or refactor work where nothing fails (tokio-6724, django-16819, logstash-15697). They are `out-of-model`, as are two out@3 rows of the same kind (zstd-983, vuejs-9507).
- **out@2:** by reason class.
  - Library API from code → `not-input-driven`.
  - CLI and startup → `argv-startup-config`.
  - Whole-site builds → `filesystem-state;not-input-driven`.
  - Compile-time code and DOM/UI → `out-of-model`.
- **out@3 and out@4:** all 60 rows set one by one from their reasons, through an override file that also handles the exceptions named here.
- **Inferred argv and timing rows:** all 85 inferred rows naming `argv-startup-config` or `timing-hang-leak` in v5 were re-checked for library-code triggers mislabelled as argv or timing: 73 outside DeepSWE and 12 in it.
  - **Outside DeepSWE (73):** 11 relabelled, 2 changed only by the ordering and licence rules (docusaurus-10309, terraform-34814), 60 unchanged.
    - **clap ×9** → `not-input-driven`: clap-2027, 2058, 2730, 2758, 3196, 3394, 4072, 5015 and 5206.
    - **coreutils-6690** → `not-input-driven`: the completion script is generated from the command definition.
    - **tokio-4867** → `not-input-driven;timing-hang-leak`: a hang in library code.
    - clap-2990 is an audited row, not one of the 73. It has the same trigger class (a derive attribute) and was relabelled to `not-input-driven` to match.
  - **DeepSWE (12):** no gap changed (goreleaser-retry-publish-auditing was only reordered in round 5, to put `out-of-model` first). bandit, fd, mnamer, prometheus, scc and task are CLIs or servers whose flags are real startup input. clack, goreleaser, kcp-go and pwntools have real timing: debounce, backoff, read deadlines, timeouts. cliffy and optique are CLI-parser libraries, and they keep `argv-startup-config` by the clap rule below, because the features change how a given argv is parsed. optique also has `not-input-driven`, since its parser is declared in code.
  - **The clap rule is a judgement call.**
    - `argv-startup-config` applies when the fix changes a parse or validation decision for a given argv: which arguments conflict, how values propagate, or which error is raised. That covers 2253, 2309 (the wrong error for two arguments from one group), 3974 and 4523.
    - `not-input-driven` applies when the fix changes what is rendered from the command definition: help layout, completions, the content of an error, or derive attributes. This holds even when a fixed argv such as `--help` triggers it. That covers 2058, 3196, 5015 and 5206 (help output) and 3394, which adds possible values to the missing-value error.
    - 3394 and the help-output rows are argv against a fixed command, much like 2309, so the line could be drawn differently. All of these rows are outs either way, so no verdict or yield depends on it.
- **Inferred rows:** take their gap from the first-pass reason, so they carry the 2–18% first-pass error rate.

**Counts** ("first" = the most fundamental gap of the row; "only" = the row's sole gap):

| Gap | Rows naming it | First | Only | Fits | Outs | DeepSWE | SWE-bench ML | MSB |
|---|---|---|---|---|---|---|---|---|
| none | 2 | 2 | 2 | 2 | 0 | 0 | 2 | 0 |
| invariant-leaks-bug | 143 | 143 | 1 | 143 | 0 | 0 | 110 | 33 |
| variants-cannot-reach | 154 | 12 | 12 | 154 | 0 | 0 | 117 | 37 |
| feature-needs-diff-acceptance | 241 | 222 | 110 | 4 | 237 | 113 | 50 | 78 |
| not-input-driven | 141 | 87 | 86 | 0 | 141 | 30 | 62 | 49 |
| argv-startup-config | 73 | 19 | 19 | 0 | 73 | 11 | 18 | 44 |
| filesystem-state | 46 | 29 | 17 | 0 | 46 | 17 | 23 | 6 |
| threads-concurrency | 21 | 9 | 7 | 0 | 21 | 10 | 8 | 3 |
| timing-hang-leak | 15 | 4 | 4 | 0 | 15 | 5 | 8 | 2 |
| child-process | 12 | 5 | 5 | 0 | 12 | 2 | 6 | 4 |
| memory-dependent | 3 | 2 | 2 | 0 | 3 | 1 | 2 | 0 |
| licence | 6 | 6 | 1 | 0 | 6 | 0 | 6 | 0 |
| out-of-model | 73 | 73 | 58 | 0 | 73 | 9 | 29 | 35 |

**out-of-model breakdown (73):**
- DOM/UI runtime: 45.
- Compile-time code: 10.
- Gateway calls not in the recording: 7. axios-4731, axios-5316 and cli-10072, plus four DeepSWE features: claude-code-by-agents delegation, eicrud keyset paging, goreleaser retries, and the ofetch circuit breaker, which removes calls.
- Nothing fails (performance, quality or refactor): 5.
- State spread across cluster nodes: 2.
- Each of these has 1: a wrong outbound gateway request, a failure below the handler in the connection layer, an OS signal registration, and nonces from crypto/rand with no injection point.

Fit gap combinations: 138 `invariant-leaks-bug;variants-cannot-reach`, 12 `variants-cannot-reach` (all crashes), 4 `invariant-leaks-bug;feature-needs-diff-acceptance;variants-cannot-reach` (the prefailure-y service fits), 1 `invariant-leaks-bug` alone (laravel-48573) and 2 `none`.

**Implications**
- **2 of 613 rows have no identified capability gap** (redis-10068, redis-10764). This is not a claim that Kavach solves them: nothing has been run, there is no Redis integration yet, and nobody has shown the 10 reproducing variants a `fixed` verdict needs (SPEC §6.1). Both also need a hand-written contract invariant.
- **Among fits, invariants are the binding constraint.**
  - Generator-based variants would remove the variants gap for the 12 crash fits whose only gap is variants. Those 12 are lucene-13494, babel-15445, caddy-5404, gin-4003, jq-2839, lombok-3042, lombok-3326, axum-1934, django-16255 and ponyc-3586/4017/4505.
  - Caveats on those 12:
    - Seven are compiler or interpreter crashes on source text (lombok ×2, ponyc ×3, babel, jq). Keeping the trigger intact while varying the input there means open-ended fuzzing, not a format generator.
    - The three ponyc rows have no fail-to-pass tests.
    - django-16255 is in SWE-bench Verified.
  - The other 143 fits also need an invariant that does not give the bug away. No tooling change fixes that; it is a property of wrong-output bugs.
- **Diff acceptance is the gate to features.**
  - 222 rows name it first, and for 110 it is the only gap: 51 DeepSWE feature-track fits, plus 59 SWE-bench ML/MSB features.
  - Of those 59, 47 are inferred from first-pass metadata and had no second gap looked for, so 110 is an upper bound.
- **The next biggest levers are input models, not determinism:**
  - an API-call-sequence harness (`not-input-driven`, 141 rows);
  - startup inputs (`argv-startup-config`, 73 rows, 44 of them in MSB, mostly `cli/cli` features).
  - Filesystem state (46) could plausibly go through SPEC's local gateways. Threads (21), timing (15), child processes (12) and memory (3) are smaller.
- **79 rows (13%) are out-of-model or licence-bound** (73 out-of-model, 6 licence). That is the share of the benchmark Kavach should be expected to lose under any roadmap.
- **One out-of-model class has a plausible capability:** "gateway calls not in the recording" (7 rows). A gateway simulator, or recording traffic against the reference build, could serve them. I kept it out-of-model because SPEC §6 explicitly treats a changed call sequence as nondeterminism and the vocabulary has no entry for it. It is a candidate for the roadmap.

## Feature track (DeepSWE)

Kavach is also used for feature work, as contract and invariant testing, so DeepSWE comes back in as a separate track.

**Method**
- I re-screened all 113 tasks with rubric item 1 (kind) waived and items 2–7 applied as before.
- For each task I read `instruction.md` and the solution patch's file list and size, and listed the hidden tests (`tests/config.json` fail-to-pass and pass-to-pass ids, and the files `test.patch` adds).
- I read the hidden tests in full only where the verdict depended on them.
- The question for item 2 is whether the feature is exercised by a stream of inputs to a handler-shaped entry point (as-is, or via a thin wrapper) rather than by code calling an API.
- The 113 rows keep their original `verdict` (out@1). The feature verdict is in `feature_track`.
- `hidden_verifier_independent` was checked by a script that flags hidden tests importing new files the instruction does not name. It flagged 5 tasks, all false positives.
  - I set it to n for oxvg-structural-selector-preservation and query-persist-restored-query-state. Their instructions are abstract, while the tests pin one concrete interpretation.
- Every DeepSWE language has a Kavach SDK (go, python, typescript, rust), so item 5 rules nothing out. All upstream licences are permissive, so item 7 rules nothing out.

**Yield**

| DeepSWE feature track | Screened | fit | fit-with-wrapper | Yield | out@2 | out@3 | out@4 | out@7 |
|---|---|---|---|---|---|---|---|---|
| **all** | 113 | 0 | 51 | 51 (45%) | 33 | 0 | 29 | 0 |
| go | 35 | 0 | 23 | 23 (66%) | 3 | 0 | 9 | 0 |
| python | 34 | 0 | 16 | 16 (47%) | 8 | 0 | 10 | 0 |
| typescript (incl. JS) | 39 | 0 | 9 | 9 (23%) | 21 | 0 | 9 | 0 |
| rust | 5 | 0 | 3 | 3 (60%) | 1 | 0 | 1 | 0 |

| Shape | Screened | fit-with-wrapper | out@2 | out@4 |
|---|---|---|---|---|
| transform | 42 | 42 | 0 | 0 |
| service | 16 | 9 | 1 | 6 |
| other | 55 | 0 | 32 | 23 |

**What the yield is made of**
- **Fits by shape and language:**
  - transform: go 21, python 10, typescript/JS 8, rust 3;
  - service: python 6, go 2, JS 1.
- **Soft fits (13):**
  - httpx-deterministic-cookie-store, kombu-single-active-consumer-priority, kombu-virtual-queue-dead-lettering, updo-policy-alerting: library objects fed operations (a CookieStore, an in-memory broker transport, an alert tracker). This is the main ledger's standard for soft calls;
  - cattrs-partial-structuring-recovery, narwhals-rolling-window-suite, skrub-duration-encoding: the wrapper predefines classes or ops;
  - fastapi-deprecation-response-headers, fastapi-implicit-head-options: route declarations are code;
  - go-git-worktree-merge-conflicts: an in-memory repo built from the input;
  - gql-incremental-graphql-delivery;
  - yaegi-go-embed-directives: an in-memory FS;
  - yjs-map-conflict-detection.
- **Where the outs go:**
  - **out@2 (33):** code-level APIs dominate: ECS (koota ×5), schema and query builders (drizzle, kysely, dynamodb-toolbox ×2, valibot), and FP libraries (returns, true-myth, ts-pattern). TypeScript is hit hardest: 21 of its 39 tasks.
  - **out@4 (29):**
    - filesystem: caches, watchers, report files, SQLite;
    - concurrency: mux streams, barriers, coalescing, async teardown;
    - gateway calls the recording would not hold: three services (claude-code-by-agents, eicrud, ofetch) and goreleaser's retries.
  - Of the 16 service-shaped tasks, 7 are out: the three above, aiomonitor (live asyncio task state), arcane (DB, scheduler and Docker state), prometheus-transactional-reload-status (files and flags) and effect-sse-httpapi-streaming (out@2, a framework API).
- **No out@3.** Timing shows up only as a secondary gap.
- **Feature-track gap counts for the 62 outs** (second gap onward; the first gap is `feature-needs-diff-acceptance`, except on the 9 out-of-model rows, which list `out-of-model` first and `feature-needs-diff-acceptance` second): not-input-driven 30, filesystem-state 17, argv-startup-config 11, threads-concurrency 10, out-of-model 9, timing-hang-leak 5, child-process 2, memory-dependent 1.
- **Hidden verifiers:** 111 of 113 are independent, and 50 of the 51 fits. Several fits have very few fail-to-pass tests: kgateway 2 (one golden file), anko-default-function-arguments 2, go-critic 3, helm-unified-manifest-stream 5, opa-template-string 5, abs-stepped-slices 6, oxvg 6, go-genai 6, yjs 9. A narrow implementation is likely to survive those.

**Best 10 feature-track candidates**

Picked on: service-shaped first; inputs Kavach's variants can perturb (JSON) next; then hidden-test strength and language spread. Four of the ten are soft calls: entries 1–4, which are library objects fed operations. There are 9 service fits, and 8 of them are soft. Entries 1–4 are kept over the other four soft services for two reasons. First, their hidden tests (17–115 F2P) exercise the runtime contract the wrapper feeds. Second, in fastapi ×2 and gql most tests vary code-level declarations that traffic cannot reach, and yjs has only 9 F2P. go-genai (5) is the only non-soft service, which is why it is on the list. It is not there for test strength: it has only 6 F2P tests and is on the weak-tests list above.

1. **httpx-deterministic-cookie-store** (Python, service, 115 F2P)
   - Input: one input is a response to absorb (Set-Cookie: combined values, Expires commas, Domain/Path/Secure/SameSite, eviction limits) or a request to stamp.
   - Output: the Cookie header; expiry via the clock call.
   - Stateful, and the existing cookie behaviour must stay unchanged.
2. **kombu-virtual-queue-dead-lettering** (Python, service, 76 F2P)
   - Input: one broker operation per input on the in-memory transport: declares with DLX/TTL/max-length, publishes, rejects, gets.
   - TTL expiry via clock.
3. **kombu-single-active-consumer-priority** (Python, service, 85 F2P)
   - Input: the same broker-operation stream, with x-single-active-consumer, x-priority consumers, cancels, channel closes and queue deletes.
   - Output: deliveries and cancel notifications.
4. **updo-policy-alerting** (Go, service, 17 F2P)
   - Input: one check result per input into the alert tracker.
   - Policies via config, cooldown via clock.
   - Output: alert events and webhook payloads.
   - The reference solution rewrites 193 existing lines, so diff acceptance matters.
5. **go-genai-streamed-function-args** (Go, service, 6 F2P)
   - Input: each streamed chunk (partialArgs fragments, willContinue, reused IDs) is one input, folded into per-call state.
   - Weak hidden tests, so this is where Kavach's own checks would matter most.
6. **arktype-json-schema-refs-dependencies** (TypeScript, transform, 25 F2P)
   - Input: {schema, data} JSON with $ref/$defs, dependencies and if/then/else.
   - Field-level variants reach it.
7. **ytt-jsonpath-query-api** (Go, transform, 103 F2P)
   - Input: {doc, path} JSON.
   - Variants reach the path.
8. **csstree-shorthand-expansion-compression** (JS, transform, 79 F2P)
   - Input: {op, property, value|longhands} JSON.
   - Variants reach it.
9. **prometheus-typed-label-sorting** (Go, transform, 17 F2P)
   - Input: sort_by_label over JSON series with mixed typed label values.
   - It deliberately reorders existing results, so it is the clearest test of diff acceptance.
10. **pest-character-class-coalescing** (Rust, transform, 104 F2P)
    - Input: grammar text in, optimised expression tree out.
    - The strongest-tested Rust fit.

Runners-up: expr-try-catch-errors (Go, 79 F2P), tomlkit-toml-table-converters (Python, JSON key paths) and katex-multicolumn-array-spans (JS, 94 F2P).

**Threats specific to the feature track**
- **Synthetic traffic is weaker than real traffic.**
  - No DeepSWE task comes with traffic. Every `traffic_needed` line is traffic someone has to generate.
  - Generated traffic covers what its author thought of, which is close to what the instruction and the hidden tests already cover.
  - Kavach's contract checks over that traffic are therefore weaker evidence than replaying production traffic for a bug fix.
- **Reference solutions change pre-existing behaviour.**
  - 38 of the 51 fit reference solutions delete or rewrite existing lines, and 15 remove 20 or more. Examples: updo 193, arktype 160, scriggo 122, abs 82, helm-unified-manifest-stream 66.
  - helm-unified-manifest-stream and prometheus-typed-label-sorting change existing output on purpose.
  - Replaying old traffic against Datacurve's reference will show diffs. Some are intended, and some may be regressions that the pass-to-pass tests do not catch.
  - Kavach needs diff acceptance (a reviewed list of intended output changes) before the feature track can score anything.
  - The reference solution is Datacurve's, not upstream's, so it is one acceptable implementation, not ground truth.
- **Contamination is low, but check the dates.**
  - No task has an upstream fix: the solutions were written for the dataset.
  - Base commits run from 2025-03 to 2026-03; 53 of 113 are 2026-03, and 12 are not on GitHub (probably rebased or private forks).
  - The images are tagged 2026-05 and the dataset was committed on 2026-08-26.
  - A model trained on data after 2026-08-26 may have seen the instructions, solutions and tests.
  - Use the `created` column to drop tasks newer or older than a model's cutoff as needed.
- **Instructions are written to the tests.** Datacurve instructions name exact error substrings, method names and output orders. That makes the hidden verifier strong, but it also means the instruction is close to a test oracle. An invariant written from it would encode the feature, which is the intended use in the feature track rather than a leak.
- **One reader.** All 113 feature verdicts are mine, read from the instruction, patch stats and test lists. 18 are soft calls (13 fits, 5 outs), and none was audited by a second pass.

## First-pass error rate

The rate is the share of audited first-pass outs that the verified pass found to fit.

| Source | Audited outs | Wrongly ruled out | Rate (95% Wilson CI) |
|---|---|---|---|
| DeepSWE | 11 | 0 | 0% (0–26%) |
| SWE-bench Multilingual | 17 | 2 (jekyll-8047, docusaurus-8927) | 12% (3–34%) |
| Multi-SWE-bench | 16 | 1 (sympy-20916) | 6% (1–28%) |
| Pooled | 44 | 3 | 7% (2–18%) |

- All three errors are per-document or per-string transforms that a coarse class rule excluded.
- The class re-screen added 3 fits out of 8 rows.
- I estimate about 25 of the 373 unaudited outs are missed fits, nearly all of them transforms.
- In the other direction, first-pass precision on candidates was 115/138 for SWE-bench Multilingual and 36/40 for Multi-SWE-bench.

## Best candidates (14)

I picked these on five criteria, in order:
1. Service-shaped.
2. Observable without an invariant, or with a simple one.
3. Variants can reach the trigger.
4. A narrow fix is plausible.
5. Spread across languages.

I shrank the list from 15 to 14 entries:
- logstash-16968 (previously #6) and druid-15402 (previously #7) were relabelled as transforms, so they no longer meet criterion 1.
- logstash-16968 is replaced by axum-1934: service-shaped, and a crash, so it needs no invariant.
- druid-15402 is dropped, because no remaining service row would add something new. The remaining Redis rows would only further concentrate a pool that is already all Redis/Valkey for as-is fits, and the other service rows are SWE-bench Verified or soft.

1. **redis__redis-10068** (C, fit, 2022-01, BSD-3)
   - The handler is Redis's command loop, one command per step. XADD auto-IDs read the clock.
   - Trigger: a command sequence whose stream master-entry ID delta overflows an int, after which XTRIM MINID deletes every entry.
   - Clock-shift variants reach it.
   - Needs an invariant.
2. **redis__redis-11734** (C, fit, 2023-01)
   - Trigger: BITCOUNT/BITPOS on a missing key reply 0 before validating their arguments.
   - The hidden tests also cover BITPOS, so a BITCOUNT-only fix is narrow. That is evidence about the hidden tests only: whether variants would expose a narrow fix depends on argument perturbation reaching BITPOS, which is unlikely.
   - Needs an invariant.
3. **redis__redis-13115** (C, fit, error-reply, 2024-03-06, BSD-3, before the relicense)
   - Trigger: EVAL passes the Lua number 2e8 to HINCRBY, which receives "2e+08" and replies `-ERR`.
   - `-ERR` is an ordinary reply, so this needs an invariant.
   - Fix changes pre-failure outputs: ? Earlier script-stored numbers would read back differently, so the input sequence must avoid them.
4. **valkey-io__valkey-1499** (C, fit, 2024-12, BSD-3)
   - Trigger: in no-touch mode, TOUCH inside EVAL checks the calling client's command (`current_client->cmd`, which is EVAL) instead of the executing client's, so OBJECT IDLETIME is wrong.
   - The LRU clock is routed through the clock call.
5. **redis__redis-10764** (C, fit, 2022-05)
   - Trigger: commands interleaved from several clients. BZMPOP registers its numeric arguments as blocking keys, so a write to key "10" wakes it.
   - Field variants on the ZADD's key reach the trigger. Reordering or dropping the BZMPOP breaks the reproduction instead.
6. **tokio-rs__axum-1934** (Rust, wrapper, crash, 2023-04)
   - Wrapper: requests fed one at a time to a Router via `tower::oneshot`, where a router nested at "/" is the fallback.
   - Trigger: MatchedPath extraction in the fallback panics. Captured without an invariant.
   - Variants: ?
7. **laravel__framework-48573** (PHP, wrapper, 2023-09, soft)
   - Wrapper: put/get operations on an ArrayStore, with Carbon's clock routed through the clock call.
   - Trigger: the expiry check compares whole seconds, so entries expire up to 1 s late.
   - Clock-shift variants reach it.
   - Fix changes pre-failure outputs: ? (the clock-read pattern in `put` changes).
   - Soft call: it is a library object, and its boundary with laravel-53206 (out@2) is a judgement.
8. **prometheus__prometheus-10633** (Go, wrapper, 2022-04)
   - Wrapper: refresh ticks drive the PuppetDB discoverer through the gateway.
   - Trigger: int and float resource parameters in the response are dropped from labels.
   - Variants: ? The recorded response is raw HTTP bytes (SPEC §4.7), and variants mutate only JSON fields (§6.1).
9. **cli__cli-3517** (Go, wrapper, error-reply, 2021-04)
   - Wrapper: `run watch` requests.
   - Trigger: a recorded 404 from the annotations endpoint aborts the watch. The fix tolerates the 404 without new requests.
   - The upstream condition looks inverted, so check the reference fix before using it.
   - Variants: ? (raw HTTP bytes).
10. **axios__axios-5892** (JS, wrapper, 2023-09, soft: stateless)
    - Wrapper: one upstream call per input.
    - Trigger: an uppercase Content-Encoding is not decompressed. A narrow fix that matches "GZIP" only is plausible.
    - Variants: ? The header sits in the recorded raw bytes, which the JSON-field variants do not mutate.
11. **axios__axios-5085** (JS, wrapper, 2022-10, soft: stateless)
    - Wrapper: one upstream call per input.
    - Trigger: AxiosHeaders normalisation collapses a set-cookie header array to a string.
    - Variants: ? (raw HTTP bytes).
12. **django__django-16255** (Python, wrapper, crash, 2022-11)
    - This is in SWE-bench Verified, the most contaminated subset.
    - Wrapper: GET requests for the sitemap index.
    - Trigger: an empty item query with a callable lastmod makes `max([])` raise ValueError.
13. **fluent__fluentd-3640** (Ruby, wrapper, 2022-02, soft: library object)
    - Wrapper: configure a retry state with a secondary output, exponential backoff and max_steps, feed failures at given clock times, and emit `secondary_transition_at`.
    - Trigger: the fix changes only `calc_max_retry_timeout`. The summed timeout is off by one exponent; per-retry backoff is unchanged.
    - Variants: ? The trigger is numeric config (max_steps, wait, backoff base, secondary), which config variants can only unset or swap for another recorded value. A clock shift moves `start` and `secondary_transition_at` together.
    - **Fix changes pre-failure outputs: y** if `secondary_transition_at` is emitted on every step. Use a wrapper that emits it only on the failing step.
14. **tokio-rs__axum-691** (Rust, wrapper, 2022-01)
    - Wrapper: requests fed one at a time to the Router.
    - Trigger: nesting at "/" strips the wrong prefix.

## Threats to validity

**Contamination**
- Every fit is dated between 2017-04 and 2025-01, and all three sources and their upstream PRs are public. Models trained up to 2026 have probably seen both the issues and the fixes.
- The worst case is the Multi-SWE-bench Python subset, which is exactly SWE-bench Verified:
  - That covers 37 sampled rows, all 6 Python fits and top-14 #12.
  - All are flagged `swe_bench_verified = y`.
  - Treat them as near-certainly memorised.
- DeepSWE is newer: its base commits run to 2026-03 and it was published on 2026-08-26. See "Threats specific to the feature track".

**Selection bias**
- Redis and Valkey supply all 11 as-is fits and 11 of the 30 service fits.
- Six repos supply 66 of the 157 fits.
- The class rules are my judgement and misfire about 7% of the time.
- 13 rows are marked `soft` or `tie`.

**Wrapper realism**
- 146 of the 157 fits need a wrapper.
- 119 fits are transforms, not long-running stateful handlers.
- Only 30 fits are service-shaped, and 7 of those are soft.

**Invariants**
- 145 of the 157 fits (114 wrong-output plus 31 error-reply) are caught live only through an invariant.
- For most of them the invariant essentially encodes the bug. Examples: "lag equals entries after last_id", "round trip is stable", "no diagnostic for this valid program".
- Only 12 fits are crashes.

**Variants vs narrow fix**
- Kavach's variants (SPEC.md §6.1) mutate JSON fields of inputs and gateway responses, reorder or drop events, and shift the clock.
- They cannot meaningfully perturb raw source text, raw bytes, or recorded raw HTTP responses (§4.7).
- So `narrow_fix_possible = y` (134 fits) is evidence about the hidden tests, not that Kavach would catch a narrow fix.
- Variants plausibly reach the trigger in only 3 fits: redis-10068 (clock), redis-10764 (field variants on the ZADD key) and laravel-48573 (clock). Reachability is unclear (?) in 35 fits and impossible (n) in the 119 transforms.

**Fixes that diverge before the failure**
- In 4 of the 30 service fits, the correct upstream fix changes outputs before the failing step, so Kavach would report `diverged`, not `fixed`. In 5 more it is unclear.
- django-13551 cannot avoid this, and Kavach probably cannot verify its fix.

**Missing fail-to-pass tests**
- Six Multi-SWE-bench fits have no fail-to-pass tests, only none-to-pass ones: ponyc-3586, ponyc-4017, ponyc-4505, nlohmann-3463, nlohmann-3523 and simdjson-1899.
- gin-3227's fail-to-pass test calls the radix tree directly and does not exercise the issue's scenario.

**First-pass error**
- Inferred outs carry the 2–18% error rate above.

**Licensing**
- Redis moved to RSALv2/SSPLv1 on 2024-03-20, and Redis 8 added an AGPLv3 option on 2025-05-01. redis-13338 is out@7.
- All 10 Redis fits predate the change and are BSD-3.
- Terraform is under BUSL-1.1 after 2023-08-10, but its rows are out anyway.

**Language mapping**
- Mapping Kotlin to the Java SDK and C++ to the C SDK has not been tested.

## Not accessed

- I did not read the DeepSWE paper or its leaderboard methodology; verdicts rest on the repo alone.
- DeepSWE base-commit dates come from the GitHub commits API. 12 of the 113 base commits returned 404, and their dates are unknown.
- Multi-SWE-bench has no `created_at` field, so its dates are GitHub PR dates.
- I read SWE-bench Multilingual and SWE-bench Verified through the HF rows API, because no parquet tooling was installed.
- No task was built and no test was run.

## Changes after verification

**Round 1**
- **Verdict changes:**
  - redis-11631: fit → out@4 (tie).
  - logstash-16968: fit → fit-with-wrapper.
  - fluentd-3631: → out@4 (tie).
- **New failure class:** `error-reply` (31 fits). Invalid-code outputs are now `wrong-output`, and nlohmann-3463 is `wrong-output`.
- **Top-list fixes:** corrected the descriptions of laravel-48573, fluentd-3640, valkey-1499 and redis-13115. Replaced nlohmann-3463 with axios-5085, and qualified the narrow-fix claim.
- **New columns:** call, swe_bench_verified, shape, variants_reach_trigger, needs_invariant, f2p_present.

**Round 2**
- **Shape relabels (service → other shapes):**
  - logstash-16968, druid-15402 and caddy-4943 → `transform`.
  - druid-14136 → `other`.
  - The service count goes from 34 to 30.
- **Variants set to `?`:** axios-5085, axios-5892, cli-3517 and prometheus-10633. Their gateway records are raw HTTP bytes, so variants reaching the trigger drops from 10 to 5.
- **micropython-13039:** → out@4 (tie), under the memory-dependent-read rule.
- **Soft marks:** fluentd-3640, prometheus-12874, axios-5085, axios-5892 and caddy-5870 marked `soft`.
- **Data cleanups:**
  - The 18 out rows still carrying the retired `error` class were mapped to the current classes.
  - `f2p_present` now uses `n`.
  - The fluentd-3640 reason was corrected.
- **Top list:** shrunk to 14. axum-1934 replaces logstash-16968, and druid-15402 is dropped.
- **Headline:** 158 → 157 fits; 147 → 146 wrapper fits.

**Round 3**
- **Variants set to `?`:** fluentd-3640 (numeric config keys) and django-13551 (an earlier-input trigger). The count of variant-reachable fits drops from 5 to 3.
- **django-13551:** a note added that its fix diverges before the failure.
- **redis-10764:** wording corrected. Field variants on the ZADD key reach the trigger; reordering or dropping BZMPOP breaks it.
- **New column `fix_changes_prefailure_outputs`** for the 30 service fits: 4 y, 5 ?, 21 n. The top-list entries are marked accordingly.

**Round 4 (gap roadmap and feature track)**
- **New column `kavach_gap`** on all 613 rows, with the vocabulary, counts and implications in "Gaps".
  - The three fits judged to have contract-level invariants (redis-10068, redis-10764, laravel-48573) were marked `none`; round 5 moved laravel-48573 back.
  - Every other wrong-output or error-reply fit is `invariant-leaks-bug`, as an upper bound.
- **DeepSWE feature track:** all 113 tasks re-screened with item 1 waived.
  - New DeepSWE-only columns: `feature_track`, `feature_shape`, `hidden_verifier_independent`, `traffic_needed` and `feature_reason`.
  - The original `verdict` is unchanged.
- **DeepSWE `created`** now carries the base commit date, the image month and the dataset commit date.
- **DeepSWE `language`** corrected for three tasks whose task.toml was wrong. The language table moves by one row: typescript 136 → 135, go 114 → 115. No verdict changed.
- **Unchanged:** all other tables. The yield tables were recomputed from the new ledger and are identical apart from the language move above.

**Round 5 fixes**
- **laravel-48573:** `none` → `invariant-leaks-bug`, by the tie rule. 2 rows remain `none` (redis-10068, redis-10764).
- **Wording:**
  - "solves 3 of 613" → "2 have no identified capability gap", with the caveats (nothing run, no Redis integration, the §6.1 variant requirement unshown).
  - "solvable set from 3 to 15" → "would remove the variants gap for 12 crash fits", with the caveats (seven need open-ended fuzzing, three ponyc rows have no fail-to-pass tests, django-16255 is in SWE-bench Verified).
  - No other "solves" or "solvable" claim remains.
- **Gap relabels:** the 73 non-DeepSWE inferred argv and timing rows were re-checked. 11 were relabelled `not-input-driven` (clap ×9, coreutils-6690, tokio-4867), 2 changed only by ordering or licence, and 60 were unchanged. The audited clap-2990 was relabelled to match. The 12 DeepSWE rows were re-checked in round 6 (see "Gaps"); no gap changed, though goreleaser-retry-publish-auditing had been reordered in round 5.
- **Licence rule:** defined once (see `licence` in "Gaps") and applied everywhere. The 5 Terraform BUSL-1.1 rows now name `licence`, as redis-13338 does.
- **Ordering:** `out-of-model` first whenever present, then `licence`. This reordered 15 rows and changed the first-gap counts.
- **Feature-track softness:**
  - httpx CookieStore, kombu ×2 and updo marked soft, using the main ledger's standard for library objects fed operations.
  - Soft fits: 9 → 13.
  - The top-10 claim was corrected.
- **Weak-tests list:** added anko-default-function-arguments (2) and abs-stepped-slices (6).
- **Unchanged:** yield, shape and language tables. They were recomputed and are identical.
