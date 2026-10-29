package services

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"

	"github.com/yuanjing-hash/OhMyCine-Server/internal/authz"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
)

func TestFollowSubtitlePolicyNormalizesAndPreservesLegacyIgnore(t *testing.T) {
	var old FollowExecutionSnapshot
	if err := json.Unmarshal([]byte(`{"version":2,"filters":{"min_seeders":1}}`), &old); err != nil {
		t.Fatal(err)
	}
	if effectiveSubtitlePolicy(old.Filters.Subtitles).Mode != "ignore" {
		t.Fatal("old snapshot changed behavior")
	}
	input := &SubtitlePolicy{Mode: "prefer", Languages: []string{"zh-CN", "zh-Hans", "zh-TW", "en"}}
	got, err := normalizeSubtitlePolicy(input)
	if err != nil || !reflect.DeepEqual(got.Languages, []string{"zh-Hans", "zh-Hant", "en"}) {
		t.Fatalf("policy=%+v err=%v", got, err)
	}
	if !reflect.DeepEqual(input.Languages, []string{"zh-CN", "zh-Hans", "zh-TW", "en"}) {
		t.Fatal("normalization mutated caller")
	}
	for _, bad := range []*SubtitlePolicy{{Mode: "unknown", Languages: []string{"zh-Hans"}}, {Mode: "require"}, {Mode: "prefer", Languages: []string{"secret://token"}}, {Mode: "prefer", Languages: make([]string, 9)}} {
		if _, err := normalizeSubtitlePolicy(bad); err == nil {
			t.Fatalf("accepted invalid %+v", bad)
		}
	}
}

func TestFollowPolicyDefaultsAreDurableCASAndDoNotRewriteSnapshots(t *testing.T) {
	queue, actor, _ := queueFixture(t)
	actor.Permissions[authz.PermissionSettingsRead] = struct{}{}
	actor.Permissions[authz.PermissionSettingsUpdate] = struct{}{}
	service := NewFollowService(queue.db, queue.audit, queue, nil, nil)
	first, err := service.PolicyDefaults(context.Background(), actor)
	if err != nil || first.Revision != 1 || first.Subtitles.Mode != "prefer" || !reflect.DeepEqual(first.Subtitles.Languages, []string{"zh-Hans", "zh-Hant"}) {
		t.Fatalf("defaults=%+v err=%v", first, err)
	}
	record := models.FollowSubscription{ID: "legacy-subtitles", OwnerID: actor.User.ID, MediaType: "tv", TMDBID: 10, Title: "Legacy", Status: models.FollowStatusActive, Revision: 1, ExecutionSnapshotJSON: `{"version":2,"filters":{"min_seeders":1}}`}
	if err := queue.db.Create(&record).Error; err != nil {
		t.Fatal(err)
	}
	changed, err := service.UpdatePolicyDefaults(context.Background(), actor, UpdateFollowPolicyDefaultsInput{Revision: first.Revision, Subtitles: SubtitlePolicy{Mode: "require", Languages: []string{"en"}}}, RequestContext{})
	if err != nil || changed.Revision != 2 {
		t.Fatalf("updated=%+v err=%v", changed, err)
	}
	if _, err := service.UpdatePolicyDefaults(context.Background(), actor, UpdateFollowPolicyDefaultsInput{Revision: 1, Subtitles: SubtitlePolicy{Mode: "ignore"}}, RequestContext{}); ErrorCode(err) != CodeConflict {
		t.Fatalf("stale revision=%v", err)
	}
	reloaded := NewFollowService(queue.db, queue.audit, queue, nil, nil)
	saved, err := reloaded.PolicyDefaults(context.Background(), actor)
	if err != nil || saved.Subtitles.Mode != "require" {
		t.Fatal("defaults not durable")
	}
	var original models.FollowSubscription
	if err := queue.db.First(&original, "id = ?", record.ID).Error; err != nil || original.ExecutionSnapshotJSON != record.ExecutionSnapshotJSON {
		t.Fatal("global defaults rewrote old snapshot")
	}
	actor.Permissions = map[string]struct{}{}
	if _, err := service.PolicyDefaults(context.Background(), actor); ErrorCode(err) != CodePermissionDenied {
		t.Fatal("settings read bypass")
	}
	if _, err := service.UpdatePolicyDefaults(context.Background(), actor, UpdateFollowPolicyDefaultsInput{Revision: 2, Subtitles: SubtitlePolicy{Mode: "ignore"}}, RequestContext{}); ErrorCode(err) != CodePermissionDenied {
		t.Fatal("settings update bypass")
	}
}
