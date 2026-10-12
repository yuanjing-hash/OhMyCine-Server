package services

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/yuanjing-hash/OhMyCine-Server/internal/authz"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/plugins/contract"
)

func onlineHistoryInput(id, segment, version string, position float64) PlayerHistoryChange {
	token := onlineHistoryToken(id, "work:1", segment, version)
	return PlayerHistoryChange{SyncKey: strings.Repeat("d", 64), SourceKind: "server", SourceID: "client-source", LibraryID: "online-library|" + id, ItemToken: token, ItemID: token, MediaIdentity: token, Title: "forged title", DisplaySubtitle: "forged subtitle", PosterURL: "https://private.invalid/image?token=secret", EpisodeNumber: intPointer(99), Position: position, Duration: floatPointer(1000), UpdatedAt: time.Now().UnixMilli()}
}

func TestOnlineHistorySuccessfulPlaybackAuthorityUserRowsAndContinue(t *testing.T) {
	s, actor, id, r := catalogueFixture(t)
	history := NewPlayerHistoryService(s.db)
	history.SetPluginService(s)
	ctx := context.Background()
	if !s.OnlineHistoryAvailable() {
		t.Fatal("production bridge not advertised")
	}
	libraries, err := s.OnlineLibraries(actor)
	mustCatalogue(t, err)
	if len(libraries) != 1 || !libraries[0].SystemHistorySupported {
		t.Fatal("summary support not wired")
	}
	denied := onlineHistoryInput(id, "special:1", "bonus", 0)
	if _, err := history.SyncContext(ctx, actor, 0, []PlayerHistoryChange{denied}); ErrorCode(err) != CodeInvalidRequest {
		t.Fatal("failed/unstarted playback created watched history")
	}
	if _, err = s.OnlinePlayback(ctx, actor, id, "work:1", "special:1", "bonus", "qn:80"); err != nil {
		t.Fatal(err)
	}
	ack, err := s.SyncOnlineProgress(ctx, actor, id, "work:1", "special:1", "bonus", "started", 0, floatPointer(1000), "fixture-start", time.Now().Format(time.RFC3339Nano))
	mustCatalogue(t, err)
	if string(ack) != `{"accepted":true,"remote":false}` || r.count("playback.progress_sync") != 0 {
		t.Fatal("system progress invoked unsupported provider write")
	}
	denied.Position = 100
	denied.UpdatedAt = time.Now().Add(time.Millisecond).UnixMilli()
	sync, err := history.SyncContext(ctx, actor, 0, []PlayerHistoryChange{denied})
	mustCatalogue(t, err)
	if len(sync.Changes) != 1 {
		t.Fatalf("canonical delta count=%d", len(sync.Changes))
	}
	row := sync.Changes[0]
	if row.ItemToken != denied.ItemToken || row.HistoryIdentity != denied.ItemToken || row.SyncKey != playerHistoryCanonicalSyncKey(denied.ItemToken) || row.Title != "官方作品" || row.DisplaySubtitle != "第1期彩蛋 · 彩蛋" || row.EpisodeNumber != nil || row.MediaType != "episode" || !strings.HasPrefix(row.PosterURL, "/api/v1/player/artwork/") {
		t.Fatalf("authority metadata mismatch: %+v", row)
	}
	page, err := history.List(actor, 1, 24, "server")
	mustCatalogue(t, err)
	if page.Total != 1 || len(page.List) != 1 || page.List[0].Position != 100 {
		t.Fatal("online canonical row absent from current Player history")
	}
	continuing, _, err := history.ServerContinueWatching(actor, 24, map[uint]struct{}{})
	mustCatalogue(t, err)
	if len(continuing) != 1 || continuing[0].ItemToken != denied.ItemToken {
		t.Fatal("Player continue watching omitted online identity")
	}
	browser, err := history.BrowserList(actor, 1, 24)
	mustCatalogue(t, err)
	if len(browser.List) != 0 {
		t.Fatal("physical browser DTO received fake online entry")
	}
	foreign := Actor{User: models.User{ID: actor.User.ID + 99}, Permissions: map[string]struct{}{authz.PermissionMediaLibrariesRead: {}}}
	page, err = history.List(foreign, 1, 24, "server")
	mustCatalogue(t, err)
	if len(page.List) != 0 {
		t.Fatal("other user's online history leaked")
	}
	delete(actor.Permissions, authz.PermissionMediaLibrariesRead)
	page, err = history.List(actor, 1, 24, "server")
	mustCatalogue(t, err)
	if len(page.List) != 0 {
		t.Fatal("revoked online read still showed history")
	}
}

