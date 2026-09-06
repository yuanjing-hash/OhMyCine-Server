package services

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/yuanjing-hash/OhMyCine-Server/internal/classification"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/metadata/tmdb"
	"gorm.io/gorm"
)

func recognitionSnapshotClient(t *testing.T, service *MediaLibraryService, handler http.HandlerFunc) {
	t.Helper()
	upstream := httptest.NewServer(handler)
	t.Cleanup(upstream.Close)
	client, err := tmdb.NewForTest("test-token", upstream.URL, upstream.Client())
	if err != nil {
		t.Fatal(err)
	}
	metadata := NewMetadataSettingsService(service.db, NewAuditService(service.db), nil, tmdb.Credential{Kind: tmdb.CredentialKindReadAccessToken, Value: "deployment-token"})
	metadata.clientFactory = func(tmdb.Credential, string, string) (*tmdb.Client, error) { return client, nil }
	service.SetMetadataSettingsService(metadata)
}

func TestCatalogRecognitionValidationUsesExactHeadWithoutEntryRescan(t *testing.T) {
	f, store, library := historySnapshotFixture(t)
	records, source, err := f.libraries.catalogMetadataContext(context.Background(), library.ID, f.movieWork)
	if err != nil {
		t.Fatal(err)
	}
	entryQueries := 0
	callback := "test:recognition_membership_query"
	if err := store.writeDB.Callback().Query().After("gorm:query").Register(callback, func(tx *gorm.DB) {
		if strings.Contains(tx.Statement.SQL.String(), "catalog_entry_facts") {
			entryQueries++
		}
	}); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.writeDB.Callback().Query().Remove(callback) }()
	validate := func() error {
		return store.writeDB.Transaction(func(tx *gorm.DB) error {
			reader, err := PinCatalogTx(tx, []uint{library.ID})
			if err != nil {
				return err
			}
			return validateCatalogRecognitionContext(tx, reader, source, records)
		})
	}
	if err := validate(); err != nil {
		t.Fatal(err)
	}
	if entryQueries != 0 {
		t.Fatalf("writer rescanned entry membership: %d", entryQueries)
	}
	c, token := catalogCandidate(t, store, library, "delta", source.Head.Revision)
	changed := records[0]
	changed.Title = "Concurrent correction"
	if err := store.AppendBatch(context.Background(), c.ID, token, CatalogFactBatch{Recognitions: []models.CatalogRecognitionFact{CatalogRecognitionFromLegacy(changed)}}); err != nil {
		t.Fatal(err)
	}
	catalogPublish(t, store, c, token)
	if err := validate(); !errors.Is(err, ErrCatalogFence) {
		t.Fatalf("stale head accepted: %v", err)
	}
}

