package services

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/authz"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/cloud"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/metadata/tmdb"
	sitepkg "github.com/yuanjing-hash/OhMyCine-Server/pkg/site"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/site/pansou"
)

func analysisIdentity() *siteShareIdentity {
	season := 1
	return &siteShareIdentity{TMDBID: 100, MediaType: "tv", Title: "Fixture Show", Names: []tmdb.SearchName{{Value: "Fixture Show"}}, Season: &season}
}

func analysisEntries(paths ...string) []SiteSharePreviewEntry {
	result := []SiteSharePreviewEntry{}
	for i, name := range paths {
		result = append(result, SiteSharePreviewEntry{Token: fmt.Sprint(i), Name: name, Path: name, Size: 100})
	}
	return result
}

func TestShareAnalysisUsesActualSparseFilesAndSharedFollowSelection(t *testing.T) {
	s := NewSiteService(nil, nil, nil, nil, zerolog.Nop())
	claim := siteResultClaim{Title: "🎬 Incorrect Work (2026) S02E01-E99", SearchIdentity: analysisIdentity()}
	entries := analysisEntries("Fixture Show(81)/11.mkv", "Fixture Show(81)/13.mkv", "Fixture Show(81)/14.mp4", "Fixture Show(81)/14.mkv", "Fixture Show(81)/11.zh.srt", "Fixture Show(81)/2026.S01E01.2160p.zip", "Fixture Show(81)/sample.mkv", "Fixture Show(81)/16.mkv")
	analysis, files := s.analyzeShareEntries(context.Background(), Actor{}, claim, entries)
	if !analysis.Complete || analysis.VideoCount != 5 || analysis.IgnoredCount != 3 || analysis.UnresolvedCount != 0 || len(analysis.Groups) != 1 || !reflect.DeepEqual(analysis.Groups[0].Episodes, []int{11, 13, 14, 16}) || analysis.Groups[0].Season == nil || *analysis.Groups[0].Season != 1 {
		t.Fatalf("analysis=%+v groups=%+v", analysis, analysis.Groups)
	}
	preview := SiteSharePreviewSummary{Entries: entries, ShareAnalysis: analysis}
	tokens, episodes := selectAnalyzedFollowFiles(preview, files, claim.SearchIdentity, 1, []int{11, 12, 13, 14, 15})
	if !reflect.DeepEqual(episodes, []int{11, 13, 14}) || !reflect.DeepEqual(tokens, []string{"0", "1", "2", "3", "4"}) {
		t.Fatalf("tokens=%v episodes=%v", tokens, episodes)
	}
}

func TestShareAnalysisKeepsMixedWorksSeasonsAndUnresolvedLeavesSeparate(t *testing.T) {
	s := NewSiteService(nil, nil, nil, nil, zerolog.Nop())
	claim := siteResultClaim{Title: "Fixture Show S01E01-E99", SearchIdentity: analysisIdentity()}
	entries := analysisEntries("Fixture Show/S01/11.mkv", "Fixture Show/S02/12.mkv", "Other Work/S01/13.mkv", "14.mkv", "Fixture Show(81)/2026.mkv", "Fixture Show/S02/Fixture.Show.S01E15.mkv", "Fixture Show/Other.Work.S01E16.mkv", "Fixture Show/S01/1080.mkv", "Fixture Show/S01/0.mkv", "Fixture Show/S01/201.mkv")
	analysis, files := s.analyzeShareEntries(context.Background(), Actor{}, claim, entries)
	if len(analysis.Groups) != 3 || analysis.UnresolvedCount != 8 {
		t.Fatalf("analysis=%+v groups=%+v files=%+v", analysis, analysis.Groups, files)
	}
	_, episodes := selectAnalyzedFollowFiles(SiteSharePreviewSummary{Entries: entries, ShareAnalysis: analysis}, files, claim.SearchIdentity, 1, []int{11, 12, 13, 14, 15, 16})
	if !reflect.DeepEqual(episodes, []int{11}) {
		t.Fatalf("foreign/unknown leaves selected: %v", episodes)
	}
}

