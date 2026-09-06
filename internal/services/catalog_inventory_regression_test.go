package services

import (
	"context"
	"net/http"
	"sync/atomic"
	"testing"

	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
)

func TestCatalogBatchClearReadsEffectiveManualOverrideNotRetainedAnchor(t *testing.T) {
	f, store, library := historySnapshotFixture(t)
	var record models.MediaLibraryRecognition
	if err := store.Read(context.Background(), []uint{library.ID}, func(reader *CatalogReader) error {
		return reader.Recognitions().Where("id=?", *f.movie[0].RecognitionID).First(&record).Error
	}); err != nil {
		t.Fatal(err)
	}
	record.ManualOverride = true
	candidate, token := catalogCandidate(t, store, library, "delta", 1)
	if err := store.AppendBatch(context.Background(), candidate.ID, token, CatalogFactBatch{Recognitions: []models.CatalogRecognitionFact{CatalogRecognitionFromLegacy(record)}}); err != nil {
		t.Fatal(err)
	}
	catalogPublish(t, store, candidate, token)
	var attempts atomic.Int64
	recognitionSnapshotClient(t, f.libraries, func(w http.ResponseWriter, _ *http.Request) {
		attempts.Add(1)
		http.Error(w, "unavailable", http.StatusServiceUnavailable)
	})
	if _, err := f.libraries.ClearCatalogRecognitionOverride(context.Background(), f.actor, library.ID, f.movieWork, RequestContext{}); err == nil {
		t.Fatal("batch clear silently skipped effective manual override because anchor was false")
	}
	if attempts.Load() == 0 {
		t.Fatal("effective manual entry never reached replacement recognition")
	}
	if err := store.Read(context.Background(), []uint{library.ID}, func(reader *CatalogReader) error {
		return reader.Recognitions().Where("id=?", record.ID).First(&record).Error
	}); err != nil || !record.ManualOverride {
		t.Fatalf("failed replacement cleared manual authority: %v", err)
	}
}
