<?php

declare(strict_types=1);

namespace Kavach\Conformance;

use Kavach\Env;
use Kavach\Gateway;
use Kavach\GatewayError;
use Kavach\HandlerError;
use Kavach\HasInvariants;
use Kavach\Input;
use Kavach\Invariant;
use Kavach\Panic;
use Kavach\Snapshotter;

/**
 * The conformance handler of SPEC 9.6. State is a count; each step that does
 * not fail adds 1, plus any `count` operations, which take effect at once.
 */
final class Handler implements \Kavach\Handler, Snapshotter, HasInvariants
{
    public int $count = 0;

    /** A gateway registry in which every name is a remote gateway. */
    public static function anyGateway(callable $connection): \Closure
    {
        return static fn (string $name): Gateway => new Gateway($connection, 'remote');
    }

    /** A connection for a host that has no live connections. */
    public static function refuse(string $request): string
    {
        throw new GatewayError('conformance host has no live connections');
    }

    public function handle(Env $env, Input $input): void
    {
        $ops = json_decode($input->data, true, 512, JSON_THROW_ON_ERROR);
        foreach ($ops as $op) {
            switch ($op['op']) {
                case 'clock':
                    $env->emit('trace', self::object(['clock' => (string) $env->nowNanos()]));
                    break;
                case 'rand':
                    $env->emit('trace', $env->random($op['n']));
                    break;
                case 'gateway':
                    try {
                        $resp = $env->query($op['gateway'], $op['request']);
                    } catch (GatewayError $e) {
                        $env->emit('trace', self::object(['error' => $e->error]));
                        break;
                    }
                    $env->emit('trace', $resp);
                    break;
                case 'config':
                    $v = $env->config($op['key']);
                    $env->emit('trace', $v === null ? self::object(['unset' => true]) : $v);
                    break;
                case 'getenv':
                    $v = getenv($op['name']);
                    $env->emit('trace', $v === false ? self::object(['unset' => true]) : $v);
                    break;
                case 'emit':
                    $env->emit($op['sink'], $op['data']);
                    break;
                case 'panic':
                    throw new Panic($op['message']);
                case 'error':
                    throw new HandlerError($op['message']);
                case 'print':
                    echo $op['text'], "\n";
                    break;
                case 'count':
                    $this->count += $op['n']; // takes effect immediately, no rollback
                    break;
                default:
                    throw new HandlerError('unknown operation ' . json_encode($op['op']));
            }
        }
        $this->count += 1;
    }

    public function snapshot(): string
    {
        return (string) $this->count;
    }

    public function restore(string $data): void
    {
        $this->count = (int) $data;
    }

    public function invariants(): array
    {
        return [new Invariant('below_limit', function (): void {
            if ($this->count >= 1000) {
                throw new \RuntimeException("count is {$this->count}");
            }
        })];
    }

    /**
     * Compact JSON, keys in the order given. Strings escape `"` `\` newline,
     * carriage return and tab, and other bytes below U+0020 as \u00xx
     * (lowercase hex); everything else is raw (SPEC 9.6). Values are strings or true.
     *
     * @param array<string, string|true> $fields
     */
    public static function object(array $fields): string
    {
        $parts = [];
        foreach ($fields as $k => $v) {
            $parts[] = self::string((string) $k) . ':' . ($v === true ? 'true' : self::string($v));
        }
        return '{' . implode(',', $parts) . '}';
    }

    private static function string(string $s): string
    {
        // Byte-wise is equivalent: bytes below 0x80 never occur inside a
        // multi-byte UTF-8 sequence.
        return '"' . preg_replace_callback(
            '/["\\\\\x00-\x1f]/',
            static fn (array $m): string => match ($m[0]) {
                '"' => '\\"',
                '\\' => '\\\\',
                "\n" => '\\n',
                "\r" => '\\r',
                "\t" => '\\t',
                default => sprintf('\\u%04x', ord($m[0])),
            },
            $s,
        ) . '"';
    }
}
