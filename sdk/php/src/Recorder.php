<?php

declare(strict_types=1);

namespace Kavach;

use Kavach\Internal\RecordEnv;
use Kavach\Internal\Support;
use Kavach\Internal\Wire;

/**
 * The SDK half of the flight recorder pipe (SPEC 3.6, 10). Runs a handler step
 * by step and writes everything to `kavach-recorder`.
 *
 * Reads are recorded live (the system clock, random_bytes(), the registered
 * gateway connections and the config provider); outputs are delivered through
 * `deliver` only after the step succeeded. There are no threads: the
 * recorder's control stream is read without blocking at step boundaries, and
 * blocking only inside flush() and close(). If the recorder cannot be started,
 * dies, or reports a fatal error, the problem is logged on stderr, recording
 * stops and steps carry on unrecorded; with `required: true` a recorder that
 * cannot be started throws RecorderError from the constructor instead.
 */
final class Recorder
{
    /** @var resource|null */
    private $proc = null;
    /** @var resource|null */
    private $stdin = null;
    /** @var resource|null */
    private $stdout = null;
    private string $inbuf = '';

    private bool $active = false;
    private bool $failedLogged = false;
    private bool $closing = false;
    private bool $closed = false;
    private bool $closedAck = false;
    private bool $ready = false;
    private bool $inStep = false;
    private int $durableCount = 0;
    private bool $snapshotRequested = false;
    private bool $snapshots;
    private ?string $buf = null;
    /** @var list<Output> */
    private array $outputs = [];

    /** @var callable(): int */
    private $clockNanos;
    /** @var callable(int): string */
    private $randomBytes;
    /** @var callable(string): ?string */
    private $configProvider;
    private string $configSource;
    /** @var callable|null */
    private $deliver;
    /** @var array<string, mixed>|callable|null */
    private $gateways;
    /** @var callable|null */
    private $onFixture;
    /** @var callable|null */
    private $logger;

    /** First segment's path, once the recorder is ready. */
    public ?string $file = null;
    public ?string $run = null;

