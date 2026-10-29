package pan115

import (
	"bytes"
	"context"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"testing"

	sdk "github.com/SheltonZhu/115driver/pkg/driver"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/cloud"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/resourcecontent"
	"golang.org/x/time/rate"
)

type selectedMemoryStore struct {
	mu           sync.Mutex
	rows         map[string]cloud.OperationReceipt
	resources    map[string]string
	failConfirm  string
	failDispatch string
	failed       bool
}

func (s *selectedMemoryStore) Claim(_ context.Context, key string, r cloud.OperationReceipt) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.rows == nil {
		s.rows = map[string]cloud.OperationReceipt{}
	}
	if s.resources == nil {
		s.resources = map[string]string{}
	}
	key = r.Provider + "/" + r.AccountID + "/" + key
	if _, exists := s.rows[r.ID]; exists {
		return errors.New("duplicate")
	}
	if _, exists := s.resources[key]; exists {
		return errors.New("resource conflict")
	}
	r.Payload = append([]byte(nil), r.Payload...)
	s.rows[r.ID], s.resources[key] = r, r.ID
	return nil
}
func (s *selectedMemoryStore) ResourceClaim(_ context.Context, key string) (cloud.OperationReceipt, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for resource, id := range s.resources {
		if strings.HasSuffix(resource, "/"+key) {
			r := s.rows[id]
			r.Payload = append([]byte(nil), r.Payload...)
			return r, nil
		}
	}
	return cloud.OperationReceipt{}, cloud.ErrOperationNotFound
}
func (s *selectedMemoryStore) ReleaseResourceClaim(_ context.Context, key string, version uint64, r cloud.OperationReceipt) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	old, ok := s.rows[r.ID]
	resource := r.Provider + "/" + r.AccountID + "/" + key
	var rejection struct{ DefiniteRejection bool }
	_ = json.Unmarshal(r.Payload, &rejection)
	failed := r.State == "failed" && (old.State == "prepared" || old.State == "submitting" && rejection.DefiniteRejection)
	if !ok || old.Revision != version || r.Revision != version+1 || s.resources[resource] != r.ID || old.Kind != cloud.OfflineResourceGuardKind || r.Kind != old.Kind || (r.State != "confirmed" && !failed) {
		return errors.New("resource cas")
	}
	r.Payload = append([]byte(nil), r.Payload...)
	s.rows[r.ID] = r
	delete(s.resources, resource)
	return nil
}
func (s *selectedMemoryStore) ReclaimResourceClaim(_ context.Context, key string, version uint64, r cloud.OperationReceipt) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	old, ok := s.rows[r.ID]
	resource := r.Provider + "/" + r.AccountID + "/" + key
	if !ok || old.State != "failed" || old.Kind != cloud.OfflineResourceGuardKind || old.Revision != version || r.Revision != version+1 || r.State != "prepared" || string(old.Payload) != string(r.Payload) || s.resources[resource] != "" {
		return errors.New("rearm cas")
	}
	s.rows[r.ID], s.resources[resource] = r, r.ID
	return nil
}
func (s *selectedMemoryStore) Create(_ context.Context, r cloud.OperationReceipt) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.rows == nil {
		s.rows = map[string]cloud.OperationReceipt{}
	}
	if _, ok := s.rows[r.ID]; ok {
		return errors.New("duplicate")
	}
	r.Payload = append([]byte(nil), r.Payload...)
	s.rows[r.ID] = r
	return nil
}
func (s *selectedMemoryStore) Load(_ context.Context, id string) (cloud.OperationReceipt, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.rows[id]
	if !ok {
		return r, cloud.ErrOperationNotFound
	}
	r.Payload = append([]byte(nil), r.Payload...)
	return r, nil
}
func (s *selectedMemoryStore) Update(_ context.Context, version uint64, r cloud.OperationReceipt) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	old, ok := s.rows[r.ID]
	if !ok || old.Revision != version {
		return errors.New("cas")
	}
	if r.State == "submitting" && s.failDispatch != "" && strings.HasSuffix(r.ID, s.failDispatch) && !s.failed {
		s.failed = true
		return errors.New("dispatch checkpoint failed")
	}
	if r.State == "confirmed" && strings.HasSuffix(r.ID, s.failConfirm) && s.failConfirm != "" && !s.failed {
		s.failed = true
		return errors.New("checkpoint lost")
	}
	r.Payload = append([]byte(nil), r.Payload...)
	s.rows[r.ID] = r
	return nil
}

