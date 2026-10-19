# Live steps from remote sandboxes

The console's Steps tab shows an attempt's steps while its agent runs. On a
local Docker sandbox, Harbor's `/logs/agent` is a bind mount, so OpenCode's
event log (`agent/opencode.txt`) fills while the agent works. On Daytona or
Modal nothing is mounted: `opencode.txt` reaches the trial directory only
when the agent ends, and until then the tab shows "0 steps" however much the
agent has done.

Two files do reach the trial directory while such an attempt runs:

- `agent/opencode-live.txt`: the opencode-live agent copies the new bytes of
  the sandbox's `opencode.txt` into it every few seconds. It holds the same
  JSON events, one per line; its last line may be incomplete.
- `agent/opencode-session.json`: the agent's OpenCode session export,
  replaced whole every 15 seconds. Its `messages` hold each message's `info`
  (`role`, `time.created`) and its `parts`: `text`, `reasoning`, `tool`
  (with `state.status`, `state.input`, `state.output`, `state.time.start`),
  `step-start` and `step-finish` (with `cost` and `tokens`).

## What the console must do

1. The steps of an attempt come from the first of these that it has:
   Harbor's ATIF trajectory (`agent/trajectory.json`), the event log
   (`agent/opencode.txt`), the live copy of the event log
   (`agent/opencode-live.txt`), the session export
   (`agent/opencode-session.json`). An attempt with none of them has no steps.
2. The live copy is read as the event log is: the same events give the same
   steps, and an incomplete last line is skipped.
3. From the session export, the steps are the agent's own work, in the order
   the export lists it: each assistant message's text, each tool call that
   has completed or failed (one still running appears once it ends), and one
   usage step per `step-finish`, with its tokens and cost. The user's message
   (the task's instruction) is not a step, nor is reasoning.
4. Step times are milliseconds since the first event or part the steps are
   read from, so never negative, and they never go back: a part with no time
   of its own is placed at the time of the part before it.
5. An export that is not JSON is reported as an error, not as an attempt with
   no steps. Lines of the live copy that are not JSON events (an incomplete
   last line, OpenCode's stderr) are skipped, as in the event log.
6. Nothing else changes: an attempt with a trajectory or an event log shows
   exactly the steps it shows now, whatever live copy or export it also has.