    /**
     * @param object $handler a Handler; optionally also Snapshotter and HasInvariants
     * @param string $start "genesis" or "snapshot" (needs a Snapshotter)
     * @param bool|null $snapshots will answer snapshot_request; default: the handler is a Snapshotter
     * @param (callable(list<Output>): void)|null $deliver called with a successful step's outputs
     * @param array<string, mixed>|(callable(string): ?Gateway)|null $gateways name => Gateway | [connection, scope] | connection
     * @param (callable(string): ?string)|null $config config provider; default getenv()
     * @param (callable(): array<string, string>)|null $flags `flag.*` facts for the environment record
     * @param list<string>|null $recorderCommand argv; else $KAVACH_RECORDER, else `kavach-recorder` on PATH
     * @param (callable(array<string, mixed>): void)|null $onFixture called with each recorder `fixture` message
     * @param (callable(): int)|null $clockNanos replaces the system clock (for tests)
     * @param (callable(int): string)|null $randomBytes replaces random_bytes() (for tests)
     * @param (callable(string, string): void)|null $logger `fn($level, $message)`; default writes to stderr
     * @param list<string>|null $secretKeys
     */
    public function __construct(
        private readonly object $handler,
        string $service,
        string $start = 'genesis',
        ?bool $snapshots = null,
        ?callable $deliver = null,
        array|callable|null $gateways = null,
        ?callable $config = null,
        ?string $configSource = null,
        ?callable $flags = null,
        ?array $recorderCommand = null,
        bool $required = false,
        ?string $handlerId = null,
        ?string $dir = null,
        ?string $compression = null,
        ?int $level = null,
        ?int $blockBytes = null,
        ?int $flushMs = null,
        ?int $segmentBytes = null,
        ?int $segmentSeconds = null,
        ?int $retainSegments = null,
        ?array $secretKeys = null,
        ?callable $onFixture = null,
        ?callable $clockNanos = null,
        ?callable $randomBytes = null,
        ?callable $logger = null,
        private readonly float $readyTimeout = 10.0,
        private readonly float $closeTimeout = 10.0,
    ) {
        if ($start !== 'genesis' && $start !== 'snapshot') {
            throw new \InvalidArgumentException("start must be 'genesis' or 'snapshot'");
        }
        $canSnapshot = $handler instanceof Snapshotter;
        if ($start === 'snapshot' && !$canSnapshot) {
            throw new \InvalidArgumentException("start 'snapshot' needs a handler that implements Snapshotter");
        }
        $this->snapshots = $snapshots ?? $canSnapshot;
        if ($this->snapshots && !$canSnapshot) {
            throw new \InvalidArgumentException('snapshots: true needs a handler that implements Snapshotter');
        }
        $this->deliver = $deliver;
        $this->gateways = $gateways;
        $this->configProvider = $config ?? static function (string $key): ?string {
            $v = getenv($key);
            return $v === false ? null : $v;
        };
        $this->configSource = $configSource ?? ($config !== null ? 'config' : 'env');
        $this->clockNanos = $clockNanos ?? Support::systemNanos(...);
        $this->randomBytes = $randomBytes ?? random_bytes(...);
        $this->onFixture = $onFixture;
        $this->logger = $logger;

        $open = [
            'protocol' => 1,
            'service' => $service,
            'start' => $start,
            'producer' => Support::PRODUCER,
            'snapshots' => $this->snapshots,
        ];
        foreach ([
            'handler' => $handlerId, 'dir' => $dir, 'compression' => $compression, 'level' => $level,
            'block_bytes' => $blockBytes, 'flush_ms' => $flushMs, 'segment_bytes' => $segmentBytes,
            'segment_seconds' => $segmentSeconds, 'retain_segments' => $retainSegments,
            'secret_keys' => $secretKeys ? array_values($secretKeys) : null,
        ] as $k => $v) {
            if ($v !== null) {
                $open[$k] = $v;
            }
        }

        try {
            $cmd = Support::findRecorder($recorderCommand);
            if ($cmd === null) {
                throw new RecorderError('kavach-recorder not found (set recorderCommand, $KAVACH_RECORDER or put it on PATH)');
            }
            $this->spawn($cmd);
            $this->write(Wire::frame(Wire::OPEN, json_encode($open, JSON_UNESCAPED_SLASHES | JSON_UNESCAPED_UNICODE | JSON_THROW_ON_ERROR)));
            $facts = ['host.runtime' => Support::runtime()];
            if ($flags !== null) {
                foreach ($flags() as $k => $v) {
                    $facts[(string) $k] = (string) $v;
                }
            }
            $this->write(Wire::factsFrame($facts));
            if ($start === 'snapshot') {
                /** @var Snapshotter $handler */
                $this->write(Wire::snapshotFrame($handler->snapshot()));
            }
            if ($required) {
                $this->waitUntil(fn () => $this->ready || !$this->active, $this->readyTimeout);
                if (!$this->ready || !$this->active) {
                    throw new RecorderError('kavach-recorder did not become ready');
                }
            }
        } catch (\Throwable $e) {
            if ($required) {
                $this->kill();
                throw $e instanceof RecorderError ? $e : new RecorderError('kavach-recorder could not be started: ' . $e->getMessage(), 0, $e);
            }
            $this->fail('could not start the recorder: ' . $e->getMessage());
        }
        register_shutdown_function($this->shutdown(...));
    }

    // -- process and control stream ---------------------------------------

    /** @param list<string> $cmd */
    private function spawn(array $cmd): void
    {
        if (!Support::executable($cmd[0])) {
            throw new RecorderError("cannot execute the recorder '{$cmd[0]}'");
        }
        $stderr = defined('STDERR') ? STDERR : ['file', 'php://stderr', 'w'];
        // The environment is passed on unchanged (SPEC 10.1).
        $proc = proc_open($cmd, [0 => ['pipe', 'r'], 1 => ['pipe', 'w'], 2 => $stderr], $pipes);
        if (!is_resource($proc)) {
            throw new RecorderError('proc_open failed for ' . implode(' ', $cmd));
        }
        $this->proc = $proc;
        $this->stdin = $pipes[0];
        $this->stdout = $pipes[1];
        stream_set_blocking($this->stdout, false);
        $this->active = true;
    }

