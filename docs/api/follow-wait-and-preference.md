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