func TestShareAnalysisDoesNotBorrowAdvertisedSeasonOrRange(t *testing.T) {
	s := NewSiteService(nil, nil, nil, nil, zerolog.Nop())
	analysis, _ := s.analyzeShareEntries(context.Background(), Actor{}, siteResultClaim{Title: "🎬 Fixture Show (2026) S03E01-E99"}, analysisEntries("Fixture Show (2026)/11.mkv", "Fixture Show (2026)/13.mkv"))
	if !analysis.Complete || len(analysis.Groups) != 1 || analysis.Groups[0].Season != nil || !reflect.DeepEqual(analysis.Groups[0].Episodes, []int{11, 13}) || analysis.Groups[0].Year == nil || *analysis.Groups[0].Year != 2026 || analysis.UnresolvedCount != 2 {
		t.Fatalf("analysis=%+v groups=%+v", analysis, analysis.Groups)
	}
}

func TestShareAnalysisDoesNotBindSameNameMovieFromConflictingYear(t *testing.T) {
	s := NewSiteService(nil, nil, nil, nil, zerolog.Nop())
	year := 2026
	identity := &siteShareIdentity{TMDBID: 100, MediaType: "movie", Title: "Fixture Movie", Year: &year}
	for _, test := range []struct {
		year    int
		matched bool
	}{{1990, false}, {2025, true}, {2026, true}, {2027, true}, {2028, false}} {
		analysis, _ := s.analyzeShareEntries(context.Background(), Actor{}, siteResultClaim{SearchIdentity: identity}, analysisEntries(fmt.Sprintf("Fixture.Movie.%d.1080p.mkv", test.year)))
		if len(analysis.Groups) != 1 || (analysis.Groups[0].Status == mediaRecognitionStatusMatched) != test.matched {
			t.Fatalf("year=%d groups=%+v", test.year, analysis.Groups)
		}
	}
}

func TestShareAnalysisMetadataAndGroupBudgetsRetainReadFacts(t *testing.T) {
	s := NewSiteService(nil, nil, nil, nil, zerolog.Nop())
	paths := []string{}
	for n := 0; n < 40; n++ {
		paths = append(paths, fmt.Sprintf("Different Work %d/S01/11.mkv", n))
	}
	analysis, _ := s.analyzeShareEntries(context.Background(), Actor{}, siteResultClaim{}, analysisEntries(paths...))
	if !analysis.Complete || len(analysis.Groups) != 32 || analysis.UnresolvedCount != 40 || analysis.Groups[8].ErrorCode != "share_recognition_budget" || !reflect.DeepEqual(analysis.Groups[31].Episodes, []int{11}) {
		t.Fatalf("analysis=%+v groups=%+v", analysis, analysis.Groups)
	}
}

type analysisFixtureDriver struct {
	*previewFixtureDriver
	reads   int
	partial bool
	onRead  func()
}

func (d *analysisFixtureDriver) InspectShareDirectory(ctx context.Context, _ string, id string) (cloud.ShareSnapshot, error) {
	d.reads++
	if d.onRead != nil {
		d.onRead()
	}
	if ctx.Err() != nil {
		return cloud.ShareSnapshot{}, ctx.Err()
	}
	if id != "0" && d.partial {
		return cloud.ShareSnapshot{}, cloud.Error(cloud.CodeResponseInvalid, false, errors.New("private provider body"))
	}
	items := []cloud.ShareItem{{ID: "provider-private-work", Name: "Fixture Show(81)", IsDir: true}}
	if id != "0" {
		items = []cloud.ShareItem{{ID: "provider-private-11", Name: "11.mkv", Size: 100}, {ID: "provider-private-13", Name: "13.mkv", Size: 100}, {ID: "provider-private-zip", Name: "S01E12.zip", Size: 100}}
	}
	return cloud.ShareSnapshot{ShareCode: "private-share-code", ReceiveCode: "abcd", Items: items}, nil
}

func newShareAnalysisFixture(t *testing.T) (shareIngestFixture, *SiteService, *analysisFixtureDriver, siteResultClaim) {
	t.Helper()
	f := newShareIngestFixture(t)
	driver := &analysisFixtureDriver{previewFixtureDriver: &previewFixtureDriver{f.driver}}
	f.downloads.downloader.connections.drivers[*f.storage.ConnectionID] = driver
	adapter := &cloudFixtureAdapter{stubResolverAdapter: &stubResolverAdapter{stubSiteAdapter: &stubSiteAdapter{kind: pansou.Kind}}}
	s := NewSiteServiceWithAdapters(f.db, NewAuditService(f.db), f.store, f.downloads, []sitepkg.Adapter{adapter}, zerolog.Nop())
	site, err := s.Create(context.Background(), f.actor, SiteInput{Name: "Analysis", Kind: pansou.Kind, BaseURL: "https://analysis.example.test", CloudConfig: &sitepkg.CloudConfig{Provider: "115", Channels: []string{"test"}}, Enabled: true, Priority: 1, TimeoutSeconds: 30, RateLimitPerMinute: 120}, RequestContext{})
	if err != nil {
		t.Fatal(err)
	}
	claim := siteResultClaim{ActorID: f.actor.User.ID, SiteID: site.ID, SiteRevision: site.Revision, TorrentID: "https://115.com/s/example?password=abcd", SourceKind: RouteSource115Share, Title: "🎬 Unrelated Advertisement (2026) 1-99", ExpiresAt: time.Now().Add(10 * time.Minute), SearchIdentity: analysisIdentity(), AuthorizationScope: discoveryAuthorizationScope(f.actor)}
	return f, s, driver, claim
}

