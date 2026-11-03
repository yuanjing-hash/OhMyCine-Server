package services

import (
	"context"

	"testing"

	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
)

// Exercise the same actual-file preparation used by Follow, then the public
// Site acquisition path and its encrypted durable plan. A helper-only episode
// count would miss a plan that still contains the overlapping physical leaf.
type contentReviewRuntime struct{ checkpoint func(any) }

func (*contentReviewRuntime) Heartbeat(*float64, *int64, *int64, *float64, *int64) error { return nil }
func (r *contentReviewRuntime) Checkpoint(value any) error {
	r.checkpoint(value)
	return nil
}

func TestContentFollowDiscardPoppedPreparationAfterRouteIsRevoked(t *testing.T) {
	f, _ := newFollowPreviewFixture(t)
	claimed, err := f.follows.queue.Claim([]string{JobTypeFollowSearch})
	if err != nil || claimed == nil {
		t.Fatalf("claim=%+v err=%v", claimed, err)
	}
	changed := false
	runtime := &contentReviewRuntime{checkpoint: func(value any) {
		checkpoint, ok := value.(map[string]any)
		if !ok || checkpoint["stage"] != "submit" || changed {
			return
		}
		changed = true
		if err := f.follows.db.Model(&models.Downloader{}).Where("id = ?", "follow-preview-115").Update("enabled", false).Error; err != nil {
			t.Fatal(err)
		}
	}}
	f.worker.Run(context.Background(), runtime, *claimed)
	if !changed {
		t.Fatal("fixture did not reach the real post-prepare submit checkpoint")
	}
	var tasks int64
	f.follows.db.Model(&models.DownloadTask{}).Count(&tasks)
	if tasks != 0 {
		t.Fatal("a revoked route submitted a download")
	}
}
