# Task: a missing key answers before its arguments are checked

You are working in a Redis checkout (C). Fix the bug below in the Redis source.
Do not change the tests.

## Report

> **[BUG] Bitcount doesn't return error for missing end parameter if key is missing**
>
> BITCOUNT is documented as
>
> ```
> BITCOUNT key [start end [BYTE | BIT]]
> ```
>
> When `start` is specified but `end` is missing (a syntax error), the command
> returns `ERR syntax error` when the key exists, but returns `0` if the key is
> missing.
>
> To reproduce (Redis 7.0.7): `BITCOUNT missing 0`
>
> Expected: `ERR syntax error` when `end` is missing, irrespective of whether
> the key exists or not.

(Upstream issue text, abridged; the full text is in the SWE-bench Multilingual
record `redis__redis-11734`. It was filed against Redis 7.0.7; this checkout is
later.)

## Building and testing

The tree is already built; `make -j4` rebuilds after a change in about 20
seconds. The tests for this area take about 2 seconds:

```
./runtest --single unit/bitops
```

The full suite (`./runtest`) takes 10 minutes or more and is not needed.