func TestCatalogRecognitionManualDeltaPreservesFileOverridesAndExactSummary(t *testing.T) {
	f, store, library := historySnapshotFixture(t)
	var record models.MediaLibraryRecognition
	if err := store.Read(context.Background(), []uint{library.ID}, func(r *CatalogReader) error {
		return r.Recognitions().First(&record, *f.episodes[0].RecognitionID).Error
	}); err != nil {
		t.Fatal(err)
	}
	c, token := catalogCandidate(t, store, library, "delta", 1)
	custom := f.episodes[0]
	custom.Title, custom.CategoryName = "分集标题覆盖", "文件分类覆盖"
	if err := store.AppendBatch(context.Background(), c.ID, token, CatalogFactBatch{Entries: []models.CatalogEntryFact{CatalogEntryFromLegacy(custom, &record)}}); err != nil {
		t.Fatal(err)
	}
	catalogPublish(t, store, c, token)
	recognitionSnapshotClient(t, f.libraries, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/tv/42" {
			_, _ = w.Write([]byte(`{"id":42,"name":"正确剧名","original_language":"zh","first_air_date":"2024-01-01","genres":[{"id":18}]}`))
			return
		}
		http.NotFound(w, r)
	})
	result, err := f.libraries.OverrideRecognition(context.Background(), f.actor, library.ID, encodeRecognitionToken(record.ID), MediaRecognitionOverrideInput{TMDBID: 42, MediaType: "tv"}, RequestContext{})
	if err != nil || result.Title != "正确剧名" || !result.ManualOverride || result.FileCount != 3 || result.SourceDirectory != "Series" {
		t.Fatalf("manual result=%+v err=%v", result, err)
	}
	exact, err := f.libraries.Recognition(context.Background(), f.actor, library.ID, result.Token)
	if err != nil || exact.Title != result.Title || exact.TMDBID == nil || *exact.TMDBID != 42 {
		t.Fatalf("exact summary=%+v err=%v", exact, err)
	}
	page, err := f.libraries.Recognitions(f.actor, library.ID, MediaPageQuery{Page: 1, PageSize: 20}, "matched", true)
	if err != nil || page.Total != 1 || page.List[0].Token != result.Token {
		t.Fatalf("manual page=%+v err=%v", page, err)
	}
	rows := catalogReadEntries(t, store, library.ID, "work_key = ?", "series:tmdb:42")
	if len(rows) != 3 || rows[0].ID != custom.ID || rows[0].Title != "分集标题覆盖" || rows[0].CategoryName != "文件分类覆盖" || rows[0].Season == nil || *rows[0].Season != 1 || rows[0].Episode == nil || *rows[0].Episode != 1 || rows[1].Title != "正确剧名" {
		t.Fatalf("entry overrides=%+v", rows)
	}
	var entryFacts, recFacts int64
	if err := store.writeDB.Model(&models.CatalogEntryFact{}).Where("library_id = ?", library.ID).Count(&entryFacts).Error; err != nil {
		t.Fatal(err)
	}
	if err := store.writeDB.Model(&models.CatalogRecognitionFact{}).Where("library_id = ?", library.ID).Count(&recFacts).Error; err != nil {
		t.Fatal(err)
	}
	if entryFacts != 6 || recFacts != 3 {
		t.Fatalf("manual write amplified across files: entry=%d recognition=%d", entryFacts, recFacts)
	}
	var anchor models.MediaLibraryRecognition
	if err := store.writeDB.First(&anchor, record.ID).Error; err != nil || anchor.Title != "stale anchor" {
		t.Fatalf("anchor mutated=%+v err=%v", anchor, err)
	}
}

func TestCatalogRecognitionNetworkSourceDriftRefusesSaveWithoutHoldingWriter(t *testing.T) {
	f, store, library := historySnapshotFixture(t)
	var mutateErr error
	recognitionSnapshotClient(t, f.libraries, func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		mutateErr = store.writeDB.WithContext(ctx).Model(&models.MediaLibrary{}).Where("id = ?", library.ID).Update("relative_root", "/replacement").Error
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":42,"title":"Wrong source result","release_date":"2024-01-01"}`))
	})
	_, err := f.libraries.OverrideRecognition(context.Background(), f.actor, library.ID, encodeRecognitionToken(*f.movie[0].RecognitionID), MediaRecognitionOverrideInput{TMDBID: 42, MediaType: "movie"}, RequestContext{})
	if mutateErr != nil || ErrorCode(err) != CodeConflict {
		t.Fatalf("network was locked or stale result accepted: mutation=%v save=%v", mutateErr, err)
	}
	rows := catalogReadEntries(t, store, library.ID, "work_key = ?", "movie:tmdb:200")
	if len(rows) != 2 || rows[0].Title != "权威电影" {
		t.Fatalf("stale result changed catalog=%+v", rows)
	}
}

