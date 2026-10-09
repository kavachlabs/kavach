<?php

declare(strict_types=1);

namespace Kavach;

/** What happened in one Recorder::step(). */
final class StepResult
{
    /**
     * @param string $kind "", "panic", "error" or "invariant"
     * @param list<Output> $outputs
     */
    public function __construct(
        public readonly bool $ok = true,
        public readonly string $kind = '',
        public readonly string $message = '',
        public readonly string $detail = '',
        public readonly ?\Throwable $exception = null,
        public readonly array $outputs = [],
    ) {
    }

    /** Re-throws the handler's exception, or a RuntimeException for an invariant. */
    public function raiseForFailure(): void
    {
        if ($this->ok) {
            return;
        }
        if ($this->exception !== null) {
            throw $this->exception;
        }
        throw new \RuntimeException("kavach: invariant {$this->message} violated: {$this->detail}");
    }
}
