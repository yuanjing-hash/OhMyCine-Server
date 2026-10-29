package services

import (
	"context"
	"fmt"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/metadata/tmdb"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestContentGroupIdentityCacheSharesOwnedWorkAndLastWaiterCancels(t *testing.T) {
	s := &SiteService{now: time.Now}
	started := make(chan struct{})
	finish := make(chan struct{})
	var calls atomic.Int32
	load := func(ctx context.Context) (SiteRecognitionSummary, error) {
		calls.Add(1)
		close(started)
		select {
		case <-finish:
			return SiteRecognitionSummary{Status: mediaRecognitionStatusMatched, Title: "Work"}, nil
		case <-ctx.Done():
			return SiteRecognitionSummary{}, ctx.Err()
		}
	}
	first, cancel := context.WithCancel(context.Background())
	a := make(chan error, 1)
	b := make(chan error, 1)
	go func() { _, err := s.readContentGroupShared(first, "actor1:g1", load); a <- err }()
	<-started
	go func() { _, err := s.readContentGroupShared(context.Background(), "actor1:g1", load); b <- err }()
	deadline := time.Now().Add(time.Second)
	for {
		state := s.contents()
		state.mu.Lock()
		count := state.groupFlights["actor1:g1"].waiters
		state.mu.Unlock()
		if count == 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("second waiter did not attach")
		}
		time.Sleep(time.Millisecond)
	}
	cancel()
	if <-a == nil {
		t.Fatal("cancelled first waiter succeeded")
	}
	close(finish)
	if err := <-b; err != nil {
		t.Fatal(err)
	}
	if _, err := s.readContentGroupShared(context.Background(), "actor1:g1", func(context.Context) (SiteRecognitionSummary, error) {
		t.Error("same group performed duplicate lookup")
		return SiteRecognitionSummary{}, nil
	}); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 1 {
		t.Fatal("cache miss")
	}
	stopped := make(chan struct{})
	work, cancelWork := context.WithCancel(context.Background())
	go func() {
		_, _ = s.readContentGroupShared(work, "actor2:g2", func(ctx context.Context) (SiteRecognitionSummary, error) {
			<-ctx.Done()
			close(stopped)
			return SiteRecognitionSummary{}, ctx.Err()
		})
	}()
	time.Sleep(5 * time.Millisecond)
	cancelWork()
	select {
	case <-stopped:
	case <-time.After(time.Second):
		t.Fatal("last waiter left group lookup running")
	}
}

func TestContentGroupMetadataIsBoundedCachedAndGenerationScoped(t *testing.T) {
	f, s, _, _ := newShareAnalysisFixture(t)
	var requests, active, peak atomic.Int32
	entered := make(chan struct{}, 64)
	release := make(chan struct{})
	var finish sync.Once
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		current := active.Add(1)
		defer active.Add(-1)
		for {
			previous := peak.Load()
			if current <= previous || peak.CompareAndSwap(previous, current) {
				break
			}
		}
		select {
		case entered <- struct{}{}:
		default:
		}
		select {
		case <-release:
			_, _ = w.Write([]byte(`{"results":[]}`))
		case <-r.Context().Done():
		}
	}))
	defer upstream.Close()
	defer finish.Do(func() { close(release) })
	metadata := NewMetadataSettingsService(f.db, s.audit, f.store, tmdb.Credential{Kind: tmdb.CredentialKindReadAccessToken, Value: "fixture-token"})
	metadata.clientFactory = func(tmdb.Credential, string, string) (*tmdb.Client, error) {
		return tmdb.NewForTest("fixture-token", upstream.URL, upstream.Client())
	}
	s.SetMetadataSettings(metadata)
	paths := []string{}
	for n := 0; n < 12; n++ {
		paths = append(paths, fmt.Sprintf("Different Work %d/Season 01/11.mkv", n))
	}
	done := make(chan *SiteShareAnalysis, 1)
	go func() {
		analysis, _ := s.analyzeShareEntries(context.Background(), f.actor, siteResultClaim{}, analysisEntries(paths...))
		done <- analysis
	}()
	for n := 0; n < 4; n++ {
		select {
		case <-entered:
		case <-time.After(2 * time.Second):
			t.Fatal("independent groups did not run concurrently")
		}
	}
	finish.Do(func() { close(release) })
	analysis := <-done
	if peak.Load() != 4 || len(analysis.Groups) != 12 || analysis.Groups[8].ErrorCode != "share_recognition_budget" {
		t.Fatalf("group budget peak=%d groups=%+v", peak.Load(), analysis.Groups)
	}
	before := requests.Load()
	for n := range paths {
		paths[n] = fmt.Sprintf("Different Work %d/Season 01/13.mkv", n)
	}
	again, _ := s.analyzeShareEntries(context.Background(), f.actor, siteResultClaim{}, analysisEntries(paths...))
	if requests.Load() != before || !reflect.DeepEqual(again.Groups[0].Episodes, []int{13}) {
		t.Fatal("group cache duplicated metadata or replaced new file coordinates with cached episodes")
	}
	if err := f.db.Model(&models.MetadataSettings{}).Where("id = 1").Update("revision", 2).Error; err != nil {
		t.Fatal(err)
	}
	_, _ = s.analyzeShareEntries(context.Background(), f.actor, siteResultClaim{}, analysisEntries(paths...))
	if requests.Load() <= before {
		t.Fatal("changed metadata configuration reused prior recognition")
	}
	before = requests.Load()
	other := f.actor
	other.User.ID++
	_, _ = s.analyzeShareEntries(context.Background(), other, siteResultClaim{}, analysisEntries(paths...))
	if requests.Load() <= before {
		t.Fatal("unrelated actor reused metadata scope")
	}
}

