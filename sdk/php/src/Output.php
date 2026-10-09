<?php

declare(strict_types=1);

namespace Kavach;

/** One effect a handler requested with Env::emit(). */
final readonly class Output
{
    public function __construct(
        public string $sink,
        public string $data,
        public bool $local = false,
    ) {
    }
}
