# Cloud offline lifecycle v1

This contract describes actual cloud-native acquisition. qBittorrent/BT/PT,
share receive, provider-item adoption, transfer and import are separate operations.
The generic service policy consumes source-aware downloader lifecycle facts and
never selects the policy by provider name.

## Shared adapter contract

- `Task.Offline *OfflineObservation` carries cloud facts through selected,
  verified-all and remote wrappers. `Task.Completed` may remain false while a
  Node transports cloud-ready output.
- `OfflineLifecycleClient.OfflineOperation(Source)` declares the actual kind,
  verified account namespace, keep-output stop and optional per-file proof.
  Missing account identity never creates a global history namespace.
- `OfflineObserver.ObserveOffline(ctx,id)` is read-only: no native submit,
  replacement operation or remote materialization. Deadline maintenance uses it
  before its completion-versus-expiry decision.
  Registry construction requires both this observer and source-aware lifecycle
  classification for every executor declaring NativeOffline; omission is an
  explicit incompatibility rather than a non-offline fallback.
- `OfflineStopper.StopOfflineKeepOutput(ctx,id)` stops only the original cloud
  operation. A parent `Cancel(false)` also stops Node transport and separately
  cleans its owned local scratch after the cloud stop is confirmed.
- `cloud.WithOfflineDispatchRecorder` binds the original task to the current
  call context. It is invoked after native admission immediately before Add or
  native wanted Add. Directory creation, seed upload, worker creation, polling,
  grant refresh, restart and progress cannot start or renew this clock.
- Native add-time is labelled `provider_added` and needs original task-directory
  ownership. It is never described as an exact observed-running timestamp.

`Source.OfflineRequired` is a separate versioned, bounded required-output goal.
Its paths use the same root-relative convention as resourcecontent selections;
RootName and MultiFile bind the actual output structure. It neither rewrites an
explicit selected plan nor changes an accepted physical wanted set. Arbitrary
files-mode requests cannot use remote whole-resource acquisition as a selection
fallback. Local native wanted retains original pure-v1 metainfo and its existing
receipt/capability restrictions; its ACK is not wanted-set readback.

A selected native acknowledgement can be lost before Server records its opaque
provider identity. `cloud.SelectiveOfflineSubmissionReconciler` re-derives only
the original same-account/root/parent/tag/hash plan, compares the complete saved
metainfo/wanted digest, then reads its existing receipt and native status. It
cannot create or repair a directory, seed or wanted set. Preparation that has not
entered native dispatch may still resume its original guarded preparation steps;
read-only native recovery is never used as permission for a replacement Submit.

A required-files observation needs every required leaf's exact root, stable ID,
parent, path and size, plus explicit finalized-object assertions. The ready
manifest lists only proven required output. Enumeration, final size, SHA1,
pickcode, title, duration and Manifest.Complete do not establish download bytes.
No media read or full hash is added for cloud readiness. Ordinary range/checksum
verification remains part of actual later transport.

## Node negotiation and sealed authority

New actual remote cloud-offline routes require `cloud_offline_lifecycle_v1` in
addition to their existing provider/read/range capabilities. Share-only routes
remain independent. The immutable StorageSourcePlan freezes:

- cloud_offline_lifecycle_version=1;
- offline_account_namespace: the verified account digest, checked against the
  credential-bound original account on Node;
- offline_required_digest when a lawful whole-resource route has a goal.

The optional exact OfflineRequiredPlan travels only in the sealed source
credential. It is retained in a separate purpose-encrypted Node cloud receipt,
not public plan JSON. Node schema 12 stores a safe cloud observation and stop
projection separately from media transport progress. Existing Quark ciphertext
purposes and account-wide resource ownership contracts are unchanged.

