<?php

declare(strict_types=1);

namespace Kavach\Internal;

use Kavach\Gateway;
use Kavach\GatewayError;
use Kavach\HandlerError;
use Kavach\HasInvariants;
use Kavach\Invariant;
use Kavach\Panic;
use Kavach\UnknownGatewayError;

/**
 * Shared by the recorder and the host: the failure mapping (SPEC 4.5), which
 * must be one function so that a recorded failure and its replay compare
 * equal, gateway lookup, invariants and recorder discovery.
 *
 * @internal
 */
final class Support
{
    public const VERSION = '0.1.0';

    /** Recorded in the journal header (`producer`) and sent in `ready.sdk`. */
    public const PRODUCER = 'kavach-php/' . self::VERSION;

    /**
     * The failure mapping. A Throwable other than Panic and HandlerError is a
     * panic whose message is get_class($e) . ': ' . $e->getMessage() and whose
     * data is the trace.
     *
     * @return array{kind: string, message: string, detail: string}
     */
    public static function classify(\Throwable $e): array
    {
        if ($e instanceof Panic) {
            return ['kind' => 'panic', 'message' => $e->getMessage(), 'detail' => self::trace($e)];
        }
        if ($e instanceof HandlerError) {
            return ['kind' => 'error', 'message' => $e->getMessage(), 'detail' => self::trace($e)];
        }
        $full = get_class($e) . ': ' . $e->getMessage();
        $message = self::stripLocation($full);
        $detail = self::trace($e);
        // The message must not depend on where the build runs (SPEC 4.5); the full text goes in the data.
        return ['kind' => 'panic', 'message' => $message, 'detail' => $message === $full ? $detail : "$full\n$detail"];
    }

    /** Removes file paths, line numbers, memory addresses and object ids the engine puts into messages. */
    public static function stripLocation(string $m): string
    {
        $m = preg_replace('/,? (?:called )?in \S+(?: on line \d+|:\d+)/', '', $m) ?? $m;
        $m = preg_replace('/ on line \d+/', '', $m) ?? $m;
        $m = preg_replace('/0x[0-9a-fA-F]{6,}/', '0x', $m) ?? $m;
        $m = preg_replace('/(@anonymous)\x00\S*/', '$1', $m) ?? $m;
        return $m;
    }

    private static function trace(\Throwable $e): string
    {
        $out = '';
        for ($t = $e; $t !== null; $t = $t->getPrevious()) {
            $out .= ($out === '' ? '' : "\nCaused by: ") . get_class($t) . ': ' . $t->getMessage()
                . ' in ' . $t->getFile() . ':' . $t->getLine() . "\n" . $t->getTraceAsString();
        }
        return $out;
    }

    /** @return list<Invariant> */
    public static function invariants(object $handler): array
    {
        if ($handler instanceof HasInvariants || method_exists($handler, 'invariants')) {
            return array_values($handler->invariants());
        }
        return [];
    }

    /**
     * The first declared invariant, in order, that fails.
     *
     * @return array{kind: string, message: string, detail: string}|null
     */
    public static function checkInvariants(object $handler): ?array
    {
        foreach (self::invariants($handler) as $inv) {
            try {
                $ok = ($inv->check)();
                if ($ok === false) {
                    return ['kind' => 'invariant', 'message' => $inv->name, 'detail' => 'check returned false'];
                }
            } catch (\Throwable $e) {
                return ['kind' => 'invariant', 'message' => $inv->name, 'detail' => $e->getMessage()];
            }
        }
        return null;
    }

    /**
     * @param array<string, mixed>|callable|null $gateways name => Gateway | [connection, scope] | connection,
     *                                                       or a resolver `fn(string $name): ?Gateway`
     */
    public static function resolveGateway(array|callable|null $gateways, string $name): Gateway
    {
        $g = null;
        if (is_array($gateways)) {
            $g = $gateways[$name] ?? null;
        } elseif ($gateways !== null) {
            $g = $gateways($name);
        }
        if ($g instanceof Gateway) {
            return $g;
        }
        if (is_array($g) && isset($g[0])) {
            return new Gateway($g[0], $g[1] ?? 'remote');
        }
        if (is_callable($g)) {
            return new Gateway($g);
        }
        throw new UnknownGatewayError("gateway '$name' is not registered");
    }

    /** @return array{0: string, 1: string} [response, error] */
    public static function callGateway(Gateway $gw, string $request): array
    {
        try {
            $resp = ($gw->connection)($request);
            if (!is_string($resp)) {
                return ['', 'gateway connection returned ' . get_debug_type($resp) . ', not a string'];
            }
            return [$resp, ''];
        } catch (GatewayError $e) {
            return ['', $e->error !== '' ? $e->error : 'gateway error'];
        } catch (\Throwable $e) {
            $msg = $e->getMessage();
            return ['', $msg !== '' ? $msg : get_class($e)];
        }
    }

    public static function runtime(): string
    {
        return 'php-' . PHP_VERSION;
    }

    /** Unix nanoseconds from the system clock. */
    public static function systemNanos(): int
    {
        $t = gettimeofday();
        return $t['sec'] * 1_000_000_000 + $t['usec'] * 1000;
    }

    /**
     * The recorder argument vector: `$command`, else $KAVACH_RECORDER, else
     * `kavach-recorder` on PATH. Null if there is none.
     *
     * @param list<string>|null $command
     * @return list<string>|null
     */
    public static function findRecorder(?array $command = null): ?array
    {
        if ($command !== null && $command !== []) {
            return array_values($command);
        }
        $env = getenv('KAVACH_RECORDER');
        if (is_string($env) && $env !== '') {
            return [$env];
        }
        foreach (explode(PATH_SEPARATOR, (string) getenv('PATH')) as $dir) {
            $p = rtrim($dir, '/') . '/kavach-recorder';
            if ($dir !== '' && is_file($p) && is_executable($p)) {
                return [$p];
            }
        }
        return null;
    }

    /** Whether argv[0] of a recorder command can be executed. */
    public static function executable(string $path): bool
    {
        if (str_contains($path, '/')) {
            return is_file($path) && is_executable($path);
        }
        foreach (explode(PATH_SEPARATOR, (string) getenv('PATH')) as $dir) {
            $p = rtrim($dir, '/') . '/' . $path;
            if ($dir !== '' && is_file($p) && is_executable($p)) {
                return true;
            }
        }
        return false;
    }
}
