<?php

declare(strict_types=1);

namespace Kavach;

/**
 * What a handler implements. A handler also implements Snapshotter to be
 * segmented or to start from a snapshot, and HasInvariants to declare
 * invariants. It must reach the world only through the Env.
 */
interface Handler
{
    public function handle(Env $env, Input $input): void;
}