func TestOnlineHistoryExactEditionOfflineSyncTombstoneAndScopeRefresh(t *testing.T) {
	s, actor, id, r := catalogueFixture(t)
	history := NewPlayerHistoryService(s.db)
	history.SetPluginService(s)
	ctx := context.Background()
	main := onlineHistoryInput(id, "main:1", "full", 100)
	special := onlineHistoryInput(id, "special:1", "bonus", 120)
	special.SyncKey = strings.Repeat("e", 64)
	result, err := history.SyncContext(ctx, actor, 0, []PlayerHistoryChange{main, special})
	mustCatalogue(t, err)
	if len(result.Changes) != 2 {
		t.Fatal("same ordinal collapsed distinct versions")
	}
	page, err := history.List(actor, 1, 24, "server")
	mustCatalogue(t, err)
	if page.Total != 2 {
		t.Fatal("positive local offline facts not accepted")
	}
	for _, row := range page.List {
		if row.ItemToken == main.ItemToken && (row.EpisodeNumber == nil || *row.EpisodeNumber != 1) {
			t.Fatal("official episode fact lost")
		}
		if row.ItemToken == special.ItemToken && row.EpisodeNumber != nil {
			t.Fatal("special guessed episode fact")
		}
	}
	forged := onlineHistoryInput(id, "absent", "bonus", 100)
	forged.SyncKey = strings.Repeat("f", 64)
	if _, err = history.SyncContext(ctx, actor, 0, []PlayerHistoryChange{forged}); ErrorCode(err) != CodeInvalidRequest {
		t.Fatal("forged segment accepted")
	}
	before := r.count("site.detail")
	mustCatalogue(t, s.db.Model(&models.PluginConnection{}).Where("id = ?", id).Update("credential_version", 2).Error)
	main.UpdatedAt += 10
	main.Position = 150
	_, err = history.SyncContext(ctx, actor, result.Cursor, []PlayerHistoryChange{main})
	mustCatalogue(t, err)
	if r.count("site.detail") != before+1 {
		t.Fatal("credential revision failed to refresh membership proof")
	}
	// Published package changes renew authority, preserving completed user facts.
	var pkg models.PluginPackage
	mustCatalogue(t, s.db.First(&pkg, "plugin_id = ?", "org.ohmycine.catalogue-test").Error)
	updatedManifest := strings.Replace(pkg.ManifestJSON, strings.Repeat("a", 64), strings.Repeat("b", 64), 1)
	mustCatalogue(t, s.db.Model(&pkg).Updates(map[string]any{"package_sha256": strings.Repeat("b", 64), "manifest_json": updatedManifest}).Error)
	special.UpdatedAt += 20
	special.Completed = true
	special.Position = 1000
	_, err = history.SyncContext(ctx, actor, 0, []PlayerHistoryChange{special})
	mustCatalogue(t, err)
	mustCatalogue(t, s.db.Where("library_id = ?", id).Delete(&models.PluginOnlineMediaIdentity{}).Error)
	page, err = history.List(actor, 1, 24, "server")
	mustCatalogue(t, err)
	if page.Total != 2 {
		t.Fatal("proof expiry erased user history")
	}
	for _, row := range page.List {
		if row.PosterURL == "" {
			t.Fatal("persisted descriptor did not regenerate history poster")
		}
	}
	special.Deleted = true
	special.UpdatedAt += 10
	// The owner can delete existing history even when proof has expired and
	// the provider is offline. A forged new tombstone still needs ownership.
	mustCatalogue(t, s.db.Where("library_id = ?", id).Delete(&models.PluginOnlineMediaIdentity{}).Error)
	r.set(func(context.Context, string, []byte) ([]byte, error) {
		return nil, fmt.Errorf("controlled provider unavailable")
	})
	_, err = history.SyncContext(ctx, actor, 0, []PlayerHistoryChange{special})
	mustCatalogue(t, err)
	r.set(catalogueResponse)
	special.Deleted = false
	special.UpdatedAt -= 10
	_, err = history.SyncContext(ctx, actor, 0, []PlayerHistoryChange{special})
	mustCatalogue(t, err)
	page, err = history.List(actor, 1, 24, "server")
	mustCatalogue(t, err)
	if page.Total != 1 {
		t.Fatal("older progress resurrected tombstone")
	}
	mustCatalogue(t, s.db.Model(&models.PluginOnlineLibrary{}).Where("id = ?", id).Update("enabled", false).Error)
	page, err = history.List(actor, 1, 24, "server")
	mustCatalogue(t, err)
	if page.Total != 0 {
		t.Fatal("disabled scope still visible")
	}
	var count int64
	mustCatalogue(t, s.db.Model(&models.PlayerPlaybackHistory{}).Where("user_id = ? AND deleted = ?", actor.User.ID, false).Count(&count).Error)
	if count != 1 {
		t.Fatal("disable erased persisted resume")
	}
}

