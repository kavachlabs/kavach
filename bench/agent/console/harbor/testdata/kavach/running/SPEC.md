# Running tool calls in the Steps tab

The Steps tab shows an attempt's steps while its agent runs, from the first
of the trajectory (`agent/trajectory.json`), OpenCode's event log
(`agent/opencode.txt`), its live copy (`agent/opencode-live.txt`) and the
session export (`agent/opencode-session.json`) that the attempt has (the
timeline specification, `../timeline/SPEC.md`). OpenCode writes a tool call
to its event log only when the call ends, so while a long command runs
(`cargo test`, `make`, a test suite) the tab shows nothing new, for minutes,
and looks stuck.

The session export, which the opencode-live agent replaces every 15 seconds
while the agent runs, holds tool calls that have not ended. Each is a part of
an assistant message with `type` `tool`, an `id`, its `tool`, and a `state`
whose `status` is `running`, with the call's `input` (for `bash`, the
`command`), `time.start`, and possibly `metadata.output`: what the command
has printed so far. A part whose status is `pending` has not started; its
input may still be arriving. The event log's `tool_use` events carry the
same `part`, `id` included.

An attempt is live while its trial directory has no `result.json` and the
agent's `agent/opencode-live.json` has no `finished` time (or does not
exist yet). An attempt that ended (it finished, failed, timed out or was
stopped) can leave an export in which a call is still `running`: the agent
was stopped mid-call.

## What the console must do

1. While an attempt is live, each tool call that the export shows as
   `running` is a step too, marked running, unless the steps already hold
   that call (the same part `id`) as ended: the export is up to 15 seconds
   older than the event log and its live copy.
2. A running step is described as the ended call will be: a `bash` call by
   its command, a `read` by its file, an edit by its file, and so on. While
   the call runs, the step's output is the end of what it has printed so far
   (`metadata.output`): its last lines, up to 2,000 bytes, starting at a
   line (or the last 2,000 bytes, when its last line is longer). It has no
   exit status and no Kavach verdict until it ends.
3. Running steps come after every other step, in the order the export lists
   them. A running step's time is the call's start (`time.start`), in the
   same milliseconds as the other steps, but never before the step before
   it.
4. A `pending` call is not a step. Nor is a running call of an attempt that
   is not live: it is never shown as running for good.
5. When the call ends, its running step is replaced by the ended call's
   step, read from the event log as now.
6. Nothing else changes: every step that is not running is exactly the step
   shown now, for every attempt, live or not, and the timeline
   specification's rules hold, with "tool calls" meaning the ended ones.