func TestCatalogRecognitionMetadataEditUsesSnapshotAndRejectsStaleManualAuthority(t *testing.T) {
	f, store, library := historySnapshotFixture(t)
	document, err := f.libraries.CatalogMetadata(context.Background(), f.actor, library.ID, f.movieWork)
	if err != nil || document.Editable.Title != "权威电影" {
		t.Fatalf("document=%+v err=%v", document, err)
	}
	stale, source, _, err := f.libraries.recognitionContext(context.Background(), library.ID, encodeRecognitionToken(*f.movie[0].RecognitionID), false)
	if err != nil {
		t.Fatal(err)
	}
	document.Editable.Title = "手工修订电影"
	document.Editable.Overview = "编辑简介"
	saved, err := f.libraries.UpdateCatalogMetadata(context.Background(), f.actor, library.ID, f.movieWork, MediaMetadataUpdateInput{Revision: document.Revision, Editable: document.Editable}, RequestContext{})
	if err != nil || !saved.ManualOverride || saved.Editable.Title != "手工修订电影" || saved.Revision == document.Revision {
		t.Fatalf("save=%+v err=%v", saved, err)
	}
	result := MediaRecognitionResult{Status: "matched", MediaType: "movie", Title: "迟到自动识别", TMDBID: stale.TMDBID, Snapshot: tmdb.Snapshot{Version: 1, MediaType: "movie", TMDBID: *stale.TMDBID, Title: "迟到自动识别"}}
	if err := f.libraries.persistRecognitionResult(stale, source.Profile, result, false, source); ErrorCode(err) != CodeConflict {
		t.Fatalf("stale automatic overwrite=%v", err)
	}
	rows := catalogReadEntries(t, store, library.ID, "work_key = ?", "movie:tmdb:200")
	if len(rows) != 2 || rows[0].Title != "手工修订电影" {
		t.Fatalf("manual authority lost=%+v", rows)
	}
	if _, err := f.libraries.UpdateCatalogMetadata(context.Background(), f.actor, library.ID, f.movieWork, MediaMetadataUpdateInput{Revision: document.Revision, Editable: document.Editable}, RequestContext{}); ErrorCode(err) != CodeConflict {
		t.Fatalf("stale editor revision=%v", err)
	}
}

func TestCatalogRecognitionDeltaBudgetAndGuardFailureNeverAcknowledgeSave(t *testing.T) {
	f, store, library := historySnapshotFixture(t)
	record, source, _, err := f.libraries.recognitionContext(context.Background(), library.ID, encodeRecognitionToken(*f.movie[0].RecognitionID), false)
	if err != nil {
		t.Fatal(err)
	}
	record.MetadataJSON = `{"large":"` + strings.Repeat("x", CatalogBatchBytes) + `"}`
	if err := f.libraries.publishCatalogRecognitionDelta(context.Background(), source.Head, []models.MediaLibraryRecognition{record}, func(*gorm.DB, *CatalogReader) error { return nil }, func(*gorm.DB) error { return nil }); !errors.Is(err, ErrCatalogBudget) {
		t.Fatalf("oversized delta=%v", err)
	}
	record.MetadataJSON = "{}"
	if err := f.libraries.publishCatalogRecognitionDelta(context.Background(), source.Head, []models.MediaLibraryRecognition{record}, func(*gorm.DB, *CatalogReader) error { return ErrCatalogFence }, func(*gorm.DB) error { t.Fatal("semantic fence failure reached commit"); return nil }); !errors.Is(err, ErrCatalogFence) {
		t.Fatalf("guard failure=%v", err)
	}
	var head models.CatalogHead
	if err := store.writeDB.First(&head, "library_id = ?", library.ID).Error; err != nil || head.Revision != 1 {
		t.Fatalf("failed save advanced head=%+v err=%v", head, err)
	}
	var abandoned int64
	if err := store.writeDB.Model(&models.CatalogSnapshot{}).Where("library_id = ? AND state = ?", library.ID, "abandoned").Count(&abandoned).Error; err != nil || abandoned != 1 {
		t.Fatalf("failed candidate not abandoned=%d err=%v", abandoned, err)
	}
}