The existing signed storage-source action endpoint also accepts
`cloud_offline_status` and `cloud_offline_stop_keep_output`. Both bind the
original Server, Node, task, operation, plan digest, storage, account and lease.
Stop additionally requires sealed `offline_cancel_keep_output` permission.
`keep_materialization=true` is accepted only after the original receipt proves
its goal ready; it preserves intended transport/export. Status/stop bypass data
admission. A grant refresh cannot upgrade a version-0 plan or add missing action
authority under an old grant ID.

Durable typed non-acceptance proof is distinct from an accepted native terminal
failure. Both release acquisition capacity without creating timeout history.
Authentication/quota/transport error codes alone never establish either proof.
A definite native rejection is marked only after its original no-task receipt or
context-bound durable rejection recorder succeeds. Unknown outcomes retain
original admission and cannot reuse Submit. Native terminal failures retain
account-wide resource ownership until an actual original keep-output stop or
authoritative absence settles it.

The durable receipt records original dispatch, acceptance, cloud readiness and
native stop entry/ACK. An ambiguous submit is reconciled through its original
identity; it cannot Submit again. An ambiguous stop only queries its original
native task and cannot repeat the cancel. A different account, root, successor
operation or expired lease cannot authorize that mutation.

An accepted v1 operation that is still queued before native dispatch reports a
queue state with no cloud clock. Its Node acceptance/worker timestamps do not
stand in for native dispatch or imply a missing-clock error.

Only exact intended files are enumerated/materialized/exported when a required
goal exists. Cloud readiness is persisted before range reads. Node scratch
cleanup never removes cloud output and remains a separate exact-owned action.

## Deadline during disconnection

Both Server and Node use an absolute 30-minute v1 budget from the durable cloud
dispatch. Node process maintenance first attempts one bounded fresh cloud
observation with an unexpired original grant and lease. Eight bounded decision
workers separate those reads from local deadline persistence; eight independent
control workers reconcile durable upstream stops, outside media admission. A committed ready fact
ends the clock before later transport. Otherwise it persists deadline_exceeded
and the original stop intent, blocking late materialization. With current valid
authority it sends the real original native Cancel(false), outside data admission.

Sealed grants last at most 15 minutes and normal operation leases are shorter
than the cloud budget. If Server disconnects until those authorities expire,
Node still persists local cancellation and stop_pending; it cannot claim cloud
stop or use expired credentials. The same task's fresh permitted grant/lease
later reconciles that original stop. Server recovers the original clock and
failure outcome on reconnect. No long-lived disconnected cancellation authority
or silent fallback execution is introduced.

## 115 App finalized-object evidence and acceptance limits

The optional bounded Cookie App reader uses the fixed
`https://proapi.115.com/android/2.0/ufile/files` endpoint and its explicit
`fid,pid,fn,fc,fs,aid,fta` schema. Only normal file leaves with a present FTA=1
are finalized; 0/2 are pending; missing, null, invalid and unknown values are
unknown. This does not assign App semantics to web STA. Every response rechecks
the requested parent, stable identity, name, size, bounded pages and complete
required coverage.

Primary protocol sources: [OpenList 115 SDK file schema](https://github.com/OpenListTeam/115-sdk-go/blob/main/fs.go),
[p115client App attribute normalizer](https://p115client.readthedocs.io/en/latest/_modules/p115client/tool/attr.html#normalize_attr_app),
and [p115client Cookie App listing](https://p115client.readthedocs.io/en/latest/_modules/p115client/client.html#P115Client.fs_files_app).
These public sources establish field semantics and endpoint shape. Protocol
fixtures do not prove that a particular Cookie platform or real offline-produced
object exposes FTA before aggregate completion. Unsupported App credentials,
missing fields and unrecognized schemas retain unknown proof and aggregate
fallback; optional App rejection cannot mark otherwise usable web/offline
credentials globally expired. Existing account revision, network admission,
rate/risk cooldown and TLS verification remain enforced.

Validation uses local fake providers, bounded HTTP fixtures and temporary Node
SQLite reopen/range/export/control scenarios. No real-account cloud stop,
provider output, media transfer, deployed Node compatibility or installed-service
acceptance is claimed.
