package services

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
)

func TestProviderEventReviewProjectionSafePagedScoped(t *testing.T) {
	s, db, actor, storage, profile := mediaLibraryTestService(t)
	library, err := s.Create(context.Background(), actor, testLibraryInput("review", storage, profile, false), RequestContext{})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 52; i++ {
		raw, _ := json.Marshal(providerEventPayload{Kind: "deleted", ItemID: "private-provider", Name: "/private/root/episode.mkv"})
		row := models.MediaLibraryProviderEvent{LibraryID: library.ID, InboxEventID: uint(i + 1), PayloadJSON: string(raw), ResolutionCode: providerEventNeedsReview, ResolutionReason: "deletion_identity_unproven", SourceFingerprint: "private-fingerprint", CreatedAt: time.Now()}
		if err := db.Create(&row).Error; err != nil {
			t.Fatal(err)
		}
	}
	result, err := s.ProviderEventReviews(context.Background(), actor, library.ID, 2)
	if err != nil || result.Total != 52 || len(result.List) != 2 {
		t.Fatalf("page=%+v error=%v", result, err)
	}
	raw, _ := json.Marshal(result)
	for _, secret := range []string{"private-provider", "private-fingerprint", "/private/root"} {
		if strings.Contains(string(raw), secret) {
			t.Fatal("private data escaped")
		}
	}
	if result.List[0].Name != "episode.mkv" {
		t.Fatal("unsafe name")
	}
	if _, err := s.ProviderEventReviews(context.Background(), Actor{}, library.ID, 1); ErrorCode(err) != CodePermissionDenied {
		t.Fatal("read permission missing")
	}
	if _, err := s.ProviderEventReviews(context.Background(), actor, library.ID, 0); err == nil {
		t.Fatal("invalid page accepted")
	}
}
func TestProviderEventReviewNameRejectsSecrets(t *testing.T) {
	for _, name := range []string{"https://example.test/private?token=secret", "cookie=value\r\n", "/", ""} {
		if providerReviewName(name) != "名称不可用" {
			t.Fatal("unsafe name exposed")
		}
	}
	if strings.Contains(providerReviewReason("private-error"), "private-error") {
		t.Fatal("raw error exposed")
	}
}

func TestProviderEventReviewReasonsExplainPausedDeletion(t *testing.T) {
	for code, expected := range map[string]string{
		"deletion_event_order_unproven":     "无法确认先后顺序",
		"deletion_scope_too_large":          "超过本次安全处理范围",
		"deletion_artifact_source_unproven": "产物属于当前媒体库来源",
	} {
		if !strings.Contains(providerReviewReason(code), expected) {
			t.Fatalf("reason %s lacks actionable explanation", code)
		}
	}
}
