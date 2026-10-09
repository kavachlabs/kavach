<?php

declare(strict_types=1);

namespace Kavach\Conformance;

final class Spec
{
    /** The spec directory: $KAVACH_SPEC_DIR, else the repository's own. */
    public static function dir(): string
    {
        $d = getenv('KAVACH_SPEC_DIR');
        if (is_string($d) && $d !== '') {
            return $d;
        }
        return dirname(__DIR__, 4) . '/spec'; // sdk/php/src/Conformance -> repository root
    }
}
