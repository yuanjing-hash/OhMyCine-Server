package pan115

import (
	"context"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path"
	"sort"
	"strings"
	"time"

	sdk "github.com/SheltonZhu/115driver/pkg/driver"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/cloud"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/resourcecontent"
	"golang.org/x/time/rate"
)

type selectiveOfflineState struct {
	store   cloud.OperationStore
	account string
	facts   cloud.OfflineFactReader
}
type selectedPlan struct {
	FileCount     int                       `json:"file_count"`
	Version       int                       `json:"version"`
	Tag           string                    `json:"tag"`
	StorageRootID string                    `json:"storage_root_id"`
	ParentID      string                    `json:"parent_id"`
	MetainfoSHA1  string                    `json:"metainfo_sha1"`
	MetainfoSize  int64                     `json:"metainfo_size"`
	InfoHash      string                    `json:"info_hash"`
	Name          string                    `json:"name"`
	MultiFile     bool                      `json:"multi_file"`
	Selection     resourcecontent.Selection `json:"selection"`
}
type selectedStage struct {
	Input  json.RawMessage `json:"input"`
	Result json.RawMessage `json:"result,omitempty"`
}
type wantedEvidence struct {
	DirectoryID   string      `json:"directory_id"`
	Seed          cloud.Item  `json:"seed"`
	Wanted        []int       `json:"wanted"`
	NativeIndices map[int]int `json:"native_indices"`
}

const selectedTaskPrefix = "omc-selected-"

