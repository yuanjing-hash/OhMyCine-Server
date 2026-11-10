# Follow observed wait and acquisition preference

These are additive fields on the existing authenticated Follow APIs. Provider
secrets, temporary links and SDK causes are never returned. Lists and history
read only local state and never refresh credentials or query a provider.

`GET /api/v1/follows`, `GET /api/v1/follows/{id}` and
`GET /api/v1/follows/{id}/runs` may return `wait` on a summary:

```json
{
  "wait": {
    "not_before": "2026-11-03T00:05:00Z",
    "reason": "provider_cooldown",
    "recorded_at": "2026-11-03T00:00:01Z",
    "server_time": "2026-11-03T00:01:00Z"
  }
}
```

`not_before` is a real local gate deadline observed during a finished failed or
partially submitted run. `recorded_at` is when that run finished recording it;
`server_time` aligns the browser clock. The bound does not promise upstream
recovery. A missing bound is omitted, never derived from an error string or the
subscription interval. An expired recorded bound remains historical evidence.
The next scheduled subscription check stays in `next_run_at`, independently.

Current summary projection matches the last run's owner, subscription, revision,
lifecycle and current error and obeys existing resource visibility. Pause, edit,
clear-error or cleared history removes the current hint. Historical rows keep
their own recorded facts. The browser uses one view clock for current waits;
history shows static recorded deadlines, without restarting a historical wait.
Reaching zero does not submit or schedule any operation.

The execution snapshot adds optional
`filters.acquisition_preference: "transfer_first" | "existing_order"`.
Missing/empty old snapshots retain the existing greedy coverage/site order.
New forms receive the current global default, initially `transfer_first`.
Changing the global default does not rewrite subscriptions, active runs or
tasks; editing a subscription creates the existing revisioned snapshot.

`transfer_first` prefers an actual executable 115/Quark share before other
qualified resources, including a torrent that covers more missing episodes.
Within the same tier the existing coverage/excess/site/quality/subtitle order
applies. Names and site categories grant no priority to a magnet. All identity,
season, missing-file, quality and required-subtitle checks still apply. After an
accepted share, the next torrent plan contains only still-missing actual files.
Read-only or definite no-task failures may fall through; an uncertain write
keeps its claim and stops fallback. The 50-candidate, two-minute, four-reader
budget remains bounded; unexamined candidates are reported as deferred.

For a verified public magnet, partial selection retains the complete original
metainfo and exports a torrent plus exact files to the executor. This derived
representation does not change the trusted original source. An admission-only
private proof binds owner, site/revision, public-BT origin, infohash and selection
digest. Both submission boundaries recheck it against the current site; browser
JSON and raw torrents cannot mint it. Accepted encrypted task sources retain the
existing selection digest and task identity. Reopen/retry loads those original
facts through the worker; it does not reconstruct a proof or submit a new task.

`GET /api/v1/settings/follows` additionally returns `acquisition_preference`.
`PATCH /api/v1/settings/follows` accepts this optional independent field along
with `revision` and `subtitles`; omission by an older client preserves the saved
preference. Existing settings permissions, CSRF, no-store and atomic revision
CAS apply. Invalid nonempty enum values are rejected. The preference is not
stored in subtitle JSON. See [the scoped OpenAPI](resource-content.openapi.json).

Migration 127 adds nullable `follow_runs.retry_not_before`, safe `retry_code`
and the independent defaults column. It does not infer historical deadlines or
rewrite any frozen snapshot, encrypted credential, receipt or subtitle policy.

Ordinary successful PanSou responses remain `completion=received` and use the
neutral received explanation. They do not imply completion of all upstream
plugins. Real source, timeout, alias and truncation warnings remain warnings.

## Bounded inspection progress

Run `filter_summary` adds `candidate_checks_started` (candidate checks that entered
the four-reader preparation window), `candidate_checks_deferred` (checks interrupted
by that window or not started) and `content_rejected` (invalid or unsafe actual
metadata). `candidate_budget` counts exhausted windows, so a value of 1 does not
mean one resource was checked. `selected` remains accepted task returns; content
read failures and unstarted checks do not count as failed download submissions.

`follow_candidate_budget` means more inspection is deferred to later scheduled
checks. `follow_content_unavailable` means actual files or their identities remain
unverified. These additive error codes retain the existing `failed` or `submitted`
run status for compatibility and use neutral waiting labels. Actual route/submit
failures keep `follow_submit_failed`/`follow_submit_partial`. Confirmed unsuitable
content remains a resource rejection or no-match. Real provider cooldown evidence
can accompany any of these unfinished outcomes; no deadline is invented.

An internal versioned Job checkpoint retains only the last started candidate's
fingerprint and season, scoped to the owner, subscription/configuration/lifecycle
and definite missing set. A later bounded run resumes its read order after that
candidate and wraps safely; a missing/old checkpoint or changed scope starts from
the normal order. This is scheduling progress, never cached inspection proof.
Within each prepared window, verified executable shares keep `transfer_first`
priority. Every selected leaf, current permission, source expiry, route and claim
is rechecked, and unknown writes keep their original reservation.

