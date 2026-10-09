<?php

declare(strict_types=1);

namespace Kavach;

interface HasInvariants
{
    /** @return list<Invariant> checked after every step that ended ok, in order. */
    public function invariants(): array;
}