func (c *Client) SupportsSelectiveOffline() bool {
	_, ok := c.sdk.(selectedOfflineSDK)
	_, mutation := c.sdk.(mutationSDK)
	_, upload := c.sdk.(uploadSDK)
	if !ok || !mutation || !upload || c.selective == nil || c.selective.store == nil || c.selective.account == "" {
		return false
	}
	_, exclusive := c.selective.store.(cloud.ResourceOperationStore)
	return exclusive
}
func selectedError(code string) error { return cloud.Error(code, false, nil) }
func selectedUnknown() error {
	return cloud.Error(cloud.CodeMutationUnknown, true, errors.New("selected offline operation requires reconciliation"))
}
func selectedDigest(value any) string {
	raw, _ := json.Marshal(value)
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}
func selectedID(plan selectedPlan) string {
	return selectedTaskPrefix + selectedDigest([]string{plan.Tag, plan.StorageRootID, plan.ParentID, plan.InfoHash})
}
func selectedScalar(s string) bool {
	return s != "" && len(s) <= 512 && !strings.ContainsAny(s, "\x00\r\n")
}
func selectedName(s string) bool {
	return selectedScalar(s) && len([]rune(s)) <= 255 && s != "." && s != ".." && !strings.ContainsAny(s, "/\\")
}
func selectedTag(s string) bool {
	return strings.HasPrefix(s, "omc-") && len(s) <= 64 && selectedName(s)
}
func selectedReceipt(c *Client, id, kind string, payload any) cloud.OperationReceipt {
	raw, _ := json.Marshal(payload)
	return cloud.OperationReceipt{ID: id, Provider: cloud.ProviderPan115, Route: "cookie", AccountID: c.selective.account, Kind: kind, Revision: 1, State: "prepared", Payload: raw, UpdatedAt: time.Now().UTC()}
}
func (c *Client) loadSelectedPlan(ctx context.Context, id string, authority cloud.SelectiveOfflineAuthority) (selectedPlan, error) {
	var plan selectedPlan
	if !c.SupportsSelectiveOffline() || !strings.HasPrefix(id, selectedTaskPrefix) || len(id) != len(selectedTaskPrefix)+64 {
		return plan, selectedError(cloud.CodeUnavailable)
	}
	r, err := c.selective.store.Load(ctx, id)
	if err != nil {
		return plan, err
	}
	if r.Provider != cloud.ProviderPan115 || r.Route != "cookie" || r.AccountID != c.selective.account || r.Kind != "selective_offline_plan" || json.Unmarshal(r.Payload, &plan) != nil || plan.Version != 1 || selectedID(plan) != id || plan.Tag != authority.Tag || plan.StorageRootID != authority.StorageRootID {
		return plan, selectedError(cloud.CodeResponseInvalid)
	}
	a, e := authority.Selection.Digest()
	b, _ := plan.Selection.Digest()
	if e != nil || a != b {
		return plan, selectedError(cloud.CodeResponseInvalid)
	}
	return plan, nil
}
func (c *Client) selectedRoot(ctx context.Context, id, root string) (cloud.Item, error) {
	if !selectedScalar(id) || !selectedScalar(root) {
		return cloud.Item{}, selectedError(cloud.CodeResponseInvalid)
	}
	first := cloud.Item{}
	seen := map[string]bool{}
	current := id
	for depth := 0; depth < 128; depth++ {
		if current == "0" && root == "0" {
			if depth == 0 {
				return cloud.Item{ID: "0", Name: "/", IsDir: true}, nil
			}
			return first, nil
		}
		if seen[current] {
			break
		}
		seen[current] = true
		item, err := c.Stat(ctx, current)
		if err != nil {
			return cloud.Item{}, err
		}
		if item.ID != current || !selectedName(item.Name) || depth > 0 && !item.IsDir {
			return cloud.Item{}, selectedError(cloud.CodeResponseInvalid)
		}
		if depth == 0 {
			first = item
		}
		if current == root {
			return first, nil
		}
		current = item.ParentID
		if current == "" || current == "0" && root != "0" {
			break
		}
	}
	return cloud.Item{}, selectedError(cloud.CodeResponseInvalid)
}
func (c *Client) selectedDirectory(ctx context.Context, parent, name string) (cloud.Item, bool, error) {
	var found cloud.Item
	seen := map[string]bool{}
	for offset := int64(0); offset < 10000; {
		page, err := c.List(ctx, parent, cloud.PageRequest{Offset: offset, Limit: 200})
		if err != nil {
			return found, false, err
		}
		if len(page.Items) == 0 && page.HasMore {
			return found, false, selectedError(cloud.CodeResponseInvalid)
		}
		for _, item := range page.Items {
			if item.ID == "" || item.ParentID != parent || !selectedName(item.Name) || seen[item.ID] {
				return found, false, selectedError(cloud.CodeResponseInvalid)
			}
			seen[item.ID] = true
			if item.Name == name {
				if found.ID != "" || !item.IsDir {
					return found, false, selectedError(cloud.CodeResponseInvalid)
				}
				found = item
			}
		}
		if !page.HasMore {
			return found, found.ID != "", nil
		}
		offset += int64(len(page.Items))
	}
	return found, false, selectedError(cloud.CodeResponseInvalid)
}
func (c *Client) selectedSeed(ctx context.Context, parent, name string, plan selectedPlan) (cloud.Item, bool, error) {
	var found cloud.Item
	seen := map[string]bool{}
	for offset := int64(0); offset < 10000; {
		page, err := c.List(ctx, parent, cloud.PageRequest{Offset: offset, Limit: 200})
		if err != nil {
			return found, false, err
		}
		if len(page.Items) == 0 && page.HasMore {
			return found, false, selectedError(cloud.CodeResponseInvalid)
		}
		for _, item := range page.Items {
			if item.ID == "" || item.ParentID != parent || !selectedName(item.Name) || seen[item.ID] {
				return found, false, selectedError(cloud.CodeResponseInvalid)
			}
			seen[item.ID] = true
			if item.Name == name {
				if found.ID != "" || item.IsDir || item.Size != plan.MetainfoSize || !strings.EqualFold(item.SHA1, plan.MetainfoSHA1) {
					return found, false, selectedError(cloud.CodeResponseInvalid)
				}
				found = item
			}
		}
		if !page.HasMore {
			return found, found.ID != "", nil
		}
		offset += int64(len(page.Items))
	}
	return found, false, selectedError(cloud.CodeResponseInvalid)
}
func (c *Client) selectedCheckpoint(ctx context.Context, r *cloud.OperationReceipt, state string, stage selectedStage) error {
	raw, err := json.Marshal(stage)
	if err != nil {
		return err
	}
	next := *r
	next.Revision++
	next.State = state
	next.Payload = raw
	next.UpdatedAt = time.Now().UTC()
	if err = c.selective.store.Update(ctx, r.Revision, next); err != nil {
		return err
	}
	*r = next
	return nil
}