func TestCatalogRecognitionClearOverrideKeepsManualOnUnavailableLookup(t *testing.T) {
	f, store, library := historySnapshotFixture(t)
	record, source, _, err := f.libraries.recognitionContext(context.Background(), library.ID, encodeRecognitionToken(*f.movie[0].RecognitionID), false)
	if err != nil {
		t.Fatal(err)
	}
	result, err := recognitionResultFromStored(record, classification.RulesV1{})
	if err != nil {
		t.Fatal(err)
	}
	if err := f.libraries.persistRecognitionResult(record, source.Profile, result, true, source); err != nil {
		t.Fatal(err)
	}
	recognitionSnapshotClient(t, f.libraries, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusServiceUnavailable) })
	_, err = f.libraries.ClearRecognitionOverride(context.Background(), f.actor, library.ID, encodeRecognitionToken(record.ID), RequestContext{})
	if err == nil {
		t.Fatal("unavailable lookup unexpectedly cleared manual authority")
	}
	if err := store.Read(context.Background(), []uint{library.ID}, func(r *CatalogReader) error { return r.Recognitions().First(&record, record.ID).Error }); err != nil {
		t.Fatal(err)
	}
	if !record.ManualOverride || record.Title != "权威电影" {
		t.Fatalf("failed clear lost manual result=%+v", record)
	}
}

func TestCatalogRecognitionDeltaChainBoundReturnsSafeConflict(t *testing.T) {
	f, store, library := historySnapshotFixture(t)
	for i := 0; i <= CatalogMaxDeltas; i++ {
		record, source, _, err := f.libraries.recognitionContext(context.Background(), library.ID, encodeRecognitionToken(*f.movie[0].RecognitionID), false)
		if err != nil {
			t.Fatal(err)
		}
		result, err := recognitionResultFromStored(record, classification.RulesV1{})
		if err != nil {
			t.Fatal(err)
		}
		result.Title += "修"
		result.Snapshot.Title = result.Title
		err = f.libraries.persistRecognitionResult(record, source.Profile, result, true, source)
		if i < CatalogMaxDeltas && err != nil {
			t.Fatalf("delta %d: %v", i, err)
		}
		if i == CatalogMaxDeltas && ErrorCode(err) != CodeConflict {
			t.Fatalf("hard bound did not return retryable conflict: %v", err)
		}
	}
	var head models.CatalogHead
	if err := store.writeDB.First(&head, "library_id = ?", library.ID).Error; err != nil || head.Revision != CatalogMaxDeltas+1 {
		t.Fatalf("head=%+v err=%v", head, err)
	}
	rows := catalogReadEntries(t, store, library.ID, "work_key = ?", "movie:tmdb:200")
	if len(rows) != 2 || rows[0].Title != "权威电影"+strings.Repeat("修", CatalogMaxDeltas) {
		t.Fatalf("rejected save leaked into catalog=%+v", rows)
	}
}

func TestCatalogRecognitionMetadataBatchCASRollsBackAllRecognitions(t *testing.T) {
	f, store, library := historySnapshotFixture(t)
	source, err := f.libraries.captureRecognitionWriteContext(library.ID)
	if err != nil {
		t.Fatal(err)
	}
	var records []models.MediaLibraryRecognition
	if err := store.Read(context.Background(), []uint{library.ID}, func(r *CatalogReader) error { return r.Recognitions().Order("id").Find(&records).Error }); err != nil {
		t.Fatal(err)
	}
	updates := make([]catalogMetadataResult, 0, len(records))
	for _, record := range records {
		result, err := recognitionResultFromStored(record, classification.RulesV1{})
		if err != nil {
			t.Fatal(err)
		}
		result.Title, result.Snapshot.Title = "不该部分保存", "不该部分保存"
		updates = append(updates, catalogMetadataResult{Record: record, Profile: source.Profile, Result: result})
	}
	updates[1].Record.InputFingerprint = "stale-input"
	if err := f.libraries.persistCatalogMetadataResults(updates, source); ErrorCode(err) != CodeConflict {
		t.Fatalf("stale member accepted=%v", err)
	}
	var current []models.MediaLibraryRecognition
	if err := store.Read(context.Background(), []uint{library.ID}, func(r *CatalogReader) error { return r.Recognitions().Order("id").Find(&current).Error }); err != nil {
		t.Fatal(err)
	}
	if len(current) != len(records) || current[0].Title != records[0].Title || current[1].Title != records[1].Title {
		t.Fatalf("partial edit escaped=%+v", current)
	}
}