func issueAnalysisClaim(t *testing.T, s *SiteService, claim siteResultClaim) string {
	t.Helper()
	token, err := s.issueClaim(claim)
	if err != nil {
		t.Fatal(err)
	}
	return token
}

func TestAutomaticShareRecognitionReadsOnceAcrossClaimsAndInvalidatesConfiguration(t *testing.T) {
	f, s, driver, claim := newShareAnalysisFixture(t)
	first := issueAnalysisClaim(t, s, claim)
	result, err := s.RecognizeResult(context.Background(), f.actor, first)
	if err != nil || result.ShareAnalysis == nil || !result.ShareAnalysis.Complete || result.Status != mediaRecognitionStatusMatched || driver.reads != 2 {
		t.Fatalf("result=%+v reads=%d err=%v", result, driver.reads, err)
	}
	for _, title := range []string{claim.Title, "different advertisement S03E99"} {
		claim.Title = title
		result, err = s.RecognizeResult(context.Background(), f.actor, issueAnalysisClaim(t, s, claim))
		if err != nil || !result.ShareAnalysis.Complete || driver.reads != 2 || !reflect.DeepEqual(result.ShareAnalysis.Groups[0].Episodes, []int{11, 13}) {
			t.Fatalf("cache result=%+v reads=%d err=%v", result, driver.reads, err)
		}
	}
	raw, _ := json.Marshal(result)
	for _, forbidden := range []string{"115.com", "abcd", "provider-private", "private-share-code", "Fixture Show(81)", "1-99"} {
		if strings.Contains(string(raw), forbidden) {
			t.Fatalf("private/title-range evidence leaked: %s", raw)
		}
	}
	s.now = func() time.Time { return time.Now().UTC().Add(6 * time.Minute) }
	result, err = s.RecognizeResult(context.Background(), f.actor, issueAnalysisClaim(t, s, claim))
	if err != nil || !result.ShareAnalysis.Complete || driver.reads != 4 {
		t.Fatalf("expiry result=%+v reads=%d err=%v", result, driver.reads, err)
	}
	if err := f.db.Model(&models.Connection{}).Where("id = ?", *f.storage.ConnectionID).Update("revision", 2).Error; err != nil {
		t.Fatal(err)
	}
	result, err = s.RecognizeResult(context.Background(), f.actor, issueAnalysisClaim(t, s, claim))
	// Connection replacement rebuilds the fixture's generic driver, which lacks
	// share browsing. The old complete tree must not survive that change.
	if err != nil || result.ShareAnalysis.Complete || len(result.ShareAnalysis.Groups) != 0 || driver.reads != 4 {
		t.Fatalf("config cache result=%+v reads=%d err=%v", result, driver.reads, err)
	}
}

func TestAutomaticShareRecognitionFailureCancellationAndPermissionNeverInventContents(t *testing.T) {
	f, s, driver, claim := newShareAnalysisFixture(t)
	token := issueAnalysisClaim(t, s, claim)
	driver.partial = true
	result, err := s.RecognizeResult(context.Background(), f.actor, token)
	if err != nil || result.ShareAnalysis == nil || result.ShareAnalysis.Complete || len(result.ShareAnalysis.Groups) != 0 || result.Episodes != nil {
		t.Fatalf("partial=%+v err=%v", result, err)
	}
	driver.partial = false
	ctx, cancel := context.WithCancel(context.Background())
	driver.onRead = cancel
	if _, err = s.RecognizeResult(ctx, f.actor, token); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel=%v", err)
	}
	driver.onRead = nil
	result, err = s.RecognizeResult(context.Background(), f.actor, token)
	if err != nil || !result.ShareAnalysis.Complete {
		t.Fatalf("failure poisoned cache: %+v %v", result, err)
	}
	reads := driver.reads
	denied := f.actor
	denied.Permissions = map[string]struct{}{authz.PermissionDiscoveryRead: {}}
	result, err = s.RecognizeResult(context.Background(), denied, token)
	if err != nil || result.ShareAnalysis == nil || result.ShareAnalysis.Complete || len(result.ShareAnalysis.Groups) != 0 || driver.reads != reads {
		t.Fatalf("denied=%+v err=%v reads=%d", result, err, driver.reads)
	}
	stranger := f.actor
	stranger.User.ID++
	if _, err = s.RecognizeResult(context.Background(), stranger, token); err == nil {
		t.Fatal("foreign actor read claim")
	}
}

