<?php

declare(strict_types=1);

namespace Kavach;

use Kavach\Internal\Abort;
use Kavach\Internal\HostStop;
use Kavach\Internal\Support;

/**
 * The host side of the replay protocol (SPEC 9): runs the handler one step at
 * a time while the `kavach` CLI serves every read and captures every output.
 *
 * Use maybeHost() from a service's entry point; this class is the session
 * itself, over a pair of streams.
 */
final class Host
{
    public const PROTOCOL = 1;
    public const HOST_ARG = 'kavach-host';

    private const JSON_OUT = JSON_UNESCAPED_SLASHES | JSON_UNESCAPED_UNICODE | JSON_INVALID_UTF8_SUBSTITUTE | JSON_PARTIAL_OUTPUT_ON_ERROR;

    private bool $aborted = false;
    private ?HostStop $stop = null;
    /** @var callable */
    private $factory;
    /** @var array<string, mixed>|callable|null */
    private $gateways;
    /** @var callable */
    private $environment;

    /**
     * @param callable(): object $factory creates a fresh handler
     * @param resource $in the driver's messages
     * @param resource $out the host's messages
     * @param array<string, mixed>|(callable(string): ?Gateway)|null $gateways
     * @param (callable(): array<string, array<string, mixed>>)|null $environment `ready.environment`
     */
    public function __construct(
        callable $factory,
        private $in,
        private $out,
        array|callable|null $gateways = null,
        ?callable $environment = null,
    ) {
        $this->factory = $factory;
        $this->gateways = $gateways;
        $this->environment = $environment ?? self::collectEnvironment(...);
    }

    /**
     * `ready.environment`: the output of `kavach-recorder facts` if it can be
     * run, plus `host.runtime` (SPEC 9.2).
     *
     * @param list<string>|null $recorderCommand
     * @return array<string, array<string, mixed>>
     */
    public static function collectEnvironment(?array $recorderCommand = null): array
    {
        $env = [];
        $cmd = Support::findRecorder($recorderCommand);
        if ($cmd !== null && Support::executable($cmd[0])) {
            $out = self::capture([...$cmd, 'facts'], 10.0);
            $facts = $out === null ? null : json_decode($out, true);
            if (is_array($facts)) {
                foreach ($facts as $k => $v) {
                    if (is_array($v) && $v !== []) {
                        $env[(string) $k] = $v;
                    }
                }
            }
        }
        $env['host.runtime'] = ['value' => base64_encode(Support::runtime())];
        return $env;
    }

    /** @param list<string> $cmd */
    private static function capture(array $cmd, float $timeout): ?string
    {
        $proc = @proc_open($cmd, [0 => ['file', '/dev/null', 'r'], 1 => ['pipe', 'w'], 2 => ['file', '/dev/null', 'w']], $pipes);
        if (!is_resource($proc)) {
            return null;
        }
        $out = '';
        $deadline = microtime(true) + $timeout;
        while (!feof($pipes[1]) && microtime(true) < $deadline) {
            $r = [$pipes[1]];
            $w = $e = null;
            if (@stream_select($r, $w, $e, 0, 200_000) > 0) {
                $chunk = fread($pipes[1], 65536);
                if ($chunk === false) {
                    break;
                }
                $out .= $chunk;
            }
        }
        $complete = feof($pipes[1]);
        fclose($pipes[1]);
        if (!$complete) {
            proc_terminate($proc);
        }
        $status = proc_close($proc);
        return $complete && $status === 0 ? $out : null;
    }

    /**
     * If this process was started as a replay host (its last argument is
     * `kavach-host`), serves the driver and exits; otherwise returns at once.
     *
     * @param list<string> $argv
     * @param array{gateways?: array<string, mixed>|callable, recorder?: list<string>} $options
     */
    public static function maybe(array $argv, callable $factory, array $options = []): void
    {
        if ($argv === [] || end($argv) !== self::HOST_ARG) {
            return;
        }
        // Take the protocol stream for ourselves before anything else, and send
        // echo/print and PHP's own diagnostics to stderr, so that a handler
        // that prints cannot corrupt it (SPEC 9.1).
        $out = @fopen('php://fd/1', 'wb') ?: STDOUT;
        stream_set_write_buffer($out, 0);
        ini_set('display_errors', 'stderr');
        ini_set('html_errors', '0');
        while (ob_get_level() > 0) {
            ob_end_clean();
        }
        ob_start(static function (string $buffer): string {
            @fwrite(STDERR, $buffer);
            return '';
        }, 1);
        $recorder = $options['recorder'] ?? null;
        $host = new self(
            $factory,
            STDIN,
            $out,
            $options['gateways'] ?? null,
            static fn () => self::collectEnvironment($recorder),
        );
        $code = $host->run();
        while (ob_get_level() > 0) {
            ob_end_flush();
        }
        exit($code);
    }

    // -- transport --------------------------------------------------------

    /** @param array<string, mixed> $msg */
    private function send(array $msg): void
    {
        $line = json_encode($msg, self::JSON_OUT) . "\n";
        $off = 0;
        $len = strlen($line);
        while ($off < $len) {
            $n = @fwrite($this->out, $off === 0 ? $line : substr($line, $off));
            if ($n === false || $n === 0) {
                throw $this->stop = new HostStop();
            }
            $off += $n;
        }
        @fflush($this->out);
    }

