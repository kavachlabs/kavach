<?php

declare(strict_types=1);

namespace Kavach\Conformance;

use Kavach\GatewayError;
use Kavach\Input;
use Kavach\Recorder;

/**
 * Runs one SDK recorder case (spec/recorder/sdk/README.md): records the
 * conformance handler with the fake recorder as the recorder command and
 * returns the fake's verdict.
 */
final class RecorderCase
{
    /** @var array<string, list<mixed>> */
    private array $queue = [];

    private function pop(string $kind): mixed
    {
        if (empty($this->queue[$kind])) {
            throw new \AssertionError("the handler read a $kind the case has no answer for");
        }
        return array_shift($this->queue[$kind]);
    }

    /** @return array{pass: bool, error: ?string, frames: list<mixed>} */
    public static function run(string $casePath, ?string $fakeRecorder = null): array
    {
        return (new self())->exec($casePath, $fakeRecorder);
    }

    /** @return array{pass: bool, error: ?string, frames: list<mixed>} */
    private function exec(string $casePath, ?string $fakeRecorder): array
    {
        $case = json_decode((string) file_get_contents($casePath), true, 512, JSON_THROW_ON_ERROR);
        $fake = $fakeRecorder ?? Spec::dir() . '/recorder/sdk/fake_recorder.py';
        $handler = new Handler();
        if (isset($case['snapshot'])) {
            $handler->restore($case['snapshot']);
        }
        $flags = $case['flags'] ?? [];
        $tmp = sys_get_temp_dir() . '/kavach-case-' . bin2hex(random_bytes(6));
        mkdir($tmp);
        $resultPath = $tmp . '/result.json';
        try {
            $rec = new Recorder(
                $handler,
                service: $case['open']['service'],
                start: $case['open']['start'],
                snapshots: $case['open']['snapshots'],
                recorderCommand: ['python3', $fake, $casePath, $resultPath],
                gateways: Handler::anyGateway(function (string $request): string {
                    $a = $this->pop('gateway');
                    if (isset($a['error'])) {
                        throw new GatewayError($a['error']);
                    }
                    return base64_decode($a['response'], true);
                }),
                config: function (string $key): ?string {
                    $a = $this->pop('config');
                    return !empty($a['unset']) ? null : base64_decode($a['value'], true);
                },
                configSource: 'case',
                flags: $flags ? static fn () => $flags : null,
                required: true,
                clockNanos: fn () => (int) $this->pop('clock'),
                randomBytes: function (int $n): string {
                    $d = base64_decode($this->pop('rand'), true);
                    if (strlen($d) !== $n) {
                        throw new \AssertionError(sprintf('case rand answer is %d bytes, handler asked for %d', strlen($d), $n));
                    }
                    return $d;
                },
            );
            foreach ($case['actions'] as $action) {
                if (isset($action['step'])) {
                    $s = $action['step'];
                    $this->queue = $action['answers'] ?? [];
                    $rec->step(new Input($s['source'], $s['position'], base64_decode($s['data'], true)));
                } elseif (isset($action['flush'])) {
                    $rec->flush(!empty($action['flush']['durable']));
                }
            }
            $rec->close();
            if (!is_file($resultPath)) {
                return ['pass' => false, 'error' => 'the fake recorder wrote no result (did the SDK close it?)', 'frames' => []];
            }
            return json_decode((string) file_get_contents($resultPath), true, 512, JSON_THROW_ON_ERROR);
        } finally {
            @unlink($resultPath);
            @rmdir($tmp);
        }
    }
}
