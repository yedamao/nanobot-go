---
name: cron
description: Schedule reminders and recurring tasks with the cron tool.
---

# Cron

Use the `cron` tool to schedule reminders or recurring tasks. A new job runs in the current chat: when it fires, the agent follows the job's `message` and the final reply goes to that chat. The user can review, edit, pause and remove jobs on the WebUI's Cron page.

Write what to do in `message`, e.g. "Remind me to drink water". Do not copy the current channel or chat ID into it.

Choose by timing: a specified time or interval uses `cron`, even when it repeats. Use `HEARTBEAT.md` for background checks with flexible timing that should only report actionable changes.

## Examples

Fixed reminder every 20 minutes:
```
cron(action="add", message="Time to take a break!", every_seconds=1200)
```

Task the agent runs each time:
```
cron(action="add", message="Check the GitHub stars of yedamao/nanobot-go and report", every_seconds=3600)
```

One-time (compute the ISO datetime from the current time):
```
cron(action="add", message="Remind me about the meeting", at="2026-02-12T10:30:00")
```

Timezone-aware cron expression:
```
cron(action="add", message="Morning standup", cron_expr="0 9 * * 1-5", tz="America/Vancouver")
```

List and remove:
```
cron(action="list")
cron(action="remove", job_id="abc123")
```

`add` takes exactly one of `every_seconds`, `cron_expr` or `at`; `tz` only goes with `cron_expr`. System jobs (heartbeat, Dream) cannot be removed.

## Time Expressions

| User says | Parameters |
|-----------|------------|
| every 20 minutes | every_seconds: 1200 |
| every hour | every_seconds: 3600 |
| every day at 8am | cron_expr: "0 8 * * *" |
| weekdays at 5pm | cron_expr: "0 17 * * 1-5" |
| at a specific time | at: ISO datetime |