    private function kill(): void
    {
        $this->active = false;
        $this->closeProcess(true);
    }

    private function closeProcess(bool $force): void
    {
        if ($this->stdin !== null) {
            @fclose($this->stdin);
            $this->stdin = null;
        }
        if ($this->proc !== null) {
            if ($force) {
                @proc_terminate($this->proc);
            } else {
                $deadline = microtime(true) + 2.0;
                while (microtime(true) < $deadline && (proc_get_status($this->proc)['running'] ?? false)) {
                    usleep(10_000);
                }
                if (proc_get_status($this->proc)['running'] ?? false) {
                    $this->log('warning', 'the recorder is still running after close');
                    @proc_terminate($this->proc);
                }
            }
            if ($this->stdout !== null) {
                @fclose($this->stdout);
                $this->stdout = null;
            }
            @proc_close($this->proc);
            $this->proc = null;
        }
        if ($this->stdout !== null) {
            @fclose($this->stdout);
            $this->stdout = null;
        }
    }

    /** Stop recording, loudly, once. Never throws. */
    private function fail(string $reason): void
    {
        $was = $this->active;
        $this->active = false;
        if (!$this->failedLogged && ($was || !$this->closing)) {
            $this->failedLogged = true;
            $this->log('error', "$reason; recording has stopped and steps run unrecorded");
        }
    }

    private function log(string $level, string $message): void
    {
        if ($this->logger !== null) {
            ($this->logger)($level, $message);
            return;
        }
        $line = 'kavach: ' . ($level === 'error' ? 'ERROR ' : ($level === 'warning' ? 'WARNING ' : '')) . $message . "\n";
        if (defined('STDERR')) {
            @fwrite(STDERR, $line);
        } else {
            error_log(rtrim($line));
        }
    }

    /**
     * Reads whatever the recorder has sent. With `$wait` 0 it never blocks;
     * otherwise it waits up to that many seconds for data.
     */
    private function pump(float $wait = 0.0): void
    {
        if ($this->stdout === null) {
            return;
        }
        while (true) {
            $r = [$this->stdout];
            $w = $e = null;
            $sec = (int) floor($wait);
            $usec = (int) (($wait - $sec) * 1_000_000);
            if (@stream_select($r, $w, $e, $sec, $usec) < 1) {
                return;
            }
            $chunk = fread($this->stdout, 65536);
            if ($chunk === false || $chunk === '') {
                if (feof($this->stdout)) {
                    $this->onEof();
                }
                return;
            }
            $this->inbuf .= $chunk;
            while (($nl = strpos($this->inbuf, "\n")) !== false) {
                $line = substr($this->inbuf, 0, $nl);
                $this->inbuf = substr($this->inbuf, $nl + 1);
                $this->onLine($line);
            }
            $wait = 0.0; // drain what is there, then return
        }
    }

    private function onEof(): void
    {
        @fclose($this->stdout);
        $this->stdout = null;
        if (!$this->closing) {
            $this->fail('the recorder exited unexpectedly');
        }
        $this->closedAck = true;
    }

