# `kavach contract` Verdicts & Lock Reference (`SPEC.md §6.4`)

## Contract Verdicts

| Verdict | Meaning | Agent Action |
| --- | --- | --- |
| `pass` | Contract is approved, lock hashes match, non-feature steps have zero regressions, all feature steps changed as required, and all invariants hold across baseline traffic and feature variants. | Audit `report.changed` against the spec and report completion. |
| `regression@N` | Non-feature step `N` produced different outputs from `--old` (or failed). | Restore `--old` behaviour on step `N`. If step `N` is causally downstream of a feature step, stop and ask the user to update `--feature-steps` and re-approve. |
| `unreached@N` | Marked feature step `N` produced the exact same outputs as `--old`. | Your new feature was not triggered or had no observable effect on step `N`. |
| `variant_failed(K)@N` | Perturbed variant `K` of a feature step panicked, errored, or violated an invariant at step `N`. | Replay the saved variant file with `kavach replay <file> --bin <new>` and generalize the implementation. |
| `invariant_violated(X)@N` | Declared invariant `X` failed after step `N` on the contract traffic. | Fix the state corruption in your feature implementation. |
| `missing_invariant(X)` | The `--new` build did not evaluate invariant `X` declared in the contract. | Ensure `X` is registered on the handler and runs after every step. |
| `baseline_mismatch(V)` | `--old` failed to replay `traffic.kavach` cleanly (inner verdict `V`). | Verify `--old` points to the unpatched baseline binary that recorded `traffic.kavach`. |
| `unapproved` | Contract checks pass, but `kavach.contract.lock` has not been approved by a human. | Ask the user to review `kavach contract show` and run `kavach contract approve`. |
| `stale_lock` | `kavach.contract.json`, `traffic.kavach`, the spec file, or an `--invariant-source` file was modified after approval. | Revert unauthorized edits to locked files, or ask the user to re-approve if the contract change was requested. |

## Selecting Feature Steps (`--feature-match` & `--feature-steps`)

- `--feature-match '<expr>'` matches JSON input payloads in `traffic.kavach`, e.g.:
  - `'$.type=="freeze"'`
  - `'$.argv[0]=="HSETEX"'`
- **Causal propagation rule:** If step `14` is a feature step that mutates state (e.g., freezes an account or sets a per-field TTL), and step `19` is a regular `GET` or `withdraw` on that same key/account whose output changes because of step `14`, step `19` **must** also be marked as a feature step via `--feature-steps 19` when initializing the contract. Otherwise, step `19`'s changed output will be flagged as `regression@19`.
