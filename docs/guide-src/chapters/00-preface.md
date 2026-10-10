# How to read this guide

This guide walks through the Kavach repository in the order data flows through it. A handler running in production calls `env.Now()`; the SDK turns that into a record; the recorder writes it to a journal; a crash cuts the journal into a fixture; replay plays the fixture back against a build; and `kavach diff` decides whether a fix really fixed it. Each chapter covers one of those stages.

Kavach asks one thing of a service: that it speaks two small protocols, the recorder protocol (`SPEC.md` §10) while it runs and the host protocol (§9) when it is replayed. The only Kavach code inside the service is a thin writer for those protocols, which sends a frame for each input, read and output of the handler. Everything else, the recorder included, runs in separate processes. An SDK is a ready-made protocol writer, and this guide reads the Go one; a service in a language with no SDK speaks the protocols itself, as `INTEGRATING.md` describes and `examples/ledger/protocol` shows.

Every chapter has three kinds of material, kept visually apart:

- **Explanation**, in normal text: what the part is for, how it works, and why it is built that way.
- **Specification**, in blue-ruled boxes titled *SPEC.md §n*: the normative text, copied verbatim from `SPEC.md` at the commit this guide was built from. When the explanation and the spec disagree, the spec wins.
- **Code**, in grey boxes titled with the file and line range: the actual source, with its real line numbers, so you can open the file and find the same lines.

Orange **Common trap** boxes flag mistakes that are easy to make when changing that part. Blue **Try it** boxes give commands to run against the repository; they assume the repository root as the working directory.

## The six chapters

1. **The journal format** (`journal/`). The file every other part reads or writes.
2. **The SDK** (`sdk/go/`). The thin protocol writer inside the service: how a handler's reads and outputs become frames for the recorder.
3. **The recorder** (`cmd/kavach-recorder`, `internal/recorder`). The separate process that writes journals, rotates segments and cuts fixtures.
4. **The shared-memory ring** (`internal/recstream`). How records cross from the service to the recorder for tens of nanoseconds each.
5. **Replay and verification** (`replay/`). Playing a fixture back against a build, and deciding whether a fix is real.
6. **Conformance and the agent loop** (`spec/`, `cmd/kavach`, `bench/`). How twelve languages are held to one contract, and how a coding agent uses all of it.

## Map of the repository

| Path | What it holds | Chapter |
| --- | --- | --- |
| `SPEC.md` | The format and both protocols (host, recorder) | all |
| `INTEGRATING.md` | Speaking both protocols without an SDK | 2, 6 |
| `journal/` | Reading and writing `.kavach` files | 1 |
| `sdk/go/` | The Go SDK: `Env`, `Recorder`, the host side of replay | 2, 5 |
| `sdk/<lang>/` | The same SDK in eleven more languages | 2, 6 |
| `cmd/kavach-recorder`, `internal/recorder` | The recorder process | 3 |
| `internal/recstream` | Record-stream frames, the ring | 3, 4 |
| `internal/envfacts` | Host and kernel facts for `environment` records | 3 |
| `replay/` | Replay engine, host driver, verification, variants, drift | 5 |
| `cmd/kavach` | The CLI and the MCP server | 6 |
| `spec/testdata`, `spec/host`, `spec/recorder` | Conformance suites | 6 |
| `examples/ledger/sdk`, `examples/ledger/protocol`, `bench/` | The demo service with the Go SDK and with no SDK, and the planted-bug benchmark | 2, 6 |
| `BENCHMARKS.md` | Every published number, with the command that measured it | 4 |

The model behind all of it is in §1 of the specification, reproduced here because every chapter builds on it.

@spec 1

## Companion: the code graph

A graph of the core code (every Go file, the spec and the test generators) was built with Graphify into `graphify-out-core/` at the repository root. `graph.html` opens in a browser. Two commands are useful alongside each chapter:

```
graphify explain "Record"            --graph graphify-out-core/graph.json
graphify path "recordEnv" "publish"  --graph graphify-out-core/graph.json
```
