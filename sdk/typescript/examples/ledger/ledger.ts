import { HandlerError, type Env, type Handler, type Input, type Invariant } from "@kavachlabs/kavach";

/** One wallet event from upstream. Upstream sends `"amount": null` for some events. */
export interface LedgerEvent {
  id: string;
  type: string; // deposit, withdraw or transfer
  account: string;
  to?: string;
  amount: number; // BUG (planted): the type promises a number; upstream sometimes sends null
}

const utf8 = (s: string) => new TextEncoder().encode(s);

export interface LedgerOptions {
  /** The fix: reject an event with a missing amount instead of crashing on it. */
  fixNullAmount?: boolean;
}

/** A single-writer wallet ledger: it folds events into balances. */
export class Ledger implements Handler {
  balances: Record<string, number> = {};
  net = 0; // deposits minus withdrawals

  constructor(private readonly options: LedgerOptions = {}) {}

  handle(env: Env, input: Input): void {
    let ev: LedgerEvent;
    try {
      ev = JSON.parse(new TextDecoder().decode(input.data)) as LedgerEvent;
    } catch (e) {
      throw new HandlerError(`decode event at ${input.position}: ${(e as Error).message}`);
    }
    if (this.options.fixNullAmount && (ev.amount === null || ev.amount === undefined)) {
      return this.reject(env, ev, "missing amount");
    }
    // With "amount": null this throws "TypeError: Cannot read properties of null",
    // which the recorder writes as a panic marker.
    const amount = Math.trunc(ev.amount.valueOf());
    if (amount <= 0) return this.reject(env, ev, "amount must be positive");
    const at = env.now();

    switch (ev.type) {
      case "deposit":
        this.net += amount;
        return this.post(env, ev, ev.account, amount, at);
      case "withdraw":
        if ((this.balances[ev.account] ?? 0) < amount) return this.reject(env, ev, "insufficient funds");
        this.net -= amount;
        return this.post(env, ev, ev.account, -amount, at);
      case "transfer":
        if ((this.balances[ev.account] ?? 0) < amount) return this.reject(env, ev, "insufficient funds");
        this.post(env, ev, ev.account, -amount, at);
        return this.post(env, ev, ev.to ?? "", amount, at);
    }
    return this.reject(env, ev, `unknown event type ${ev.type}`);
  }

  private post(env: Env, ev: LedgerEvent, account: string, delta: number, at: Date): void {
    this.balances[account] = (this.balances[account] ?? 0) + delta;
    const txn = Buffer.from(env.random(8)).toString("hex");
    const entry = { txn, event: ev.id, account, delta, balance: this.balances[account], at: at.toISOString() };
    env.emit("ledger.entries", utf8(JSON.stringify(entry)));
  }

  private reject(env: Env, ev: LedgerEvent, reason: string): void {
    env.emit("ledger.rejections", utf8(JSON.stringify({ event: ev.id, reason })));
  }

  /** Checked after every step, live and during replay. */
  invariants(): Invariant[] {
    return [
      {
        name: "balances_non_negative",
        check: () => {
          for (const a of Object.keys(this.balances).sort()) {
            if (this.balances[a]! < 0) throw new Error(`account ${a} has balance ${this.balances[a]}`);
          }
        },
      },
      {
        name: "money_conserved",
        check: () => {
          const sum = Object.values(this.balances).reduce((x, y) => x + y, 0);
          if (sum !== this.net) throw new Error(`balances sum to ${sum}, deposits minus withdrawals is ${this.net}`);
        },
      },
    ];
  }

  snapshot(): Uint8Array {
    return utf8(JSON.stringify({ balances: this.balances, net: this.net }));
  }

  restore(data: Uint8Array): void {
    const s = JSON.parse(new TextDecoder().decode(data)) as { balances: Record<string, number>; net: number };
    this.balances = s.balances ?? {};
    this.net = s.net ?? 0;
  }
}
