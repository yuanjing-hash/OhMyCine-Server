package services

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/authz"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
	downloadpkg "github.com/yuanjing-hash/OhMyCine-Server/pkg/downloader"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/downloader/qbittorrent"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/resourcecontent"
	sitepkg "github.com/yuanjing-hash/OhMyCine-Server/pkg/site"
	"gorm.io/gorm"
)

// External provider fixture only: the production qB HTTP adapter, source
// envelope, worker, transfer and catalog writer remain unwrapped. No swarm or
// media download is used. Only two zero-filled 16-MiB local test files and
// two 1-KiB sidecars are materialized, after the exact native wanted readback.
type integratedSelectionQB struct {
	mu                     sync.Mutex
	raw                    []byte
	manifest               resourcecontent.Manifest
	wanted                 map[int]bool
	priorities             map[int]int
	workspace, tags, state string
	exists, complete       bool
	adds, resumes          int
}

func (q *integratedSelectionQB) handler(t *testing.T) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q.mu.Lock()
		defer q.mu.Unlock()
		fail := func(err error) { t.Error(err); http.Error(w, "synthetic qB contract failed", http.StatusBadRequest) }
		switch r.URL.Path {
		case "/api/v2/auth/login":
			http.SetCookie(w, &http.Cookie{Name: "SID", Value: "loopback-fixture"})
			_, _ = io.WriteString(w, "Ok.")
		case "/api/v2/app/version":
			_, _ = io.WriteString(w, "v5.0.0")
		case "/api/v2/app/webapiVersion":
			_, _ = io.WriteString(w, "2.11.3")
		case "/api/v2/torrents/info":
			if !q.exists || r.URL.Query().Get("tag") != "" && !strings.Contains(q.tags, r.URL.Query().Get("tag")) {
				_, _ = io.WriteString(w, "[]")
				return
			}
			_ = json.NewEncoder(w).Encode([]map[string]any{{"hash": q.manifest.Identity.V1, "name": q.manifest.Name, "state": q.state, "tags": q.tags, "save_path": q.workspace, "progress": 0.01, "total_size": 52*minimumAutomaticTransferVideoBytes + 2048, "downloaded": 2*minimumAutomaticTransferVideoBytes + 2048}})
		case "/api/v2/torrents/files":
			rows := []map[string]any{}
			for index := len(q.manifest.Files) - 1; index >= 0; index-- {
				file := q.manifest.Files[index]
				progress := 0.0
				if q.complete && q.wanted[file.Index] {
					progress = 1
				}
				rows = append(rows, map[string]any{"index": file.Index*3 + 7, "name": q.manifest.Name + "/" + file.Path, "size": file.Size, "priority": q.priorities[file.Index*3+7], "progress": progress})
			}
			_ = json.NewEncoder(w).Encode(rows)
		case "/api/v2/torrents/add":
			if err := r.ParseMultipartForm(4 << 20); err != nil {
				fail(err)
				return
			}
			file, _, err := r.FormFile("torrents")
			if err != nil {
				fail(err)
				return
			}
			raw, err := io.ReadAll(file)
			_ = file.Close()
			if err != nil || !bytes.Equal(raw, q.raw) || r.FormValue("paused") != "true" || r.FormValue("urls") != "" || r.FormValue("autoTMM") != "false" {
				fail(fmt.Errorf("not original stopped metainfo"))
				return
			}
			q.exists, q.workspace, q.tags, q.state = true, r.FormValue("savepath"), r.FormValue("tags"), "stoppedDL"
			q.adds++
			_, _ = io.WriteString(w, "Ok.")
		case "/api/v2/torrents/filePrio":
			if err := r.ParseForm(); err != nil {
				fail(err)
				return
			}
			priority, err := strconv.Atoi(r.Form.Get("priority"))
			if err != nil {
				fail(err)
				return
			}
			for _, raw := range strings.Split(r.Form.Get("id"), "|") {
				index, e := strconv.Atoi(raw)
				if e != nil || (index-7)%3 != 0 || index < 7 || (index-7)/3 >= len(q.manifest.Files) {
					fail(fmt.Errorf("source/display index substituted for native index: %s", raw))
					return
				}
				q.priorities[index] = priority
			}
		case "/api/v2/torrents/addTags", "/api/v2/torrents/removeTags":
			_ = r.ParseForm()
			if strings.HasSuffix(r.URL.Path, "addTags") {
				q.tags += "," + r.Form.Get("tags")
				return
			}
			kept := []string{}
			for _, tag := range strings.Split(q.tags, ",") {
				if tag != r.Form.Get("tags") {
					kept = append(kept, tag)
				}
			}
			q.tags = strings.Join(kept, ",")
		case "/api/v2/torrents/setCategory":
		case "/api/v2/torrents/setLocation":
			fail(fmt.Errorf("shared selected workspace was moved"))
		case "/api/v2/torrents/pause", "/api/v2/torrents/stop":
			q.state = "stoppedDL"
		case "/api/v2/torrents/resume", "/api/v2/torrents/start":
			for _, file := range q.manifest.Files {
				expected := 0
				if q.wanted[file.Index] {
					expected = 1
				}
				if q.priorities[file.Index*3+7] != expected {
					fail(fmt.Errorf("resumed unverified wanted set at original index %d", file.Index))
					return
				}
				if !q.wanted[file.Index] {
					continue
				}
				target := filepath.Join(q.workspace, q.manifest.Name, filepath.FromSlash(file.Path))
				if err := os.MkdirAll(filepath.Dir(target), 0700); err != nil {
					fail(err)
					return
				}
				handle, err := os.OpenFile(target, os.O_CREATE|os.O_RDWR, 0600)
				if err != nil {
					fail(err)
					return
				}
				err = handle.Truncate(file.Size)
				_ = handle.Close()
				if err != nil {
					fail(err)
					return
				}
			}
			q.complete, q.state = true, "uploading"
			q.resumes++
		default:
			fail(fmt.Errorf("unexpected qB endpoint: %s", r.URL.Path))
		}
	})
}

