package services

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/authz"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
	downloadpkg "github.com/yuanjing-hash/OhMyCine-Server/pkg/downloader"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/metadata/tmdb"
	sitepkg "github.com/yuanjing-hash/OhMyCine-Server/pkg/site"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/site/pansou"
)

func TestCloudSiteCredentialsAndSharePipeline(t *testing.T) {
	f := newShareIngestFixture(t)
	f.actor.Permissions[authz.PermissionSystemAdmin] = struct{}{}
	f.actor.Permissions[authz.PermissionDiscoveryRead] = struct{}{}
	adapter := &cloudFixtureAdapter{stubResolverAdapter: &stubResolverAdapter{stubSiteAdapter: &stubSiteAdapter{kind: pansou.Kind}, resolved: sitepkg.Source{ShareURL: "https://115.com/s/example?password=abcd", CloudProvider: "115"}}}
	s := NewSiteServiceWithAdapters(f.db, NewAuditService(f.db), f.store, f.downloads, []sitepkg.Adapter{adapter}, zerolog.Nop())
	input := SiteInput{Name: "TG", Kind: pansou.Kind, BaseURL: "https://pansou.example.test", CloudConfig: &sitepkg.CloudConfig{Provider: "115", Channels: []string{"@Movies"}, AuthEnabled: true}, Username: "user", Password: "pansou-secret", Enabled: true, Priority: 100, TimeoutSeconds: 30, RateLimitPerMinute: 120}
	created, err := s.Create(context.Background(), f.actor, input, RequestContext{})
	if err != nil {
		t.Fatal(err)
	}
	public, _ := json.Marshal(created)
	if strings.Contains(string(public), input.Password) || !created.PasswordConfigured || !created.CredentialConfigured || created.CloudConfig.Channels[0] != "movies" {
		t.Fatalf("summary=%s", public)
	}
	var record models.Site
	if err := f.db.First(&record, created.ID).Error; err != nil {
		t.Fatal(err)
	}
	if strings.Contains(record.CredentialCiphertext, input.Password) || strings.Contains(record.CloudConfigJSON, input.Password) {
		t.Fatal("plaintext credentials persisted")
	}
	search := func() string {
		t.Helper()
		groups, err := s.Search(context.Background(), f.actor, SiteSearchInput{Keyword: "Seven Samurai", Page: 1, SiteID: &created.ID})
		if err != nil || len(groups) != 1 || len(groups[0].Items) != 1 {
			t.Fatalf("groups=%+v err=%v", groups, err)
		}
		encoded, _ := json.Marshal(groups)
		if strings.Contains(string(encoded), "password=") {
			t.Fatal("share exposed")
		}
		return groups[0].Items[0].Token
	}
	oldToken := search()
	blank := ""
	updated, err := s.Update(context.Background(), f.actor, created.ID, SiteUpdateInput{Revision: created.Revision, Password: &blank}, RequestContext{})
	if err != nil {
		t.Fatal(err)
	}
	if adapter.lastConfig.Password != input.Password {
		t.Fatal("blank patch cleared password")
	}
	adapter.testErr = map[string]error{input.BaseURL: sitepkg.ErrAuthentication}
	bad := "wrong"
	if _, err := s.Update(context.Background(), f.actor, created.ID, SiteUpdateInput{Revision: updated.Revision, Password: &bad}, RequestContext{}); err == nil {
		t.Fatal("failed probe persisted")
	}
	adapter.testErr = nil
	if err := f.db.First(&record, created.ID).Error; err != nil {
		t.Fatal(err)
	}
	cred, err := s.decryptCredential(record)
	if err != nil || cred.Password != input.Password || record.Revision != updated.Revision {
		t.Fatal("failed update replaced valid credentials")
	}
	if _, err := s.Download(context.Background(), f.actor, SiteDownloadInput{ResultToken: oldToken, DownloaderID: f.downloader.ID}, RequestContext{}); ErrorCode(err) != CodeSiteResultExpired {
		t.Fatalf("stale result err=%v", err)
	}
	qbit := models.Downloader{ID: "qbit-cloud-test", Name: "qbit", NameNormalized: "qbit-cloud-test", Type: models.DownloaderTypeQBittorrent, Enabled: true, CapabilitiesJSON: `{}`}
	if err := f.db.Create(&qbit).Error; err != nil {
		t.Fatal(err)
	}
	token := search()
	if _, err := s.Download(context.Background(), f.actor, SiteDownloadInput{ResultToken: token, DownloaderID: qbit.ID}, RequestContext{}); ErrorCode(err) != CodeDownloadSourceInvalid {
		t.Fatalf("share accepted by qbit: %v", err)
	}
	library := f.createLibrary(t, "电影", "library", "intake", "/中转")
	if err := f.db.Model(&models.MediaLibrary{}).Where("id = ?", library.ID).Update("enabled", true).Error; err != nil {
		t.Fatal(err)
	}
	// A configuration edit after source resolution must still prevent persistence.
	if _, err := s.Download(context.Background(), f.actor, SiteDownloadInput{ResultToken: token, DownloaderID: f.downloader.ID, MediaLibraryID: &library.ID, BeforeSubmit: func() error {
		_, err := s.Update(context.Background(), f.actor, created.ID, SiteUpdateInput{Revision: updated.Revision}, RequestContext{})
		return err
	}}, RequestContext{}); ErrorCode(err) != CodeSiteResultExpired {
		t.Fatalf("configuration race accepted: %v", err)
	}
	var rejectedCount int64
	if err := f.db.Model(&models.DownloadTask{}).Count(&rejectedCount).Error; err != nil || rejectedCount != 0 {
		t.Fatalf("stale task persisted: count=%d err=%v", rejectedCount, err)
	}
	token = search()
	result, err := s.Download(context.Background(), f.actor, SiteDownloadInput{ResultToken: token, DownloaderID: f.downloader.ID, MediaLibraryID: &library.ID}, RequestContext{})
	if err != nil {
		t.Fatal(err)
	}
	var task models.DownloadTask
	if err := f.db.First(&task, "id = ?", result.ID).Error; err != nil {
		t.Fatal(err)
	}
	raw, err := f.store.Decrypt(downloadSourcePurpose(task.ID), task.SourceCiphertext)
	if err != nil {
		t.Fatal(err)
	}
	var source downloadSourceEnvelope
	if err := json.Unmarshal([]byte(raw), &source); err != nil {
		t.Fatal(err)
	}
	if source.Kind != downloadpkg.SourcePan115Share || task.SourceOrigin != models.DownloadSourceOriginShare || task.StagingProviderDirectoryID != "intake" || task.JobID == "" {
		t.Fatalf("wrong pipeline: source=%+v task=%+v", source, task)
	}
	if _, err := s.Download(context.Background(), f.actor, SiteDownloadInput{ResultToken: token, DownloaderID: f.downloader.ID, MediaLibraryID: &library.ID}, RequestContext{}); ErrorCode(err) != CodeSiteResultExpired {
		t.Fatalf("replayed claim: %v", err)
	}
}

