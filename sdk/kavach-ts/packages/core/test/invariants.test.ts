import assert from "node:assert/strict";
import test from "node:test";
import { InvariantEvaluator, parseInvariantsYAML } from "@kavachlabs/core";

test("zero-dep invariants.yaml parser and evaluator enforce all 4 invariant kinds", () => {
  const yaml = `
# Kavach billing invariants
service: events-processor
invariants:
  - name: atomic_single_emit_on_retry
    kind: at_most_once_emit
    sink: kafka:events_charged_in_advance
    key: $.transaction_id
    on_error_emit_count: 0

  - name: no_silent_billing_drop
    kind: anti_lobotomy
    sinks: [kafka:events_enriched, kafka:events_charged_in_advance]
    min_ratio_vs_baseline: 1.0

  - name: non_negative_charge
    kind: non_negative
    sink: kafka:events_charged_in_advance
    field: $.amount_cents

  - name: double_entry_conservation
    kind: conservation
    sink: ledger:entries
    field: $.entries[*].delta
`;

  const parsed = parseInvariantsYAML(yaml);
  assert.equal(parsed.service, "events-processor");
  assert.equal(parsed.invariants.length, 4);
  assert.equal(parsed.invariants[0].on_error_emit_count, 0);
  assert.deepEqual(parsed.invariants[1].sinks, ["kafka:events_enriched", "kafka:events_charged_in_advance"]);

  const ev = new InvariantEvaluator(parsed.invariants);

  // Failing step emitting to events_charged_in_advance violates on_error_emit_count: 0
  const vErr = ev.checkStep({
    seq: 1n,
    attempt: 1,
    err: new Error("LOADING Redis is loading"),
    outputs: [
      {
        sink: "kafka:events_charged_in_advance",
        data: Buffer.from('{"transaction_id":"tx_1","amount_cents":500}'),
      },
    ],
  });
  assert.ok(vErr);
  assert.equal(vErr.name, "atomic_single_emit_on_retry");

  // Retry emitting duplicate transaction_id violates at_most_once_emit even on success
  const vDup = ev.checkStep({
    seq: 2n,
    attempt: 2,
    outputs: [
      {
        sink: "kafka:events_charged_in_advance",
        data: Buffer.from('{"transaction_id":"tx_1","amount_cents":500}'),
      },
    ],
  });
  assert.ok(vDup);
  assert.equal(vDup.name, "atomic_single_emit_on_retry");

  // Fresh session: negative amount violates non_negative
  ev.reset();
  const vNeg = ev.checkStep({
    seq: 1n,
    attempt: 1,
    outputs: [
      {
        sink: "kafka:events_charged_in_advance",
        data: Buffer.from('{"transaction_id":"tx_2","amount_cents":-10}'),
      },
    ],
  });
  assert.ok(vNeg);
  assert.equal(vNeg.name, "non_negative_charge");

  // Conservation violation
  ev.reset();
  const vCons = ev.checkStep({
    seq: 1n,
    attempt: 1,
    outputs: [
      {
        sink: "ledger:entries",
        data: Buffer.from('{"entries":[{"delta":100},{"delta":-90}]}'),
      },
    ],
  });
  assert.ok(vCons);
  assert.equal(vCons.name, "double_entry_conservation");

  // Balanced conservation + anti_lobotomy checkSession
  ev.reset();
  ev.setBaselineCounts({ "kafka:events_enriched": 1, "kafka:events_charged_in_advance": 1 });
  const vOk = ev.checkStep({
    seq: 1n,
    attempt: 1,
    outputs: [
      { sink: "kafka:events_enriched", data: Buffer.from('{"transaction_id":"tx_3"}') },
      { sink: "ledger:entries", data: Buffer.from('{"entries":[{"delta":100},{"delta":-100}]}') },
    ],
  });
  assert.equal(vOk, null);
  const vLobotomy = ev.checkSession();
  assert.ok(vLobotomy);
  assert.equal(vLobotomy.name, "no_silent_billing_drop");
});
