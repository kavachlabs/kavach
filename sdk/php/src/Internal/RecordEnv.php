<?php

declare(strict_types=1);

namespace Kavach\Internal;

use Kavach\Env;
use Kavach\Recorder;

/**
 * The Env handed to the handler while recording.
 *
 * @internal
 */
final class RecordEnv extends Env
{
    public function __construct(private readonly Recorder $recorder)
    {
    }

    public function nowNanos(): int
    {
        return $this->recorder->envNowNanos();
    }

    public function random(int $n): string
    {
        return $this->recorder->envRandom($n);
    }

    public function query(string $gateway, string $request): string
    {
        return $this->recorder->envQuery($gateway, $request);
    }

    public function config(string $key): ?string
    {
        return $this->recorder->envConfig($key);
    }

    public function emit(string $sink, string $data, bool $local = false): void
    {
        $this->recorder->envEmit($sink, $data, $local);
    }
}
