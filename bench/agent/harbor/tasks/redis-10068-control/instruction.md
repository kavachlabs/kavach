# Task: XTRIM MINID deletes entries above the threshold

You are working in a Redis checkout (C). Fix the bug below in the Redis source.
Do not change the tests.

## Report

> **XTRIM MINID may delete messages whose IDs are higher than threshold**
>
> In a certain scenario, the XTRIM command will delete messages with IDs higher
> than the threshold provided by the MINID option. In fact, all messages in the
> stream get deleted in this specific scenario.
>
> One must add a message to the stream providing an ID, say "10-1". Then other
> messages must be added to the stream without providing an ID (using "*"). If
> we call XTRIM passing one of the auto-generated IDs as the MINID, all
> messages in the stream will be deleted, even those with IDs higher than the
> threshold.
>
> Expected: only messages with IDs lower than the threshold are removed.

(Upstream issue text, abridged; the full text is in the SWE-bench Multilingual
record `redis__redis-10068`.)