func integratedEpisodeTorrent(t *testing.T) ([]byte, resourcecontent.Manifest) {
	t.Helper()
	var info bytes.Buffer
	info.WriteString("d5:filesl")
	var total int64
	for index := 1; index <= 54; index++ {
		name, size := fmt.Sprintf("Fixture.Show.S01E%02d.1080p.mkv", index), minimumAutomaticTransferVideoBytes
		if index > 52 {
			name, size = fmt.Sprintf("Fixture.Show.S01E%02d.1080p.zh-Hans.srt", 13+(index-53)*2), int64(1024)
		}
		fmt.Fprintf(&info, "d6:lengthi%de4:pathl9:Season 01%d:%see", size, len(name), name)
		total += size
	}
	pieceBytes := int((total+16383)/16384) * 20
	fmt.Fprintf(&info, "e4:name12:Fixture Show12:piece lengthi16384e6:pieces%d:", pieceBytes)
	info.Write(bytes.Repeat([]byte{1}, pieceBytes))
	info.WriteByte('e')
	raw := append(append([]byte("d4:info"), info.Bytes()...), 'e')
	manifest, err := resourcecontent.ParseTorrent(raw, resourcecontent.Limits{})
	if err != nil {
		t.Fatal(err)
	}
	return raw, manifest
}

