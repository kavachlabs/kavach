---
name: condition-based-waiting
description: >
  Use when tests have race conditions, timing dependencies or flaky, intermittent behavior. Replaces arbitrary timeouts with polling for the actual condition.
---

# condition-based-waiting

Replace sleeps with waits on the condition the test cares about.
