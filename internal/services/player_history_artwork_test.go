package services

import (
	"bytes"
	"context"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"strings"
	"testing"
	"time"

	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
)

func TestHistoryArtworkPreservesPNGAlphaAndAcceptsJPEG(t *testing.T) {
	source := image.NewNRGBA(image.Rect(0, 0, 2, 2))
	source.SetNRGBA(0, 0, color.NRGBA{R: 200, G: 100, B: 40, A: 64})
	for _, format := range []string{"png", "jpeg"} {
		var input bytes.Buffer
		var err error
		if format == "png" {
			err = png.Encode(&input, source)
		} else {
			err = jpeg.Encode(&input, source, nil)
		}
		if err != nil {
			t.Fatal(err)
		}
		body, err := normalizeHistoryArtwork(context.Background(), "image/"+format, bytes.NewReader(input.Bytes()))
		if err != nil {
			t.Fatal(err)
		}
		decoded, got, err := image.Decode(bytes.NewReader(body))
		if err != nil || got != format {
			t.Fatalf("format=%s error=%v", got, err)
		}
		if format == "png" {
			_, _, _, alpha := decoded.At(0, 0).RGBA()
			if alpha != 64*257 {
				t.Fatalf("lost alpha=%d", alpha)
			}
		}
	}
}

func historyArtworkPNG(t *testing.T, width, height int) []byte {
	t.Helper()
	var out bytes.Buffer
	if err := png.Encode(&out, image.NewRGBA(image.Rect(0, 0, width, height))); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}

func TestHistoryArtworkOwnershipReplacementSyncAndCleanup(t *testing.T) {
	queue, actor, _ := queueFixture(t)
	s := NewPlayerHistoryService(queue.db)
	ctx := context.Background()
	change := PlayerHistoryChange{SyncKey: strings.Repeat("c", 64), SourceKind: "emby", SourceID: "bedroom", MediaIdentity: "movie:1", Title: "External", Position: 20, UpdatedAt: 1000}
	if _, err := s.Sync(actor, 0, []PlayerHistoryChange{change}); err != nil {
		t.Fatal(err)
	}
	data := historyArtworkPNG(t, 16, 24)
	first, err := s.PutArtwork(ctx, actor, change.SyncKey, "poster", "image/png", bytes.NewReader(data))
	if err != nil || first.AssetID == "" || first.Revision == 0 {
		t.Fatalf("receipt=%+v err=%v", first, err)
	}
	asset, err := s.Artwork(ctx, actor, first.AssetID)
	if err != nil {
		t.Fatal(err)
	}
	if _, format, err := image.Decode(bytes.NewReader(asset)); err != nil || format != "png" {
		t.Fatalf("not canonical PNG: %s %v", format, err)
	}
	foreign := actor
	foreign.User.ID += 999
	if _, err := s.Artwork(ctx, foreign, first.AssetID); ErrorCode(err) != CodeNotFound {
		t.Fatalf("foreign read: %v", err)
	}
	if _, err := s.PutArtwork(ctx, foreign, change.SyncKey, "poster", "image/png", bytes.NewReader(data)); ErrorCode(err) != CodeNotFound {
		t.Fatalf("foreign write: %v", err)
	}
	if _, err := s.PutArtwork(ctx, actor, change.SyncKey, "poster", "image/png", strings.NewReader("bad")); ErrorCode(err) != CodeHistoryArtworkInvalid {
		t.Fatalf("invalid: %v", err)
	}
	if _, err := s.Artwork(ctx, actor, first.AssetID); err != nil {
		t.Fatal("validation removed old asset", err)
	}
	// Old clients uploading progress cannot clear or forge asset associations.
	change.PosterAssetID, change.UpdatedAt = "forged", 2000
	result, err := s.Sync(actor, 0, []PlayerHistoryChange{change})
	if err != nil || len(result.Changes) != 1 || result.Changes[0].PosterAssetID != first.AssetID {
		t.Fatalf("asset lost: %+v %v", result, err)
	}
	browser, err := s.BrowserList(actor, 1, 10)
	if err != nil || len(browser.List) != 1 || browser.List[0].PosterURL != browserHistoryArtworkURL(first.AssetID) {
		t.Fatalf("browser=%+v %v", browser, err)
	}
	second, err := s.PutArtwork(ctx, actor, change.SyncKey, "poster", "image/png", bytes.NewReader(data))
	if err != nil || second.AssetID == first.AssetID {
		t.Fatal(second, err)
	}
	if _, err := s.Artwork(ctx, actor, first.AssetID); ErrorCode(err) != CodeNotFound {
		t.Fatalf("replaced asset readable: %v", err)
	}
	change.Deleted, change.UpdatedAt = true, 3000
	if _, err := s.Sync(actor, 0, []PlayerHistoryChange{change}); err != nil {
		t.Fatal(err)
	}
	var count int64
	if err := queue.db.Model(&models.PlayerHistoryArtwork{}).Count(&count).Error; err != nil || count != 0 {
		t.Fatalf("orphan count=%d err=%v", count, err)
	}
	if _, err := s.Artwork(ctx, actor, second.AssetID); ErrorCode(err) != CodeNotFound {
		t.Fatalf("deleted read=%v", err)
	}
}

func TestHistoryArtworkBoundsAndQuotaPreserveOldAsset(t *testing.T) {
	ctx := context.Background()
	for _, sample := range []struct {
		contentType string
		data        []byte
	}{
		{"image/svg+xml", []byte("<svg/>")},
		{"image/jpeg", historyArtworkPNG(t, 1, 1)},
		{"image/png", make([]byte, HistoryArtworkMaxBytes+1)},
		{"image/png", historyArtworkPNG(t, 4001, 4000)},
	} {
		if _, err := normalizeHistoryArtwork(ctx, sample.contentType, bytes.NewReader(sample.data)); ErrorCode(err) != CodeHistoryArtworkInvalid {
			t.Fatalf("bounds error=%v", err)
		}
	}
	queue, actor, _ := queueFixture(t)
	s := NewPlayerHistoryService(queue.db)
	change := PlayerHistoryChange{SyncKey: strings.Repeat("d", 64), SourceKind: "local", SourceID: "a", MediaIdentity: "1", Title: "Local", UpdatedAt: 1000}
	if _, err := s.Sync(actor, 0, []PlayerHistoryChange{change}); err != nil {
		t.Fatal(err)
	}
	data := historyArtworkPNG(t, 1, 1)
	first, err := s.PutArtwork(ctx, actor, change.SyncKey, "poster", "image/png", bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	// Exhaust the byte budget using a SQL BLOB, without allocating 64 MiB in Go.
	if err := queue.db.Exec("INSERT INTO player_history_artworks (id,user_id,sync_key,slot,body,created_at) VALUES ('quota',?,?,'backdrop',zeroblob(?),?)", actor.User.ID, change.SyncKey, historyArtworkQuotaBytes, time.Now().UTC()).Error; err != nil {
		t.Fatal(err)
	}
	if _, err := s.PutArtwork(ctx, actor, change.SyncKey, "poster", "image/png", bytes.NewReader(data)); ErrorCode(err) != CodeHistoryArtworkQuota {
		t.Fatalf("quota=%v", err)
	}
	if _, err := s.Artwork(ctx, actor, first.AssetID); err != nil {
		t.Fatal("quota removed old asset", err)
	}
}
