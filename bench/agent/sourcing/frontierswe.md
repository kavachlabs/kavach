# FrontierSWE v2 screening

All 34 FrontierSWE v2 tasks were screened against the problem-sourcer rubric, with item 1 (kind) waived as in the DeepSWE feature track. Each task is one row in `frontierswe.csv`, and the `kavach_gap` vocabulary is the one defined in `README.md`. A new column, `differential_contract`, records whether the task's original program could serve as the recorded reference for an equivalence check with `kavach diff`.

Accessed 2026-10-10. No task was built, no image was pulled, and no agent was run.

**Headline**
- **Yield:** 9 of 34 tasks fit with a wrapper (26%): 7 transforms and 2 services. None fits as-is. 1 is out@2, 16 are out@3 and 8 are out@4. Five of the nine fits are soft (cranelift, verilog, SPICE, QE, Lean), and so is stepper.
- **Differential contracts:** 17 of 34 tasks have one: a reference program whose outputs could be recorded, with the agent's build required to match them. All 9 fits are among the 17. One more (crash-proof-flash-filesystem) is disputed.
- **Exact or tolerance.** Of the 9 fits, 6 have exact contracts (libexpat, dart-style, verilog, Lean, cranelift, stepper). 3 need a tolerance diff (SPICE, QE, FFmpeg).
- **The best candidates are pure ports with exact output contracts:**
  - libexpat → x86-64 assembly (not soft);
  - dart-style → Haskell (not soft);
  - Icarus Verilog → Swift (soft: file and argv inputs);
  - the Lean 4 kernel → Pascal (soft: inputs over the 16 MiB record limit).
- **The two largest ports are both blocked:**
  - git → Zig, by pervasive filesystem state, argv inputs and child processes;
  - PostgreSQL → Zig on SQLite, by on-disk database state and process lifecycle.
- **The research and scientific tasks are out.** 16 tasks are scored on model or solution quality, so there is no contract for Kavach to check.

## Sources and access

| What | Where | Version | Status |
|---|---|---|---|
| Task repo | github.com/Proximal-Labs/frontier-swe-v2 | commit da83f84f (2026-09-19). Repo created 2026-09-02; CHANGELOG has the first public release on 2026-09-03 and an image-pinning update on 2026-09-19. | Public, cloned in full (about 300 MB). All 34 tasks present. |
| Site and leaderboard | frontierswe.com | as of 2026-10-10 | Public |
| Launch blog | frontierswe.com/blog/v2 | September 2026 (no exact day given) | Public |
| Runner | github.com/Proximal-Labs/px-eval, over Harbor | not inspected | Public per the README |
| Harness | Proximus | not public | The README says it will be merged into Harbor |

**The count is confirmed:** 34 task directories, matching the README table and the site.

**What each task ships:**
- `instruction.md`;
- `task.toml`, with images pinned by digest, a 72,000 s agent timeout (20 h; reconnaissance-blind-chess-recovery has 54,000 s, 15 h), and verifier timeouts of 30 min to 8 h;
- `environment/`: the Dockerfile, setup, the hidden `tests/` with `compute_reward.py`, and the workspace;
- `solution/solve.sh` (the oracle);
- `preflight/`.

**What I read:**
- every `instruction.md` and `task.toml`;
- each task's test file list;
- the Dockerfiles, to find which reference programs are baked in;
- git-to-zig's `compute_reward.py`;
- the comparison code of the visual, SPICE and QE tasks.

I did not read the per-task READMEs inside the workspaces, which hold the full contracts. Verdicts rest on the instructions plus the graders.

**Licence.**
- The repo has no licence file. Its README says: "Task content and test harnesses are provided for evaluation purposes. See individual task directories for attribution of upstream projects and datasets."
- Use for evaluation appears allowed. Redistribution and derived task sets are not granted. I did not mark `licence` as a gap, because evaluation is the intended use, but check this before shipping a track.

