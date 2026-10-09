"""A single-writer wallet ledger: folds events into balances.

The planted bug: an event with ``"amount": null`` reaches ``amount <= 0`` and
raises ``TypeError``. ``Ledger(fix=True)`` rejects it instead.
"""

from __future__ import annotations

import json

from kavach import Env, HandlerError, Input, Invariant


def _dump(obj) -> bytes:
    return json.dumps(obj, separators=(",", ":")).encode()


def _rfc3339(env: Env) -> str:
    return env.now().strftime("%Y-%m-%dT%H:%M:%S.%fZ")


class Ledger:
    def __init__(self, fix: bool = False):
        self.fix = fix
        self.balances: dict[str, int] = {}
        self.net = 0  # deposits minus withdrawals

    def handle(self, env: Env, input: Input) -> None:
        try:
            ev = json.loads(input.data)
        except ValueError as e:
            raise HandlerError(f"decode event at {input.position}: {e}") from e
        amount = ev.get("amount")
        if self.fix and amount is None:
            return self._reject(env, ev, "missing amount")
        if amount <= 0:  # TypeError when amount is None and fix is off
            return self._reject(env, ev, "amount must be positive")
        at = _rfc3339(env)
        account = ev["account"]

        kind = ev["type"]
        if kind == "deposit":
            self.net += amount
            self._post(env, ev, account, amount, at)
        elif kind == "withdraw":
            if self.balances.get(account, 0) < amount:
                return self._reject(env, ev, "insufficient funds")
            self.net -= amount
            self._post(env, ev, account, -amount, at)
        elif kind == "transfer":
            if self.balances.get(account, 0) < amount:
                return self._reject(env, ev, "insufficient funds")
            self._post(env, ev, account, -amount, at)
            self._post(env, ev, ev["to"], amount, at)
        else:
            self._reject(env, ev, "unknown event type " + str(kind))

    def _post(self, env: Env, ev: dict, account: str, delta: int, at: str) -> None:
        self.balances[account] = self.balances.get(account, 0) + delta
        txn = env.random(8).hex()
        env.emit("ledger.entries", _dump({
            "txn": txn, "event": ev["id"], "account": account,
            "delta": delta, "balance": self.balances[account], "at": at,
        }))

    def _reject(self, env: Env, ev: dict, reason: str) -> None:
        env.emit("ledger.rejections", _dump({"event": ev["id"], "reason": reason}))

    def invariants(self) -> list[Invariant]:
        def non_negative():
            for a in sorted(self.balances):
                if self.balances[a] < 0:
                    raise AssertionError(f"account {a} has balance {self.balances[a]}")

        def conserved():
            total = sum(self.balances.values())
            if total != self.net:
                raise AssertionError(f"balances sum to {total}, deposits minus withdrawals is {self.net}")

        return [Invariant("balances_non_negative", non_negative), Invariant("money_conserved", conserved)]

    def snapshot(self) -> bytes:
        return _dump({"balances": self.balances, "net": self.net})

    def restore(self, data: bytes) -> None:
        state = json.loads(data)
        self.balances, self.net = state["balances"], state["net"]
