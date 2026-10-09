<?php

declare(strict_types=1);

namespace Kavach;

/** Fails the step as a `panic` whose marker message is exactly the exception message. */
final class Panic extends \Exception
{
}