func TestOnlineHistoryIdentityCanonicalEncodingAndSafeErrorReasons(t *testing.T) {
	id := "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"
	token := onlineHistoryToken(id, "作品:/1", "video:2", "full!()")
	identity, err := parseOnlineHistoryToken(token)
	mustCatalogue(t, err)
	if identity.work != "作品:/1" || identity.version != "full!()" {
		t.Fatal("canonical identity corrupted")
	}
	for _, bad := range []string{strings.Replace(token, "%3A", "%3a", 1), token + "|extra", strings.Replace(token, id, "invalid", 1)} {
		if _, err := parseOnlineHistoryToken(bad); err == nil {
			t.Fatal("noncanonical identity accepted")
		}
	}
	for _, reason := range []string{"entitlement-required", "region-restricted", "drm-unsupported", "quality-unavailable", "incomplete-stream", "asset-domain-denied", "network-access-denied", "download-unavailable"} {
		err := mapPluginOnlineErrorReason("permission-denied", reason, contract.CapabilityMediaPlayback)
		if ErrorCode(err) == CodePluginOnlineLibraryUnavailable || strings.Contains(ErrorMessage(err), "secret") {
			t.Fatal("closed reason lost stable mapping")
		}
	}
	err = mapPluginOnlineErrorReason("permission-denied", "https://secret.invalid", contract.CapabilityMediaPlayback)
	if ErrorCode(err) != CodePermissionDenied || strings.Contains(ErrorMessage(err), "secret.invalid") {
		t.Fatal("unknown reason exposed provider diagnostics")
	}
}