type cloudFixtureAdapter struct {
	*stubResolverAdapter
	searchByKeyword map[string]sitepkg.Result
}

func (a *cloudFixtureAdapter) Search(_ context.Context, _ sitepkg.Config, q sitepkg.Query) (sitepkg.Page, error) {
	if item, ok := a.searchByKeyword[q.Keyword]; ok {
		return sitepkg.Page{Page: q.Page, Items: []sitepkg.Result{item}}, nil
	}
	title := a.searchTitle
	if title == "" {
		title = "Seven.Samurai.1954"
	}
	return sitepkg.Page{Page: q.Page, Items: []sitepkg.Result{{TorrentID: "42", Title: title, SourceKind: "115_share", CloudProvider: "115", Channel: "movies", Fingerprint: "stable-share-id"}}}, nil
}

func TestCloudFollowReusesShareForNewEpisodes(t *testing.T) {
	f := newShareIngestFixture(t)
	now := time.Now().UTC()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/tv/100":
			w.Write([]byte(`{"id":100,"name":"Fixture Show","original_name":"Fixture Show","original_language":"en","first_air_date":"2020-01-01","seasons":[{"id":1,"season_number":1,"name":"Season 1","episode_count":2}],"alternative_titles":{"results":[]},"translations":{"translations":[]}}`))
		case "/tv/100/season/1":
			w.Write([]byte(`{"season_number":1,"episodes":[{"id":11,"season_number":1,"episode_number":1,"air_date":"2020-01-01"},{"id":12,"season_number":1,"episode_number":2,"air_date":"2020-01-08"}]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer upstream.Close()
	var role models.Role
	if err := f.db.First(&role, "code = ?", authz.RoleAdministrator).Error; err != nil {
		t.Fatal(err)
	}
	if err := f.db.Create(&models.UserRole{UserID: f.actor.User.ID, RoleID: role.ID, CreatedAt: now}).Error; err != nil {
		t.Fatal(err)
	}
	actor, err := NewAuthorizationService(f.db).Resolve(f.actor.User.ID)
	if err != nil {
		t.Fatal(err)
	}
	metadata := NewMetadataSettingsService(f.db, NewAuditService(f.db), f.store, tmdb.Credential{Kind: tmdb.CredentialKindReadAccessToken, Value: "test"})
	metadata.clientFactory = func(tmdb.Credential, string, string) (*tmdb.Client, error) {
		return tmdb.NewForTest("test", upstream.URL, upstream.Client())
	}
	adapter := &cloudFixtureAdapter{stubResolverAdapter: &stubResolverAdapter{stubSiteAdapter: &stubSiteAdapter{kind: pansou.Kind, searchTitle: "Fixture.Show.S01E01.1080p.WEB-DL"}, resolved: sitepkg.Source{ShareURL: "https://115.com/s/episodes?password=abcd", CloudProvider: "115"}}}
	sites := NewSiteServiceWithAdapters(f.db, NewAuditService(f.db), f.store, f.downloads, []sitepkg.Adapter{adapter}, zerolog.Nop())
	sites.SetMetadataSettings(metadata)
	site, err := sites.Create(context.Background(), actor, SiteInput{Name: "Episodes TG", Kind: pansou.Kind, BaseURL: "https://pansou.example.test", CloudConfig: &sitepkg.CloudConfig{Provider: "115", Channels: []string{"movies"}}, Enabled: true, Priority: 100, TimeoutSeconds: 30, RateLimitPerMinute: 120}, RequestContext{})
	if err != nil {
		t.Fatal(err)
	}
	library := f.createLibrary(t, "剧集", "library", "intake", "/中转")
	if err := f.db.Model(&models.MediaLibrary{}).Where("id = ?", library.ID).Updates(map[string]any{"enabled": true, "status": models.MediaLibraryStatusListening, "baseline_generation": 1, "last_successful_scan_at": now}).Error; err != nil {
		t.Fatal(err)
	}
	if err := f.db.Create(&models.MediaLibraryScanRun{LibraryID: library.ID, Kind: "full", Status: "success", Generation: 1, StartedAt: now, FinishedAt: &now}).Error; err != nil {
		t.Fatal(err)
	}
	if err := f.db.Exec("UPDATE media_library_structure_auto_states SET diagnosed_revision=source_revision WHERE library_id=?", library.ID).Error; err != nil {
		t.Fatal(err)
	}
	coverage := NewMediaCoverageService(f.db, metadata)
	follows := NewFollowService(f.db, NewAuditService(f.db), f.downloads.queue, coverage, NewAuthorizationService(f.db))
	follows.SetDownloadService(f.downloads)
	snapshot := FollowExecutionSnapshot{Seasons: []int{1}, SiteIDs: []uint{site.ID}, DownloaderID: f.downloader.ID, MediaLibraryID: library.ID, Schedule: FollowSchedule{Kind: "interval", Minutes: 60}, Filters: FollowFilters{MinSeeders: 10}, MaxResourcesPerRun: 2}
	subscription, err := follows.Create(context.Background(), actor, CreateFollowInput{TMDBID: 100, Title: "Fixture Show", Snapshot: snapshot}, RequestContext{})
	if err != nil {
		t.Fatal(err)
	}
	// Different language searches can return different-age posts for the same share.
	older, newer := now.Add(-time.Hour), now
	adapter.searchByKeyword = map[string]sitepkg.Result{
		"示例剧":          {TorrentID: "42", Title: "Fixture.Show.S01E01.1080p", SourceKind: "115_share", Fingerprint: "stable-share-id", Published: &older},
		"Fixture Show": {TorrentID: "42", Title: "Fixture.Show.S01E01-E02.1080p", SourceKind: "115_share", Fingerprint: "stable-share-id", Published: &newer},
	}
	var siteRecord models.Site
	if err := f.db.First(&siteRecord, site.ID).Error; err != nil {
		t.Fatal(err)
	}
	merged := sites.searchMediaIdentitySite(context.Background(), actor, MediaIdentitySearchInput{MediaType: "tv", TMDBID: 100, Page: 1}, tmdb.Match{}, []tmdb.SearchName{{Value: "示例剧"}, {Value: "Fixture Show"}}, siteRecord)
	if len(merged.Items) != 1 || merged.Items[0].Title != "Fixture.Show.S01E01-E02.1080p" {
		t.Fatalf("new share metadata lost: %+v", merged)
	}
	adapter.searchByKeyword = nil
	worker := NewFollowSearchWorker(follows, sites)
	run := func() {
		t.Helper()
		claimed, err := f.downloads.queue.Claim([]string{JobTypeFollowSearch})
		if err != nil || claimed == nil {
			readiness, _ := libraryReadiness(f.db, library.ID)
			t.Fatalf("claim=%+v err=%v readiness=%+v", claimed, err, readiness)
		}
		result := worker.Run(context.Background(), &providerWakeRuntime{}, *claimed)
		if result.ErrorCode != "" {
			t.Fatalf("worker=%+v", result)
		}
		if err := f.downloads.queue.Complete(claimed.Job.ID, claimed.LeaseToken); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := follows.Enqueue(context.Background(), actor, subscription.ID, "manual", RequestContext{}); err != nil {
		t.Fatal(err)
	}
	run()
	adapter.searchTitle = "Fixture.Show.S01E01-E02.1080p.WEB-DL"
	if _, err := follows.Enqueue(context.Background(), actor, subscription.ID, "manual", RequestContext{}); err != nil {
		t.Fatal(err)
	}
	run()
	var tasks []models.DownloadTask
	if err := f.db.Where("follow_subscription_id = ?", subscription.ID).Find(&tasks).Error; err != nil {
		t.Fatal(err)
	}
	if len(tasks) != 2 {
		var runs []models.FollowRun
		f.db.Where("subscription_id = ?", subscription.ID).Find(&runs)
		t.Fatalf("tasks=%d runs=%+v", len(tasks), runs)
	}
	if tasks[0].FollowResourceFingerprint == tasks[1].FollowResourceFingerprint {
		t.Fatal("later episodes reused original download")
	}
	for _, task := range tasks {
		if task.SourceOrigin != models.DownloadSourceOriginShare {
			t.Fatal("follow bypassed share pipeline")
		}
	}
	if _, err := follows.Enqueue(context.Background(), actor, subscription.ID, "manual", RequestContext{}); err != nil {
		t.Fatal(err)
	}
	run()
	var count int64
	f.db.Model(&models.DownloadTask{}).Where("follow_subscription_id = ?", subscription.ID).Count(&count)
	if count != 2 {
		t.Fatalf("active episodes duplicated: %d", count)
	}
}