    private function onLine(string $line): void
    {
        $msg = json_decode($line, true);
        if (!is_array($msg) || !isset($msg['t']) || !is_string($msg['t'])) {
            $this->log('warning', 'unreadable message from the recorder: ' . substr($line, 0, 200));
            return;
        }
        switch ($msg['t']) {
            case 'ready':
                $this->file = isset($msg['file']) ? (string) $msg['file'] : null;
                $this->run = isset($msg['run']) ? (string) $msg['run'] : null;
                $this->ready = true;
                break;
            case 'snapshot_request':
                $this->snapshotRequested = true;
                break;
            case 'durable':
                $this->durableCount++;
                break;
            case 'segment':
                break;
            case 'fixture':
                $this->log('warning', sprintf(
                    'wrote fixture %s (input seq %s, %s)',
                    (string) ($msg['file'] ?? ''),
                    (string) ($msg['seq'] ?? ''),
                    (string) ($msg['failure'] ?? ''),
                ));
                if ($this->onFixture !== null) {
                    try {
                        ($this->onFixture)($msg);
                    } catch (\Throwable $e) {
                        $this->log('error', 'onFixture callback failed: ' . $e->getMessage());
                    }
                }
                break;
            case 'error':
                $fatal = !empty($msg['fatal']);
                $this->log('error', 'recorder ' . ($fatal ? 'fatal error' : 'error') . ': ' . (string) ($msg['message'] ?? ''));
                if ($fatal) {
                    $this->fail('the recorder reported a fatal error: ' . (string) ($msg['message'] ?? ''));
                }
                break;
            case 'closed':
                $this->closedAck = true;
                break;
        }
    }

    /** Pumps the control stream until `$cond` holds, the recorder is gone, or `$timeout` seconds pass. */
    private function waitUntil(callable $cond, float $timeout): bool
    {
        $deadline = microtime(true) + $timeout;
        while (true) {
            $this->pump();
            if ($cond()) {
                return true;
            }
            $left = $deadline - microtime(true);
            if ($left <= 0 || $this->stdout === null) {
                return (bool) $cond();
            }
            $this->pump(min($left, 0.25));
            if ($cond()) {
                return true;
            }
        }
    }

    // -- writing ----------------------------------------------------------

    private function write(string $data): void
    {
        if (!$this->active || $data === '' || $this->stdin === null) {
            return;
        }
        $off = 0;
        $len = strlen($data);
        while ($off < $len) {
            $n = @fwrite($this->stdin, $off === 0 ? $data : substr($data, $off));
            if ($n === false || $n === 0) {
                $this->fail('could not write to the recorder');
                return;
            }
            $off += $n;
        }
    }

    private function rec(string $frame): void
    {
        if ($this->buf !== null && $this->active) {
            $this->buf .= $frame;
        }
    }

    // -- stepping ---------------------------------------------------------

    /**
     * Runs the handler on one input. Does not throw for handler failures; it
     * returns a StepResult (see StepResult::raiseForFailure()).
     */
    public function step(Input $input): StepResult
    {
        if ($this->closed) {
            throw new \LogicException('kavach: step on a closed Recorder');
        }
        if ($this->inStep) {
            throw new \LogicException('kavach: step() called from inside a step');
        }
        $this->inStep = true;
        try {
            return $this->runStep($input);
        } finally {
            $this->inStep = false;
        }
    }

    private function runStep(Input $input): StepResult
    {
        $this->pump();
        $this->answerSnapshotRequest();
        $this->buf = '';
        $this->outputs = [];
        // The input goes in before the handler runs, so that a step that kills
        // the process still leaves it on record (SPEC 10.2).
        $this->write(Wire::inputRecord($input->source, $input->position, $input->data));
        $failure = null;
        $raised = null;
        try {
            $this->handler->handle(new RecordEnv($this), $input);
        } catch (\Throwable $e) {
            $failure = Support::classify($e);
            $raised = $e;
        }
        $failure ??= Support::checkInvariants($this->handler);
        if ($failure !== null) {
            $this->rec(Wire::markerRecord($failure['kind'], $failure['message'], $failure['detail']));
        }
        $this->rec(Wire::frame(Wire::STEP_END));
        $buf = $this->buf;
        $outputs = $this->outputs;
        $this->buf = null;
        $this->outputs = [];
        $this->write($buf);
        if ($failure === null) {
            if ($this->deliver !== null && $outputs !== []) {
                ($this->deliver)($outputs);
            }
            $result = new StepResult(outputs: $outputs);
        } else {
            $result = new StepResult(false, $failure['kind'], $failure['message'], $failure['detail'], $raised, $outputs);
        }
        $this->pump();
        return $result;
    }

