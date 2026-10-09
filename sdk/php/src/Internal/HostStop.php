<?php

declare(strict_types=1);

namespace Kavach\Internal;

/**
 * The host cannot continue: the driver left (`$fatal` null) or the protocol
 * was violated (`$fatal` is the message for a `fatal` message).
 *
 * @internal
 */
final class HostStop extends \Error
{
    public function __construct(public readonly ?string $fatal = null)
    {
        parent::__construct($fatal ?? 'the driver closed the pipe');
    }
}
