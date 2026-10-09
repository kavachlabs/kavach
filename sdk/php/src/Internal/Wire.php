<?php

declare(strict_types=1);

namespace Kavach\Internal;

/**
 * Encoders for the record stream (SPEC 10.2) and the record payloads (SPEC 4).
 *
 * @internal
 */
final class Wire
{
    public const OPEN = 0x01;
    public const RECORD = 0x02;
    public const STEP_END = 0x03;
    public const FACTS = 0x04;
    public const SNAPSHOT = 0x05;
    public const FLUSH = 0x06;
    public const CLOSE = 0x07;

    public const T_INPUT = 0x01;
    public const T_CLOCK = 0x02;
    public const T_RAND = 0x03;
    public const T_OUTPUT = 0x04;
    public const T_MARKER = 0x05;
    public const T_GATEWAY = 0x07;
    public const T_CONFIG = 0x09;

    public static function uvarint(int $n): string
    {
        $out = '';
        while ($n >= 0x80) {
            $out .= chr(($n & 0x7F) | 0x80);
            $n >>= 7;
        }
        return $out . chr($n);
    }

    /** A length-prefixed byte string. */
    public static function bytes(string $b): string
    {
        return self::uvarint(strlen($b)) . $b;
    }

    /** A length-prefixed string; invalid UTF-8 is replaced, since the journal requires UTF-8. */
    public static function string(string $s): string
    {
        return self::bytes(self::utf8($s));
    }

    public static function utf8(string $s): string
    {
        if (preg_match('//u', $s) === 1) {
            return $s;
        }
        return mb_convert_encoding($s, 'UTF-8', 'UTF-8');
    }

    public static function frame(int $kind, string $payload = ''): string
    {
        return self::uvarint(1 + strlen($payload)) . chr($kind) . $payload;
    }

    public static function record(int $type, bool $critical, string $payload): string
    {
        return self::frame(self::RECORD, chr($type) . chr($critical ? 1 : 0) . $payload);
    }

    public static function inputRecord(string $source, string $position, string $data): string
    {
        return self::record(self::T_INPUT, false, self::string($source) . self::string($position) . self::bytes($data));
    }

    public static function clockRecord(int $unixNanos): string
    {
        return self::record(self::T_CLOCK, false, pack('P', $unixNanos));
    }

    public static function randRecord(string $data): string
    {
        return self::record(self::T_RAND, false, self::bytes($data));
    }

    public static function outputRecord(string $sink, string $data, bool $local): string
    {
        return self::record(self::T_OUTPUT, false, self::string($sink) . self::bytes($data) . chr($local ? 1 : 0));
    }

    public static function markerRecord(string $kind, string $message, string $data): string
    {
        return self::record(self::T_MARKER, false, self::string($kind) . self::string($message) . self::bytes($data));
    }

    public static function gatewayRecord(string $gateway, string $request, string $response, string $error, bool $local): string
    {
        return self::record(
            self::T_GATEWAY,
            true,
            self::string($gateway) . self::bytes($request) . self::bytes($response) . self::string($error) . chr($local ? 1 : 0),
        );
    }

    public static function configRecord(string $key, ?string $value, string $source): string
    {
        return self::record(
            self::T_CONFIG,
            true,
            self::string($key) . chr($value === null ? 0 : 1) . self::bytes($value ?? '') . self::string($source),
        );
    }

    /** @param array<string, string> $facts key => value, all form 0 */
    public static function factsFrame(array $facts): string
    {
        $p = self::uvarint(count($facts));
        foreach ($facts as $k => $v) {
            $p .= self::string((string) $k) . chr(0) . self::bytes($v);
        }
        return self::frame(self::FACTS, $p);
    }

    public static function snapshotFrame(string $data): string
    {
        return self::frame(self::SNAPSHOT, self::bytes($data));
    }

    public static function flushFrame(bool $durable): string
    {
        return self::frame(self::FLUSH, chr($durable ? 1 : 0));
    }
}