    private function answerSnapshotRequest(): void
    {
        if (!$this->snapshotRequested) {
            return;
        }
        $this->snapshotRequested = false;
        if (!$this->active || !$this->snapshots) {
            return;
        }
        try {
            /** @var Snapshotter $h */
            $h = $this->handler;
            $data = $h->snapshot();
        } catch (\Throwable $e) {
            $this->log('error', 'snapshot failed; staying in the current segment: ' . $e->getMessage());
            return;
        }
        $this->write(Wire::snapshotFrame($data));
    }

    // -- flush and close --------------------------------------------------

    /**
     * Asks the recorder to close its open block now. With `$durable` also
     * waits (up to `$timeout` seconds) until it is on disk. Returns false if
     * recording has stopped or the wait timed out. Call it between steps.
     */
    public function flush(bool $durable = false, float $timeout = 10.0): bool
    {
        if ($this->inStep) {
            throw new \LogicException('kavach: flush() must not be called from inside a step');
        }
        $this->pump();
        if (!$this->active) {
            return false;
        }
        $before = $this->durableCount;
        $this->write(Wire::flushFrame($durable));
        if (!$durable) {
            return $this->active;
        }
        $this->waitUntil(fn () => $this->durableCount > $before || !$this->active, $timeout);
        return $this->durableCount > $before;
    }

    /** Orderly shutdown: sends `close` and waits for `closed`. */
    public function close(): void
    {
        if ($this->inStep) {
            throw new \LogicException('kavach: close() must not be called from inside a step');
        }
        if ($this->closed) {
            return;
        }
        $this->closed = true;
        if ($this->proc === null) {
            return;
        }
        if ($this->active) {
            $this->closing = true;
            $this->write(Wire::frame(Wire::CLOSE));
            if (!$this->waitUntil(fn () => $this->closedAck, $this->closeTimeout)) {
                $this->log('warning', sprintf('the recorder did not answer close within %.0fs', $this->closeTimeout));
            }
        }
        $this->closing = true;
        $this->active = false;
        $this->closeProcess(false);
    }

    /**
     * At process exit: close in an orderly way between steps; if the process
     * ends inside a step, only drop the pipe, so the recorder marks the step
     * as a crash (SPEC 10.5).
     */
    private function shutdown(): void
    {
        if ($this->closed) {
            return;
        }
        if ($this->inStep) {
            $this->closed = true;
            $this->closing = true;
            $this->active = false;
            $this->closeProcess(false);
            return;
        }
        $this->close();
    }

    public function isRecording(): bool
    {
        return $this->active;
    }

    // -- the Env handed to the handler (internal) -------------------------

    /** @internal */
    public function envNowNanos(): int
    {
        $ns = ($this->clockNanos)();
        $this->rec(Wire::clockRecord($ns));
        return $ns;
    }

    /** @internal */
    public function envRandom(int $n): string
    {
        if ($n <= 0) {
            return '';
        }
        $data = ($this->randomBytes)($n);
        $this->rec(Wire::randRecord($data));
        return $data;
    }

    /** @internal */
    public function envQuery(string $gateway, string $request): string
    {
        $gw = Support::resolveGateway($this->gateways, $gateway);
        [$resp, $err] = Support::callGateway($gw, $request);
        $this->rec(Wire::gatewayRecord($gateway, $request, $resp, $err, $gw->scope === 'local'));
        if ($err !== '') {
            throw new GatewayError($err);
        }
        return $resp;
    }

    /** @internal */
    public function envConfig(string $key): ?string
    {
        $v = ($this->configProvider)($key);
        $v = $v === null ? null : (string) $v;
        $this->rec(Wire::configRecord($key, $v, $this->configSource));
        return $v;
    }

    /** @internal */
    public function envEmit(string $sink, string $data, bool $local): void
    {
        $this->outputs[] = new Output($sink, $data, $local);
        $this->rec(Wire::outputRecord($sink, $data, $local));
    }
}
