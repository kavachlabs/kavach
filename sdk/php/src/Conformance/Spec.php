<?php

declare(strict_types=1);

namespace Kavach\Conformance;

final class Spec
{
    /** The spec directory: $KAVACH_SPEC_DIR, else the main checkout's. */
    public static function dir(): string
    {
        $d = getenv('KAVACH_SPEC_DIR');
        if (is_string($d) && $d !== '') {
            return $d;
        }
        $repo = dirname(__DIR__, 4) . '/spec'; // sdk/php/src/Conformance -> repository root
        return is_dir($repo . '/host') ? $repo : '/Users/koustav/code/kavach-labs/kavach/spec';
    }
}
