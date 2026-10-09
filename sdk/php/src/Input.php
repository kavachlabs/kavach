<?php

declare(strict_types=1);

namespace Kavach;

/** One event consumed by a handler. */
final readonly class Input
{
    public function __construct(
        /** Where the event came from, e.g. "kafka:wallet-events". */
        public string $source,
        /** Its position in that source, e.g. "3:1042". */
        public string $position,
        /** The event exactly as received. */
        public string $data,
    ) {
    }
}
