# Task: BZMPOP blocks on arguments that are not keys

You are working in a Redis checkout (C). Fix the bug below in the Redis source.
Do not change the tests.

## Report

> **[BUG] BZMPOP blocks on non key arguments**
>
> In Redis 7.0 BZMPOP was introduced allowing to block for any of the provided
> sets to have at least one element. However this command introduced a change
> in command arguments for which the current generic blocking code for zset
> commands is not considering.
>
> When issuing a bzmpop with timeout and count the command also blocks on the
> timeout/numkeys/max/min which are not keys. Since the keys might not exist at
> the time the command is issued it may block on these keys. In case the user
> will add zset key which matches one of these arguments, it will cause the
> command to be unblocked and provide the results for this key.
>
> To reproduce, client 1:
>
> ```
> bzmpop 100 1 myzset max count 10      <--- blocks here
> ```
>
> Client 2:
>
> ```
> zadd max 1 one 2 two 3 three
> ```
>
> Client 1 is unblocked with this output:
>
> ```
> 1) "max"
> 2) 1) 1) "three"
>       2) "3"
>    2) 1) "two"
>       2) "2"
>    3) 1) "one"
>       2) "1"
> ```
>
> Expected: client 1 stays blocked, waiting for `myzset` to be written.

(Upstream issue text, abridged; the full text is in the SWE-bench Multilingual
record `redis__redis-10764`.)

## Building and testing

The tree is already built; `make` rebuilds only what you change (about
3 seconds after a one-file change). The sorted-set blocking tests run with

```
./runtest --single unit/type/zset --only BZMPOP
```

in about 3 seconds. `--only` takes a regular expression over test names, so
`--only BZPOP` runs the BZPOPMIN/BZPOPMAX tests. A test that waits for a
reply that never comes stalls the run for 20 minutes by default; add
`--timeout 60` to give up after a minute. The whole `unit/type/zset` file
takes about 15 seconds. Redis's full test suite (`./runtest` or
`make test`) takes tens of minutes and is not needed for this task.
