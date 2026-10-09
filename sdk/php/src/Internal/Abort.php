<?php

declare(strict_types=1);

namespace Kavach\Internal;

/**
 * Unwinds a step after the driver answered a request with `abort` (SPEC 9.4).
 * It extends Error so that `catch (\Exception)` does not swallow it; PHP has no
 * uncatchable exception, so the host also tracks the aborted state.
 *
 * @internal
 */
final class Abort extends \Error
{
}
