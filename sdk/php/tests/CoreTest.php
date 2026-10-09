<?php

declare(strict_types=1);

use Kavach\Conformance\Handler;
use Kavach\Env;
use Kavach\HandlerError;
use Kavach\Input;
use Kavach\Internal\Support;
use Kavach\Internal\Wire;
use Kavach\Panic;
use Kavach\Recorder;
use Kavach\RecorderError;

test('uvarint encodes like protobuf', function () {
    assertSame("\x00", Wire::uvarint(0));
    assertSame("\x7f", Wire::uvarint(127));
    assertSame("\x80\x01", Wire::uvarint(128));
    assertSame("\xac\x02", Wire::uvarint(300));
});

test('a clock record is little-endian i64 and not critical', function () {
    $f = Wire::clockRecord(1759752000000000000);
    assertSame(chr(11) . chr(Wire::RECORD) . chr(Wire::T_CLOCK) . chr(0) . pack('P', 1759752000000000000), $f);
});

test('gateway and config records are critical', function () {
    assertSame(1, ord(Wire::gatewayRecord('g', 'q', 'r', '', false)[3]));
    assertSame(1, ord(Wire::configRecord('k', null, 's')[3]));
});

test('invalid UTF-8 in a journal string is replaced', function () {
    assertTrue(preg_match('//u', Wire::utf8("a\xffb")) === 1, 'valid UTF-8');
});

test('conformance JSON follows the escaping rule of SPEC 9.6', function () {
    assertSame('{"error":"a\\"b\\\\c\\n\\r\\t\\u0001\\u001f é<>&/"}', Handler::object(['error' => "a\"b\\c\n\r\t\x01\x1f é<>&/"]));
    assertSame('{"unset":true}', Handler::object(['unset' => true]));
});

test('failure mapping', function () {
    assertSame(['kind' => 'panic', 'message' => 'boom'], array_intersect_key(Support::classify(new Panic('boom')), ['kind' => 1, 'message' => 1]));
    assertSame(['kind' => 'error', 'message' => 'bad'], array_intersect_key(Support::classify(new HandlerError('bad')), ['kind' => 1, 'message' => 1]));
    $f = Support::classify(new InvalidArgumentException('nope'));
    assertSame('panic', $f['kind']);
    assertSame('InvalidArgumentException: nope', $f['message']);
    assertTrue(str_contains($f['detail'], 'CoreTest.php'), 'trace in detail');
});

test('invariants: first failing one, in order, wins', function () {
    $h = new Handler();
    $h->count = 5000;
    $f = Support::checkInvariants($h);
    assertSame('invariant', $f['kind']);
    assertSame('below_limit', $f['message']);
    $h->count = 1;
    assertSame(null, Support::checkInvariants($h));
});

test('Env::now is UTC with microsecond precision', function () {
    $env = new class extends Env {
        public function nowNanos(): int { return 1759752000123456789; }
        public function random(int $n): string { return ''; }
        public function query(string $gateway, string $request): string { return ''; }
        public function config(string $key): ?string { return null; }
        public function emit(string $sink, string $data, bool $local = false): void {}
    };
    assertSame('2025-10-06T12:00:00.123456Z', $env->now()->format('Y-m-d\TH:i:s.u\Z'));
    assertSame('UTC', $env->now()->getTimezone()->getName());
});

test('a missing recorder fails loudly and never fails the step', function () {
    $log = [];
    $rec = new Recorder(
        new Handler(),
        service: 'x',
        recorderCommand: ['/nonexistent/kavach-recorder'],
        logger: function (string $level, string $msg) use (&$log) { $log[] = "$level: $msg"; },
    );
    $r = $rec->step(new Input('t', '0', '[{"op":"emit","sink":"s","data":"d"}]'));
    assertTrue($r->ok, 'step ok');
    assertSame(1, count($r->outputs));
    assertSame(1, count($log));
    assertTrue(str_starts_with($log[0], 'error: '), 'logged at error');
    assertTrue(!$rec->isRecording(), 'not recording');
    $rec->close();
});

test('required recording fails construction without a recorder', function () {
    try {
        new Recorder(new Handler(), service: 'x', recorderCommand: ['/nonexistent/kavach-recorder'], required: true);
    } catch (RecorderError) {
        return;
    }
    throw new AssertionError('expected RecorderError');
});

test('a failed step returns its StepResult and keeps going', function () {
    $rec = new Recorder(new Handler(), service: 'x', recorderCommand: ['/nonexistent'], logger: fn () => null);
    $r = $rec->step(new Input('t', '0', '[{"op":"panic","message":"boom"}]'));
    assertSame('panic', $r->kind);
    assertSame('boom', $r->message);
    $r = $rec->step(new Input('t', '1', '[{"op":"error","message":"bad"}]'));
    assertSame('error', $r->kind);
    assertTrue($rec->step(new Input('t', '2', '[]'))->ok, 'next step ok');
});

test('failure messages carry no file or line; the data keeps the full text', function () {
    require_once __DIR__ . '/other/Thrower.php';
    try {
        (function () { return Other\Thrower::want(null); })();
    } catch (TypeError $e) {
    }
    assertTrue(str_contains($e->getMessage(), 'CoreTest.php'), 'engine message has a location');
    $f = Support::classify($e);
    assertSame('TypeError: Other\Thrower::want(): Argument #1 ($n) must be of type int, null given', $f['message']);
    assertTrue(str_contains($f['detail'], $e->getMessage()), 'full message in data');
    assertSame('RuntimeException: x', Support::classify(new RuntimeException('x in /a/b.php:12'))['message']);
});
