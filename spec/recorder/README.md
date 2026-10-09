# Recorder conformance

[SPEC.md §10.6](../../SPEC.md#106-recorder-conformance) splits recorder
conformance into two halves, tested separately.

- **SDK half**, in [`sdk/`](sdk/): see [`sdk/README.md`](sdk/README.md).
- **Recorder half**, in [`journal/`](journal/): record streams, each paired with
  the control messages, journals and fixtures `kavach-recorder` must produce.
  This file describes it.

## Recorder half

One directory per case, `journal/<case>/`, with three files:

| File | Holds |
| --- | --- |
| `stream.json` | `description` and `frames`: the record stream to feed to the recorder, frame by frame. |
| `facts.json` | The recorder's test-mode input (`--test-facts`): `run`, `recorded_at` and `facts`, the `env.` and `host.` facts it must use instead of collecting them. `facts` is an object from key to `{"value": <base64>}`, `{"sha256": <base64>}` or `{"unset": true}`, as `kavach-recorder facts` prints it. Test mode also turns host watching off. |
| `expected.json` | What the recorder must do: `exit` (its exit status), `control` (the control messages it writes, in order) and `files` (every `.kavach` file left under the case's directory, decoded). |

### stream.json

Frames use the notation of the SDK half (`sdk/README.md`): bytes are base64,
`unix_nanos` is a string, `scope` is `"remote"` or `"local"`, `present` is a
boolean.

| Frame | JSON |
| --- | --- |
| open | `{"frame": "open", "open": {...}}`. The runner sets `open.dir` to a fresh empty directory, whatever the case says. |
| record | `{"frame": "record", "type": ..., "critical": bool, ...fields}` |
| step_end, close | `{"frame": "step_end"}`, `{"frame": "close"}` |
| facts | `{"frame": "facts", "facts": {key: {"value": ...} \| {"sha256": ...} \| {"unset": true}}}` |
| snapshot | `{"frame": "snapshot", "data": b64}` |
| flush | `{"frame": "flush", "durable": bool}` |
| raw | `{"frame": "raw", "raw_kind": n, "payload": b64}`: any frame kind, for malformed input |

The runner writes the frames into the recorder's standard input through a pipe
and closes it after the last one: a stream with no `close` frame ends the way a
dead service's would (SPEC.md §10.5).

A recorder passes every case over both transports (SPEC.md §10.7). Over the
ring, the runner sends the `open` frame with `ring` added on standard input,
publishes all the other frames into a ring it passes as file descriptor 3,
writes one doorbell byte and closes standard input. The expected output is the
same. A stream that does not start with an `open` frame goes over the pipe.

### expected.json

- `exit` is 0 unless the case ends in a fatal error.
- `control` lists the JSON objects on the recorder's standard output. A `file`
  field is given relative to `open.dir`.
- `files` maps each file's path relative to `open.dir` to its decoded journal,
  in the format of `spec/testdata/valid/*.json` (`major`, `minor`, `meta`,
  `records`, `truncated`). Files are named `<service>-<run>-<segment, six
  digits>.kavach` for segments and `fixtures/<service>-<run>-<seq, six
  digits>.kavach` for fixtures, `seq` being the failing step's input. No other
  file may be left behind. A file the recorder deleted (retention) is absent.
- Journals are compared decoded, never byte for byte: block boundaries and
  compressed bytes are not part of the contract. Every file must also pass
  `zstd -t`, where the tool is installed.

### Running

`go test ./cmd/kavach-recorder` builds `kavach-recorder`, runs every case and
compares. `go test ./cmd/kavach-recorder -update` regenerates `stream.json` and
`facts.json` from the case definitions in `cmd/kavach-recorder/conformance_test.go`
and `expected.json` from what the recorder writes; review the diff.
