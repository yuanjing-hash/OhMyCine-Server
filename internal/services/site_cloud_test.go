package services

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/authz"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/cloud"
	downloadpkg "github.com/yuanjing-hash/OhMyCine-Server/pkg/downloader"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/metadata/tmdb"
	sitepkg "github.com/yuanjing-hash/OhMyCine-Server/pkg/site"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/site/pansou"
)

func TestCloudSiteCredentialsAndSharePipeline(t *testing.T) {
	f := newShareIngestFixture(t)
	f.downloads.downloader.connections.drivers[*f.storage.ConnectionID] = &previewFixtureDriver{f.driver}
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
	f.downloads.downloader.connections.drivers[*f.storage.ConnectionID] = &previewFixtureDriver{f.driver}
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

type previewFixtureDriver struct{ *shareIngestDriver }

func (d *previewFixtureDriver) InspectShare(ctx context.Context, raw string) (cloud.ShareSnapshot, error) {
	return d.InspectShareDirectory(ctx, raw, "0")
}
func (d *previewFixtureDriver) InspectShareDirectory(_ context.Context, _ string, id string) (cloud.ShareSnapshot, error) {
	snapshot := cloud.ShareSnapshot{ShareCode: "example", ReceiveCode: "abcd"}
	if id == "0" {
		snapshot.Items = []cloud.ShareItem{{ID: "private-dir", Name: "Movies", IsDir: true}}
	} else {
		snapshot.Items = []cloud.ShareItem{{ID: "private-one", Name: "Seven.Samurai.1954.mkv", Size: 100}, {ID: "private-two", Name: "Other.mkv", Size: 200}}
	}
	return snapshot, nil
}
func (d *previewFixtureDriver) ReceiveShare(context.Context, cloud.ShareSnapshot, string) error {
	panic("preview must never receive files")
}

type previewFixtureAdapter struct{ *cloudFixtureAdapter }

func (a *previewFixtureAdapter) ResolveSource(context.Context, sitepkg.Config, string) (sitepkg.Source, error) {
	return a.resolved, nil
}
func TestCloudSharePreviewSelectionAuthorizationAndPersistence(t *testing.T) {
	f := newShareIngestFixture(t)
	f.actor.Permissions[authz.PermissionSystemAdmin] = struct{}{}
	f.actor.Permissions[authz.PermissionDiscoveryRead] = struct{}{}
	driver := &previewFixtureDriver{f.driver}
	connections := f.downloads.downloader.connections
	connections.drivers[*f.storage.ConnectionID] = driver
	adapter := &cloudFixtureAdapter{stubResolverAdapter: &stubResolverAdapter{stubSiteAdapter: &stubSiteAdapter{kind: pansou.Kind}, resolved: sitepkg.Source{ShareURL: "https://115.com/s/example?password=abcd", CloudProvider: "115"}}}
	service := NewSiteServiceWithAdapters(f.db, NewAuditService(f.db), f.store, f.downloads, []sitepkg.Adapter{&previewFixtureAdapter{adapter}}, zerolog.Nop())
	site, err := service.Create(context.Background(), f.actor, SiteInput{Name: "Preview", Kind: pansou.Kind, BaseURL: "https://pansou.example.test", CloudConfig: &sitepkg.CloudConfig{Provider: "115", Channels: []string{"movies"}}, Enabled: true, Priority: 100, TimeoutSeconds: 30, RateLimitPerMinute: 120}, RequestContext{})
	if err != nil {
		t.Fatal(err)
	}
	claim := siteResultClaim{ActorID: f.actor.User.ID, SiteID: site.ID, SiteRevision: site.Revision, TorrentID: "https://115.com/s/example?password=abcd", Title: "Collection", ExpiresAt: time.Now().Add(10 * time.Minute)}
	token, err := service.issueClaim(claim)
	if err != nil {
		t.Fatal(err)
	}
	preview, err := service.PreviewShare(context.Background(), f.actor, token, f.downloader.ID)
	if err != nil {
		t.Fatal(err)
	}
	if preview.TotalSize != 300 || preview.FileCount != 2 || len(preview.Entries) != 3 {
		t.Fatalf("preview=%+v", preview)
	}
	raw, _ := json.Marshal(preview)
	if strings.Contains(string(raw), "private-") || strings.Contains(string(raw), "abcd") || strings.Contains(string(raw), "115.com") {
		t.Fatal("private source exposed")
	}
	stranger := f.actor
	stranger.User.ID++
	if _, err := service.RecognizeShareEntry(context.Background(), stranger, preview.Token, preview.Entries[1].Token); err == nil {
		t.Fatal("other actor used preview")
	}
	var selected string
	for _, entry := range preview.Entries {
		if entry.Name == "Seven.Samurai.1954.mkv" {
			selected = entry.Token
		}
	}
	if result, err := service.RecognizeShareEntry(context.Background(), f.actor, preview.Token, selected); err != nil || result.Year == nil || *result.Year != 1954 {
		t.Fatalf("file recognition=%+v err=%v", result, err)
	}
	input := SiteDownloadInput{ResultToken: token, DownloaderID: f.downloader.ID, PreviewToken: preview.Token, SelectedEntryTokens: []string{selected}}
	src := DownloadSourceInput{Kind: downloadpkg.SourcePan115Share, URL: claim.TorrentID}
	invalid := input
	invalid.SelectedEntryTokens = nil
	if _, _, err := service.resolveShareSelection(context.Background(), f.actor, invalid, claim, src); err == nil {
		t.Fatal("empty selection became full share")
	}
	invalid.SelectedEntryTokens = []string{"forged"}
	if _, _, err := service.resolveShareSelection(context.Background(), f.actor, invalid, claim, src); err == nil {
		t.Fatal("forged entry accepted")
	}
	folder := input
	folder.SelectedEntryTokens = []string{preview.Entries[0].Token}
	expanded, _, err := service.resolveShareSelection(context.Background(), f.actor, folder, claim, src)
	if err != nil || len(expanded.Files) != 2 {
		t.Fatalf("folder not frozen: %+v %v", expanded, err)
	}
	library := f.createLibrary(t, "Selected Movies", "library", "intake", "/中转")
	f.db.Model(&models.MediaLibrary{}).Where("id = ?", library.ID).Update("enabled", true)
	input.MediaLibraryID = &library.ID
	// Fail configuration races within the actual download transaction.
	input.BeforeSubmit = func() error {
		return f.db.Model(&models.Downloader{}).Where("id = ?", f.downloader.ID).Update("provider_directory_id", "nested").Error
	}
	if _, err := service.Download(context.Background(), f.actor, input, RequestContext{}); ErrorCode(err) != CodeSiteResultExpired {
		t.Fatalf("config race accepted: %v", err)
	}
	f.db.Model(&models.Downloader{}).Where("id = ?", f.downloader.ID).Update("provider_directory_id", "intake")
	input.BeforeSubmit = nil
	task, err := service.Download(context.Background(), f.actor, input, RequestContext{})
	if err != nil {
		t.Fatal(err)
	}
	var stored models.DownloadTask
	f.db.First(&stored, "id = ?", task.ID)
	plaintext, err := f.store.Decrypt(downloadSourcePurpose(task.ID), stored.SourceCiphertext)
	if err != nil {
		t.Fatal(err)
	}
	var envelope downloadSourceEnvelope
	if err := json.Unmarshal([]byte(plaintext), &envelope); err != nil {
		t.Fatal(err)
	}
	if envelope.ShareSelection == nil || len(envelope.ShareSelection.Files) != 1 || envelope.ShareSelection.Files[0].ID != "private-one" {
		t.Fatalf("wrong scope: %+v", envelope.ShareSelection)
	}
	worker := NewDownloadWorker(f.downloads)
	goodManifest := downloadpkg.Manifest{Complete: true, Files: []downloadpkg.File{{RelativePath: "Movies/Seven.Samurai.1954.mkv", Size: 100}}}
	if err := worker.validateSelectedShareManifest(stored, goodManifest); err != nil {
		t.Fatal(err)
	}
	goodManifest.Files = append(goodManifest.Files, downloadpkg.File{RelativePath: "Movies/Other.mkv", Size: 200})
	if err := worker.validateSelectedShareManifest(stored, goodManifest); err == nil {
		t.Fatal("unselected file entered import manifest")
	}
	if strings.Contains(stored.SourceCiphertext, "private-one") {
		t.Fatal("selection not encrypted")
	}

	// A collection creates independent pipelines and retry resolves their persisted keys.
	batchToken, _ := service.issueClaim(claim)
	batchPreview, err := service.PreviewShare(context.Background(), f.actor, batchToken, f.downloader.ID)
	if err != nil {
		t.Fatal(err)
	}
	batchInput := SiteDownloadInput{ResultToken: batchToken, DownloaderID: f.downloader.ID, MediaLibraryID: &library.ID, PreviewToken: batchPreview.Token, SelectedEntryTokens: []string{batchPreview.Entries[0].Token}}
	batch, err := service.Download(context.Background(), f.actor, batchInput, RequestContext{})
	if err != nil || len(batch.SelectionTasks) != 2 {
		t.Fatalf("collection not split: %+v %v", batch, err)
	}
	selection, _, err := service.resolveShareSelection(context.Background(), f.actor, batchInput, claim, src)
	if err != nil {
		t.Fatal(err)
	}
	retrySource := src
	retrySource.ShareSelection = &selection
	retry, err := service.submitShareSelection(context.Background(), f.actor, batchPreview.Token, SubmitDownloadInput{DownloaderID: f.downloader.ID, MediaLibraryID: &library.ID, Source: retrySource}, RequestContext{})
	if err != nil || len(retry.SelectionTasks) != 2 || retry.SelectionTasks[0].ID != batch.SelectionTasks[0].ID || retry.SelectionTasks[1].ID != batch.SelectionTasks[1].ID {
		t.Fatalf("retry duplicated batch: %+v %v", retry, err)
	}
	if _, err := service.PreviewShare(context.Background(), f.actor, token, f.downloader.ID); err == nil {
		t.Fatal("consumed claim replayed")
	}
}

func TestSelectedSharePackagesKeepIndependentMoviesAndSubtitles(t *testing.T) {
	if got := siteResultMediaTypeHint("cloud_share", "名称: [LGNB全球顶级封装][名侦探柯南剧场版M11：绀碧之棺][BD-REMUX][Detective.Conan.Jolly.Roger.in.the.Deep.Azure.2007.Bluray.REMUX.1080p]", "tv"); got != "movie" {
		t.Fatalf("search TV hint overrode theatrical evidence: %s", got)
	}
	if got := siteResultMediaTypeHint("cloud_share", "Detective.Conan.S01E01.1080p", "tv"); got != "tv" {
		t.Fatalf("ordinary TV changed: %s", got)
	}

	selection := cloud.ShareSelection{Version: 1, Files: []cloud.ShareTreeItem{{ID: "a", RelativePath: "Movies/A.2000.mkv", Size: 10}, {ID: "b", RelativePath: "Movies/B.2001.mkv", Size: 20}, {ID: "sub", RelativePath: "Movies/B.2001.zh.ass", Size: 1}}}
	groups := selectedSharePackages(selection)
	if len(groups) != 2 || len(groups[0].Files) != 1 || len(groups[1].Files) != 2 || groups[1].Files[1].ID != "sub" {
		t.Fatalf("scope merged or lost: %+v", groups)
	}
}

type validationFixtureDriver struct {
	*previewFixtureDriver
	failure error
	reads   int
}

func (d *validationFixtureDriver) InspectShare(ctx context.Context, raw string) (cloud.ShareSnapshot, error) {
	return d.InspectShareDirectory(ctx, raw, "0")
}
func (d *validationFixtureDriver) InspectShareDirectory(ctx context.Context, raw, id string) (cloud.ShareSnapshot, error) {
	d.reads++
	if d.failure != nil {
		return cloud.ShareSnapshot{}, d.failure
	}
	return d.previewFixtureDriver.InspectShareDirectory(ctx, raw, id)
}

func TestCloudShareValidationIsolationExpiryAndFailedPreflight(t *testing.T) {
	f := newShareIngestFixture(t)
	f.actor.Permissions[authz.PermissionSystemAdmin] = struct{}{}
	f.actor.Permissions[authz.PermissionDiscoveryRead] = struct{}{}
	driver := &validationFixtureDriver{previewFixtureDriver: &previewFixtureDriver{f.driver}}
	f.downloads.downloader.connections.drivers[*f.storage.ConnectionID] = driver
	raw := "https://115.com/s/example?password=abcd"
	adapter := &cloudFixtureAdapter{stubResolverAdapter: &stubResolverAdapter{stubSiteAdapter: &stubSiteAdapter{kind: pansou.Kind}, resolved: sitepkg.Source{ShareURL: raw, CloudProvider: "115"}}}
	var logs bytes.Buffer
	service := NewSiteServiceWithAdapters(f.db, NewAuditService(f.db), f.store, f.downloads, []sitepkg.Adapter{&previewFixtureAdapter{adapter}}, zerolog.New(&logs))
	now := time.Now().UTC()
	service.now = func() time.Time { return now }
	site, err := service.Create(context.Background(), f.actor, SiteInput{Name: "validation", Priority: 100, Kind: pansou.Kind, BaseURL: "https://pansou.example.test", CloudConfig: &sitepkg.CloudConfig{Provider: "115", Channels: []string{"movies"}}, Enabled: true, RateLimitPerMinute: 120, TimeoutSeconds: 30}, RequestContext{})
	if err != nil {
		t.Fatal(err)
	}
	token, err := service.issueClaim(siteResultClaim{ActorID: f.actor.User.ID, SiteID: site.ID, SiteRevision: site.Revision, TorrentID: raw, Title: "Movie", ExpiresAt: now.Add(time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	driver.failure = cloud.Error(cloud.CodeShareExpired, false, errors.New("private upstream body password=abcd"))
	_, err = service.PreviewShare(context.Background(), f.actor, token, f.downloader.ID, RequestContext{RequestID: "check-request"})
	var app *AppError
	if !errors.As(err, &app) || app.Code != cloud.CodeShareExpired || app.ShareValidation == nil || app.ShareValidation.Status != "expired" {
		t.Fatalf("error=%v", err)
	}
	cached := service.cachedShareValidation(context.Background(), f.actor, raw)
	if cached == nil || cached.ExpiresAt.Sub(cached.CheckedAt) != 2*time.Minute {
		t.Fatal("missing invalid evidence")
	}
	before := driver.reads
	if _, err = service.Download(context.Background(), f.actor, SiteDownloadInput{ResultToken: token, DownloaderID: f.downloader.ID}, RequestContext{}); ErrorCode(err) != cloud.CodeShareExpired {
		t.Fatalf("preflight err=%v", err)
	}
	if driver.reads != before+1 {
		t.Fatal("submit trusted cache")
	}
	var count int64
	f.db.Model(&models.DownloadTask{}).Count(&count)
	if count != 0 {
		t.Fatal("failed preflight enqueued work")
	}
	driver.failure = cloud.Error(cloud.CodeRateLimited, true, nil)
	_, err = service.PreviewShare(context.Background(), f.actor, token, f.downloader.ID)
	if !errors.As(err, &app) || app.ShareValidation.Status != "unavailable" {
		t.Fatal("risk marked invalid")
	}
	driver.failure = nil
	preview, err := service.PreviewShare(context.Background(), f.actor, token, f.downloader.ID)
	if err != nil || preview.TotalSize != 300 || preview.ShareValidation.Status != "valid" {
		t.Fatalf("preview=%+v err=%v", preview, err)
	}
	if preview.ShareValidation.ExpiresAt.Sub(preview.ShareValidation.CheckedAt) != 5*time.Minute {
		t.Fatal("wrong success ttl")
	}
	another := f.actor
	another.User.ID++
	if service.cachedShareValidation(context.Background(), another, raw) != nil || service.cachedShareValidation(context.Background(), f.actor, raw+"x") != nil {
		t.Fatal("cross-actor/password evidence leak")
	}
	cached = service.cachedShareValidation(context.Background(), f.actor, raw)
	if cached == nil || cached.Status != "valid" {
		t.Fatal("retry did not replace invalid status")
	}
	now = now.Add(6 * time.Minute)
	if service.cachedShareValidation(context.Background(), f.actor, raw) != nil {
		t.Fatal("expired evidence retained")
	}
	now = now.Add(-6 * time.Minute)
	f.db.Model(&models.Connection{}).Where("id = ?", *f.storage.ConnectionID).Update("revision", 100)
	if service.cachedShareValidation(context.Background(), f.actor, raw) != nil {
		t.Fatal("configuration change retained evidence")
	}
	if strings.Contains(logs.String(), "password") || strings.Contains(logs.String(), "private upstream") || strings.Contains(logs.String(), "example") || !strings.Contains(logs.String(), "check-request") {
		t.Fatal("unsafe or uncorrelated diagnostic log")
	}
}
