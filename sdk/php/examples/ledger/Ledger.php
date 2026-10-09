<?php

declare(strict_types=1);

namespace LedgerDemo;

use Kavach\Env;
use Kavach\Handler;
use Kavach\HandlerError;
use Kavach\HasInvariants;
use Kavach\Input;
use Kavach\Invariant;
use Kavach\Snapshotter;

/**
 * A single-writer wallet ledger: folds events into balances.
 *
 * The planted bug: an event with "amount": null reaches positive(int $amount)
 * and throws TypeError. `new Ledger(fix: true)` rejects it instead.
 */
final class Ledger implements Handler, Snapshotter, HasInvariants
{
    /** @var array<string, int> */
    private array $balances = [];
    private int $net = 0; // deposits minus withdrawals

    public function __construct(private readonly bool $fix = false)
    {
    }

    public function handle(Env $env, Input $input): void
    {
        $ev = json_decode($input->data, true);
        if (!is_array($ev)) {
            throw new HandlerError("decode event at {$input->position}: " . json_last_error_msg());
        }
        $amount = $ev['amount'] ?? null;
        if ($this->fix && $amount === null) {
            $this->reject($env, $ev, 'missing amount');
            return;
        }
        if (!$this->positive($amount)) { // TypeError when $amount is null and fix is off
            $this->reject($env, $ev, 'amount must be positive');
            return;
        }
        $at = $env->now()->format('Y-m-d\TH:i:s.u\Z');
        $account = $ev['account'];

        switch ($ev['type']) {
            case 'deposit':
                $this->net += $amount;
                $this->post($env, $ev, $account, $amount, $at);
                break;
            case 'withdraw':
                if (($this->balances[$account] ?? 0) < $amount) {
                    $this->reject($env, $ev, 'insufficient funds');
                    return;
                }
                $this->net -= $amount;
                $this->post($env, $ev, $account, -$amount, $at);
                break;
            case 'transfer':
                if (($this->balances[$account] ?? 0) < $amount) {
                    $this->reject($env, $ev, 'insufficient funds');
                    return;
                }
                $this->post($env, $ev, $account, -$amount, $at);
                $this->post($env, $ev, $ev['to'], $amount, $at);
                break;
            default:
                $this->reject($env, $ev, 'unknown event type ' . $ev['type']);
        }
    }

    private function positive(int $amount): bool
    {
        return $amount > 0;
    }

    /** @param array<string, mixed> $ev */
    private function post(Env $env, array $ev, string $account, int $delta, string $at): void
    {
        $this->balances[$account] = ($this->balances[$account] ?? 0) + $delta;
        $txn = bin2hex($env->random(8));
        $env->emit('ledger.entries', self::dump([
            'txn' => $txn, 'event' => $ev['id'], 'account' => $account,
            'delta' => $delta, 'balance' => $this->balances[$account], 'at' => $at,
        ]));
    }

    /** @param array<string, mixed> $ev */
    private function reject(Env $env, array $ev, string $reason): void
    {
        $env->emit('ledger.rejections', self::dump(['event' => $ev['id'], 'reason' => $reason]));
    }

    public function invariants(): array
    {
        return [
            new Invariant('balances_non_negative', function (): void {
                $names = array_keys($this->balances);
                sort($names);
                foreach ($names as $a) {
                    if ($this->balances[$a] < 0) {
                        throw new \RuntimeException("account $a has balance {$this->balances[$a]}");
                    }
                }
            }),
            new Invariant('money_conserved', function (): void {
                $sum = array_sum($this->balances);
                if ($sum !== $this->net) {
                    throw new \RuntimeException("balances sum to $sum, deposits minus withdrawals is {$this->net}");
                }
            }),
        ];
    }

    public function snapshot(): string
    {
        return self::dump(['balances' => (object) $this->balances, 'net' => $this->net]);
    }

    public function restore(string $data): void
    {
        $state = json_decode($data, true, 512, JSON_THROW_ON_ERROR);
        $this->balances = $state['balances'];
        $this->net = $state['net'];
    }

    /** @param array<string, mixed> $v */
    private static function dump(array $v): string
    {
        return json_encode($v, JSON_UNESCAPED_SLASHES | JSON_UNESCAPED_UNICODE | JSON_THROW_ON_ERROR);
    }
}
