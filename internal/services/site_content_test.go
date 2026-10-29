package services

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/authz"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
	downloadpkg "github.com/yuanjing-hash/OhMyCine-Server/pkg/downloader"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/resourcecontent"
	sitepkg "github.com/yuanjing-hash/OhMyCine-Server/pkg/site"
)

func inspectionTorrent(paths ...string) []byte {
	return inspectionTorrentWithSize(100, paths...)
}
func inspectionTorrentWithSize(size int64, paths ...string) []byte {
	var info bytes.Buffer
	info.WriteString("d5:filesl")
	for _, file := range paths {
		fmt.Fprintf(&info, "d6:lengthi%de4:pathl", size)
		for _, part := range strings.Split(file, "/") {
			fmt.Fprintf(&info, "%d:%s", len(part), part)
		}
		info.WriteString("ee")
	}
	pieceBytes := int((size*int64(len(paths))+16383)/16384) * 20
	fmt.Fprintf(&info, "e4:name12:Fixture Show12:piece lengthi16384e6:pieces%d:", pieceBytes)
	info.Write(bytes.Repeat([]byte{1}, pieceBytes))
	info.WriteByte('e')
	return append(append([]byte("d4:info"), info.Bytes()...), 'e')
}

func TestContentInspectionReadsShareWithoutReceivingConfigurationAndKeepsSafeFiles(t *testing.T) {
	f, s, driver, claim := newShareAnalysisFixture(t)
	if err := f.db.Delete(&models.Downloader{}, "id = ?", f.downloader.ID).Error; err != nil {
		t.Fatal(err)
	}
	token := issueAnalysisClaim(t, s, claim)
	preview, err := s.PreviewContent(context.Background(), f.actor, ContentPreviewInput{ResultToken: token})
	if err != nil || !preview.Complete || driver.reads != 2 || preview.TotalCount != 4 {
		t.Fatalf("preview=%+v reads=%d err=%v", preview, driver.reads, err)
	}
	var selectedToken string
	for _, file := range preview.Files {
		if strings.HasSuffix(file.Path, "/13.mkv") {
			selectedToken = file.Token
			if !reflect.DeepEqual(file.Episodes, []int{13}) || file.WorkStatus != "matched" {
				t.Fatalf("file facts=%+v", file)
			}
		}
	}
	selection, err := s.SelectContent(context.Background(), f.actor, ContentSelectionInput{ResultToken: token, PreviewToken: preview.Token, SelectedEntryTokens: []string{selectedToken}})
	if err != nil || selection.SelectedBytes != 100 || selection.VideoCount != 1 {
		t.Fatalf("selection=%+v err=%v", selection, err)
	}
	resolved, err := s.ResolveContentSelection(context.Background(), f.actor, ContentSelectionInput{ResultToken: token, SelectionToken: selection.Token}, claim)
	if err != nil || resolved.Share == nil || len(resolved.Share.Files) != 1 || resolved.Share.Files[0].ID != "provider-private-13" {
		t.Fatalf("private selection=%+v err=%v", resolved, err)
	}
	encoded, _ := json.Marshal([]any{preview, selection})
	for _, secret := range []string{"provider-private", "private-share-code", "115.com", "password=", "infohash", "\"index\""} {
		if strings.Contains(string(encoded), secret) {
			t.Fatalf("public projection exposed private source field %q", secret)
		}
	}
	var tasks int64
	f.db.Model(&models.DownloadTask{}).Count(&tasks)
	if tasks != 0 {
		t.Fatal("read-only inspection created a download")
	}
	foreign := f.actor
	foreign.User.ID++
	if _, err := s.PreviewContent(context.Background(), foreign, ContentPreviewInput{ResultToken: token}); err == nil {
		t.Fatal("foreign actor read the source claim")
	}
}

func TestContentInspectionRejectsAccountChangeDuringRead(t *testing.T) {
	f, s, driver, claim := newShareAnalysisFixture(t)
	driver.onRead = func() {
		_ = f.db.Model(&models.Connection{}).Where("id = ?", *f.storage.ConnectionID).Update("account_id", "replacement-account").Error
	}
	preview, err := s.PreviewContent(context.Background(), f.actor, ContentPreviewInput{ResultToken: issueAnalysisClaim(t, s, claim)})
	if err == nil || len(preview.Files) != 0 {
		t.Fatalf("stale account published files: %+v err=%v", preview, err)
	}
}