type selectedTestSDK struct {
	*mutationTestSDK
	mu                                    sync.Mutex
	files                                 map[string]sdk.File
	children                              map[string][]string
	info                                  selectedTorrentInfo
	task                                  *sdk.OfflineTask
	creates, uploads, submits, cancels    int
	wanted                                []int
	seedBody                              []byte
	loseDirectory, loseUpload, loseSubmit bool
	applyUpload, applySubmit              bool
	parseErr                              error
}

func (s *selectedTestSDK) GetFile(id string) (*sdk.File, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	item, ok := s.files[id]
	if !ok {
		return nil, errors.New("not found")
	}
	return &item, nil
}
func (s *selectedTestSDK) ListPage(parent string, offset, limit int64, _ ...sdk.ListOption) (*[]sdk.File, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	all := s.children[parent]
	from := int(offset)
	if from > len(all) {
		from = len(all)
	}
	to := from + int(limit)
	if to > len(all) {
		to = len(all)
	}
	out := []sdk.File{}
	for _, id := range all[from:to] {
		out = append(out, s.files[id])
	}
	return &out, nil
}
func (s *selectedTestSDK) add(item sdk.File) {
	s.files[item.FileID] = item
	s.children[item.ParentID] = append(s.children[item.ParentID], item.FileID)
}
func (s *selectedTestSDK) Mkdir(parent, name string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.creates++
	id := "owned-task"
	if s.creates > 1 {
		id = fmt.Sprintf("owned-task-%d", s.creates)
	}
	s.add(sdk.File{FileID: id, ParentID: parent, Name: name, IsDirectory: true})
	if s.loseDirectory {
		return "", errors.New("response lost")
	}
	return id, nil
}
func (s *selectedTestSDK) UploadFastOrByMultipart(parent, name string, size int64, file *os.File, _ ...sdk.UploadMultipartOption) error {
	raw, err := io.ReadAll(file)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.uploads++
	s.seedBody = append([]byte(nil), raw...)
	if s.applyUpload {
		sum := sha1.Sum(raw)
		s.add(sdk.File{FileID: "owned-seed", ParentID: parent, Name: name, Size: size, Sha1: hex.EncodeToString(sum[:])})
	}
	if s.loseUpload {
		return errors.New("upload response lost")
	}
	return nil
}
func (s *selectedTestSDK) ParseOfflineTorrent(context.Context, string) (selectedTorrentInfo, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.info, s.parseErr
}
func (s *selectedTestSDK) AddOfflineWanted(_ context.Context, r selectedWantedRequest) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.submits++
	s.wanted = append([]int(nil), r.Wanted...)
	if s.applySubmit {
		s.task = &sdk.OfflineTask{InfoHash: r.InfoHash, Name: r.SavePath, DirId: r.DirectoryID, Status: 1, Size: 30}
	}
	if s.loseSubmit {
		return errors.New("submit response lost")
	}
	return nil
}
func (s *selectedTestSDK) ListOfflineTask(int64) (sdk.OfflineTaskResp, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	tasks := []*sdk.OfflineTask{}
	if s.task != nil {
		copy := *s.task
		tasks = append(tasks, &copy)
	}
	return sdk.OfflineTaskResp{Page: 1, PageCount: 1, Tasks: tasks}, nil
}
func (s *selectedTestSDK) DeleteOfflineTasks(hashes []string, deleteData bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if deleteData || len(hashes) != 1 || s.task == nil || hashes[0] != s.task.InfoHash {
		return errors.New("unsafe cancel")
	}
	s.cancels++
	s.task = nil
	return nil
}
func selectedFixture(t *testing.T) (*Client, *selectedTestSDK, *selectedMemoryStore, cloud.SelectiveOfflineRequest) {
	t.Helper()
	raw := []byte("d4:infod5:filesld6:lengthi10e4:pathl5:a.mkveed6:lengthi20e4:pathl5:b.mkveee4:name4:Show12:piece lengthi16384e6:pieces20:aaaaaaaaaaaaaaaaaaaaee")
	manifest, err := resourcecontent.ParseTorrent(raw, resourcecontent.Limits{})
	if err != nil {
		t.Fatal(err)
	}
	state := &selectedTestSDK{mutationTestSDK: &mutationTestSDK{bulkSDK: &bulkSDK{}}, files: map[string]sdk.File{}, children: map[string][]string{}, applyUpload: true, applySubmit: true, info: selectedTorrentInfo{InfoHash: manifest.Identity.V1, Name: manifest.Name, Files: []selectedTorrentFile{{Path: "Show/b.mkv", Size: 20}, {Path: "Show/a.mkv", Size: 10}}}}
	state.add(sdk.File{FileID: "root", ParentID: "0", Name: "Storage", IsDirectory: true})
	state.add(sdk.File{FileID: "intake", ParentID: "root", Name: "Receive", IsDirectory: true})
	store := &selectedMemoryStore{}
	client := selectedClient(state, store)
	selection := resourcecontent.Selection{Version: 1, Identity: manifest.Identity, Files: []resourcecontent.SelectedFile{{Index: manifest.Files[0].Index, Path: manifest.Files[0].Path, Size: manifest.Files[0].Size}}}
	return client, state, store, cloud.SelectiveOfflineRequest{ParentID: "intake", Torrent: raw, Authority: cloud.SelectiveOfflineAuthority{Tag: "omc-selection-fixture", StorageRootID: "root", Selection: selection}}
}
func selectedClient(state *selectedTestSDK, store *selectedMemoryStore) *Client {
	client := newMutationTestClient(state)
	client.offlineRate = rate.NewLimiter(rate.Inf, 1)
	client.selective = &selectiveOfflineState{store: store, account: "fixture-account"}
	return client
}
func completedSelected(state *selectedTestSDK, extra bool) {
	state.mu.Lock()
	defer state.mu.Unlock()
	state.task.Status = 2
	state.task.DelFileId = "owned-output"
	state.add(sdk.File{FileID: "owned-output", ParentID: "owned-task", Name: "Show", IsDirectory: true})
	state.add(sdk.File{FileID: "video-a", ParentID: "owned-output", Name: "a.mkv", Size: 10})
	if extra {
		state.add(sdk.File{FileID: "video-b", ParentID: "owned-output", Name: "b.mkv", Size: 20})
	}
}
func TestSelectedOfflinePreservesMetainfoAndNativeWantedMapping(t *testing.T) {
	c, s, store, request := selectedFixture(t)
	result, err := c.SubmitSelectedOffline(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(s.seedBody, request.Torrent) || fmt.Sprint(s.wanted) != "[1]" || result.Task.Completed || result.Task.BytesTotal == nil || *result.Task.BytesTotal != 10 {
		t.Fatalf("wanted/result=%v %+v", s.wanted, result)
	}
	completedSelected(s, false)
	cold := selectedClient(s, store)
	result, err = cold.ObserveSelectedOffline(context.Background(), result.Task.ID, request.Authority)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Complete || !result.Task.Completed || len(result.Files) != 1 || result.Files[0].Path != "a.mkv" || result.Files[0].Index != 1 || result.Files[0].Item.ID != "video-a" {
		t.Fatalf("result=%+v", result)
	}
	if _, err = cold.SubmitSelectedOffline(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	if s.creates != 1 || s.uploads != 1 || s.submits != 1 {
		t.Fatal("confirmed operation replayed", s.creates, s.uploads, s.submits)
	}
}
func TestSelectedOfflineLostRepliesReconcileWithoutAnotherMutation(t *testing.T) {
	for _, stage := range []string{"directory", "seed", "submit", "seed-not-applied", "submit-not-applied", "checkpoint-directory", "checkpoint-seed", "checkpoint-submit"} {
		t.Run(stage, func(t *testing.T) {
			c, s, store, request := selectedFixture(t)
			switch stage {
			case "directory":
				s.loseDirectory = true
			case "seed":
				s.loseUpload = true
			case "submit":
				s.loseSubmit = true
			case "seed-not-applied":
				s.loseUpload = true
				s.applyUpload = false
			case "submit-not-applied":
				s.loseSubmit = true
				s.applySubmit = false
			default:
				store.failConfirm = "/" + strings.TrimPrefix(stage, "checkpoint-")
			}
			_, err := c.SubmitSelectedOffline(context.Background(), request)
			if err == nil {
				t.Fatal("expected interrupted operation")
			}
			cold := selectedClient(s, store)
			_, err = cold.SubmitSelectedOffline(context.Background(), request)
			if strings.HasSuffix(stage, "not-applied") {
				if code, _ := cloud.ErrorInfo(err); code != cloud.CodeMutationUnknown {
					t.Fatal("uncertain operation changed", err)
				}
				_, _ = cold.SubmitSelectedOffline(context.Background(), request)
			} else if err != nil {
				t.Fatal(err)
			}
			if s.creates > 1 || s.uploads > 1 || s.submits > 1 {
				t.Fatal("unknown mutation replayed", s.creates, s.uploads, s.submits)
			}
		})
	}
}
func TestSelectedOfflineRejectsHashPathAndAmbiguousProviderTables(t *testing.T) {
	for _, kind := range []string{"hash", "name", "size", "traversal", "duplicate", "count"} {
		t.Run(kind, func(t *testing.T) {
			c, s, _, r := selectedFixture(t)
			switch kind {
			case "hash":
				s.info.InfoHash = strings.Repeat("f", 40)
			case "name":
				s.info.Name = "Other"
			case "size":
				s.info.Files[1].Size++
			case "traversal":
				s.info.Files[1].Path = "../a.mkv"
			case "duplicate":
				s.info.Files[1] = s.info.Files[0]
			case "count":
				s.info.Files = s.info.Files[:1]
			}
			_, err := c.SubmitSelectedOffline(context.Background(), r)
			if err == nil || s.submits != 0 {
				t.Fatal("unverified table submitted", err)
			}
		})
	}
}
func TestSelectedOfflineRejectsWholeOutputAndScopeDrift(t *testing.T) {
	c, s, _, r := selectedFixture(t)
	result, err := c.SubmitSelectedOffline(context.Background(), r)
	if err != nil {
		t.Fatal(err)
	}
	completedSelected(s, true)
	if _, err = c.ObserveSelectedOffline(context.Background(), result.Task.ID, r.Authority); err == nil {
		t.Fatal("whole resource filtered and reported as exact")
	}
	s.mu.Lock()
	s.children["owned-output"] = s.children["owned-output"][:1]
	s.task.DirId = "foreign-root"
	s.mu.Unlock()
	if _, err = c.ObserveSelectedOffline(context.Background(), result.Task.ID, r.Authority); err == nil {
		t.Fatal("foreign native task adopted")
	}
	if err = c.CancelSelectedOffline(context.Background(), result.Task.ID, r.Authority, false); err == nil || s.cancels != 0 {
		t.Fatal("foreign native task cancelled")
	}
}
func TestSelectedOfflineFrozenSelectionAndCancelNeverDeletesData(t *testing.T) {
	c, s, _, r := selectedFixture(t)
	result, err := c.SubmitSelectedOffline(context.Background(), r)
	if err != nil {
		t.Fatal(err)
	}
	wrong := r.Authority
	wrong.Tag = "omc-other"
	if _, err = c.ObserveSelectedOffline(context.Background(), result.Task.ID, wrong); err == nil {
		t.Fatal("wrong plan adopted")
	}
	if err = c.CancelSelectedOffline(context.Background(), result.Task.ID, r.Authority, true); err == nil || s.cancels != 0 {
		t.Fatal("delete-data accepted")
	}
	if err = c.CancelSelectedOffline(context.Background(), result.Task.ID, r.Authority, false); err != nil {
		t.Fatal(err)
	}
	if err = c.CancelSelectedOffline(context.Background(), result.Task.ID, r.Authority, false); err != nil {
		t.Fatal("cancel replay not idempotent", err)
	}
	if s.cancels != 1 || s.files["owned-seed"].FileID == "" {
		t.Fatal("cancel deleted files or replayed")
	}
}
func TestSelectedOfflineNoStorePermissionsAndDifferentPlanCannotWrite(t *testing.T) {
	c, s, store, r := selectedFixture(t)
	c.selective.store = nil
	if _, err := c.SubmitSelectedOffline(context.Background(), r); err == nil || s.creates != 0 {
		t.Fatal("missing ledger wrote")
	}
	c.selective.store = store
	c.beforeCall = func(context.Context) error { return cloud.Error(cloud.CodeAuthExpired, false, nil) }
	if _, err := c.SubmitSelectedOffline(context.Background(), r); err == nil || s.creates != 0 {
		t.Fatal("revoked credential wrote")
	}
	c.beforeCall = nil
	c.stateMu.Lock()
	c.authExpired = false
	c.stateMu.Unlock()
	result, err := c.SubmitSelectedOffline(context.Background(), r)
	if err != nil {
		t.Fatal(err)
	}
	r.Authority.Tag = "omc-another-plan"
	if _, err = c.SubmitSelectedOffline(context.Background(), r); err == nil || s.submits != 1 {
		t.Fatal("different plan changed wanted")
	}
	if result.Task.ID == "" {
		t.Fatal("missing task")
	}
}

func TestSelectedOfflineLocalRejectionDoesNotReserveResource(t *testing.T) {
	for _, kind := range []string{"metainfo", "selection", "tag", "root"} {
		t.Run(kind, func(t *testing.T) {
			c, s, store, request := selectedFixture(t)
			switch kind {
			case "metainfo":
				request.Torrent = []byte("invalid")
			case "selection":
				request.Authority.Selection.Files[0].Size++
			case "tag":
				request.Authority.Tag = "outside-managed-task"
			case "root":
				request.Authority.StorageRootID = "unrelated-root"
			}
			if _, err := c.SubmitSelectedOffline(context.Background(), request); err == nil {
				t.Fatal("invalid local authority accepted")
			}
			if len(store.rows) != 0 || s.creates != 0 || s.uploads != 0 || s.submits != 0 {
				t.Fatal("local validation failure reserved a resource or dispatched", len(store.rows))
			}
		})
	}
}

func TestSelectedOfflineCompletedOrCancelledResourceCannotChangePlan(t *testing.T) {
	for _, terminal := range []string{"completed", "cancelled"} {
		t.Run(terminal, func(t *testing.T) {
			c, s, store, request := selectedFixture(t)
			result, err := c.SubmitSelectedOffline(context.Background(), request)
			if err != nil {
				t.Fatal(err)
			}
			if terminal == "completed" {
				completedSelected(s, false)
				if _, err = c.ObserveSelectedOffline(context.Background(), result.Task.ID, request.Authority); err != nil {
					t.Fatal(err)
				}
			} else if err = c.CancelSelectedOffline(context.Background(), result.Task.ID, request.Authority, false); err != nil {
				t.Fatal(err)
			}
			request.Authority.Tag = "omc-new-plan-after-terminal"
			if _, err = selectedClient(s, store).SubmitSelectedOffline(context.Background(), request); err == nil {
				t.Fatal("terminal state silently released native resource ownership")
			}
			if s.creates != 1 || s.uploads != 1 || s.submits != 1 {
				t.Fatal("terminal owner allowed a new mutation", s.creates, s.uploads, s.submits)
			}
		})
	}
}
func TestSelectedOfflineCancellationAfterLostCancelReplyReconciles(t *testing.T) {
	c, s, store, r := selectedFixture(t)
	result, err := c.SubmitSelectedOffline(context.Background(), r)
	if err != nil {
		t.Fatal(err)
	}
	store.failConfirm = "/cancel"
	if err = c.CancelSelectedOffline(context.Background(), result.Task.ID, r.Authority, false); err == nil {
		t.Fatal("checkpoint failure not surfaced")
	}
	if err = selectedClient(s, store).CancelSelectedOffline(context.Background(), result.Task.ID, r.Authority, false); err != nil || s.cancels != 1 {
		t.Fatal("cancel recovery replayed", err)
	}
}

func TestSelectedOfflinePreparedSubmissionResumesWithoutWholeFallback(t *testing.T) {
	c, s, store, r := selectedFixture(t)
	store.failDispatch = "/submit"
	if _, err := c.SubmitSelectedOffline(context.Background(), r); err == nil || s.submits != 0 {
		t.Fatal("failed predispatch made I/O")
	}
	if _, err := selectedClient(s, store).SubmitSelectedOffline(context.Background(), r); err != nil || s.submits != 1 || s.uploads != 1 {
		t.Fatal("prepared resume", err)
	}
}
func TestSelectedOfflinePreexistingNativeTaskCannotCreateSeedOrDirectory(t *testing.T) {
	c, s, _, r := selectedFixture(t)
	s.task = &sdk.OfflineTask{InfoHash: s.info.InfoHash, DirId: "user-directory", Status: 1}
	if _, err := c.SubmitSelectedOffline(context.Background(), r); err == nil || s.creates != 0 || s.uploads != 0 || s.submits != 0 {
		t.Fatal("foreign same hash performed mutation", err)
	}
}

func TestSelectedOfflineConcurrentPlansDispatchEachStageOnce(t *testing.T) {
	c, s, _, r := selectedFixture(t)
	var group sync.WaitGroup
	for i := 0; i < 8; i++ {
		group.Add(1)
		go func() { defer group.Done(); _, _ = c.SubmitSelectedOffline(context.Background(), r) }()
	}
	group.Wait()
	if _, err := c.SubmitSelectedOffline(context.Background(), r); err != nil {
		t.Fatal("reconcile concurrent owners", err)
	}
	if s.creates != 1 || s.uploads != 1 || s.submits != 1 {
		t.Fatal("CAS did not prevent duplicate mutation", s.creates, s.uploads, s.submits)
	}
}

func TestSelectedOfflineConcurrentDifferentOwnersCannotBothWrite(t *testing.T) {
	c, s, _, r := selectedFixture(t)
	next := r
	next.Authority.Tag = "omc-another-concurrent-plan"
	var group sync.WaitGroup
	for _, request := range []cloud.SelectiveOfflineRequest{r, next} {
		group.Add(1)
		go func(request cloud.SelectiveOfflineRequest) {
			defer group.Done()
			_, _ = c.SubmitSelectedOffline(context.Background(), request)
		}(request)
	}
	group.Wait()
	if s.creates != 1 || s.uploads != 1 || s.submits != 1 {
		t.Fatal("different plans raced native mutation", s.creates, s.uploads, s.submits)
	}
}
