---
name: root-cause-tracing
description: >
  Use when an error occurs deep in execution and you need to trace back to find the original trigger, including production incidents, intermittent failures and corrupted state. Traces bad values backward through the call stack.
---

# root-cause-tracing

Start at the symptom. Find what called it with the bad value. Keep tracing up until you find where the bad value originated. Fix it there, and add instrumentation if the trace is incomplete.