Modern inspectable BT/PT/magnet results may enter this bounded check when their
work name is plausible but title season/episode facts are absent. Unrelated work
names, contradictory known seasons and conflicting verified identities still
reject. Actual file verification supplies acquisition facts, never a post range.

Content retries wait for a canceled physical read to finish cleanup before
starting a replacement read. The same applies to shared actual-file identity
lookups. Only successful content reads are cached. Fixed safe errors distinguish
busy queues, finite read timeout, unavailable magnet metadata, invalid metainfo,
unsafe paths, hash mismatch, limits and unsupported format; none expose source
URLs, tracker/peer information or upstream error bodies. The engine keeps its
existing two active/32 pending sessions and one-minute total including queue wait.


## Subscription owner display

Subscription list/detail and successful create/edit/pause/resume summaries add
optional `owner_name`. It is the current display name with the account username
when different; username is the fallback for an empty display name. A missing
account is explicitly shown as `已删除账号`. Existing `owner_id` is unchanged.
This local projection is display-only, never authorization or execution identity.

Owner names are resolved in one bounded batch after existing own/all subscription
scoping. No user-directory permission is required and no user directory, roles,
login state or credential fields are returned. Clients talking to an older Server
can show `账号 #<owner_id>` when the optional field is absent. Renaming an account
changes the current label without rewriting the subscription or its frozen plan.


## Follow eligibility, source order and seeder observations

A Follow-only internal identity-search policy checks the frozen subscription's eligibility before merging equivalent resource observations. A rejected alias cannot suppress a later eligible observation of the same resource. A later better eligible observation retains its own actor-bound claim/material; different actual resource identities remain separate. The caller policy digest includes the full frozen snapshot and definite missing set, isolating its PanSou snapshot from ordinary manual search. No HTTP caller can supply this policy or executable callbacks.

For explicit `transfer_first`, potential share-producing Sites are queried before ordinary Sites while retaining their original configured rank inside candidate selection. Actual share source kinds also precede BT inside the identity snapshot before local pagination and at the start of a bounded inspection cycle. A private scheduling-scope version restarts old BT-biased cursors at this new boundary. New cursors still advance only after checks actually start, so a run can continue past a failed share prefix to later sources instead of retrying that prefix forever. A cursor grants no inspection proof or source authority. Missing/legacy `existing_order` preserves the earlier source order and checkpoint scope. All current receiver, Site, permission, exact missing-file and subtitle checks remain mandatory; this preference never changes a saved Site's channel/type policy or fabricates a share result.

The shared authorized route choice adds `can_try_without_seeders`, projected from the local executor registry rather than a provider test or type-name branch in Follow. Only enabled choices with this capability may bypass the public live-swarm minimum. Ineligible live-swarm choices are removed again during preparation, actual-selection route validation and final serial handoff/fallback. This permits an offline attempt; it does not prove provider availability, acceptance or download completion. Actual file selection remains required.

Existing `seeders` remains the compatibility rejection count. Additional safe reasons distinguish `seeders_zero`, `seeders_unknown`, `seeders_zero_offline` and `seeders_unknown_offline`. `share_results` counts returned share observations including aliases, `share_candidates` counts eligible deduplicated share candidates, and `share_checks_started` counts entered share preparations. A zero count does not by itself prove the configured upstream has no shares: current Site type policy, authorized usable receiver association, actual query completion, returned data and deferred inspection are separate facts. Aggregate title samples do not establish whether entries share one resource identity.

## Completion and current subscriptions

The optional list filter `status=current` includes active, paused and blocked
subscriptions. An omitted filter retains the historical all-status view. The
summary adds `caught_up`, based on the saved known-aired target and actual
present counts. Ongoing series remain active when currently caught up.

A subscription becomes `completed` only when every selected season has a
complete official episode list, finished-season evidence and actual complete
catalog coverage. Ended/canceled series or a later officially declared season
can establish that a selected season finished. Future dates, unavailable facts
and truncated lists keep it active. Completed subscriptions have no
`next_run_at`, and their managed search schedule is disabled. History remains
available. A manual check or policy edit uses the existing authorized entrypoint.
When manual reconciliation returns a completed subscription to active, it
restores the managed schedule's next check in the same guarded transaction and
preserves its Cron, timezone and execution policies.

Legacy completed rows with a next check meant currently caught up. List and
detail reads project these as active unless an authorized enabled target
catalog proves an ended/canceled selected season is fully present. The legacy
classification prefix is bounded to 1000 rows and the local metadata lookup to
two seconds. Unresolved rows remain current until normal worker reconciliation.
The current/completed filter uses the same captured classification for total,
paging and row status.

Missing `poster_ref` is filled from the authorized effective catalog using the
shared discovery image gateway. Normal worker coverage also saves a missing
poster from its existing verified metadata read. Lists and detail reads do no
provider calls, preserve a nonempty saved poster and recheck current library
visibility. Dashboard artwork uses the same configured metadata/image gateway
as the existing catalog cards.