    /** @return array<string, mixed> */
    private function recv(): array
    {
        $line = fgets($this->in);
        if ($line === false) {
            throw $this->stop = new HostStop();
        }
        $msg = json_decode($line, true);
        if (!is_array($msg) || !isset($msg['t']) || !is_string($msg['t'])) {
            throw $this->stop = new HostStop('malformed message');
        }
        return $msg;
    }

    /**
     * Sends one request and waits for its answer; unwinds on `abort`.
     *
     * @param array<string, mixed> $msg
     * @return array<string, mixed>
     */
    public function request(array $msg, string $want): array
    {
        if ($this->stop !== null) {
            throw $this->stop;
        }
        if ($this->aborted) {
            throw new Abort();
        }
        $this->send($msg);
        $ans = $this->recv();
        if ($ans['t'] === 'abort') {
            $this->aborted = true;
            throw new Abort();
        }
        if ($ans['t'] !== $want) {
            throw $this->stop = new HostStop("expected a '$want' answer, got '{$ans['t']}'");
        }
        return $ans;
    }

    /** @internal */
    public function emit(string $sink, string $data, bool $local): void
    {
        if ($this->stop !== null) {
            throw $this->stop;
        }
        if ($this->aborted) {
            throw new Abort();
        }
        $this->send(['t' => 'emit', 'sink' => $sink, 'data' => base64_encode($data), 'scope' => $local ? 'local' : 'remote']);
    }

    /** @internal */
    public function resolveGateway(string $name): Gateway
    {
        return Support::resolveGateway($this->gateways, $name);
    }

    /** @internal */
    public function sendObserved(string $response, string $error): void
    {
        $this->send($error !== '' ? ['t' => 'observed', 'error' => $error] : ['t' => 'observed', 'response' => base64_encode($response)]);
    }

    // -- loop -------------------------------------------------------------

    /** Serves the driver until `end` or EOF. Returns the exit status. */
    public function run(): int
    {
        $handler = null;
        try {
            while (true) {
                try {
                    $msg = $this->recv();
                } catch (HostStop $e) {
                    if ($e->fatal === null) {
                        return 0; // between steps: the driver left
                    }
                    throw $e;
                }
                switch ($msg['t']) {
                    case 'hello':
                        $handler = $this->hello($msg);
                        break;
                    case 'step':
                        if ($handler === null) {
                            throw new HostStop('step before hello');
                        }
                        $this->step($handler, $msg);
                        break;
                    case 'end':
                        return 0;
                    case 'abort':
                        break; // a stray abort between steps needs no answer
                    default:
                        throw new HostStop("unexpected message '{$msg['t']}'");
                }
            }
        } catch (HostStop $e) {
            if ($e->fatal === null) {
                return 1;
            }
            try {
                $this->send(['t' => 'fatal', 'message' => $e->fatal]);
            } catch (HostStop) {
            }
            return 1;
        }
    }

    /** @param array<string, mixed> $msg */
    private function hello(array $msg): object
    {
        if (($msg['protocol'] ?? null) !== self::PROTOCOL) {
            throw new HostStop('unsupported protocol ' . json_encode($msg['protocol'] ?? null));
        }
        // Sandbox replay (SPEC 6.3) is not supported by this SDK.
        if (($msg['mode'] ?? 'process') === 'sandbox') {
            throw new HostStop('sandbox mode not supported');
        }
        try {
            $handler = ($this->factory)();
        } catch (\Throwable $e) {
            throw new HostStop('could not create the handler: ' . get_class($e) . ': ' . $e->getMessage());
        }
        if (($msg['start'] ?? 'genesis') === 'snapshot') {
            if (!$handler instanceof Snapshotter) {
                throw new HostStop('the journal starts from a snapshot but the handler is not a Snapshotter');
            }
            $data = base64_decode((string) ($msg['snapshot'] ?? ''), true);
            if ($data === false) {
                throw new HostStop('invalid base64 in snapshot');
            }
            try {
                $handler->restore($data);
            } catch (\Throwable $e) {
                throw new HostStop('could not restore the snapshot: ' . get_class($e) . ': ' . $e->getMessage());
            }
        }
        $this->send([
            't' => 'ready',
            'protocol' => self::PROTOCOL,
            'sdk' => Support::PRODUCER,
            'invariants' => array_map(static fn (Invariant $i) => $i->name, Support::invariants($handler)),
            'environment' => ($this->environment)(),
        ]);
        return $handler;
    }

    /** @param array<string, mixed> $msg */
    private function step(object $handler, array $msg): void
    {
        $data = base64_decode((string) ($msg['data'] ?? ''), true);
        if ($data === false) {
            throw new HostStop('invalid base64 in step data');
        }
        $input = new Input((string) ($msg['source'] ?? ''), (string) ($msg['position'] ?? ''), $data);
        $this->aborted = false;
        $failure = null;
        try {
            $handler->handle(new Internal\HostEnv($this), $input);
        } catch (Abort) {
        } catch (HostStop) {
        } catch (\Throwable $e) {
            $failure = Support::classify($e);
        }
        // A handler may have caught the unwinding exception; the state wins.
        if ($this->stop !== null) {
            throw $this->stop;
        }
        if ($this->aborted) {
            $this->send(['t' => 'done', 'outcome' => 'aborted']);
            return;
        }
        $failure ??= Support::checkInvariants($handler);
        $done = ['t' => 'done', 'outcome' => $failure['kind'] ?? 'ok'];
        if ($failure !== null) {
            $done['message'] = $failure['message'];
            if ($failure['detail'] !== '') {
                $done['detail'] = $failure['detail'];
            }
        }
        $this->send($done);
    }
}
