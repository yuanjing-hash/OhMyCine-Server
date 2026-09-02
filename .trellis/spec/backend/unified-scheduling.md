# Unified Scheduling

## Scope

Use this contract for configurable periodic work, schedule APIs/UI, migration from legacy interval fields, or queue dispatch from Cron.

## Definition contract

- Persist a standard five-field Cron expression, IANA timezone, enabled flag, misfire policy, overlap policy, retry count/delay, maximum runtime, revision, and next-run time.
- Reject six/seven-field expressions and invalid timezones. Preview future occurrences with the same parser used by execution.
- Scheduler definitions enqueue existing domain jobs; business workers do not embed a second Cron loop.
- Configurable jobs include TV follow search, media-library full scan, CookieCloud sync, 115 recycle cleanup, structure diagnosis/optional repair, and STRM consistency reconciliation.
- Life-event listeners, filesystem/provider watcher reconciliation, queue lease heartbeats, downloader polling, upload progress, and UI refresh are runtime loops rather than user Cron jobs.

## Managed migration contract

- Every system-migrated definition has a stable non-empty `managed_key` derived from action + target type + target ID. User-created schedules keep an empty key.
- The database enforces uniqueness only for non-empty managed keys. Multiple user schedules may target the same domain object.
- Startup migration creates missing managed definitions but never adopts a manual same-target schedule and never overwrites a user-edited Cron, timezone, overlap, retry, or runtime policy.
- An explicit change to the owning legacy business setting may update its managed definition. Unrelated connection/media-library edits must not overwrite it.
- Deleting an owning domain record deletes only its managed definition, not manual schedules for the same target.

## Execution contract

- Claim due definitions transactionally and record every queued, running, skipped, failed, and completed run.
- Misfire and overlap behavior is deterministic across restart. Retries are bounded by the frozen definition revision.
- Domain execution revalidates owner permission and exact target scope before mutation.

## Required tests

- Parser/timezone preview, invalid fields, misfire, overlap, retry, maximum runtime, restart idempotency, and queue handoff.
- Upgrade from the prior migration adds the partial managed-key index, backfills only recognizable generated definitions, and leaves manual definitions unmanaged.
- Startup preserves user-edited managed schedules; explicit business schedule changes update them; unrelated edits do not.
