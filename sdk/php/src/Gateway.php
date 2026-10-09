<?php

declare(strict_types=1);

namespace Kavach;

/**
 * A registered gateway: the connection that makes the query, and its scope.
 *
 * The connection is `fn(string $request): string`, called when recording.
 * Throw anything to report a failure; its message is recorded as the
 * gateway's error.
 */
final readonly class Gateway
{
    /** @var \Closure(string): string */
    public \Closure $connection;

    public function __construct(callable $connection, public string $scope = 'remote')
    {
        if ($scope !== 'remote' && $scope !== 'local') {
            throw new \InvalidArgumentException("gateway scope must be 'remote' or 'local'");
        }
        $this->connection = $connection(...);
    }
}
