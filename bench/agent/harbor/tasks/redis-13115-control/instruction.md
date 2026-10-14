# Task: HINCRBY from a Lua script rejects some whole numbers

You are working in a Redis checkout (C). Fix the bug below in the Redis source.
Do not change the tests.

## Report

> **[BUG] hIncrBy from lua "ERR value is not an integer or out of range" with
> numeric values of the form n * 100,000,000 in redis 7.2, but not 6.2 or 7.0**
>
> We have a lua script that does the equivalent of
> `redis.call("HINCRBY", "key", "field", tonumber(ARGV[1]))`
>
> This works fine in redis 6.2 for all values. Under redis 7.2, the `HINCRBY`
> throws `ERR value is not an integer or out of range` if and only if
> `ARGV[1]` is a value that matches the form `n * 100,000,000`.
>
> A second script that passes `ARGV[1]` without `tonumber` succeeds for the
> same values. With `tonumber`, for values from 0 to 2,000,000,000 in steps of
> 50,000,000, 1000000000, 1100000000, 1200000000, … fail with
> `ERR value is not an integer or out of range script: …, on @user_script:1.`
> and 1050000000, 1150000000, … succeed.
>
> Redis 7.0 behaves the same as 6.2; the error only appears in 7.2.

(Upstream issue text, abridged; the full text is in the SWE-bench Multilingual
record `redis__redis-13115`.)

## Building and testing

Redis is already built; `make -j4` rebuilds what you change in about 10 seconds.
The scripting tests run with

```
./runtest --single unit/scripting
```

in under 30 seconds. Run that. The full suite (`./runtest`, `make test`)
takes about 15 minutes and is not needed.

