<?php

declare(strict_types=1);

namespace Kavach;

/**
 * Passed to a handler for each input. Everything nondeterministic a handler
 * does must go through it: never time(), microtime(), random_bytes(),
 * getenv() for config, or direct effects.
 */
abstract class Env
{
    /** Nanoseconds since the Unix epoch, UTC. */
    abstract public function nowNanos(): int;

    /** The clock as a UTC DateTimeImmutable (microsecond precision). */
    public function now(): \DateTimeImmutable
    {
        $ns = $this->nowNanos();
        $sec = intdiv($ns, 1_000_000_000);
        $micro = intdiv($ns - $sec * 1_000_000_000, 1000);
        if ($micro < 0) {
            $sec -= 1;
            $micro += 1_000_000;
        }
        $utc = new \DateTimeZone('UTC');
        return (new \DateTimeImmutable('@' . $sec))->setTimezone($utc)->modify('+' . $micro . ' microseconds');
    }

    /** @return string $n random bytes */
    abstract public function random(int $n): string;

    /**
     * Query a gateway.
     *
     * @throws GatewayError if the query failed
     * @throws UnknownGatewayError if the gateway is not registered
     */
    abstract public function query(string $gateway, string $request): string;

    /** A config value that can change what the handler does, or null if unset. */
    abstract public function config(string $key): ?string;

    /** An output, delivered after the step succeeds. `$local` marks a resource of this host. */
    abstract public function emit(string $sink, string $data, bool $local = false): void;
}