func TestContentInspectionParsesTorrentWithoutAnyDownloaderAndFreezesOriginalIndices(t *testing.T) {
	s, stub, actor, _, downloads, downloaders := siteFixture(t)
	raw := inspectionTorrent("Season 01/01.mkv", "Season 01/13.mkv", "Season 01/13.zh-Hans.srt", "Season 01/15.mkv")
	s.adapters["mikan"] = &stubResolverAdapter{stubSiteAdapter: &stubSiteAdapter{kind: "mikan"}, resolved: sitepkg.Source{Torrent: raw}}
	created, err := s.Create(context.Background(), actor, SiteInput{Name: "Content fixture", Kind: "mikan", BaseURL: "https://mikanani.me", Enabled: true, Priority: 1, TimeoutSeconds: 15, RateLimitPerMinute: 120}, RequestContext{})
	if err != nil {
		t.Fatal(err)
	}
	claim := siteResultClaim{ActorID: actor.User.ID, SiteID: created.ID, SiteRevision: created.Revision, TorrentID: "42", Title: "Advertisement 1-99", SearchIdentity: analysisIdentity(), ExpiresAt: s.now().Add(time.Minute)}
	token := issueAnalysisClaim(t, s, claim)
	preview, err := s.PreviewContent(context.Background(), actor, ContentPreviewInput{ResultToken: token})
	if err != nil || !preview.Complete || preview.TotalCount != 4 {
		t.Fatalf("content=%+v err=%v", preview, err)
	}
	var selected []string
	for _, file := range preview.Files {
		if strings.HasSuffix(file.Path, "/13.mkv") || strings.HasSuffix(file.Path, "/15.mkv") {
			selected = append(selected, file.Token)
		}
	}
	resolved, err := s.ResolveContentSelection(context.Background(), actor, ContentSelectionInput{ResultToken: token, PreviewToken: preview.Token, SelectedEntryTokens: selected}, claim)
	if err != nil || resolved.Torrent == nil || resolved.SelectedBytes != 200 || len(resolved.Torrent.Files) != 2 || resolved.Torrent.Files[0].Index != 1 || resolved.Torrent.Files[1].Index != 3 {
		t.Fatalf("selected=%+v err=%v", resolved, err)
	}
	original, _ := resourcecontent.ParseTorrent(raw, resourcecontent.Limits{})
	if resolved.Manifest.Identity != original.Identity || !bytes.Equal(resolved.Manifest.MetainfoBytes, raw) {
		t.Fatal("selection rewrote or trimmed the original torrent")
	}
	if stub.downloads != 0 {
		t.Fatal("inspection used a downloader")
	}
	var count int64
	s.db.Model(&models.DownloadTask{}).Count(&count)
	if count != 0 {
		t.Fatal("inspection submitted a task")
	}
	client := &stubDownloadClient{}
	registry := downloadpkg.NewRegistry()
	if err := registry.Register(models.DownloaderTypeQBittorrent, downloadpkg.Capabilities{SelectiveFiles: true, Pause: true, Resume: true}, func(downloadpkg.Config) (downloadpkg.Client, error) { return client, nil }); err != nil {
		t.Fatal(err)
	}
	downloaders.registry = registry
	downloader, err := downloaders.Create(actor, DownloaderInput{Name: "Selective fixture", Type: models.DownloaderTypeQBittorrent, BaseURL: "http://executor.example.test", Enabled: true}, RequestContext{})
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	storage := models.Storage{Name: "Selection library", NameNormalized: "selection-library", Type: models.StorageTypeLocal, RootPath: root, RootPathNormalized: root, Enabled: true, Capabilities: `{}`}
	if err := s.db.Create(&storage).Error; err != nil {
		t.Fatal(err)
	}
	var profile models.MediaClassificationProfile
	if err := s.db.First(&profile, "code = ?", "default-v1").Error; err != nil {
		t.Fatal(err)
	}
	library := models.MediaLibrary{Name: "Selection library", NameNormalized: "selection-library", StorageID: storage.ID, ProfileID: profile.ID, ProfileRevision: profile.Revision, RelativeRoot: "/", Enabled: true, Status: models.MediaLibraryStatusListening, TransferMode: models.MediaLibraryTransferMove, VideoExtensionsJSON: `[]`, STRMAssetExtraExtensionsJSON: `[]`, IgnorePatternsJSON: `[]`}
	if err := s.db.Create(&library).Error; err != nil {
		t.Fatal(err)
	}
	receipt, err := s.SelectContent(context.Background(), actor, ContentSelectionInput{ResultToken: token, PreviewToken: preview.Token, SelectedEntryTokens: selected})
	if err != nil {
		t.Fatal(err)
	}
	queued, err := s.Download(context.Background(), actor, SiteDownloadInput{ResultToken: token, ContentSelectionToken: receipt.Token, DownloaderID: downloader.ID, MediaLibraryID: &library.ID}, RequestContext{})
	if err != nil {
		t.Fatalf("actual opaque handoff failed: %v", err)
	}
	var task models.DownloadTask
	if err := s.db.First(&task, "id = ?", queued.ID).Error; err != nil {
		t.Fatal(err)
	}
	plaintext, err := downloads.credentials.Decrypt(downloadSourcePurpose(task.ID), task.SourceCiphertext)
	if err != nil {
		t.Fatal(err)
	}
	var frozen downloadSourceEnvelope
	if json.Unmarshal([]byte(plaintext), &frozen) != nil || frozen.ContentSelection == nil || frozen.SelectionMode != "files" || !bytes.Equal(frozen.Torrent, raw) || frozen.ContentSelection.Files[0].Index != 1 || frozen.ContentSelection.Files[1].Index != 3 {
		t.Fatal("Site handoff lost the exact selected indices or original torrent")
	}
	if client.submits != 0 {
		t.Fatal("preparation or local queue persistence invoked external Submit")
	}
}