func TestShareAnalysisRejectsManualIdentityChangeAndConfigurationRace(t *testing.T) {
	f, s, driver, claim := newShareAnalysisFixture(t)
	token := issueAnalysisClaim(t, s, claim)
	preview, err := s.PreviewShare(context.Background(), f.actor, token, f.downloader.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.bindClaimRecognition(token, f.actor.User.ID, 200, "tv", mediaIdentitySourceManual, mediaIdentityStatusVerified, true); err != nil {
		t.Fatal(err)
	}
	if _, err := s.resolveSharePreview(context.Background(), f.actor, preview.Token); err == nil {
		t.Fatal("manual change reused old content identity")
	}
	current, err := s.RecognizeResult(context.Background(), f.actor, token)
	if err != nil || current.ShareAnalysis == nil || current.ShareAnalysis.Groups[0].Status == mediaRecognitionStatusMatched || current.ShareAnalysis.Groups[0].ErrorCode != CodeSiteResultIdentityMismatch {
		t.Fatalf("manual conflict ignored: %+v err=%v", current, err)
	}
	driver.onRead = func() {
		_ = f.db.Model(&models.Downloader{}).Where("id = ?", f.downloader.ID).Update("provider_directory_id", "changed").Error
	}
	if _, err := s.PreviewShare(context.Background(), f.actor, token, f.downloader.ID); err == nil {
		t.Fatal("configuration race published stale tree")
	}
}

func TestAutomaticShareAnalysisConcurrentClaimsShareReadAndUnavailableReceiverDoesNotReuse(t *testing.T) {
	f, s, driver, claim := newShareAnalysisFixture(t)
	tokens := []string{}
	for i := 0; i < 4; i++ {
		tokens = append(tokens, issueAnalysisClaim(t, s, claim))
	}
	var wg sync.WaitGroup
	for _, token := range tokens {
		wg.Add(1)
		go func(token string) {
			defer wg.Done()
			result, err := s.RecognizeResult(context.Background(), f.actor, token)
			if err != nil || result.ShareAnalysis == nil || !result.ShareAnalysis.Complete {
				t.Errorf("concurrent result=%+v err=%v", result, err)
			}
		}(token)
	}
	wg.Wait()
	if driver.reads != 2 {
		t.Fatalf("duplicate reads=%d", driver.reads)
	}
	if err := f.db.Model(&models.Downloader{}).Where("id = ?", f.downloader.ID).Update("enabled", false).Error; err != nil {
		t.Fatal(err)
	}
	result, err := s.RecognizeResult(context.Background(), f.actor, tokens[0])
	if err != nil || result.ShareAnalysis == nil || result.ShareAnalysis.Complete || driver.reads != 2 {
		t.Fatalf("disabled cached=%+v err=%v reads=%d", result, err, driver.reads)
	}
}

func TestFollowCloudTitleDoesNotLimitActualMissingEpisodes(t *testing.T) {
	f, driver := newFollowPreviewFixture(t)
	adapter := f.worker.sites.adapters["pansou_tg"].(*followSharesAdapter)
	for i := range adapter.items {
		adapter.items[i].Title = "🎬 Unrelated Advertisement (2026) S02E01"
	}
	run := f.run(t)
	if run.Selected != 1 || len(driver.reads) != 5 {
		t.Fatalf("title gated actual content: run=%+v reads=%v", run, driver.reads)
	}
	var claims []models.FollowEpisodeClaim
	if err := f.worker.sites.db.Where("subscription_id = ? AND run_id = ?", f.subscription.ID, run.ID).Find(&claims).Error; err != nil {
		t.Fatal(err)
	}
	if len(claims) != 5 {
		t.Fatalf("claims=%+v", claims)
	}
	for _, claim := range claims {
		if claim.EpisodeNumber < 11 || claim.EpisodeNumber > 15 {
			t.Fatalf("unproven episode=%+v", claim)
		}
	}
}

func TestFollowInvalidShareTreesDoNotExhaustPreviewCapacity(t *testing.T) {
	f, _ := newFollowPreviewFixture(t)
	adapter := f.worker.sites.adapters["pansou_tg"].(*followSharesAdapter)
	valid := adapter.items[1]
	adapter.items = nil
	for n := 0; n < 32; n++ {
		item := valid
		item.TorrentID = fmt.Sprintf("https://115.com/s/unusable-%02d?password=test", n)
		seeders := 100
		item.Seeders = &seeders
		adapter.items = append(adapter.items, item)
	}
	adapter.items = append(adapter.items, valid)
	run := f.run(t)
	if run.Selected != 1 || run.Candidates != 33 {
		t.Fatalf("preview capacity hid valid candidate: %+v", run)
	}
	if len(f.worker.sites.sharePreviews) != 1 {
		t.Fatalf("unusable previews retained: %d", len(f.worker.sites.sharePreviews))
	}
}

func TestUnsupportedCloudRecognitionDoesNotFallBackToTitleEpisodes(t *testing.T) {
	f, s, driver, claim := newShareAnalysisFixture(t)
	claim.SourceKind, claim.CloudProvider, claim.Title = "external_link", "aliyun", "Fixture.Show.S01E01-E99"
	result, err := s.RecognizeResult(context.Background(), f.actor, issueAnalysisClaim(t, s, claim))
	if err != nil || result.ShareAnalysis == nil || result.ShareAnalysis.Complete || len(result.ShareAnalysis.Groups) != 0 || result.Episodes != nil || driver.reads != 0 {
		t.Fatalf("unsupported=%+v err=%v", result, err)
	}
}

func TestBindExpectedCloudIdentityUsesActualContentsDespiteWrongPostTitle(t *testing.T) {
	f, s, _, claim := newShareAnalysisFixture(t)
	upstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/tv/100":
			_, _ = writer.Write([]byte(`{"id":100,"name":"Fixture Show","original_name":"Fixture Show","first_air_date":"2026-01-01","number_of_seasons":1,"alternative_titles":{"results":[]},"translations":{"translations":[]}}`))
		case "/tv/200":
			_, _ = writer.Write([]byte(`{"id":200,"name":"Other Work","original_name":"Other Work","first_air_date":"2026-01-01","number_of_seasons":1,"alternative_titles":{"results":[]},"translations":{"translations":[]}}`))
		default:
			_, _ = writer.Write([]byte(`{"results":[]}`))
		}
	}))
	defer upstream.Close()
	metadata := NewMetadataSettingsService(f.db, s.audit, f.store, tmdb.Credential{Kind: tmdb.CredentialKindReadAccessToken, Value: "test-token"})
	metadata.clientFactory = func(tmdb.Credential, string, string) (*tmdb.Client, error) {
		return tmdb.NewForTest("test-token", upstream.URL, upstream.Client())
	}
	s.SetMetadataSettings(metadata)
	claim.SearchIdentity = nil
	token := issueAnalysisClaim(t, s, claim)
	if err := s.BindExpectedIdentity(context.Background(), f.actor, token, "tv", 100); err != nil {
		t.Fatalf("actual work was gated by post title: %v", err)
	}
	bound, err := s.resolveClaim(token, f.actor.User.ID)
	if err != nil || bound.ManualTMDBID == nil || *bound.ManualTMDBID != 100 {
		t.Fatalf("bound=%+v err=%v", bound, err)
	}
	token = issueAnalysisClaim(t, s, claim)
	if err := s.BindExpectedIdentity(context.Background(), f.actor, token, "tv", 200); ErrorCode(err) != CodeSiteResultIdentityMismatch {
		t.Fatalf("wrong expected work bound actual files: %v", err)
	}
	bound, err = s.resolveClaim(token, f.actor.User.ID)
	if err != nil || bound.ManualTMDBID != nil || bound.InFlight {
		t.Fatalf("failed content match changed accepted identity: %+v err=%v", bound, err)
	}
}

type evolvingCloudShareFixtureDriver struct {
	*previewFixtureDriver
	episodes int
}

func (d *evolvingCloudShareFixtureDriver) InspectShareDirectory(ctx context.Context, raw, id string) (cloud.ShareSnapshot, error) {
	result, err := d.previewFixtureDriver.InspectShareDirectory(ctx, raw, id)
	if strings.Contains(raw, "/episodes?") && len(result.Items) > d.episodes {
		result.Items = result.Items[:d.episodes]
	}
	return result, err
}
