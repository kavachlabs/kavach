<?php

declare(strict_types=1);

namespace Kavach\Internal;

use Kavach\Env;
use Kavach\GatewayError;
use Kavach\Host;

/**
 * The Env handed to the handler while replaying: every read is served by the driver.
 *
 * @internal
 */
final class HostEnv extends Env
{
    public function __construct(private readonly Host $host)
    {
    }

    public function nowNanos(): int
    {
        return (int) $this->host->request(['t' => 'clock'], 'clock')['unix_nanos'];
    }

    public function random(int $n): string
    {
        if ($n <= 0) {
            return '';
        }
        $ans = $this->host->request(['t' => 'rand', 'n' => $n], 'rand');
        return self::decode($ans['data'] ?? '');
    }

    public function query(string $gateway, string $request): string
    {
        $gw = $this->host->resolveGateway($gateway);
        $ans = $this->host->request(
            ['t' => 'gateway', 'gateway' => $gateway, 'request' => base64_encode($request), 'scope' => $gw->scope],
            'gateway',
        );
        if (isset($ans['error']) && $ans['error'] !== '') {
            throw new GatewayError((string) $ans['error']);
        }
        return self::decode($ans['response'] ?? '');
    }

    public function config(string $key): ?string
    {
        $ans = $this->host->request(['t' => 'config', 'key' => $key], 'config');
        if (empty($ans['present'])) {
            return null;
        }
        return self::decode($ans['value'] ?? '');
    }

    public function emit(string $sink, string $data, bool $local = false): void
    {
        $this->host->emit($sink, $data, $local);
    }

    private static function decode(mixed $b64): string
    {
        $d = is_string($b64) ? base64_decode($b64, true) : false;
        if ($d === false) {
            throw new HostStop('invalid base64');
        }
        return $d;
    }
}