func TestContentInspectionAssociationReadIsSeparateFromAcquireAndForeignIDsCannotExpandIt(t *testing.T) {
	f, s, driver, claim := newShareAnalysisFixture(t)
	actor := f.actor
	actor.Permissions = map[string]struct{}{}
	for code := range f.actor.Permissions {
		if code != authz.PermissionConnectionsRead && code != authz.PermissionMediaLibrariesRead {
			actor.Permissions[code] = struct{}{}
		}
	}
	if err := f.db.Where("user_id = ? AND permission_code IN ?", actor.User.ID, []string{authz.PermissionConnectionsRead, authz.PermissionMediaLibrariesRead}).Delete(&models.UserAuthorizationRule{}).Error; err != nil {
		t.Fatal(err)
	}
	actor.ResourceAccessPolicies = map[string]ResourceAccessPolicy{models.ResourceAccessScopeDownloaderUse: {Scope: models.ResourceAccessScopeDownloaderUse, Mode: models.ResourceAccessModeAllowlist, ResourceIDs: []string{f.downloader.ID}}}
	raw, _ := json.Marshal([]string{f.downloader.ID})
	if err := f.db.Create(&models.UserResourceAccessPolicy{UserID: actor.User.ID, Scope: models.ResourceAccessScopeDownloaderUse, Mode: models.ResourceAccessModeAllowlist, ResourceIDsJSON: string(raw)}).Error; err != nil {
		t.Fatal(err)
	}
	if err := f.db.Model(&models.Downloader{}).Where("id = ?", f.downloader.ID).Updates(map[string]any{"enabled": false, "provider_directory_id": ""}).Error; err != nil {
		t.Fatal(err)
	}
	claim.AuthorizationScope = discoveryAuthorizationScope(actor)
	token := issueAnalysisClaim(t, s, claim)
	preview, err := s.PreviewContent(context.Background(), actor, ContentPreviewInput{ResultToken: token})
	if err != nil || !preview.Complete {
		t.Fatalf("authorized read required an executable receiver: %v", err)
	}
	reads := driver.reads
	if _, err := s.PreviewContent(context.Background(), actor, ContentPreviewInput{ResultToken: token, DownloaderID: "foreign-downloader"}); err == nil || driver.reads != reads {
		t.Fatal("client downloader ID expanded read authority")
	}
	if _, err := s.PreviewShare(context.Background(), actor, token, f.downloader.ID); err == nil {
		t.Fatal("legacy receive preview acquired through the read-only exception")
	}
}