// The checkpoint callback runs INSIDE the existing account guard/rate/slot
// boundary and immediately before provider I/O. Preflight failure consumes no
// dispatch. An unknown stage can only query its original operation.
func (c *Client) selectedStep(ctx context.Context, id, kind string, input any, limiter *rate.Limiter, call func() (any, error), recover func() (any, bool, error)) (json.RawMessage, error) {
	encoded, _ := json.Marshal(input)
	stage := selectedStage{Input: encoded}
	r, err := c.selective.store.Load(ctx, id)
	if errors.Is(err, cloud.ErrOperationNotFound) {
		r = selectedReceipt(c, id, kind, stage)
		if err = c.selective.store.Create(ctx, r); err != nil {
			r, err = c.selective.store.Load(ctx, id)
		}
	}
	if err != nil {
		return nil, err
	}
	var saved selectedStage
	if r.Provider != cloud.ProviderPan115 || r.AccountID != c.selective.account || r.Route != "cookie" || r.Kind != kind || json.Unmarshal(r.Payload, &saved) != nil || string(saved.Input) != string(encoded) {
		return nil, selectedError(cloud.CodeResponseInvalid)
	}
	if r.State == "confirmed" {
		return saved.Result, nil
	}
	if r.State == "failed" {
		return nil, selectedError(cloud.CodeUnavailable)
	}
	confirm := func(value any) (json.RawMessage, error) {
		raw, e := json.Marshal(value)
		if e != nil {
			return nil, e
		}
		saved.Result = raw
		reconcile, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		if e = c.selectedCheckpoint(reconcile, &r, "confirmed", saved); e != nil {
			return nil, selectedUnknown()
		}
		return raw, nil
	}
	if r.State == "submitting" || r.State == "unknown" {
		value, found, e := recover()
		if e != nil {
			return nil, e
		}
		if !found {
			return nil, selectedUnknown()
		}
		return confirm(value)
	}
	if r.State != "prepared" {
		return nil, selectedError(cloud.CodeResponseInvalid)
	}
	dispatched := false
	var result any
	err = c.waitAndCall(ctx, limiter, func() error {
		if e := c.selectedCheckpoint(ctx, &r, "submitting", saved); e != nil {
			return e
		}
		dispatched = true
		var e error
		result, e = call()
		return e
	})
	if err != nil {
		if !dispatched {
			return nil, mapError(err)
		}
		reconcile, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		_ = c.selectedCheckpoint(reconcile, &r, "unknown", saved)
		return nil, selectedUnknown()
	}
	if result == nil {
		value, found, e := recover()
		if e != nil || !found {
			reconcile, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
			defer cancel()
			_ = c.selectedCheckpoint(reconcile, &r, "unknown", saved)
			return nil, selectedUnknown()
		}
		result = value
	}
	return confirm(result)
}
func selectedPath(value, root string, multi bool) (string, error) {
	if value == "" || len(value) > 4096 || strings.ContainsAny(value, "\\\x00\r\n") || strings.HasPrefix(value, "/") || path.Clean(value) != value || value == "." || strings.HasPrefix(value, "../") || strings.Contains(value, ":") {
		return "", selectedError(cloud.CodeResponseInvalid)
	}
	if multi && strings.HasPrefix(value, root+"/") {
		value = strings.TrimPrefix(value, root+"/")
	}
	return value, nil
}
func selectedWanted(info selectedTorrentInfo, manifest resourcecontent.Manifest, selection resourcecontent.Selection) ([]int, error) {
	if !strings.EqualFold(info.InfoHash, manifest.Identity.V1) || info.Name != manifest.Name || len(info.Files) != len(manifest.Files) {
		return nil, selectedError(cloud.CodeResponseInvalid)
	}
	expected := map[string]resourcecontent.File{}
	for _, file := range manifest.Files {
		if file.Padding {
			return nil, selectedError(cloud.CodeUnavailable)
		}
		expected[file.Path] = file
	}
	positions := map[string]int{}
	for index, row := range info.Files {
		p, err := selectedPath(row.Path, manifest.Name, manifest.MultiFile)
		if err != nil {
			return nil, err
		}
		file, ok := expected[p]
		if !ok || file.Size != row.Size {
			return nil, selectedError(cloud.CodeResponseInvalid)
		}
		if _, duplicate := positions[p]; duplicate {
			return nil, selectedError(cloud.CodeResponseInvalid)
		}
		positions[p] = index
	}
	wanted := make([]int, 0, len(selection.Files))
	for _, file := range selection.Files {
		index, ok := positions[file.Path]
		if !ok {
			return nil, selectedError(cloud.CodeResponseInvalid)
		}
		wanted = append(wanted, index)
	}
	sort.Ints(wanted)
	return wanted, nil
}
func (c *Client) selectedNativeTask(ctx context.Context, hash, dir string) (*sdk.OfflineTask, bool, error) {
	maxPage := int64(1)
	var found *sdk.OfflineTask
	for page := int64(1); page <= maxPage; page++ {
		data, err := c.listOfflinePage(ctx, page)
		if err != nil {
			return nil, false, err
		}
		if data.PageCount < 0 || data.PageCount > maxOfflineTaskPages || (data.PageCount == 0 && len(data.Tasks) != 0) {
			return nil, false, selectedError(cloud.CodeResponseInvalid)
		}
		if data.PageCount > maxPage {
			maxPage = data.PageCount
		}
		for _, task := range data.Tasks {
			if task != nil && strings.EqualFold(task.InfoHash, hash) {
				if found != nil || dir != "" && task.DirId != dir {
					return nil, false, selectedError(cloud.CodeOfflineTaskExists)
				}
				copy := *task
				found = &copy
			}
		}
	}
	return found, found != nil, nil
}

