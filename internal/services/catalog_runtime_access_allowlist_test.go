package services

// Reviewed against catalog-integration-inventory.md sections 3 and 9.
// Exact function/declaration entries, NOT directory/file exemptions. The suffix
// is normalized AST syntax evidence; changing a query receiver with unchanged
// E/R/A counts still fails. Re-review legacy/versioned routing before updating.
// Data-only model references are retained so a new query cannot inherit a pure
// planner/DTO exemption. Never regenerate this list solely to make CI green.
// Batch review: event/baseline reads remain pinned. Legacy artifact scopes and
// collection queries retain exact library/generation filters and mode gates.
// Health/naming planners add no queries; retirement uses child-first batches.
// Release integration review: upstream business-log calls and sample contexts
// change nine function digests without adding E/R/A references or changing the
// legacy/catalog gates, pinned facts, publication fences or write receivers.
var catalogRuntimeAccessAllowlist = map[string]string{
	// Existing acquisition projection: task ownership/resource checks and this
	// exact work lookup share withCatalogReadTx. Readability is checked before
	// PinCatalogTx; VisibleEntries is bounded to one authorized library/work.
	"internal/services/acquisition.go:acquisitionProjection.catalogTarget": "model:E=1,query:Take:E=1|cda110c5981cb399",
	// Quark review: initialization only counts legacy rows inside the fresh-library
	// create transaction and refuses any existing facts; it is not a converter.
	"internal/services/provider_observation_catalog.go:initializeFreshQuarkCatalogTx": "model:A=1,model:E=1,model:R=1|aba749825fc599f8",
	// Recovery envelopes are private data, never alternate catalog queries.
	// Bindings read Entries/Recognitions from PinCatalogTx in the publication
	// transaction. Restored evidence is bounded to matching scope/root/epoch and
	// existing identity anchors before the standard candidate allocator accepts it.
	"internal/services/provider_observation_recovery.go:declaration(observationRecovery)":            "model:E=1,model:R=1|fbd5b6a03c071af0",
	"internal/services/provider_observation_recovery.go:declaration(observationRecoveryEntry)":       "model:E=1|b45a8c2938ccc8fe",
	"internal/services/provider_observation_recovery.go:declaration(observationRecoveryRecognition)": "model:R=1|afd97ddfbadb25d6",
	"internal/services/provider_observation_recovery.go:prepareObservationRecovery":                  "model:E=1,model:R=1|ea9338c583b2ba60",
	// Confirmed Quark removal captures the current pinned E/R before head change,
	// under the same transaction and source epoch, capped at 250 rows / 1 MiB.
	"internal/services/provider_observation_recovery.go:saveObservationRecoveryBindingsTx": "model:E=1,model:R=1,query:Find:E=1,query:Find:R=1|abcfd8255280dd80",
	// Exact restoration IDs are read from a pinned effective head on the deferred
	// pool; an omitted partial-baseline row is never treated as a tombstone.
	"internal/services/provider_observation_recovery.go:appendObservationRecoveryBaseline": "model:E=1,model:R=1,query:Find:R=2|eb9ed67b53121143",
	// Confirmed Quark removals revoke only the bounded, prepared STRM manifests
	// after a pinned new-head absence check, within the publication transaction.
	"internal/services/catalog_scan_revocation.go:revokeQuarkObservationSTRMTx": "model:E=1,query:Find:E=1|6c353cfff0e648a2",
	// quark_live-only validation: existing snapshot data and a pinned read via
	// MediaLibraryService.withCatalogRead; no production or legacy-table bypass.
	"internal/services/quark_observation_live_validation.go:declaration(quarkObservationLiveSnapshot)": "model:E=1|6be932ae66281b87",
	"internal/services/quark_observation_live_validation.go:quarkObservationLiveRuntime.snapshotAt":    "model:E=1,query:Find:E=1|e15e90f872b409e3",
	// quark_live retirement acceptance reads the current recognition through
	// the existing pinned catalog reader, scoped to its sole verified entry.
	"internal/services/quark_observation_retirement_live_validation.go:quarkObservationLiveRuntime.retirementAuthority": "model:R=1,query:First:R=1|ca0f0eff797f70a4",
	// Validation-only: the immutable exact published base/delta is source-fenced
	// before projecting its sole E/R pair, preserving shared metadata masks.
	"internal/services/quark_observation_retirement_live_validation.go:quarkObservationLiveRuntime.retirementVersionFacts": "model:E=1,query:First:E=1|82b2a3e837079b3c",
	// Signed Quark playback reads the unique provider item through a pinned
	// VisibleEntries query, not raw anchors, and repeats it after root resolution.
	"internal/services/quark_playback.go:SignedProxyService.quarkArtifactEntry": "model:E=3,query:Find:E=1|e108e03166cbb726",
	// Reviewed logical-exclusion visibility: public and artifact readers use
	// VisibleEntries; exact source members are captured through pinned raw facts.
	"internal/services/catalog_snapshot_reader.go:declaration(catalogVisibleEntryPredicate)": "sql:E=1|856d7aeb2a29f8c9",
	"internal/services/media_catalog_exclusion.go:MediaLibraryService.ExcludeCatalogWork":    "model:E=2,query:Find:E=1,query:First:E=1|a19b30971f5417ca",
	// Row-catalog scan transaction: stable ProviderID rename, bounded by library/id/provider.
	"internal/services/media_library_asset_identity.go:matchRenamedSourceAssetsTx":         "model:A=3,query:Model:A=1|07fb089edf33a31a",
	"internal/services/media_artifact_deletions.go:captureDeletedEntryRecognitionScopesTx": "model:E=1,model:R=2,query:First:R=1,query:Model:R=1,query:Where:R=1|0b136dc598c84da1",
	// Exact event-only no-op writes; legacy-only work pruning and pinned restore checks.
	"internal/services/media_library_event_deletion.go:MediaLibraryService.completeAbsentProviderDeletion": "model:A=1,model:E=1,query:Find:A=2,query:Find:E=2|e7237e5fe03bc93b",
	"internal/services/media_artifact_deletions.go:captureDeletedRecognitionScopesTx":                      "model:E=2,query:Find:E=1|447efa6e74d1deeb",
	"internal/services/media_artifact_deletions.go:pruneDeletedEmptyRecognitionsTx":                        "model:E=1,model:R=1,query:Delete:R=1,query:Model:E=1|df602cfe26929f72",
	"internal/services/media_artifact_deletions.go:checkDeletedWorkRestoration":                            "model:E=1,query:Find:E=1|a4c842030e139443",
	// Provider deletion reads use the library-pinned CatalogReader; cleanup
	// checks current identity restoration before removing exact owned manifests.
	"internal/services/media_library_event_deletion.go:MediaLibraryService.prepareLocalProviderDeletion":  "model:A=1,model:E=1,query:Find:A=1,query:Find:E=1|c3a910cc525a0bed",
	"internal/services/media_library_event_deletion.go:MediaLibraryService.prepareLocalDirectoryDeletion": "model:A=2,model:E=2,query:Find:A=2,query:Find:E=2|1485e2fa5257ff43",
	"internal/services/strm_management.go:STRMManagementService.buildCleanupPlan":                         "model:A=1,model:E=1,query:Find:A=1,query:Find:E=1|ae9a42df3609c775",
	"internal/services/media_library_events.go:MediaLibraryService.providerDeltaAlreadyApplied":           "model:A=2,model:E=2,query:Find:A=1,query:Find:E=1|7da3ea34396f24ac",
	"internal/services/media_library_local_events.go:MediaLibraryService.knownLocalCatalogPaths":          "model:A=1,model:E=1,query:Find:A=1,query:Find:E=1|df629a97c376924c",
	"internal/services/media_library_collections.go:reconcileTMDBCollectionsScopeTx":                      "model:E=2,model:R=2,query:Find:E=1,query:Find:R=2|39b9c72417930f42",
	"internal/services/media_artifact_full_audit.go:MediaArtifactService.filterLegacyFullArtifactWork":    "model:A=3,model:E=4,model:R=3|8ee8fb3ad7e823e9",
	"internal/services/media_artifact_followup.go:freezeBatchArtifactCheckpointTx":                        "model:A=1,model:E=1,query:Model:A=1,query:Model:E=1|179444f02214c2e8",
	"internal/services/catalog_scan_batch_baseline.go:loadLegacyBatchFactsTx":                             "model:A=3,model:E=3,query:Find:A=1,query:Find:E=1|7711b314e95d505a",
	"internal/services/media_artifact_scope.go:MediaArtifactService.freezeLegacyArtifactScope":            "model:A=1,model:E=1,query:Find:E=1,query:Model:A=1|c97c1f7644fe2b57",
	"internal/services/media_artifact_scope.go:artifactAssetSourceFingerprint":                            "model:A=1|c537323a6c5df407",
	"internal/services/catalog_scan_batch_baseline.go:MediaLibraryService.loadCatalogBatchBaseline":       "model:A=1,model:E=1,model:R=3,query:Find:A=1,query:Find:E=1,query:Find:R=2|f7afda5392c23032",
	"internal/database/migration_structure_review.go:migrateStructureReviewWorkspace":                     "sql:R=1|f65867ba97727d6c",
	"internal/services/catalog_artifact_worker.go:MediaArtifactService.generateBoundArtifacts":            "model:A=2,model:E=5,model:R=2,query:Find:E=1,query:Find:R=3|af61c87cc1220413",
	"internal/services/catalog_artifact_worker.go:MediaArtifactService.prepareCatalogArtifactScope":       "model:A=2,model:E=4,model:R=2,query:Find:A=1,query:Find:E=2,query:Find:R=1|ed1d9ef63e3d508e",
	"internal/services/catalog_artifact_worker.go:MediaArtifactService.fullRecognitionArtifactsHealthy":   "model:E=1,model:R=1|bca8ad4c6cd85164",
	"internal/services/catalog_artifact_worker.go:MediaArtifactService.catalogSTRMArtifactsHealthy":       "model:E=1|8d3a0905fac3d425",
	"internal/services/catalog_collection_read.go:catalogCollectionMembers":                               "call:layerSQL=1,sql:E=1|04386d88affdf413",
	"internal/services/catalog_compaction.go:CatalogSnapshotStore.copyCatalogCompactionFacts":             "call:effectiveSQL=1|bd904910612724b6",
	"internal/services/catalog_conversion_copy.go:CatalogSnapshotStore.catalogConversionDigest":           "call:catalogConversionPage=1|1122971267a9905f",
	"internal/services/catalog_conversion_copy.go:CatalogSnapshotStore.copyCatalogConversionFacts":        "call:catalogConversionPage=1,call:catalogConversionTable=1|6aa70ea1ee2f16bf",
	"internal/services/catalog_conversion_copy.go:catalogConversionPage":                                  "call:catalogConversionTable=1,helper:catalogConversionPage=1,model:A=1,model:E=1,model:R=4,query:Find:A=3,query:Find:R=1|4483148d2110165d",
	"internal/services/catalog_conversion_copy.go:catalogConversionTable":                                 "helper:catalogConversionTable=1,sql:A=1,sql:E=1,sql:R=1|6c64a07f2aef9102",
	"internal/services/catalog_deletion_writer.go:MediaLibraryService.prepareCatalogDeletion":             "model:R=1,query:Find:R=1|14b50003c6fe03cc",
	"internal/services/catalog_deletion_writer.go:catalogDeletionAssetDigest":                             "model:A=2|b2e25796a70af78e",
	"internal/services/catalog_deletion_writer.go:declaration(catalogDeletionWrite)":                      "model:A=1,model:E=1|a3f776532238e71d",
	"internal/services/catalog_deletion_writer.go:validateCatalogDeletionTx":                              "model:A=1,model:E=1,query:Find:E=2|25ff7969524efa88",
	"internal/services/catalog_manual_unit_stabilization.go:declaration(existingRecognitionEntryIndex)":   "model:E=2|e2ea6b95f73d22f4",
	"internal/services/catalog_manual_unit_stabilization.go:existingRecognitionEntryID":                   "model:E=1|d6a34c68a2f8094a",
	// Pure pinned-entry index retains pending physical membership to prevent
	// extending a manual override to already-existing, temporarily unlinked files.
	"internal/services/catalog_manual_unit_stabilization.go:indexExistingRecognitionEntries":                 "model:E=4|f07058cfe07d65d1",
	"internal/services/catalog_manual_unit_stabilization.go:partitionExistingManualRecognitionUnits":         "model:R=2|63d96ee911b04948",
	"internal/services/catalog_raw_facts.go:CatalogReader.RawEntryFacts":                                     "call:effectiveSQL=1|4a83e16f1757b04f",
	"internal/services/catalog_raw_facts.go:CatalogReader.RawSourceAssetFacts":                               "call:effectiveSQL=1|6eab70323e6c1959",
	"internal/services/catalog_recognition_base.go:MediaLibraryService.copyCatalogRecognitionBase":           "call:effectiveSQL=1|c93532548cc2ff68",
	"internal/services/catalog_recognition_writer.go:MediaLibraryService.persistVersionedRecognitionResults": "model:R=2|613768a5f5e924b0",
	"internal/services/catalog_recognition_writer.go:MediaLibraryService.publishCatalogRecognitionDelta":     "model:R=1|d96394d1a0b55041",
	"internal/services/catalog_recognition_writer.go:validateCatalogRecognitionContext":                      "model:R=3,query:Find:R=1|06c4382b13162cd7",
	"internal/services/catalog_reorganization_writer.go:MediaReorganizationWorker.prepareReorganization":     "model:R=2,query:Find:R=1|ff045d6e6275b6ad",
	"internal/services/catalog_scan_publication.go:MediaLibraryService.loadCatalogScanBaseline":              "model:A=1,model:E=1,model:R=1|30308740c95314ea",
	"internal/services/catalog_scan_publication.go:MediaLibraryService.prepareCatalogScanAssets":             "model:A=8|823af9c330522bb6",
	"internal/services/catalog_scan_publication.go:MediaLibraryService.prepareCatalogScanFacts":              "model:E=6,model:R=5|993d8b3f15873c58",
	"internal/services/catalog_scan_publication.go:mediaLibraryRecognitionArtifactChanged":                   "model:R=1|115ab1d5b26c7d0c",
	"internal/services/catalog_scan_publication.go:catalogScanEntry":                                         "model:E=2,model:R=1|f68047e0fcce0c7c",
	"internal/services/catalog_scan_publication.go:declaration(catalogScanBaseline)":                         "model:A=1,model:E=1,model:R=1|8513ca1363eab390",
	"internal/services/catalog_snapshot_identity.go:CatalogSnapshotStore.resolveCatalogIdentityBatch":        "call:allocateCatalogAnchors=1|b3383e6bd3bdc760",
	"internal/services/catalog_snapshot_identity.go:allocateCatalogAnchors":                                  "helper:allocateCatalogAnchors=1,model:A=3,model:E=2,model:R=2,query:Model:A=1|f51e2e23267ea7f1",
	"internal/services/catalog_snapshot_lifecycle.go:validateCatalogCandidate":                               "sql:E=3|1026ac785db4c741",
	"internal/services/catalog_snapshot_reader.go:CatalogEntryFromLegacy":                                    "model:E=1,model:R=1|092430b0789d5c60",
	"internal/services/catalog_snapshot_reader.go:CatalogReader.Entries":                                     "call:effectiveSQL=1,call:layerSQL=1,call:unionQuery=1,sql:E=1|f1e225dbe5190ca4",
	"internal/services/catalog_snapshot_reader.go:CatalogReader.Recognitions":                                "call:effectiveSQL=1,call:unionQuery=1,sql:R=1|5c99724b8ea03445",
	"internal/services/catalog_snapshot_reader.go:CatalogReader.SourceAssets":                                "call:effectiveSQL=1,call:unionQuery=1,sql:A=1|e7933f60b726c79b",
	"internal/services/catalog_snapshot_reader.go:CatalogReader.effectiveSQL":                                "call:layerSQL=1,helper:effectiveSQL=1|6ee292c81d47aaef",
	"internal/services/catalog_snapshot_reader.go:CatalogReader.entryIdentities":                             "call:effectiveSQL=1,call:unionQuery=1,sql:E=1|4662a8ba4b8bde7c",
	"internal/services/catalog_snapshot_reader.go:CatalogReader.layerSQL":                                    "helper:layerSQL=1|074650d1026dec93",
	"internal/services/catalog_snapshot_reader.go:CatalogReader.unionQuery":                                  "helper:unionQuery=1|ef6b52e688100076",
	"internal/services/catalog_snapshot_reader.go:CatalogRecognitionFromLegacy":                              "model:R=1|1b8d98c94fdb2f7e",
	"internal/services/catalog_structure_reader.go:MediaLibraryStructureService.loadStructureCatalog":        "model:A=1,model:E=1|7c454df6bf093e41",
	"internal/services/catalog_structure_reader.go:declaration(structureCatalogFacts)":                       "model:A=1,model:E=1|21e67de855c24967",
	"internal/services/catalog_structure_reader.go:loadStructureCatalogPages":                                "model:A=1,model:E=1,query:Find:A=2|870b833f1620d738",
	"internal/services/library_artwork.go:mediaCategoryCandidatesTx":                                         "model:R=1,query:Find:R=1,sql:E=1,sql:R=3|b35a9ac7762ce1e1",
	"internal/services/media_artifact.go:MediaArtifactService.generateArtifacts":                             "helper:generateArtifacts=1,model:A=1,model:E=2,model:R=1,query:Find:A=1,query:Find:E=1,query:Find:R=1,sql:E=1,sql:R=1|8a605be95587bfdb",
	"internal/services/media_artifact.go:MediaArtifactService.writeNFO":                                      "model:E=1,model:R=1|6c940e74308819d7",
	"internal/services/media_artifact.go:MediaArtifactService.writeSTRM":                                     "model:E=1|8ec546daccc85410",
	"internal/services/media_artifact.go:MediaArtifactService.writeSourceAsset":                              "model:A=1|72ecab2bca7060b9",
	"internal/services/media_artifact.go:MediaArtifactWorker.Run":                                            "call:generateArtifacts=1|11c809451fde239f",
	"internal/services/media_artifact.go:nfoRelativePath":                                                    "model:E=1|c619a5d73b232599",
	"internal/services/media_catalog.go:MediaLibraryService.aggregateCatalogTx":                              "sql:E=2|6f22d75fc1003f97",
	"internal/services/media_catalog.go:MediaLibraryService.catalogDetailTx":                                 "model:E=1,query:Find:E=1|b94ca59841b85e67",
	"internal/services/media_catalog.go:MediaLibraryService.catalogItemsTx":                                  "model:R=2,query:Find:R=1|55b4d98d55d6ea37",
	"internal/services/media_catalog.go:MediaLibraryService.catalogManagedTransfersTx":                       "model:E=1|8dcbcecdedac2f1d",
	"internal/services/media_catalog.go:MediaLibraryService.entryPageTx":                                     "model:E=1,query:Find:E=1|cad20008c83a5064",
	"internal/services/media_catalog.go:catalogCategories":                                                   "sql:E=3|3cf0519b20506290",
	"internal/services/media_catalog.go:catalogEpisode":                                                      "model:E=1|900014ad08f9a709",
	"internal/services/media_catalog.go:declaration(MediaLibraryEntryPage)":                                  "model:E=1|077712704199536b",
	"internal/services/media_catalog.go:resolvedCatalogEpisodeFacts":                                         "model:E=1|0ebc80e8d5222f4c",
	// The ordering helper receives only the caller's pinned VisibleEntries
	// query. Added-time ordering precedes pagination; no alternate table access.
	"internal/services/media_catalog.go:selectOrderedCatalogRows":                            "sql:E=3|93d7e8d0d4679938",
	"internal/services/media_catalog_deletion.go:MediaLibraryService.ConfirmCatalogDeletion": "model:E=2,model:R=1,query:Delete:E=1,query:Delete:R=1,query:Model:E=1|04f8cfc85a462e24",
	"internal/services/media_catalog_deletion.go:MediaLibraryService.catalogSTRMImpact":      "model:E=1|62be18822cbac2ec",
	"internal/services/media_catalog_deletion.go:catalogDeletionDigest":                      "model:E=2|4c87f12ec3ee85f4",
	"internal/services/media_catalog_deletion.go:catalogDeletionTitle":                       "model:E=1|9a2790a28c71bb78",
	"internal/services/media_catalog_deletion.go:deletionItemFromEntry":                      "model:E=1|11e81a65cfdacf18",
	"internal/services/media_catalog_deletion.go:snapshotMatchesEntries":                     "model:E=2|b5b8f520547d67d5",
	// Coverage uses authorized enabled library IDs, pins those heads and queries
	// VisibleEntries in the same query-only transaction. The single-work wrapper
	// retains its unlimited contract; bounded calendar consumers cannot infer
	// absence from a truncated prefix. No alternate anchor-table query is added.
	"internal/services/media_coverage.go:MediaCoverageService.coverageCatalog":     "model:E=1|2626cd586351a75f",
	"internal/services/media_coverage.go:MediaCoverageService.coverageCatalogMany": "model:E=3,query:Find:E=1|dc06a22b31bb17cf",
	// Calendar's only E reference groups already-authorized coverage facts. It
	// obtains those facts through coverageCatalogMany and preserves unknown when
	// coverage is incomplete or bounded; this function makes no E/R/A query.
	"internal/services/subscription_calendar.go:SubscriptionCalendarService.Read": "model:E=1|a7b5fda7e30f387a",
	"internal/services/media_coverage.go:coverageEntries":                         "model:E=2,query:Find:E=1|d971fc33bbc4812d",
	"internal/services/media_coverage.go:uniqueEntryLibraries":                    "model:E=1|6f3a02a90b57b2c6",
	"internal/services/media_library.go:MediaLibraryService.Entries":              "model:E=2,query:Find:E=1|05e191c97a84a7ed",
	"internal/services/media_library.go:MediaLibraryService.Update":               "model:A=1,model:E=1,model:R=1,query:Delete:A=1,query:Delete:E=1,query:Delete:R=1|664ebb2bb9cffcf5",
	// Quark enumeration is redirected to observation; verified outputs retain
	// versioned-versus-legacy dispatch and commit their receipt in the same tx.
	// The legacy E/R/A writes remain gated by requireLegacyCatalogWriteTx and
	// the current source fingerprint; versioned publication returns before them.
	"internal/services/media_library.go:MediaLibraryService.reconcile":                                       "call:mergeScopedPan115Catalog=1,call:publishFastPan115Scan=1,call:recognizeLibraryUnits=1,call:reconcileTMDBCollectionsTx=1,call:stabilizeExistingRecognitionUnits=1,model:A=4,model:E=5,model:R=3,query:Delete:A=2,query:Delete:E=2,query:Delete:R=1,query:Find:A=1,query:Find:E=1,query:First:R=1,query:Save:A=1,query:Save:E=1,query:Save:R=1|9caac6f3f4f92468",
	"internal/services/media_library.go:applyRecognitionEpisodeHints":                                        "model:E=1|7d763b9b7156f0fa",
	"internal/services/media_library.go:mediaLibraryEntryProjectionChanged":                                  "model:E=1|256c6390a86c4f76",
	"internal/services/media_library.go:mediaRecognitionProjectionChanged":                                   "model:R=1|67d93d5424dac75c",
	"internal/services/media_library_collections.go:reconcileTMDBCollectionsTx":                              "helper:reconcileTMDBCollectionsTx=1|ce2cba09571aaca5",
	"internal/services/media_library_events.go:MediaLibraryService.mergeScopedPan115Catalog":                 "helper:mergeScopedPan115Catalog=1,model:A=1,model:E=1,query:Find:A=1,query:Find:E=1|15c3ea763bb32a11",
	"internal/services/media_library_events.go:mergeScopedPan115CatalogFacts":                                "model:A=1,model:E=1|73fdeb5e2befa544",
	"internal/services/media_library_fast_scan.go:MediaLibraryRecognitionWorker.Run":                         "call:completeFastMediaLibraryRecognition=1|0c898379c9b5c395",
	"internal/services/media_library_fast_scan.go:MediaLibraryService.completeFastMediaLibraryRecognition":   "call:recognizeLibraryUnits=1,call:reconcileTMDBCollectionsTx=1,helper:completeFastMediaLibraryRecognition=1,model:E=1,model:R=3,query:Delete:R=1,query:Find:E=1,query:First:R=1,query:Save:R=1|a8386f3b4edf9db0",
	"internal/services/media_library_fast_scan.go:MediaLibraryService.publishFastPan115Scan":                 "call:deleteFastScanRows=5,call:reconcileTMDBCollectionsTx=2,call:stabilizeExistingRecognitionUnits=1,helper:publishFastPan115Scan=1,model:A=7,model:E=8,model:R=5,query:Delete:R=1,query:Find:A=1,query:Find:E=1,query:Find:R=1,query:Model:E=1,query:Model:R=1,query:Save:A=1,query:Save:E=1,sql:E=1,sql:R=1|4430a0ff24953133",
	"internal/services/media_library_fast_scan.go:deleteFastScanRows":                                        "helper:deleteFastScanRows=1|35d8a5e3d730359f",
	"internal/services/media_library_recognition.go:MediaLibraryService.recognizeLibraryUnits":               "helper:recognizeLibraryUnits=1,model:R=1,query:Find:R=1|ba309115a1d70b86",
	"internal/services/media_library_recognition.go:MediaLibraryService.recognizeLibraryUnitsWithExisting":   "model:R=2|e6247e4bc3c7391f",
	"internal/services/media_library_recognition.go:MediaLibraryService.stabilizeExistingRecognitionUnits":   "helper:stabilizeExistingRecognitionUnits=1,model:E=1,model:R=1,query:Find:E=1,query:Find:R=1|4de2fe18f6cc1ff6",
	"internal/services/media_library_recognition.go:mediaLibraryRecognitionProjectionFresh":                  "model:R=1|de5efecfa67b6d0a",
	"internal/services/media_library_recognition.go:recognitionResultFromStored":                             "model:R=1|2915bfd7ebd0e2ad",
	"internal/services/media_library_recognition.go:stabilizeRecognitionUnits":                               "model:E=1,model:R=2|14cfda952bd18d48",
	"internal/services/media_library_recognition_api.go:MediaLibraryService.Recognition":                     "model:R=1,query:First:R=1|8563560e32a2898b",
	"internal/services/media_library_recognition_api.go:MediaLibraryService.Recognitions":                    "model:R=1,query:Find:R=1|2abe0f7286f00ea6",
	"internal/services/media_library_recognition_api.go:MediaLibraryService.persistRecognitionResult":        "model:E=2,model:R=4,query:First:R=2,query:Model:E=2,query:Model:R=1,query:Where:R=5|4648d6f3bb410891",
	"internal/services/media_library_recognition_api.go:MediaLibraryService.recognitionContext":              "model:E=2,model:R=3,query:Find:E=1,query:First:R=1,query:Where:R=1|6eea99e6931a4477",
	"internal/services/media_library_recognition_api.go:MediaLibraryService.recognitionSummaryByID":          "model:R=1,query:First:R=1|b471b93065cc1960",
	"internal/services/media_library_recognition_api.go:MediaLibraryService.recognizeStoredUnit":             "model:E=1|8a687a8fd5310d81",
	"internal/services/media_library_recognition_api.go:recognitionPackageName":                              "model:E=1|a08ed012745b7140",
	"internal/services/media_library_recognition_api.go:recognitionSummaryTx":                                "model:E=1,model:R=1,query:First:E=1,query:Where:R=2|b10dcfc82679876c",
	"internal/services/media_library_retirement_cleanup.go:declaration(libraryRetirementCleanup)":            "sql:A=1,sql:E=1,sql:R=1|228465b2bde5f08f",
	"internal/services/media_library_structure.go:MediaLibraryStructureService.EnsureWorkLayout":             "call:updateStructureCatalogPaths=1|90e232604935b8f6",
	"internal/services/media_library_structure.go:MediaLibraryStructureService.RefreshRecognitionProjection": "model:E=1,model:R=1,query:First:R=1|b8c9c06a6d6b0789",
	// Cancellation/partial-result audit (2026-09-08): runRepair dispatches any
	// versioned state/head before reaching these original-table writes. Only
	// verified successes reach the legacy outcome transaction; failures are
	// observed unchanged before partial settlement. Global physical fences stay.
	"internal/services/media_library_structure.go:MediaLibraryStructureService.runRepair":         "call:removeStructureCatalogItems=1,call:updateStructureCatalogPaths=1|2c6c03c723644d87",
	"internal/services/media_library_structure.go:MediaLibraryStructureService.structureIssuesTx": "model:R=2,query:Find:R=1|ed44d1d7f35e2fb7",
	"internal/services/media_library_structure.go:removeStructureCatalogItems":                    "helper:removeStructureCatalogItems=1,model:A=1,model:E=1,query:Delete:A=1,query:Delete:E=1|a46b1f03b11411a5",
	"internal/services/media_library_structure.go:updateStructureCatalogPaths":                    "helper:updateStructureCatalogPaths=1,model:A=1,model:E=1,query:Model:A=1,query:Model:E=1|f2f4bad6fa3d9870",
	// Exact state-0 cancelled owner only: scan boundary + original/rebound
	// runtime/revision/source/epoch permit, read-only physical observations, then
	// replacement-identity validation and checked-subset writes/settlement in
	// one transaction. Versioned cancellation uses snapshot publication instead.
	"internal/services/media_library_structure_cancel.go:MediaLibraryStructureService.reconcileCancelledStructureRepair": "call:removeStructureCatalogItems=1,call:updateStructureCatalogPaths=1|b39afbabb81f994d",
	// Sole caller is the preceding legacy cancellation transaction. Reads only
	// provider ID, size and mtime for this library's frozen source/target paths;
	// rejects replacement rows before any update/delete. No E/A mutation here.
	"internal/services/media_library_structure_cancel.go:validateStructureOutcomeCatalogTx":         "sql:A=1,sql:E=1|3281272129eba74f",
	"internal/services/media_library_structure_planner.go:StructurePlanner.Build":                   "model:A=1,model:E=1|16bf7db06ea2fae4",
	"internal/services/media_library_structure_planner.go:StructurePlanner.BuildContext":            "model:A=2,model:E=2|b7fbbb6616be3a7b",
	"internal/services/media_library_structure_planner.go:buildStructureAssociationIndex":           "model:E=1|d8b6772b82d1faf7",
	"internal/services/media_library_structure_planner.go:buildStructureSidecarCandidate":           "model:A=1|c65154ecee3233b7",
	"internal/services/media_library_structure_planner.go:buildStructureVideoCandidate":             "model:E=1|35264025b31a1751",
	"internal/services/media_library_structure_planner.go:declaration(structurePlanTask)":           "model:A=1,model:E=1|cbfd460b45c00339",
	"internal/services/media_library_structure_planner.go:structureVideoTarget":                     "model:E=1|3b172c0e5d09170d",
	"internal/services/media_metadata_editor.go:MediaLibraryService.catalogMetadataContext":         "model:R=2,query:Find:R=1|065b7c0f82e9233e",
	"internal/services/media_metadata_editor.go:MediaLibraryService.metadataDocument":               "model:R=1|490956d8d162ba62",
	"internal/services/media_metadata_editor.go:MediaLibraryService.persistCatalogMetadataResults":  "model:E=2,model:R=2,query:Model:E=2,query:Model:R=1|85c03241ea25be9a",
	"internal/services/media_metadata_editor.go:declaration(catalogMetadataResult)":                 "model:R=1|173f561ed1ff80d7",
	"internal/services/media_reorganization.go:MediaReorganizationWorker.finalize":                  "model:E=1,query:Model:E=1|3fd1ce37b9255b1f",
	"internal/services/player_episode_metadata.go:MediaLibraryService.persistPlayerEpisodeMetadata": "model:R=3,query:First:R=1,query:Model:R=1|b6a650f120583280",
	"internal/services/player_episode_metadata.go:MediaLibraryService.playerEpisodeMetadata":        "model:E=1|fdaad04eb5b98014",
	"internal/services/player_episode_metadata.go:declaration(playerEpisodeMetadataSource)":         "model:R=1|011ce84d756b0f5f",
	"internal/services/player_episode_metadata.go:playerEpisodeFallbackTitle":                       "model:E=1|cac3f4d752666bbb",
	"internal/services/player_episode_metadata.go:playerEpisodeMetadataSourceTx":                    "sql:E=2,sql:R=2|5091846f083b8407",
	"internal/services/player_history.go:PlayerHistoryService.availableServerHistoryRows":           "model:E=2,query:Find:E=1|2cbfe87093fb5d2e",
	"internal/services/player_history.go:PlayerHistoryService.resolveServerHistoryAuthority":        "model:E=1,model:R=1,query:First:E=2,query:First:R=2|e71d59f0e86ee712",
	"internal/services/player_history.go:serverHistoryIdentityAvailable":                            "model:E=1|2d8685db986c6552",
	// Player adds Quark redirect capability and added-time projection; catalog
	// and recognition reads still use the authorized pinned visible catalog.
	"internal/services/player_media.go:MediaLibraryService.PlayerCatalogDetail":          "model:E=1,query:Find:E=1|c8e1de8a8158a64f",
	"internal/services/player_media.go:MediaLibraryService.playerLibrariesTx":            "sql:E=2|8ceb1c012c83f26d",
	"internal/services/player_media.go:MediaLibraryService.playerMediaItemTx":            "model:R=1,query:First:R=1,sql:E=2,sql:R=2|d3fd4b845ce5d157",
	"internal/services/player_media.go:SignedProxyService.playerStreamSource":            "sql:E=1|ca96ab7c34835e2e",
	"internal/services/player_media.go:declaration(playerStreamSource)":                  "model:E=1|9a865ce5819b45c3",
	"internal/services/player_media.go:openLocalPlayerEntry":                             "model:E=1,query:First:E=1|cee6db3368d9c331",
	"internal/services/player_media.go:openLocalPlayerEntrySource":                       "model:E=1|ce2377f4f0209f68",
	"internal/services/player_media.go:playerVersionLabels":                              "model:E=1|1f35a946532e1940",
	"internal/services/player_media_pages.go:PlayerMediaStateService.readableStateQuery": "sql:E=2|d754e106fdb58f48",
	"internal/services/transfer_deletion.go:TransferService.finalizeTransferDeletion":    "model:E=1,query:Delete:E=1|a7d0856f9a55d037",
}
