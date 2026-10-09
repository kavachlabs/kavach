<?php

declare(strict_types=1);

namespace Kavach;

/**
 * A named property of handler state that must hold after every step.
 *
 * The check holds when it returns normally (or returns anything but false);
 * it is violated when it throws or returns false.
 */
final readonly class Invariant
{
    /** @var \Closure(): mixed */
    public \Closure $check;

    public function __construct(public string $name, callable $check)
    {
        $this->check = $check(...);
    }
}
