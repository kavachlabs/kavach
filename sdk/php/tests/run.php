<?php

declare(strict_types=1);

// A tiny test runner: every tests/*Test.php file calls test() for its cases.
//
//     php tests/run.php

require __DIR__ . '/../src/autoload.php';

$GLOBALS['kavach_tests'] = [];

function test(string $name, callable $fn): void
{
    $GLOBALS['kavach_tests'][] = [$name, $fn];
}

function assertSame(mixed $want, mixed $got, string $what = ''): void
{
    if ($want !== $got) {
        throw new AssertionError(($what !== '' ? "$what: " : '') . 'expected ' . var_export($want, true) . ', got ' . var_export($got, true));
    }
}

function assertTrue(bool $cond, string $what = 'condition'): void
{
    if (!$cond) {
        throw new AssertionError("$what is false");
    }
}

foreach (glob(__DIR__ . '/*Test.php') ?: [] as $file) {
    require $file;
}

$failed = 0;
foreach ($GLOBALS['kavach_tests'] as [$name, $fn]) {
    try {
        $fn();
        echo "PASS  $name\n";
    } catch (Throwable $e) {
        $failed++;
        echo "FAIL  $name\n  ", get_class($e), ': ', str_replace("\n", "\n  ", $e->getMessage()), "\n";
    }
}
$total = count($GLOBALS['kavach_tests']);
echo $total - $failed, "/$total tests passed\n";
exit($failed ? 1 : 0);