**The README warns:** "Task content and scoring may still be updated."

**Not used for numbers:** the Epoch, Mercor and BenchLM pages, as the brief asked.

## Categories and scoring

The README table gives each task one or two categories, and the first is primary. The csv records the primary category, and the yield table counts it:
- `implementation`
- `perf` (Performance Optimisation)
- `scientific` (Scientific Computing)
- `visual` (Visual Reasoning)
- `research` (AI Research)

**Every task is graded continuously by its own `compute_reward.py`, to a reward in [0, 1].**
- Ports score weighted pass fractions against the reference. git-to-zig, for example, uses per-script completion capped at 0.75% weight per script.
- Performance tasks score speed. This is gated on equivalence with the unmodified build, and per the blog it uses an instruction-count proxy rather than wall-clock time.
- Research tasks score quality against hidden data.
- Seven graders use explicit anchors or baselines.

**The verifiers are independent of Kavach.** Each ships with its task and runs in its own image. The blog says they use "1:1 structural mutation", meaning held-out twins and perturbed suites, to defeat hard-coding.

## Yield by category

| Category (primary) | Screened | fit | fit-with-wrapper | out@3 | out@4 | differential_contract y |
|---|---|---|---|---|---|---|
| implementation | 9 | 0 | 4 | 0 | 4 | 8 |
| perf | 6 | 0 | 3 | 3 | 0 | 3 |
| visual | 6 | 0 | 1 | 3 | 2 | 3 |
| scientific | 6 | 0 | 1 | 5 | 0 | 1 |
| research | 7 | 0 | 0 | 5 | 2 | 2 |
| **all** | 34 | 0 | 9 | 16 | 8 | 17 |

The implementation row also has crash-proof-flash-filesystem at out@2, with its differential contract disputed (`?`).

- **Fits by shape:** 7 transforms and 2 services. The services are lean-4-kernel-type-checker-in-pascal (one NDJSON line per input, see below) and stepper-music-sequencer-gba.
- **Gap counts** (rows naming the gap / rows where it is the most fundamental gap):

  | Gap | Rows naming it | First |
  |---|---|---|
  | out-of-model | 16 | 16 |
  | threads-concurrency | 7 | 4 |
  | filesystem-state | 7 | 3 |
  | feature-needs-diff-acceptance | 6 | 3 |
  | argv-startup-config | 6 | 0 |
  | child-process | 6 | 2 |
  | timing-hang-leak | 5 | 1 |
  | variants-cannot-reach | 5 | 3 |
  | not-input-driven | 1 | 1 |
  | none | 1 | 1 |

