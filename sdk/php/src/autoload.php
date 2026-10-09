<?php

declare(strict_types=1);

// A PSR-4 autoloader for Kavach\ so that nothing needs Composer installed:
//
//     require '/path/to/sdk/php/src/autoload.php';

require_once __DIR__ . '/functions.php';

spl_autoload_register(static function (string $class): void {
    $prefix = 'Kavach\\';
    if (strncmp($class, $prefix, strlen($prefix)) !== 0) {
        return;
    }
    $file = __DIR__ . '/' . str_replace('\\', '/', substr($class, strlen($prefix))) . '.php';
    if (is_file($file)) {
        require $file;
    }
});