func (c *Client) SubmitSelectedOffline(ctx context.Context, request cloud.SelectiveOfflineRequest) (cloud.SelectiveOfflineSnapshot, error) {
	if !c.SupportsSelectiveOffline() {
		return cloud.SelectiveOfflineSnapshot{}, selectedError(cloud.CodeUnavailable)
	}
	m, err := resourcecontent.ParseTorrent(request.Torrent, resourcecontent.Limits{MaxMetainfoBytes: 4 << 20})
	if err != nil || m.Identity.V1 == "" || m.Identity.V2 != "" || !selectedName(m.Name) || !selectedTag(request.Authority.Tag) || request.Authority.Selection.ValidateAgainst(m) != nil || resourcecontent.ValidateExecutableMetainfo(m) != nil {
		return cloud.SelectiveOfflineSnapshot{}, selectedError(cloud.CodeResponseInvalid)
	}
	for _, file := range m.Files {
		if file.Padding {
			return cloud.SelectiveOfflineSnapshot{}, selectedError(cloud.CodeUnavailable)
		}
	}
	dir, err := c.selectedRoot(ctx, request.ParentID, request.Authority.StorageRootID)
	if err != nil || !dir.IsDir {
		if err == nil {
			err = selectedError(cloud.CodeResponseInvalid)
		}
		return cloud.SelectiveOfflineSnapshot{}, err
	}
	sum := sha1.Sum(request.Torrent)
	plan := selectedPlan{Version: 1, Tag: request.Authority.Tag, StorageRootID: request.Authority.StorageRootID, ParentID: request.ParentID, MetainfoSHA1: hex.EncodeToString(sum[:]), MetainfoSize: int64(len(request.Torrent)), InfoHash: m.Identity.V1, Name: m.Name, MultiFile: m.MultiFile, FileCount: len(m.Files), Selection: request.Authority.Selection}
	id := selectedID(plan)
	resourceKey := selectedDigest([]string{"native-bt-v1", plan.InfoHash})
	ownerID := "omc-selected-owner-" + resourceKey
	owner := selectedReceipt(c, ownerID, "selective_offline_owner", struct {
		PlanID string `json:"plan_id"`
		Digest string `json:"digest"`
	}{id, selectedDigest(plan)})
	existing, e := c.selective.store.Load(ctx, ownerID)
	if errors.Is(e, cloud.ErrOperationNotFound) {
		if _, exists, readErr := c.selectedNativeTask(ctx, plan.InfoHash, ""); readErr != nil {
			return cloud.SelectiveOfflineSnapshot{}, readErr
		} else if exists {
			return cloud.SelectiveOfflineSnapshot{}, selectedError(cloud.CodeOfflineTaskExists)
		}
		if e = c.selective.store.(cloud.ExclusiveOperationStore).Claim(ctx, resourceKey, owner); e != nil {
			existing, e = c.selective.store.Load(ctx, ownerID)
		} else {
			existing = owner
		}
	}
	if e != nil {
		return cloud.SelectiveOfflineSnapshot{}, selectedError(cloud.CodeOfflineTaskExists)
	}
	if existing.Provider != owner.Provider || existing.AccountID != owner.AccountID || existing.Kind != owner.Kind || string(existing.Payload) != string(owner.Payload) {
		return cloud.SelectiveOfflineSnapshot{}, selectedError(cloud.CodeOfflineTaskExists)
	}

	r, err := c.selective.store.Load(ctx, id)
	if errors.Is(err, cloud.ErrOperationNotFound) {
		if _, exists, e := c.selectedNativeTask(ctx, plan.InfoHash, ""); e != nil {
			return cloud.SelectiveOfflineSnapshot{}, e
		} else if exists {
			return cloud.SelectiveOfflineSnapshot{}, selectedError(cloud.CodeOfflineTaskExists)
		}
		r = selectedReceipt(c, id, "selective_offline_plan", plan)
		if err = c.selective.store.Create(ctx, r); err != nil {
			r, err = c.selective.store.Load(ctx, id)
		}
	}
	if err != nil {
		return cloud.SelectiveOfflineSnapshot{}, err
	}
	var saved selectedPlan
	if json.Unmarshal(r.Payload, &saved) != nil || selectedDigest(saved) != selectedDigest(plan) {
		return cloud.SelectiveOfflineSnapshot{}, selectedError(cloud.CodeResponseInvalid)
	}
	mutation, ok := c.sdk.(mutationSDK)
	if !ok {
		return cloud.SelectiveOfflineSnapshot{}, selectedError(cloud.CodeUnavailable)
	}
	raw, err := c.selectedStep(ctx, id+"/directory", "selective_offline_directory", []string{selectedDigest(plan), plan.ParentID, plan.Tag}, c.mkdirRate, func() (any, error) {
		identity, e := mutation.Mkdir(plan.ParentID, plan.Tag)
		if e != nil {
			return nil, e
		}
		if identity == "" {
			return nil, selectedUnknown()
		}
		return cloud.Item{ID: identity, ParentID: plan.ParentID, Name: plan.Tag, IsDir: true}, nil
	}, func() (any, bool, error) { return c.selectedDirectory(ctx, plan.ParentID, plan.Tag) })
	if err != nil {
		return cloud.SelectiveOfflineSnapshot{}, err
	}
	var workspace cloud.Item
	if json.Unmarshal(raw, &workspace) != nil || workspace.ParentID != plan.ParentID || workspace.Name != plan.Tag || workspace.ID == "" || !workspace.IsDir {
		return cloud.SelectiveOfflineSnapshot{}, selectedError(cloud.CodeResponseInvalid)
	}
	if actual, e := c.selectedRoot(ctx, workspace.ID, plan.StorageRootID); e != nil || !actual.IsDir || actual.ParentID != plan.ParentID || actual.Name != plan.Tag {
		if e == nil {
			e = selectedError(cloud.CodeResponseInvalid)
		}
		return cloud.SelectiveOfflineSnapshot{}, e
	}
	seedName := "omc-seed-" + plan.MetainfoSHA1 + ".torrent"
	upload, ok := c.sdk.(uploadSDK)
	if !ok {
		return cloud.SelectiveOfflineSnapshot{}, selectedError(cloud.CodeUnavailable)
	}
	raw, err = c.selectedStep(ctx, id+"/seed", "selective_offline_seed", []string{selectedDigest(plan), workspace.ID, seedName}, c.uploadRate, func() (any, error) {
		file, e := os.CreateTemp("", "omc-selected-*.torrent")
		if e != nil {
			return nil, e
		}
		defer os.Remove(file.Name())
		defer file.Close()
		if e = file.Chmod(0600); e != nil {
			return nil, e
		}
		if _, e = file.Write(request.Torrent); e != nil {
			return nil, e
		}
		if _, e = file.Seek(0, io.SeekStart); e != nil {
			return nil, e
		}
		if e = upload.UploadFastOrByMultipart(workspace.ID, seedName, plan.MetainfoSize, file, sdk.UploadMultipartWithThreadsNum(1)); e != nil {
			return nil, e
		}
		return nil, nil
	}, func() (any, bool, error) { return c.selectedSeed(ctx, workspace.ID, seedName, plan) })
	if err != nil {
		return cloud.SelectiveOfflineSnapshot{}, err
	}
	var seed cloud.Item
	if json.Unmarshal(raw, &seed) != nil || seed.ID == "" || seed.ParentID != workspace.ID || seed.Size != plan.MetainfoSize || !strings.EqualFold(seed.SHA1, plan.MetainfoSHA1) {
		return cloud.SelectiveOfflineSnapshot{}, selectedError(cloud.CodeResponseInvalid)
	}
	if previous, e := c.selective.store.Load(ctx, id+"/submit"); e == nil {
		if previous.State != "prepared" {
			return c.ObserveSelectedOffline(ctx, id, request.Authority)
		}
	} else if !errors.Is(e, cloud.ErrOperationNotFound) {
		return cloud.SelectiveOfflineSnapshot{}, e
	}
	fresh, err := c.Stat(ctx, seed.ID)
	if err != nil {
		return cloud.SelectiveOfflineSnapshot{}, err
	}
	if fresh.IsDir || fresh.ParentID != workspace.ID || fresh.Name != seedName || fresh.Size != plan.MetainfoSize || !strings.EqualFold(fresh.SHA1, plan.MetainfoSHA1) {
		return cloud.SelectiveOfflineSnapshot{}, selectedError(cloud.CodeResponseInvalid)
	}
	var info selectedTorrentInfo
	err = c.waitAndCall(ctx, c.offlineRate, func() error {
		var e error
		info, e = c.sdk.(selectedOfflineSDK).ParseOfflineTorrent(ctx, seed.SHA1)
		return e
	})
	if err != nil {
		return cloud.SelectiveOfflineSnapshot{}, mapError(err)
	}
	wanted, err := selectedWanted(info, m, plan.Selection)
	if err != nil {
		return cloud.SelectiveOfflineSnapshot{}, err
	}
	// Existing same-hash native tasks are never adopted into a newly prepared
	// operation, regardless of their display name or apparent progress.
	if _, exists, e := c.selectedNativeTask(ctx, plan.InfoHash, ""); e != nil {
		return cloud.SelectiveOfflineSnapshot{}, e
	} else if exists {
		return cloud.SelectiveOfflineSnapshot{}, selectedError(cloud.CodeOfflineTaskExists)
	}
	native := map[int]int{}
	for _, own := range plan.Selection.Files {
		for index, row := range info.Files {
			p, _ := selectedPath(row.Path, m.Name, m.MultiFile)
			if p == own.Path {
				native[own.Index] = index
			}
		}
	}
	evidence := wantedEvidence{DirectoryID: workspace.ID, Seed: seed, Wanted: wanted, NativeIndices: native}
	_, err = c.selectedStep(ctx, id+"/submit", "selective_offline_submit", evidence, c.offlineRate, func() (any, error) {
		e := c.sdk.(selectedOfflineSDK).AddOfflineWanted(ctx, selectedWantedRequest{InfoHash: plan.InfoHash, DirectoryID: workspace.ID, SavePath: plan.Name, Wanted: wanted})
		return evidence, e
	}, func() (any, bool, error) {
		_, found, e := c.selectedNativeTask(ctx, plan.InfoHash, workspace.ID)
		return evidence, found, e
	})
	if err != nil {
		return cloud.SelectiveOfflineSnapshot{}, err
	}
	return c.ObserveSelectedOffline(ctx, id, request.Authority)
}
func (c *Client) selectedSubmission(ctx context.Context, id string, plan selectedPlan) (wantedEvidence, error) {
	var evidence wantedEvidence
	r, err := c.selective.store.Load(ctx, id+"/submit")
	if err != nil {
		return evidence, selectedUnknown()
	}
	var stage selectedStage
	if r.Provider != cloud.ProviderPan115 || r.AccountID != c.selective.account || r.Kind != "selective_offline_submit" || r.Route != "cookie" || json.Unmarshal(r.Payload, &stage) != nil || json.Unmarshal(stage.Input, &evidence) != nil || evidence.DirectoryID == "" || len(evidence.Wanted) != len(plan.Selection.Files) || evidence.Seed.ParentID != evidence.DirectoryID || evidence.Seed.Size != plan.MetainfoSize || !strings.EqualFold(evidence.Seed.SHA1, plan.MetainfoSHA1) {
		return evidence, selectedError(cloud.CodeResponseInvalid)
	}
	if len(evidence.NativeIndices) != len(plan.Selection.Files) {
		return evidence, selectedError(cloud.CodeResponseInvalid)
	}
	nativeSeen := map[int]bool{}
	for _, own := range plan.Selection.Files {
		index, ok := evidence.NativeIndices[own.Index]
		if !ok || index < 0 || index >= plan.FileCount || nativeSeen[index] {
			return evidence, selectedError(cloud.CodeResponseInvalid)
		}
		nativeSeen[index] = true
	}
	for _, index := range evidence.Wanted {
		if !nativeSeen[index] {
			return evidence, selectedError(cloud.CodeResponseInvalid)
		}
		delete(nativeSeen, index)
	}
	if len(nativeSeen) != 0 {
		return evidence, selectedError(cloud.CodeResponseInvalid)
	}
	if r.State == "confirmed" {
		return evidence, nil
	}
	if r.State == "prepared" {
		return evidence, selectedUnknown()
	}
	if r.State != "submitting" && r.State != "unknown" {
		return evidence, selectedError(cloud.CodeResponseInvalid)
	}
	_, found, err := c.selectedNativeTask(ctx, plan.InfoHash, evidence.DirectoryID)
	if err != nil {
		return evidence, err
	}
	if !found {
		return evidence, selectedUnknown()
	}
	stage.Result = stage.Input
	if err = c.selectedCheckpoint(ctx, &r, "confirmed", stage); err != nil {
		return evidence, selectedUnknown()
	}
	return evidence, nil
}
func (c *Client) ObserveSelectedOffline(ctx context.Context, id string, authority cloud.SelectiveOfflineAuthority) (cloud.SelectiveOfflineSnapshot, error) {
	plan, err := c.loadSelectedPlan(ctx, id, authority)
	if err != nil {
		return cloud.SelectiveOfflineSnapshot{}, err
	}
	evidence, err := c.selectedSubmission(ctx, id, plan)
	if err != nil {
		return cloud.SelectiveOfflineSnapshot{}, err
	}
	root, err := c.selectedRoot(ctx, evidence.DirectoryID, plan.StorageRootID)
	if err != nil || !root.IsDir || root.Name != plan.Tag || root.ParentID != plan.ParentID {
		if err == nil {
			err = selectedError(cloud.CodeResponseInvalid)
		}
		return cloud.SelectiveOfflineSnapshot{}, err
	}
	task, found, err := c.selectedNativeTask(ctx, plan.InfoHash, evidence.DirectoryID)
	if err != nil {
		return cloud.SelectiveOfflineSnapshot{}, err
	}
	if !found {
		return cloud.SelectiveOfflineSnapshot{}, selectedUnknown()
	}
	mapped := mapOfflineTask(task)
	mapped.ID = id
	var total int64
	for _, file := range plan.Selection.Files {
		total += file.Size
	}
	mapped.BytesTotal = &total
	mapped.Progress = nil
	mapped.ETASeconds = nil
	snapshot := cloud.SelectiveOfflineSnapshot{Task: mapped, Name: plan.Name}
	if !mapped.Completed {
		return snapshot, nil
	}
	snapshot.Task.Completed = false
	if mapped.OutputItemID == "" {
		return cloud.SelectiveOfflineSnapshot{}, selectedUnknown()
	}
	output, err := c.selectedRoot(ctx, mapped.OutputItemID, evidence.DirectoryID)
	if err != nil {
		return cloud.SelectiveOfflineSnapshot{}, err
	}
	files, err := c.selectedOutput(ctx, output, plan, evidence)
	if err != nil {
		return cloud.SelectiveOfflineSnapshot{}, err
	}
	snapshot.Files = files
	snapshot.Complete = true
	snapshot.Task.Completed = true
	progress := 1.0
	snapshot.Task.Progress = &progress
	return snapshot, nil
}
func (c *Client) selectedOutput(ctx context.Context, root cloud.Item, plan selectedPlan, evidence wantedEvidence) ([]cloud.SelectiveOfflineFile, error) {
	expected := map[string]resourcecontent.SelectedFile{}
	for _, file := range plan.Selection.Files {
		expected[file.Path] = file
	}
	got := map[string]bool{}
	ids := map[string]bool{}
	out := []cloud.SelectiveOfflineFile{}
	type node struct {
		Item   cloud.Item
		Prefix string
	}
	pending := []node{{Item: root}}
	count := 0
	for len(pending) > 0 {
		entry := pending[0]
		pending = pending[1:]
		if ids[entry.Item.ID] {
			return nil, selectedError(cloud.CodeResponseInvalid)
		}
		ids[entry.Item.ID] = true
		count++
		if count > 10000 {
			return nil, selectedError(cloud.CodeResponseInvalid)
		}
		if !entry.Item.IsDir {
			if entry.Item.ID == evidence.Seed.ID {
				continue
			}
			relative := entry.Prefix
			if relative == "" {
				relative = entry.Item.Name
			}
			p, e := selectedPath(relative, plan.Name, plan.MultiFile)
			if e != nil {
				return nil, e
			}
			want, ok := expected[p]
			if !ok || got[p] || want.Size != entry.Item.Size {
				return nil, selectedError(cloud.CodeResponseInvalid)
			}
			got[p] = true
			out = append(out, cloud.SelectiveOfflineFile{Index: evidence.NativeIndices[want.Index], Path: want.Path, Item: entry.Item})
			continue
		}
		for offset := int64(0); offset < 10000; {
			page, e := c.List(ctx, entry.Item.ID, cloud.PageRequest{Offset: offset, Limit: 200})
			if e != nil {
				return nil, e
			}
			if len(page.Items) == 0 && page.HasMore {
				return nil, selectedError(cloud.CodeResponseInvalid)
			}
			for _, item := range page.Items {
				if item.ID == "" || item.ParentID != entry.Item.ID || !selectedName(item.Name) {
					return nil, selectedError(cloud.CodeResponseInvalid)
				}
				p := item.Name
				if entry.Prefix != "" {
					p = entry.Prefix + "/" + p
				}
				pending = append(pending, node{Item: item, Prefix: p})
			}
			if !page.HasMore {
				break
			}
			offset += int64(len(page.Items))
			if offset >= 10000 {
				return nil, selectedError(cloud.CodeResponseInvalid)
			}
		}
	}
	if len(got) != len(expected) {
		return nil, selectedUnknown()
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out, nil
}
func (c *Client) CancelSelectedOffline(ctx context.Context, id string, authority cloud.SelectiveOfflineAuthority, deleteData bool) error {
	if deleteData {
		return selectedError(cloud.CodeUnavailable)
	}
	plan, err := c.loadSelectedPlan(ctx, id, authority)
	if err != nil {
		return err
	}
	evidence, err := c.selectedSubmission(ctx, id, plan)
	if err != nil {
		return err
	}
	root, e := c.selectedRoot(ctx, evidence.DirectoryID, plan.StorageRootID)
	if e != nil {
		return e
	}
	if !root.IsDir || root.Name != plan.Tag || root.ParentID != plan.ParentID {
		return selectedError(cloud.CodeResponseInvalid)
	}
	previous, lookup := c.selective.store.Load(ctx, id+"/cancel")
	if lookup != nil && !errors.Is(lookup, cloud.ErrOperationNotFound) {
		return lookup
	}
	if lookup == nil && previous.State == "confirmed" {
		return nil
	}
	if errors.Is(lookup, cloud.ErrOperationNotFound) || previous.State == "prepared" {
		if _, found, e := c.selectedNativeTask(ctx, plan.InfoHash, evidence.DirectoryID); e != nil {
			return e
		} else if !found {
			return selectedUnknown()
		}
	}
	_, err = c.selectedStep(ctx, id+"/cancel", "selective_offline_cancel", []string{plan.InfoHash, evidence.DirectoryID}, c.offlineRate, func() (any, error) { return true, c.sdk.DeleteOfflineTasks([]string{plan.InfoHash}, false) }, func() (any, bool, error) {
		_, found, e := c.selectedNativeTask(ctx, plan.InfoHash, evidence.DirectoryID)
		return true, !found, e
	})
	return err
}

var _ cloud.SelectiveOfflineDriver = (*Client)(nil)