func TestContentInspectionRejectsAuthzVersionChangeAndSelectionWithoutCreatePermission(t *testing.T) {
	f, s, driver, claim := newShareAnalysisFixture(t)
	driver.onRead = func() {
		_ = f.db.Model(&models.User{}).Where("id = ?", f.actor.User.ID).Update("authz_version", f.actor.User.AuthzVersion+1).Error
	}
	preview, err := s.PreviewContent(context.Background(), f.actor, ContentPreviewInput{ResultToken: issueAnalysisClaim(t, s, claim)})
	if err == nil || len(preview.Files) != 0 {
		t.Fatal("revoked authorization published late files")
	}
	actor := f.actor
	actor.User.AuthzVersion++
	actor.Permissions = map[string]struct{}{}
	for code := range f.actor.Permissions {
		if code != authz.PermissionDownloadsCreate {
			actor.Permissions[code] = struct{}{}
		}
	}
	if err := f.db.Where("user_id = ? AND permission_code = ?", actor.User.ID, authz.PermissionDownloadsCreate).Delete(&models.UserAuthorizationRule{}).Error; err != nil {
		t.Fatal(err)
	}
	if _, err := s.ResolveContentSelection(context.Background(), actor, ContentSelectionInput{}, claim); ErrorCode(err) != CodePermissionDenied {
		t.Fatalf("service selection bypassed downloads.create: %v", err)
	}
}

func TestContentInspectionSingleflightWaiterCancellation(t *testing.T) {
	s := &SiteService{now: time.Now}
	var starts atomic.Int32
	started := make(chan struct{})
	finish := make(chan struct{})
	stopped := make(chan struct{})
	load := func(ctx context.Context) (inspectedContent, error) {
		starts.Add(1)
		close(started)
		select {
		case <-finish:
			return inspectedContent{RootName: "safe", ExpiresAt: time.Now().Add(time.Minute)}, nil
		case <-ctx.Done():
			close(stopped)
			return inspectedContent{}, ctx.Err()
		}
	}
	first, cancelFirst := context.WithCancel(context.Background())
	firstResult := make(chan error, 1)
	secondResult := make(chan error, 1)
	go func() { _, err := s.readContentShared(first, "same-scope", load); firstResult <- err }()
	<-started
	go func() { _, err := s.readContentShared(context.Background(), "same-scope", load); secondResult <- err }()
	deadline := time.Now().Add(time.Second)
	for {
		state := s.contents()
		state.mu.Lock()
		waiters := state.flights["same-scope"].waiters
		state.mu.Unlock()
		if waiters == 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("second waiter did not attach")
		}
		time.Sleep(time.Millisecond)
	}
	cancelFirst()
	if err := <-firstResult; !errors.Is(err, context.Canceled) {
		t.Fatalf("first cancel=%v", err)
	}
	select {
	case <-stopped:
		t.Fatal("first caller cancelled another waiter")
	default:
	}
	close(finish)
	if err := <-secondResult; err != nil {
		t.Fatal(err)
	}
	if starts.Load() != 1 {
		t.Fatalf("duplicate metadata reads=%d", starts.Load())
	}
	all, cancelAll := context.WithCancel(context.Background())
	allStarted := make(chan struct{})
	allStopped := make(chan struct{})
	go func() {
		_, _ = s.readContentShared(all, "other-scope", func(ctx context.Context) (inspectedContent, error) {
			close(allStarted)
			<-ctx.Done()
			close(allStopped)
			return inspectedContent{}, ctx.Err()
		})
	}()
	<-allStarted
	cancelAll()
	select {
	case <-allStopped:
	case <-time.After(time.Second):
		t.Fatal("last waiter left an owned read running")
	}
}

func TestContentNumericLeavesKeepRealParentAndFilenameEvidence(t *testing.T) {
	s := NewSiteService(nil, nil, nil, nil, zerolog.Nop())
	claim := siteResultClaim{Title: "Unrelated 1-99", SearchIdentity: analysisIdentity()}
	entries := analysisEntries("Fixture Show [01-52]/Season 01/01.mkv", "Fixture Show [01-52]/Season 01/13.mkv", "Fixture Show [01-52]/Season 01/13.zh-Hans.srt", "Other Work/Season 01/15.mkv", "52.mkv")
	analysis, _ := s.analyzeShareEntries(context.Background(), Actor{}, claim, entries)
	if len(analysis.Groups) == 0 || !reflect.DeepEqual(analysis.Groups[0].Episodes, []int{1, 13}) || analysis.Groups[0].Status != mediaRecognitionStatusMatched {
		t.Fatalf("actual parent facts=%+v", analysis)
	}
	bare, _ := shareFileFacts(entries[4], claim)
	if !reflect.DeepEqual(bare.Episodes, []int{52}) || bare.Title != "" {
		t.Fatalf("bare leaf lost filename facts or borrowed advertisement: %+v", bare)
	}
}