- **All 16 out-of-model rows have the same cause:** the score measures quality (model accuracy, compression size, routing timesteps, win rate, lap time), and there is no contract for Kavach to check.
- **For FrontierSWE, `feature-needs-diff-acceptance` mostly means tolerance.** FFmpeg (PSNR ≥ 60/40 dB), QE (pw tolerances), SPICE (rtol 0.01 plus a waveform metric), the Remotion video, the flight sim and Wan 2.1 all compare within numeric or pixel tolerances. `kavach diff` would need to accept bounded differences, not only listed ones. The GBA sequencer does not need this: its contract is exact.
- **File-path CLIs are soft.** cranelift, verilog, SPICE and QE read their inputs from files named on argv (WASI `--dir`; `` `include ``; cwd and `.include`; `--pseudo-dir`) and some write output files (simulation byproducts; QE's `results.json`). They carry `filesystem-state` and `argv-startup-config`, as git does. Fit-with-wrapper assumes the deliverable reads its inputs through the protocol, which changes the task.
- **SDKs:**
  - Ports whose language has an SDK: Haskell, Rust, Go, Python, TypeScript and C (for C-ABI libraries).
  - Ports with no SDK: Zig (flash FS, libswscale, git, PostgreSQL), Pascal (Lean), Swift (Verilog), x86-64 assembly (libexpat) and Mojo.
    - Zig and assembly export C ABIs, so a C SDK wrapper can drive them.
    - Pascal and Swift are standalone binaries that would have to speak the protocols (INTEGRATING.md).
  - On the original side, an SDK is not needed. The original's outputs only have to be recorded at the boundary (stdin/stdout, the wire, the C ABI); only the agent's port has to replay.

## Differential-contract candidates

A differential contract here means the original program's outputs on a set of inputs are recorded as the reference, and the agent's build must produce the same outputs, checked with `kavach diff`. Kavach could assert this equivalence as a contract.

| Task | Original → port | Original as recorded reference | Port handler-shaped? | SDK | What it would need |
|---|---|---|---|---|---|
| libexpat-optimization | libexpat (C) → x86-64 asm, C ABI | yes, exact: event traces are already baked | **yes**, as a transform: one (document, parse mode) per input, whole-document trace out. Not per chunk: the grader feeds fixed 7-byte chunks and coalesces character data across callbacks and `XML_Parse` calls (`cd_append`/`flush_cd`), so only the whole trace is the contract | C wrapper around the asm `.so` | Nothing for correctness. Speed is outside Kavach. |
| dart-style-in-haskell | dart_style (Dart) → Haskell | yes: CLI stdin/stdout | yes: one source (+ config) per input | Haskell SDK | Generator variants for source text; corpus coverage otherwise |
| verilog-simulator-in-swift | Icarus Verilog → Swift | yes, exact: goldens | soft: one design per input, but designs are file paths on argv, `` `include `` reads from disk, and simulations write byproduct files | none for Swift (protocols) | A Swift protocol client; includes and byproducts routed through the protocol. Soft. |
| lean-4-kernel-type-checker-in-pascal | Lean 4 kernel → Free Pascal | yes, exact: accept/reject verdicts | soft: export files run to about 90 MB, over SPEC §3's 16 MiB record limit (§11 "Large inputs"). I chose one NDJSON export line per input, folded into the checker's environment, with the verdict emitted at an end-of-file input. That makes it a service. The alternative is a SPEC change to raise the record limit. The wrapper must feed raw line bytes faithfully, because malformed or structurally invalid lines are reject cases. Line sizes are unverified: the export corpora are not in git, the dataset registry refused an anonymous pull (401), and the data is only obtainable by pulling the multi-GB prebuilt images | none for Pascal (protocols) | A Pascal protocol client. Soft. |
| spice-circuit-simulator-in-rust | ngspice (C) → Rust | tolerance: rtol 0.01 plus a waveform metric | soft: one netlist per input, but run from the netlist's directory and `.include` reads further files | Rust SDK | Tolerance diff; cwd and includes routed through the protocol. Soft. |
| quantum-espresso-pwx-in-rust | QE pw.x (Fortran) → Rust | tolerance: pw tolerances against gold.out | soft: one input deck per input, but it reads `--pseudo-dir` from disk and writes `results.json` | Rust SDK | Tolerance diff; pseudopotentials and the output file routed through the protocol. Soft. |
| ffmpeg-libswscale-optimization | libswscale (C) → Zig, C ABI | tolerance: PSNR ≥ 60/40 dB | yes: one conversion per input | C wrapper | Tolerance diff; speed is outside Kavach |
| stepper-music-sequencer-gba | reference ROM → new ROM, both under mGBA | yes, exact: pixel-identical frames, identical audio, and two identical reference captures required | yes, but the handler is the emulator plus the ROM; key events per input | SDK on the emulator host only | Emulator as handler. Soft. |
| cranelift-codegen-opt | unmodified Cranelift → optimised (same language) | yes, exact | soft: one Wasm workload per input, but workloads run under Wasmtime with WASI `--dir` and input files, and clock, rand and fs go through WASI | Rust SDK | WASI clock, rand and fs routed through the protocol. Speed is outside Kavach. Soft. |
| crash-proof-flash-filesystem | littlefs (C) → Zig, C ABI | disputed: littlefs is the reference only through its own white-box C tests | no: the contract is littlefs's TOML-embedded C test cases compiled into `runners/test_runner.c`. test_powerloss.toml reads `dir.m.pair` and `dir.m.rev`, so the port must match the C struct layout, and power loss is injected by `longjmp` out of the block device (`powerloss_longjmp`) with exhaustive branching. No operation stream exists | C wrapper over the Zig exports | Re-expressing the tests as an operation stream (rubric item 2). Out@2, tie call. |
| postgresql-18-on-sqlite | PostgreSQL 18.3 → Zig on SQLite | yes: replies recordable at the wire | the SQL session is; the process is not | none for Zig | On-disk database state (initdb), pg_ctl process lifecycle, argv[0] dispatch. Out@4, soft. |
| git-to-zig | git (C) → Zig | yes | no: argv commands against a repository on disk; test scripts spawn processes | none for Zig | Filesystem state everywhere, argv as input, child processes. Out@4. |
| lua-native-compiler | Lua 5.4 → Go AOT compiler | yes: expected stdout | compile is a transform, but checking means running the ELF (two targets emulated) | Go SDK | Child processes. Out@4. |
| fitness-recap-video-in-remotion | reference generator → Remotion | yes | JSON in, frames out, but rendering spawns headless Chrome and ffmpeg | TS SDK | Child processes; tolerance. Out@4. |
| flight-sim-renderer-in-opengl | reference renderer → new C/C++ renderer | yes | the flight model is (240 Hz key events fold into state); rendering on llvmpipe uses threads | C SDK | Threads; pixel tolerance. Out@4, soft. |
| wan-2.1-on-max-mojo | PyTorch Wan 2.1 → MAX/Mojo | yes | no: GPU generation is nondeterministic | Python SDK; Mojo behind it | GPU nondeterminism; tolerance. Out@4. |
| granite-mamba2-inference-optimization | reference layer → optimised (same language) | yes | no: bf16 CUDA nondeterminism | Python SDK | GPU nondeterminism; tolerance. Out@4. |
| sglang-inference-system-optimization | starting server → optimised (same language) | yes | request-shaped, but GPU batching is nondeterministic and the gate is a 0.95 similarity threshold | Python SDK | Concurrency; threshold contract. Out@4. |

**Where Kavach would add something.** The graders already compare against the reference, so Kavach's equivalence contract would duplicate them. It adds something only in two cases:
- Kavach's inputs reach behaviour the suites do not. How far field-level variants (SPEC §6.1) reach differs by fit:
  - **stepper:** yes. Its inputs are JSON key events, so field changes, drops, reorders and clock shifts all apply.
  - **Lean:** partly. Each NDJSON line is mutable, but a malformed or structurally invalid line is itself a reject case, so many variants test the reject path rather than the original behaviour.
  - **libexpat, dart-style, verilog, SPICE, QE, cranelift and FFmpeg:** not usefully. Their inputs are whole documents, sources, netlists, decks, Wasm modules or images. Field-level mutation of the input record mostly yields malformed input, so meaningful variants need generators for source text, netlists and XML.
- Kavach is used during the run, as the agent's own check. FrontierSWE already gives agents a self-check tool for this.

## Threats

- **Cost.**
  - Each trial is a 20-hour agent budget (15 h for one task), 5 trials per task, plus 30 min to 8 h of verifier time.
  - GPU tasks need L4 up to B200, and several need Modal VM profiles (`job.yaml`, per the CHANGELOG).
  - Proximal's leaderboard reports average API costs per trial from $8.57 to $1,029.65. The full set is 34 tasks × 5 trials = 170 trials, so about **$1.5k–$175k per model**, API cost only.
  - The showcase below is 4–5 tasks × 5 trials = 20–25 trials, so **$171–$25.7k per model**, API cost only.
  - Sandbox and GPU compute come on top of both. This is my extrapolation from per-model averages; there are no per-task costs.
- **Evaluation-aware harness.**
  - Proximal's results come from Proximus. Its system prompt references grading, and its `submit` tool tells the model it is being evaluated. The blog calls both choices intentional.
  - The blog reports five confirmed reward-hacking incidents, all zeroed:
    - Flash FS: reading hidden references via the Modal daemon (GPT-5.6 Sol);
    - Git to Zig: depending on the host's git (Muse 1.2);
    - TORCS: bypassing the telemetry gate;
    - GBA Stepper: exfiltrating the reference ROM (Muse 1.2);
    - TORCS: restoring files before submitting.
  - The SPICE gold-file lookup was a separate self-check episode, not one of the five, and was not zeroed: it scored 33/99.
  - Kavach runs would use a different harness, so the scores are not comparable with the leaderboard.
  - **Reference artefacts in the sandbox leak.** The flash-FS and GBA Stepper incidents both show it, and Stepper is one of the fits: its reference ROM is the thing a Kavach track would record from. Recorded references must stay outside the agent's sandbox.
- **Continuous scoring vs binary verdicts.**
  - FrontierSWE scores partial credit; Kavach's replay verdicts are binary per journal.
  - A Kavach track would have to report the fraction of recorded inputs on which the port is equivalent, which is what the graders already compute.
  - The three tolerance fits (SPICE, QE, FFmpeg) also need `kavach diff` to accept numeric tolerances, which it does not today.
- **Data access.** Lean's export corpora, and the data for 11 other tasks (all outs), are not in git. They come only from Proximal's dataset registry, which refused an anonymous pull (401), or from the multi-GB prebuilt images. Lean's per-line sizes are therefore unverified.
- **Contamination.**
  - The tasks went public on 2026-09-03.
  - The originals (git, PostgreSQL, libexpat, dart_style, Icarus, ngspice, QE, Lean) are famous open-source programs that every model has seen. The ports are new, but the behaviour to match is not.
- **SDK coverage.** Of the four best candidates, three need a non-SDK language to speak the protocols: Swift, Pascal, and assembly via a C wrapper. That is real integration work per language, and it is untested.
- **Single reader.** All verdicts are mine, from instructions and graders, not workspace READMEs. Nine are soft calls: cranelift-codegen-opt, crash-proof-flash-filesystem (a tie, scored out@2), flight-sim-renderer-in-opengl, lean-4-kernel-type-checker-in-pascal, postgresql-18-on-sqlite, quantum-espresso-pwx-in-rust, spice-circuit-simulator-in-rust, stepper-music-sequencer-gba and verilog-simulator-in-swift. One verifier pass checked the repo, licence, tables and leaderboard numbers, and corrected several verdicts.

## Leaderboard (from Proximal's own pages)

**frontierswe.com, as of 2026-10-10**, the site's default "Per provider · Best" view, which shows each provider's best model only (mean@5 over 34 tasks, Proximus harness; the page gives no per-model dates; "maximum reasoning effort" is from the blog, not the site):

| # | Model | Score | ± | Avg cost / trial | Time |
|---|---|---|---|---|---|
| 1 | GPT-6 Astra | 65.5% | 8.9 | $1,029.65 | 12.1 h |
| 2 | Claude Opus 5.5 | 62.3% | 9.8 | $98.87 | 14.2 h |
| 3 | Gemini 4 Argon | 55.0% | 9.9 | $129.36 | 10.6 h |
| 4 | **GLM-5.3** | 30.2% | 11.5 | $97.22 | 17.0 h |
| 5 | Grok 4.7 | 29.5% | 12.6 | $318.77 | 12.1 h |
| 6 | Kimi K3 | 25.9% | 11.8 | $109.71 | 18.4 h |
| 7 | Muse Spark 1.3 | 25.8% | 14.2 | $243.34 | 17.0 h |
| 8 | Qwen3.8-Max-0902 | 17.8% | 9.4 | $51.27 | 19.4 h |
| 9 | DeepSeek V4 Flash Vision Exp | 14.8% | 9.7 | $8.57 | 14.8 h |
| 10 | Inkling | 4.1% | 5.3 | $9.15 | 70 min |

- The page notes Gemini 4 Argon ran with a much lower cache-hit rate, because of a non-production setting.
- **The full model list is longer.** The site's Pareto data covers 22 models. Ranked by mean reward:
  - above GLM-5.3: GPT-6 Astra, Claude Opus 5.5, Claude Sonnet 5.5 (0.6189), Fable 5.1 (0.5653), Gemini 4 Argon, Opus 5 (0.5201), Fable 5 (0.4720), Haiku 5.5 (0.4377) and GPT-5.6 Sol (0.3220);
  - GLM-5.3 (0.3018) is 10th of 22;
  - below it, among others: Grok 4.6 (0.2529), Gemini 3.7 Flash (0.2026), Gemini 3.8 Flash (0.1963), GLM-5.3 Flash (0.1813, $11.03 per run), Qwen3.8-Max-0902 (0.1781, $51.27), Qwen3.8-Max (0.1581) and Muse Spark 1.2 (0.1197).

**Launch blog, September 2026** (frontierswe.com/blog/v2). Earlier results:
- Claude Fable 5.1: 56.29% (Opus 5 was used as a fallback where content filters blocked it).
- GPT-5.6 Sol: 32.2%.
- **GLM-5.3: 30.2%.**
- Kimi K3: 25.9%.
- Grok 4.6: 25.3%.
- Gemini 3.7 Flash: 20.3%.
- Qwen3.8-Max: 15.8%.
- DeepSeek V4 Flash Vision Exp: 14.8%.
- Muse Spark 1.2: 12.0%.
- Inkling: 4.1%.

**Open models.**
- The blog calls GLM-5.3 "the strongest open-weight model". It ranks 4th in the default per-provider view and 10th of 22 models in the full data.
- Neither page labels the other models as open or closed. I have not confirmed which of Kimi K3, Qwen3.8-Max, DeepSeek V4 Flash Vision Exp and GLM-5.3 Flash are open-weight from Proximal's source.

## Recommendation

**A small showcase track is worth it, if it is scoped to the differential ports.**

**Use 4–5 CPU-only tasks with exact contracts** (20–25 trials, $171–$25.7k per model in API cost):
- libexpat-optimization, the best fit: a transform from one (document, parse mode) to its whole-document event trace, with exact equality and a C ABI;
- dart-style-in-haskell, which has an SDK language;
- verilog-simulator-in-swift and lean-4-kernel-type-checker-in-pascal, which show Kavach's protocols working without an SDK. Both are soft: verilog needs its includes and byproducts routed through the protocol, and Lean needs the per-line boundary (or a SPEC change);
- optionally cranelift-codegen-opt, which is exact but soft (WASI clock, rand and fs).

**What the showcase can claim:** `kavach diff` works as a cross-language equivalence contract against a recorded reference, and Kavach scores agree or disagree with the independent graders.

**What it cannot claim:** that Kavach measures FrontierSWE performance. The graders are continuous and already differential, and the scores are not comparable with Proximal's harness.

**Two prerequisites:**
- recorded references kept outside the sandbox;
- for SPICE, QE and FFmpeg, numeric-tolerance diff acceptance, if they are added later.

**Do not pursue:**
- git-to-zig or postgresql-18-on-sqlite as a showcase until Kavach handles filesystem state and process lifecycle;
- crash-proof-flash-filesystem: its contract is white-box C test code, not an input stream;
- the 16 quality-scored research and scientific tasks: Kavach has nothing to check there.
