<?php

declare(strict_types=1);

namespace Kavach;

interface Snapshotter
{
    /** The handler's state in its own encoding. */
    public function snapshot(): string;

    public function restore(string $data): void;
}
