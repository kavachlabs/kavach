#!/usr/bin/env php
<?php

declare(strict_types=1);

// Kavach's demo service, in PHP.
//
//     php main.php [--fix] [--in FILE] [--fixtures DIR]
//
//     php main.php          # the buggy build: TypeError on evt-008
//     php main.php --fix    # the fixed build
//
// The build is chosen by a command-line flag and never an environment
// variable: environment variables are served from the journal on replay, so a
// replay of the buggy build's fixture would otherwise run the buggy code. The
// replay hosts are `php main.php` (old) and `php main.php --fix` (new).
//
// If kavach-recorder is available ($KAVACH_RECORDER or PATH) the service records
// through it; otherwise it says so loudly on stderr and runs unrecorded.

require __DIR__ . '/../../src/autoload.php';
require __DIR__ . '/Ledger.php';

use Kavach\Input;
use Kavach\Recorder;
use LedgerDemo\Ledger;

$fix = in_array('--fix', $argv, true);

// Lets the kavach CLI use this script to replay fixtures. `kavach-host` is
// the last argument, so --fix is still visible to $argv.
\Kavach\maybeHost($argv, static fn () => new Ledger(fix: $fix));

$path = __DIR__ . '/../../../../examples/ledger/testdata/events.jsonl';
$fixtures = 'fixtures';
for ($i = 1; $i < count($argv); $i++) {
    match ($argv[$i]) {
        '--in' => $path = $argv[++$i] ?? die("ledger: --in needs a file\n"),
        '--fixtures' => $fixtures = $argv[++$i] ?? die("ledger: --fixtures needs a directory\n"),
        '--fix' => null,
        default => die("ledger: unknown argument {$argv[$i]}\n"),
    };
}

$rec = new Recorder(
    new Ledger(fix: $fix),
    service: 'ledger',
    dir: $fixtures,
    deliver: static function (array $outs): void {
        foreach ($outs as $o) {
            printf("%-18s %s\n", $o->sink, $o->data);
        }
    },
);

$in = fopen($path, 'rb') ?: die("ledger: cannot open $path\n");
$n = 0;
while (($line = fgets($in)) !== false) {
    $n++;
    $line = rtrim($line, "\r\n");
    if ($line === '') {
        continue;
    }
    $result = $rec->step(new Input('file:' . basename($path), (string) $n, $line));
    if ($result->kind === 'panic') {
        // Stop the service, as the Go demo does. The recorder has already
        // marked the failure and cut a fixture.
        $rec->close();
        fwrite(STDERR, "ledger: line $n: {$result->message}\n");
        exit(1);
    }
    if (!$result->ok) {
        fwrite(STDERR, "ledger: line $n: {$result->kind}: {$result->message}\n");
    }
}
$rec->close();
