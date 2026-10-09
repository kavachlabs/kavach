<?php

declare(strict_types=1);

use Kavach\Conformance\Handler;
use Kavach\Env;
use Kavach\Host;
use Kavach\Input;

/**
 * Plays a script of driver messages against a Host over in-memory streams and
 * returns the host's messages. Only for sessions that need no answers.
 *
 * @param list<array<string, mixed>> $driver
 * @return array{0: int, 1: list<array<string, mixed>>}
 */
function playHost(array $driver, callable $factory): array
{
    $in = fopen('php://memory', 'w+b');
    foreach ($driver as $m) {
        fwrite($in, json_encode($m) . "\n");
    }
    rewind($in);
    $out = fopen('php://memory', 'w+b');
    $host = new Host($factory, $in, $out, null, fn () => ['host.runtime' => ['value' => 'cGhw']]);
    $code = $host->run();
    rewind($out);
    $msgs = [];
    while (($line = fgets($out)) !== false) {
        $msgs[] = json_decode($line, true);
    }
    return [$code, $msgs];
}

$hello = ['t' => 'hello', 'protocol' => 1, 'service' => 'c', 'start' => 'genesis', 'mode' => 'process'];

test('host: sandbox mode is refused with fatal', function () use ($hello) {
    [$code, $msgs] = playHost([[...$hello, 'mode' => 'sandbox']], fn () => new Handler());
    assertSame(1, $code);
    assertSame([['t' => 'fatal', 'message' => 'sandbox mode not supported']], $msgs);
});

test('host: ready lists invariants and the runtime', function () use ($hello) {
    [$code, $msgs] = playHost([$hello, ['t' => 'end']], fn () => new Handler());
    assertSame(0, $code);
    assertSame('ready', $msgs[0]['t']);
    assertSame(['below_limit'], $msgs[0]['invariants']);
    assertTrue(str_starts_with($msgs[0]['sdk'], 'kavach-php/'), 'sdk');
});

test('host: done omits message for ok and carries it for failures', function () use ($hello) {
    $data = fn (string $json) => base64_encode($json);
    [, $msgs] = playHost([
        $hello,
        ['t' => 'step', 'seq' => '0', 'source' => 's', 'position' => '0', 'data' => $data('[]')],
        ['t' => 'step', 'seq' => '1', 'source' => 's', 'position' => '1', 'data' => $data('[{"op":"panic","message":"boom"}]')],
        ['t' => 'step', 'seq' => '2', 'source' => 's', 'position' => '2', 'data' => $data('[{"op":"count","n":2000}]')],
        ['t' => 'end'],
    ], fn () => new Handler());
    assertSame(['t' => 'done', 'outcome' => 'ok'], $msgs[1]);
    assertSame('panic', $msgs[2]['outcome']);
    assertSame('boom', $msgs[2]['message']);
    assertSame(['t' => 'done', 'outcome' => 'invariant', 'message' => 'below_limit'], array_diff_key($msgs[3], ['detail' => 1]));
});

test('host: an abort is reported even if the handler swallows the unwinding exception', function () use ($hello) {
    $handler = new class implements Kavach\Handler {
        public function handle(Env $env, Input $input): void
        {
            try {
                $env->nowNanos();
            } catch (Throwable) {
            }
            $env->emit('after', 'x'); // must not reach the driver
        }
    };
    [, $msgs] = playHost([
        $hello,
        ['t' => 'step', 'seq' => '0', 'source' => 's', 'position' => '0', 'data' => ''],
        ['t' => 'abort', 'detail' => 'd'],
        ['t' => 'end'],
    ], fn () => $handler);
    assertSame(['t' => 'clock'], $msgs[1]);
    assertSame(['t' => 'done', 'outcome' => 'aborted'], $msgs[2]);
    assertSame(3, count($msgs));
});