func TestContent52EpisodeMissingSubsetThroughActualQBTransferAndCatalog(t *testing.T) {
	sites, _, actor, _, downloads, downloaders := siteFixture(t)
	queue := downloads.queue
	actor.Permissions[authz.PermissionMediaLibrariesCreate] = struct{}{}
	actor.Permissions[authz.PermissionMediaLibrariesScan] = struct{}{}
	persistFixtureAuthority(t, queue.db, actor)
	queue.SetClock(realClock{})
	raw, manifest := integratedEpisodeTorrent(t)
	provider := &integratedSelectionQB{raw: raw, manifest: manifest, wanted: map[int]bool{12: true, 14: true, 52: true, 53: true}, priorities: map[int]int{}}
	upstream := httptest.NewServer(provider.handler(t))
	defer upstream.Close()
	registry := downloadpkg.NewRegistry()
	if err := registry.Register(models.DownloaderTypeQBittorrent, qbittorrent.Capabilities, func(config downloadpkg.Config) (downloadpkg.Client, error) { return qbittorrent.New(config) }); err != nil {
		t.Fatal(err)
	}
	downloaders.registry = registry
	executor, err := downloaders.Create(actor, DownloaderInput{Name: "Actual loopback qB", Type: models.DownloaderTypeQBittorrent, BaseURL: upstream.URL, Enabled: true}, RequestContext{})
	if err != nil {
		t.Fatal(err)
	}

	libraries := NewMediaLibraryService(queue.db, queue.audit, zerolog.Nop())
	structure := NewMediaLibraryStructureService(queue.db, queue.audit, queue, nil, zerolog.Nop())
	libraries.SetStructureService(structure)
	installHydrationMetadata(t, libraries, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":100,"name":"Fixture Show","original_name":"Fixture Show","first_air_date":"2026-01-01","number_of_seasons":1,"seasons":[{"season_number":1,"episode_count":52}],"genres":[{"id":18,"name":"Drama"}],"origin_country":["CN"]}`)
	})
	downloads.SetMetadataSettings(libraries.metadata)
	sites.SetMetadataSettings(libraries.metadata)
	root := t.TempDir()
	storage := models.Storage{Name: "Integrated target", NameNormalized: "integrated-target", Type: models.StorageTypeLocal, RootPath: root, RootPathNormalized: strings.ToLower(root), Enabled: true, Capabilities: `{}`}
	if err := queue.db.Create(&storage).Error; err != nil {
		t.Fatal(err)
	}
	var profile models.MediaClassificationProfile
	if err := queue.db.First(&profile, "code=?", "default-v1").Error; err != nil {
		t.Fatal(err)
	}
	input := testLibraryInput("Integrated TV", storage, profile, false)
	noArtifacts := false
	input.MetadataArtifactsEnabled = &noArtifacts
	input.TransferMode, input.ConflictPolicy = models.MediaLibraryTransferCopy, models.MediaLibraryConflictAsk
	library, err := libraries.Create(context.Background(), actor, input, RequestContext{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := libraries.Scan(context.Background(), actor, library.ID, "full"); err != nil {
		t.Fatal(err)
	}
	if err := queue.db.Model(&models.MediaLibrary{}).Where("id=?", library.ID).Updates(map[string]any{"enabled": true, "status": models.MediaLibraryStatusListening}).Error; err != nil {
		t.Fatal(err)
	}
	diagnosisJob, err := queue.Claim([]string{JobTypeMediaLibraryStructureDiagnosis})
	if err != nil || diagnosisJob == nil {
		t.Fatalf("baseline diagnosis=%+v err=%v", diagnosisJob, err)
	}
	if result := NewMediaLibraryStructureDiagnosisWorker(structure).Run(context.Background(), workerRuntime{queue: queue, job: *diagnosisJob}, *diagnosisJob); result.ErrorCode != "" || result.RetryAt != nil || result.Wait != nil {
		t.Fatalf("baseline diagnosis=%+v", result)
	}
	if err := queue.Complete(diagnosisJob.Job.ID, diagnosisJob.LeaseToken); err != nil {
		t.Fatal(err)
	}
	transfers := NewTransferService(queue.db, queue.audit, queue, zerolog.Nop())
	transfers.SetMediaLibraryReconciler(libraries)
	transfers.SetDownloaderService(downloaders)
	downloads.SetTransferService(transfers)
	sites.adapters["mikan"] = &stubResolverAdapter{stubSiteAdapter: &stubSiteAdapter{kind: "mikan"}, resolved: sitepkg.Source{Torrent: raw}}
	site, err := sites.Create(context.Background(), actor, SiteInput{Name: "52 episode site", Kind: "mikan", BaseURL: "https://mikanani.me", Enabled: true, Priority: 1, TimeoutSeconds: 15, RateLimitPerMinute: 120}, RequestContext{})
	if err != nil {
		t.Fatal(err)
	}
	claim := siteResultClaim{ActorID: actor.User.ID, SiteID: site.ID, SiteRevision: site.Revision, TorrentID: "42", Title: "Fixture Show [01-52]", SearchIdentity: analysisIdentity(), ExpiresAt: sites.now().Add(5 * time.Minute)}
	token := issueAnalysisClaim(t, sites, claim)
	prepared, err := sites.prepareFollowContent(context.Background(), actor, token, 1, []int{13, 15}, SubtitlePolicy{Mode: "prefer", Languages: []string{"zh-Hans", "zh-Hant"}})
	if err != nil || !reflect.DeepEqual(prepared.Episodes, []int{13, 15}) || prepared.UnconfirmedSubtitles != 0 {
		t.Fatalf("prepare=%+v err=%v", prepared, err)
	}
	queued, err := sites.Download(context.Background(), actor, SiteDownloadInput{ResultToken: token, ContentMode: "files", ContentSelectionToken: prepared.SelectionToken, DownloaderID: executor.ID, MediaLibraryID: &library.ID}, RequestContext{})
	if err != nil {
		t.Fatal(err)
	}
	var task models.DownloadTask
	if err := queue.db.First(&task, "id=?", queued.ID).Error; err != nil {
		t.Fatal(err)
	}
	plain, err := downloads.credentials.Decrypt(downloadSourcePurpose(task.ID), task.SourceCiphertext)
	if err != nil {
		t.Fatal(err)
	}
	var frozen downloadSourceEnvelope
	if err := json.Unmarshal([]byte(plain), &frozen); err != nil {
		t.Fatal(err)
	}
	if frozen.ContentSelection == nil || frozen.SelectionMode != "files" || !bytes.Equal(frozen.Torrent, raw) || len(frozen.ContentSelection.Files) != 4 {
		t.Fatal("opaque source did not freeze four exact original leaves")
	}
	for _, file := range frozen.ContentSelection.Files {
		if !provider.wanted[file.Index] {
			t.Fatalf("unwanted original file frozen: %+v", file)
		}
	}
	if provider.adds != 0 {
		t.Fatal("inspection/preparation created an external downloader task")
	}
	downloadJob, err := queue.Claim([]string{"download"})
	if err != nil || downloadJob == nil {
		var job models.Job
		_ = queue.db.First(&job, "id=?", task.JobID).Error
		readiness, readinessErr := libraryReadiness(queue.db, library.ID)
		t.Fatalf("download claim=%+v err=%v job_status=%s job_type=%s readiness=%+v readiness_err=%v", downloadJob, err, job.Status, job.JobType, readiness, readinessErr)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	result := NewDownloadWorker(downloads).Run(ctx, workerRuntime{queue: queue, job: *downloadJob}, *downloadJob)
	if result.ErrorCode != "" || result.RetryAt != nil || result.Wait != nil {
		t.Fatalf("Download actual qB pipeline=%+v", result)
	}
	if err := queue.Complete(downloadJob.Job.ID, downloadJob.LeaseToken); err != nil {
		t.Fatal(err)
	}
	transferJob, err := queue.Claim([]string{"transfer"})
	if err != nil || transferJob == nil {
		t.Fatalf("transfer claim=%+v err=%v", transferJob, err)
	}
	result = NewTransferWorker(transfers).Run(ctx, workerRuntime{queue: queue, job: *transferJob}, *transferJob)
	if result.ErrorCode != "" || result.RetryAt != nil || result.Wait != nil {
		t.Fatalf("Transfer/catalog pipeline=%+v", result)
	}
	var transfer models.TransferTask
	if err := queue.db.Where("download_task_id=?", task.ID).First(&transfer).Error; err != nil {
		t.Fatal(err)
	}
	if transfer.CatalogPublishedAt == nil || transfer.Phase != models.TransferTaskStatusCompleted {
		t.Fatalf("real catalog publication not completed: %+v", transfer)
	}
	episodes := []int{}
	if err := libraries.withCatalogRead(ctx, []uint{library.ID}, func(_ *gorm.DB, reader *CatalogReader) error {
		var rows []models.MediaLibraryEntry
		if err := reader.Entries().Where("library_id=?", library.ID).Find(&rows).Error; err != nil {
			return err
		}
		for _, row := range rows {
			if row.TMDBID == nil || *row.TMDBID != 100 || row.Season == nil || *row.Season != 1 || row.Episode == nil {
				return fmt.Errorf("published identity lost: %+v", row)
			}
			episodes = append(episodes, *row.Episode)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	sort.Ints(episodes)
	if !reflect.DeepEqual(episodes, []int{13, 15}) {
		t.Fatalf("catalog expanded actual missing subset: %v", episodes)
	}
	var managed []models.MediaManagedItem
	if err := queue.db.Where("transfer_task_id=? AND active=?", transfer.ID, true).Find(&managed).Error; err != nil {
		t.Fatal(err)
	}
	if len(managed) != 4 {
		t.Fatalf("selected sidecars lost or extra outputs imported: %d", len(managed))
	}
	provider.mu.Lock()
	defer provider.mu.Unlock()
	if provider.adds != 1 || provider.resumes < 1 {
		t.Fatalf("unexpected native calls adds=%d resumes=%d", provider.adds, provider.resumes)
	}
	if _, err := os.Stat(filepath.Join(provider.workspace, manifest.Name, "Season 01", "Fixture.Show.S01E14.1080p.mkv")); !os.IsNotExist(err) {
		t.Fatal("unselected E14 was materialized")
	}
}
