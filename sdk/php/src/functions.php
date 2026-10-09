<?php

declare(strict_types=1);

namespace Kavach;

/**
 * If this process was started as a replay host (its last argument is
 * `kavach-host`), serves the `kavach` CLI over stdin/stdout and exits;
 * otherwise returns at once. Call it first thing in the entry point, before
 * consuming any input.
 *
 * @param list<string> $argv
 * @param callable(): object $factory creates a fresh handler
 * @param array{gateways?: array<string, mixed>|callable, recorder?: list<string>} $options
 */
function maybeHost(array $argv, callable $factory, array $options = []): void
{
    Host::maybe($argv, $factory, $options);
}
