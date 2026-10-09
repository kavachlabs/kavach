<?php

declare(strict_types=1);

namespace Kavach;

/** A gateway query failed. `$error` is the text the connection reported. */
final class GatewayError extends \RuntimeException
{
    public function __construct(public readonly string $error)
    {
        parent::__construct($error);
    }
}
