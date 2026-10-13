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

Redis is already built; `make -j4` rebuilds what you change in a few seconds.
The scripting tests run with

```
./runtest --single unit/scripting
```

in about 30 seconds. Run that. The full suite (`./runtest`, `make test`)
takes well over 20 minutes and is not needed.

## Kavach arm only

This checkout has the Kavach integration applied (`redis-kavach.patch`); do not
change `src/kavach.c` or `src/kavach.h`. Production runs this Redis with Kavach
recording. The incident was recorded:
`/task/incident.kavach` is the fixture, and `/opt/redis-old/redis-server` is
the build that failed. The `kavach` tools are available as an MCP
server (`kavach mcp`) and on the command line:

```
kavach inspect /task/incident.kavach
kavach replay  /task/incident.kavach --bin ./src/redis-server
kavach diff    /task/incident.kavach --old /opt/redis-old/redis-server --new ./src/redis-server
```

`./src/redis-server` acts as a replay host when its last argument is
`kavach-host`; `kavach` appends it. The fixture fails the invariant
`script-number-contract`, the contract of numbers that scripts pass to
commands (see `src/kavach.c`).

You are done when you believe the fix is correct. Grading does not use Kavach.