func TestContentQuickTitleLaneCannotReplaceActualIntendedIdentity(t *testing.T) {
	f, s, _, claim := newShareAnalysisFixture(t)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/search/tv":
			_, _ = w.Write([]byte(`{"results":[{"id":200,"name":"Other Work","original_name":"Other Work","first_air_date":"2026-01-01","popularity":50}]}`))
		case "/tv/200":
			_, _ = w.Write([]byte(`{"id":200,"name":"Other Work","original_name":"Other Work","first_air_date":"2026-01-01","number_of_seasons":1,"genres":[{"id":18,"name":"Drama"}]}`))
		default:
			_, _ = w.Write([]byte(`{"results":[]}`))
		}
	}))
	defer upstream.Close()
	metadata := NewMetadataSettingsService(f.db, s.audit, f.store, tmdb.Credential{Kind: tmdb.CredentialKindReadAccessToken, Value: "fixture-token"})
	metadata.clientFactory = func(tmdb.Credential, string, string) (*tmdb.Client, error) {
		return tmdb.NewForTest("fixture-token", upstream.URL, upstream.Client())
	}
	s.SetMetadataSettings(metadata)
	intended := int64(100)
	claim.ManualTMDBID = &intended
	claim.ManualMediaType = "tv"
	claim.RecognitionSource, claim.RecognitionStatus = mediaIdentitySourceDirectID, mediaIdentityStatusVerified
	claim.Title, claim.MediaTypeHint = "Other.Work.S01E13.1080p", "tv"
	token := issueAnalysisClaim(t, s, claim)
	advice, err := s.RecognizeTitleResult(context.Background(), f.actor, token)
	if err != nil || advice.Status != mediaRecognitionStatusMatched || advice.TMDBID == nil || *advice.TMDBID != 200 {
		t.Fatalf("title advice=%+v err=%v", advice, err)
	}
	current, err := s.resolveClaim(token, f.actor.User.ID)
	if err != nil || current.ManualTMDBID == nil || *current.ManualTMDBID != 100 || current.SearchIdentity == nil || current.SearchIdentity.TMDBID != 100 {
		t.Fatal("fast title analysis replaced actual intended identity")
	}
}

func TestContentSubtitleEvidenceDoesNotInventTracksFromAudioLanguage(t *testing.T) {
	if len(contentSubtitleEvidence(SiteSharePreviewEntry{Path: "Show.S01E01.JPN.ENG.mkv"})) != 0 {
		t.Fatal("audio name became a subtitle track")
	}
	for _, file := range []struct{ path, language string }{{"Show.S01E01.en.srt", "en"}, {"Show.S01E01.ja.ass", "ja"}, {"Show.S01E01.ko.srt", "ko"}, {"Show.S01E01.zh-Hans.srt", "zh"}} {
		got := contentSubtitleEvidence(SiteSharePreviewEntry{Path: file.path})
		if len(got) != 1 || got[0].Language != file.language || got[0].Source != "sidecar_filename" {
			t.Fatalf("sidecar evidence=%+v", got)
		}
	}
}