func TestOnlineHistoryOwnedTombstoneRejectsForeignIdentityAndNewerReplayRestores(t *testing.T) {
	s, actor, id, r := catalogueFixture(t)
	history := NewPlayerHistoryService(s.db)
	history.SetPluginService(s)
	ctx := context.Background()
	played := onlineHistoryInput(id, "special:1", "bonus", 120)
	_, err := history.SyncContext(ctx, actor, 0, []PlayerHistoryChange{played})
	mustCatalogue(t, err)
	mustCatalogue(t, s.db.Where("library_id = ?", id).Delete(&models.PluginOnlineMediaIdentity{}).Error)
	r.set(func(context.Context, string, []byte) ([]byte, error) {
		return nil, fmt.Errorf("controlled provider unavailable")
	})
	before := r.count("site.detail")
	deleted := played
	deleted.Deleted = true
	deleted.UpdatedAt += 20
	foreign := Actor{User: models.User{ID: actor.User.ID + 99}, Permissions: map[string]struct{}{authz.PermissionMediaLibrariesRead: {}}}
	if _, err = history.SyncContext(ctx, foreign, 0, []PlayerHistoryChange{deleted}); ErrorCode(err) != CodeNotFound {
		t.Fatal("another user could tombstone the owner's online identity")
	}
	forged := onlineHistoryInput(id, "absent", "bonus", 120)
	forged.Deleted = true
	if _, err = history.SyncContext(ctx, actor, 0, []PlayerHistoryChange{forged}); ErrorCode(err) != CodeNotFound {
		t.Fatal("invented online identity created a tombstone")
	}
	result, err := history.SyncContext(ctx, actor, 0, []PlayerHistoryChange{deleted})
	mustCatalogue(t, err)
	if r.count("site.detail") != before {
		t.Fatal("tombstone ownership validation attempted a provider fetch")
	}
	if len(result.Changes) != 1 || !result.Changes[0].Deleted || result.Changes[0].ItemToken != played.ItemToken {
		t.Fatal("owner tombstone did not retain exact canonical identity")
	}
	page, err := history.List(actor, 1, 24, "server")
	mustCatalogue(t, err)
	if page.Total != 0 {
		t.Fatal("tombstoned online history remained visible")
	}
	r.set(catalogueResponse)
	_, err = history.SyncContext(ctx, actor, result.Cursor, []PlayerHistoryChange{played})
	mustCatalogue(t, err)
	page, err = history.List(actor, 1, 24, "server")
	mustCatalogue(t, err)
	if page.Total != 0 {
		t.Fatal("older offline progress resurrected the tombstone")
	}
	replayed := played
	replayed.UpdatedAt = deleted.UpdatedAt + 20
	replayed.Position = 15
	_, err = history.SyncContext(ctx, actor, 0, []PlayerHistoryChange{replayed})
	mustCatalogue(t, err)
	page, err = history.List(actor, 1, 24, "server")
	mustCatalogue(t, err)
	if page.Total != 1 || page.List[0].ItemToken != played.ItemToken || page.List[0].Position != 15 || page.List[0].Completed {
		t.Fatal("later legitimate replay did not restore the exact online identity")
	}
	delete(actor.Permissions, authz.PermissionMediaLibrariesRead)
	deleted.UpdatedAt = replayed.UpdatedAt + 20
	if _, err = history.SyncContext(ctx, actor, 0, []PlayerHistoryChange{deleted}); ErrorCode(err) != CodePermissionDenied {
		t.Fatal("owned tombstone bypassed revoked online read authorization")
	}
}

func TestOnlineHistoryProviderFailureKeepsSystemFactAndDefaultMembership(t *testing.T) {
	s, actor, id, r := catalogueFixture(t)
	history := NewPlayerHistoryService(s.db)
	history.SetPluginService(s)
	ctx := context.Background()
	var pkg models.PluginPackage
	mustCatalogue(t, s.db.First(&pkg).Error)
	manifest := strings.Replace(pkg.ManifestJSON, `"feed.refresh"`, `"feed.refresh","playback.progress_sync"`, 1)
	mustCatalogue(t, s.db.Model(&pkg).Update("manifest_json", manifest).Error)
	_, err := s.SyncOnlineProgress(ctx, actor, id, "work:1", "main:1", "full", "progress", 100, floatPointer(1000), "synthetic", time.Now().Format(time.RFC3339Nano))
	mustCatalogue(t, err)
	if r.count("playback.progress_sync") != 1 {
		t.Fatal("declared provider operation was not preserved")
	}
	page, err := history.List(actor, 1, 24, "server")
	mustCatalogue(t, err)
	if len(page.List) != 1 {
		t.Fatal("provider failure rolled back user history")
	}
	response, _ := catalogueResponse(ctx, "site.detail", nil)
	var work contract.MediaWork
	mustCatalogue(t, json.Unmarshal(response, &work))
	work.DefaultSegmentID = "unknown"
	bad, _ := json.Marshal(work)
	r.set(func(ctx context.Context, op string, in []byte) ([]byte, error) {
		if op == "site.detail" {
			return bad, nil
		}
		return catalogueResponse(ctx, op, in)
	})
	if _, err = s.OnlineDetail(ctx, actor, id, "work:1"); ErrorCode(err) != CodePluginResponseInvalid {
		t.Fatal("foreign default segment accepted")
	}
}
