//go:build quark_live

package handlers_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/yuanjing-hash/OhMyCine-Server/pkg/cloud"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/cloud/quark"
)

func TestQuarkLiveMultipartInspect(t *testing.T) {
	dir := os.Getenv("OMC_QUARK_MULTIPART_INSPECT")
	if dir == "" {
		t.Skip("explicit existing multipart batch required for local inspection")
	}
	report, e := inspectMultipartDirectory(dir)
	if e != nil {
		t.Fatal("multipart_local_inspection_rejected")
	}
	b, _ := json.Marshal(report)
	t.Log("multipart_local_inspection=" + string(b))
}

func TestQuarkMultipartOfflineLocalInspection(t *testing.T) {
	s, _ := multipartFixture(t)
	for _, c := range []struct {
		size   int64
		rapid  bool
		reason string
		count  int64
	}{{4 << 20, false, "part_count_exceeds_probe_budget", 9}, {32 << 20, false, "eligible_but_stopped", 2}, {64 << 20, false, "single_part", 1}, {0, true, "rapid_upload", 0}, {63, false, "part_size_alignment_invalid", 532611}} {
		p := multipartPayload{Size: multipartSize, Name: s.state.UploadName, SHA256: s.state.SourceSHA, TaskID: "PRIVATE-TASK", PartSize: c.size}
		pre := map[string]any{"task_id": "PRIVATE-TASK", "part_size": c.size, "finish": c.rapid, "upload_urls": []any{map[string]string{"upload_url": "https://private.invalid/?token=SECRET"}}}
		if c.rapid {
			p.Handle = "PRIVATE-HANDLE"
			pre["fid"] = p.Handle
		}
		raw, _ := json.Marshal(p)
		s.state.Operations[s.state.UploadID] = cloud.OperationReceipt{ID: s.state.UploadID, Payload: raw}
		s.state.PreResponse, _ = json.Marshal(pre)
		s.state.Stopped = "multipart_not_exercised"
		r := inspectMultipartState(s.state)
		encoded, _ := json.Marshal(r)
		if r.Reason != c.reason || r.PartCount != c.count || !r.ReceiptMatchesPre || r.RapidUpload != c.rapid || r.ActualPartBytes != c.size || r.SampleBytes != multipartSize || strings.Contains(string(encoded), "PRIVATE") || strings.Contains(string(encoded), "SECRET") || strings.Contains(string(encoded), "private.invalid") {
			t.Fatalf("safe inspection case size=%d reason=%s count=%d", c.size, r.Reason, r.PartCount)
		}
	}
	// Expired/stopped batches remain locally inspectable without any login or
	// donor path. Original ciphertext/key contents and modification times stay.
	s.state.Started = time.Now().UTC().Add(-time.Hour)
	if s.save() != nil {
		t.Fatal("fixture save")
	}
	s.lock.Close()
	paths := []string{filepath.Join(s.dir, "multipart.enc"), filepath.Join(s.dir, "multipart.key")}
	original := map[string][]byte{}
	times := map[string]time.Time{}
	for _, p := range paths {
		original[p], _ = os.ReadFile(p)
		info, _ := os.Stat(p)
		times[p] = info.ModTime()
	}
	report, e := inspectMultipartDirectory(s.dir)
	if e != nil || !report.DeadlineExpired || report.InspectionCloudRequests != 0 {
		t.Fatal("local expired inspection")
	}
	for _, p := range paths {
		after, _ := os.ReadFile(p)
		info, _ := os.Stat(p)
		if !bytes.Equal(original[p], after) || !times[p].Equal(info.ModTime()) {
			t.Fatal("inspection modified original state")
		}
	}
}

func TestQuarkMultipartOfflineFrozenProfiles(t *testing.T) {
	for _, profile := range []string{multipartStandardProfile, multipartSmallProfile, multipartFinalSmallProfile, "legacy"} {
		t.Run(profile, func(t *testing.T) {
			selected := profile
			if selected == "legacy" {
				selected = multipartStandardProfile
			}
			s, parent := multipartFixture(t, selected)
			if profile == "legacy" {
				// Exact pre-profile disk schema, including the old stopped state.
				s.state.FixtureProfile, s.state.FixtureBytes = "", 0
			}
			s.state.Stopped = "multipart_not_exercised"
			s.state.Pre, s.state.API, s.state.Requests = 1, 31, 31
			s.state.Writes["pre"] = true
			if s.save() != nil {
				t.Fatal("save frozen fixture")
			}
			beforeState, _ := json.Marshal(s.state)
			type saved struct {
				data     []byte
				modified time.Time
			}
			files := map[string]saved{}
			for _, name := range []string{"multipart.key", "multipart.enc", "sample.bin"} {
				path := filepath.Join(s.dir, name)
				data, err := os.ReadFile(path)
				info, se := os.Stat(path)
				if err != nil || se != nil {
					t.Fatal("read synthetic fixture")
				}
				files[name] = saved{data, info.ModTime()}
			}
			s.lock.Close()
			for _, wrong := range []string{multipartStandardProfile, multipartSmallProfile, multipartFinalSmallProfile} {
				if wrong == selected {
					continue
				}
				if opened, err := openMultipartStore(s.dir, parent, wrong); err == nil {
					opened.lock.Close()
					t.Fatal("accepted a different frozen fixture profile")
				}
			}
			reopened, err := openMultipartStore(s.dir, parent, selected)
			if err != nil {
				t.Fatal("same profile failed cold read")
			}
			afterState, _ := json.Marshal(reopened.state)
			reopened.lock.Close()
			if !bytes.Equal(beforeState, afterState) {
				t.Fatal("resume mutated frozen state")
			}
			inspection, err := inspectMultipartDirectory(s.dir)
			if err != nil || inspection.FixtureProfile != selected || inspection.SampleBytes != multipartProfileSize(selected) || !inspection.SavedStopped {
				t.Fatal("profile inspection mismatch")
			}
			for name, prior := range files {
				path := filepath.Join(s.dir, name)
				data, err := os.ReadFile(path)
				info, se := os.Stat(path)
				if err != nil || se != nil || !bytes.Equal(data, prior.data) || !info.ModTime().Equal(prior.modified) {
					t.Fatal("profile rejection or resume changed original files")
				}
			}
		})
	}
}

func TestQuarkMultipartOfflineSmallProfileValidation(t *testing.T) {
	s, _ := multipartFixture(t, multipartSmallProfile)
	if multipartStateSize(s.state) != 8388609 || !multipartManifest(s.state) {
		t.Fatal("small profile size")
	}
	for _, invalid := range []multipartState{
		{FixtureProfile: multipartSmallProfile, FixtureBytes: multipartSize},
		{FixtureProfile: multipartStandardProfile, FixtureBytes: multipartSmallSize},
		{FixtureProfile: "unrecognized", FixtureBytes: multipartSmallSize},
		{FixtureProfile: "", FixtureBytes: multipartSmallSize},
	} {
		st := s.state
		st.FixtureProfile, st.FixtureBytes = invalid.FixtureProfile, invalid.FixtureBytes
		if multipartStateSize(st) != 0 || multipartManifest(st) {
			t.Fatal("malformed profile accepted")
		}
	}
	for _, oversize := range []string{"upload", "read"} {
		st := s.state
		if oversize == "upload" {
			st.UploadReserved = multipartSmallSize + 1
		} else {
			st.ReadReserved = 3*multipartSmallSize + 1
		}
		if multipartManifest(st) {
			t.Fatal("small profile accepted standard byte budget")
		}
	}
	p := multipartPayload{Parent: s.state.ChildAgent, Name: s.state.UploadName, Size: multipartSize, SHA256: s.state.SourceSHA, PartSize: 4 << 20}
	if newMultipartGuard(s).valid(p) {
		t.Fatal("cross-profile upload payload accepted")
	}
	p.Size = multipartSmallSize
	if n, ok := multipartPlan(p); !ok || n != 3 {
		t.Fatal("4 MiB plan must be three parts for small profile")
	}
	s.state.PreResponse = json.RawMessage(`{"finish":false,"task_id":"PRIVATE","part_size":4194304}`)
	p.TaskID = "PRIVATE"
	payload, _ := json.Marshal(p)
	s.state.Operations[s.state.UploadID] = cloud.OperationReceipt{Payload: payload}
	r := inspectMultipartState(s.state)
	if r.SampleBytes != multipartSmallSize || r.PartCount != 3 || r.ActualPartBytes != 4<<20 || !r.ReceiptMatchesPre || r.Reason != "eligible_recorded" {
		t.Fatal("small profile local pre inspection")
	}
}

func TestQuarkMultipartOfflineFinalSmallProfile(t *testing.T) {
	s, _ := multipartFixture(t, multipartFinalSmallProfile)
	if multipartStateSize(s.state) != multipartSmallSize || multipartMetadataBudget(s.state) != 32<<20 || !multipartManifest(s.state) {
		t.Fatal("final profile must freeze both size and metadata allowance")
	}
	for _, invalid := range []int64{0, 8 << 20, 64 << 20} {
		st := s.state
		st.MetadataLimit = invalid
		if multipartMetadataBudget(st) != 0 || multipartManifest(st) {
			t.Fatal("final profile accepted arbitrary or missing metadata allowance")
		}
	}
	for _, profile := range []string{multipartSmallProfile, multipartStandardProfile, ""} {
		st := s.state
		st.FixtureProfile, st.FixtureBytes = profile, multipartProfileSize(profile)
		if multipartMetadataBudget(st) != 0 || multipartManifest(st) {
			t.Fatal("old profile accepted final metadata allowance")
		}
	}
	cache, err := os.UserCacheDir()
	if err != nil {
		t.Fatal("cache directory")
	}
	exact := filepath.Join(cache, multipartFinalDirectory)
	if multipartValidateLiveProfile(multipartFinalSmallProfile, exact) != nil || multipartValidateLiveProfile(multipartFinalSmallProfile, filepath.Join(cache, "another")) == nil || multipartValidateLiveProfile(multipartSmallProfile, exact) == nil {
		t.Fatal("final live batch directory must be exact and profile-exclusive")
	}
	for _, mutate := range []func(*multipartState){
		func(st *multipartState) { st.UploadReserved = multipartSmallSize + 1 },
		func(st *multipartState) { st.ReadReserved = 3*multipartSmallSize + 1 },
		func(st *multipartState) { st.Metadata = 32<<20 + 1 },
	} {
		st := s.state
		mutate(&st)
		if multipartManifest(st) {
			t.Fatal("final profile exceeded fixed byte allowance")
		}
	}
}

func TestQuarkMultipartOfflineFinalUnknownLengthBudget(t *testing.T) {
	for _, profile := range []string{multipartSmallProfile, multipartFinalSmallProfile} {
		t.Run(profile, func(t *testing.T) {
			s, parent := multipartFixture(t, profile)
			g := newMultipartGuard(s)
			g.delay = 0
			calls := 0
			transport := g.wrap(multipartTestTransport(func(*http.Request) (*http.Response, error) {
				calls++
				return &http.Response{StatusCode: 200, Header: http.Header{}, ContentLength: -1, Body: io.NopCloser(strings.NewReader(`{"status":0,"data":{}}`))}, nil
			}), false)
			request := func() error {
				r, _ := http.NewRequest("GET", "https://open-api-drive.quark.cn/user?method=user_info", nil)
				resp, err := transport.RoundTrip(r)
				if resp != nil {
					resp.Body.Close()
				}
				return err
			}
			accepted := 31
			if profile == multipartFinalSmallProfile {
				accepted = 112
			}
			for i := 0; i < accepted; i++ {
				if request() != nil {
					t.Fatal("unknown-length reservation rejected within frozen allowance")
				}
				if i == 31 && s.state.Metadata != 32*(256<<10+1) {
					t.Fatal("32nd API response was not reserved")
				}
			}
			if s.state.Metadata != int64(accepted)*(256<<10+1) || request() == nil {
				t.Fatal("reservation or request maximum was not enforced")
			}
			if profile == multipartFinalSmallProfile && calls != 112 || profile == multipartSmallProfile && calls != 32 {
				t.Fatal("unexpected dispatch beyond fixed budget")
			}
			before := s.state.Metadata
			s.lock.Close()
			cold, err := openMultipartStore(s.dir, parent, profile)
			if err != nil {
				t.Fatal("cold original budget")
			}
			defer cold.lock.Close()
			if cold.state.Metadata != before || multipartMetadataBudget(cold.state) != multipartMetadataBudget(s.state) {
				t.Fatal("cold reopen changed fixed reservation")
			}
		})
	}
	// Independent maximum response-reservation case: reaching the response may
	// consume one request, but its body cannot be read outside the fixed budget.
	s, _ := multipartFixture(t, multipartFinalSmallProfile)
	s.state.Metadata = 32<<20 - (256 << 10)
	g := newMultipartGuard(s)
	g.delay = 0
	reads := &multipartCountReader{r: strings.NewReader(`{"status":0}`)}
	tr := g.wrap(multipartTestTransport(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Header: http.Header{}, ContentLength: -1, Body: io.NopCloser(reads)}, nil
	}), false)
	r, _ := http.NewRequest("GET", "https://open-api-drive.quark.cn/user?method=user_info", nil)
	before := s.state.Metadata
	if _, err := tr.RoundTrip(r); err == nil || reads.n != 0 || s.state.Metadata != before {
		t.Fatal("metadata ceiling consumed unreserved bytes")
	}
}

func TestQuarkMultipartOfflineLocalPartDiagnostics(t *testing.T) {
	s, _ := multipartFixture(t, multipartSmallProfile)
	p := multipartPayload{Size: multipartSmallSize, Name: s.state.UploadName, SHA256: s.state.SourceSHA, TaskID: "PRIVATE-TASK", PartSize: 4 << 20, Phase: "put_received", Parts: []multipartPart{{Number: 1, ETag: "PRIVATE-ETAG"}}}
	s.state.PreResponse = json.RawMessage(`{"task_id":"PRIVATE-TASK","part_size":4194304}`)
	s.state.Put, s.state.UploadActual, s.state.UploadReserved = 1, 4<<20, 4<<20
	s.state.Writes["part:1"] = true
	s.state.ETags[1] = "PRIVATE-ETAG"
	dest := multipartDestination{Number: 1, URL: "https://private.invalid/?token=SECRET"}
	dest.Signature.Value = "PRIVATE-SIGNATURE"
	s.state.Destinations[1] = dest
	set := func(state string) {
		raw, _ := json.Marshal(p)
		s.state.Operations[s.state.UploadID] = cloud.OperationReceipt{State: state, Payload: raw}
	}
	checkRedaction := func(out multipartInspection) {
		t.Helper()
		raw, _ := json.Marshal(out)
		for _, secret := range []string{"PRIVATE", "SECRET", "private.invalid", s.state.UploadID, s.state.SourceSHA} {
			if strings.Contains(string(raw), secret) {
				t.Fatal("private diagnostic field escaped")
			}
		}
	}
	set("accepted")
	out := inspectMultipartState(s.state)
	if out.ReceiptState != "accepted" || out.ReceiptPhase != "put_received" || out.DiagnosticStage != "next_part_url_or_response_unresolved" || out.NextPart != 2 || out.NextDestinationRecorded || !out.PartETagsMatch || len(out.ConfirmedParts) != 1 || out.ConfirmedParts[0] != 1 || out.URLResponseEvidence != "not_recorded_by_original_tool" {
		t.Fatal("saved first-part diagnostic")
	}
	checkRedaction(out)
	// A saved second destination and native in-flight checkpoint, without a
	// dispatch reservation, is distinct from a failed URL acquisition.
	p.Phase, p.InFlightPart = "put_inflight", 2
	dest.Number = 2
	s.state.Destinations[2] = dest
	set("unknown")
	out = inspectMultipartState(s.state)
	if out.DiagnosticStage != "part_checkpoint_without_dispatch" || out.InFlightPart != 2 || !out.NextDestinationRecorded || len(out.DispatchedParts) != 1 {
		t.Fatal("undispatched second-part diagnostic")
	}
	checkRedaction(out)
	s.state.Writes["part:2"] = true
	set("unknown")
	if out = inspectMultipartState(s.state); out.DiagnosticStage != "part_dispatch_result_unresolved" || len(out.DispatchedParts) != 2 {
		t.Fatal("uncertain part reservation")
	}
	// Invalid or attacker-controlled strings/numbers never become output.
	p.Phase, p.InFlightPart = "PRIVATE-PHASE", 999999
	p.Parts = append(p.Parts, multipartPart{Number: 999999, ETag: "PRIVATE-ETAG"})
	set("PRIVATE-STATE")
	out = inspectMultipartState(s.state)
	if out.ReceiptState != "unrecognized" || out.ReceiptPhase != "unrecognized" || out.InFlightPartValid || out.InFlightPart != 0 || out.ReceiptPartsValid || out.PartETagsMatch {
		t.Fatal("unsafe diagnostic shape")
	}
	checkRedaction(out)
	p.Phase, p.InFlightPart = "put_received", 0
	set("accepted")
	if out = inspectMultipartState(s.state); out.DiagnosticStage != "receipt_parts_unresolved" {
		t.Fatal("invalid parts must not be diagnosed as all confirmed")
	}
	p.Parts = p.Parts[:1]
	p.PartSize = 0
	set("accepted")
	if out = inspectMultipartState(s.state); out.DiagnosticStage != "receipt_parts_unresolved" {
		t.Fatal("invalid plan must not be diagnosed as all confirmed")
	}
	p.PartSize = 4 << 20
	// InspectOnly remains local even with expired state and unresolved parts.
	p.Phase, p.InFlightPart = "put_received", 0
	p.Parts = p.Parts[:1]
	set("accepted")
	s.state.Started = time.Now().UTC().Add(-2 * multipartLifetime)
	if s.save() != nil {
		t.Fatal("save diagnostic fixture")
	}
	type saved struct {
		bytes    []byte
		modified time.Time
	}
	files := map[string]saved{}
	for _, name := range []string{"multipart.enc", "multipart.key", "sample.bin"} {
		path := filepath.Join(s.dir, name)
		data, err := os.ReadFile(path)
		info, se := os.Stat(path)
		if err != nil || se != nil {
			t.Fatal("read synthetic local evidence")
		}
		files[name] = saved{data, info.ModTime()}
	}
	s.lock.Close()
	result, err := inspectMultipartDirectory(s.dir)
	if err != nil || !result.DeadlineExpired || result.InspectionCloudRequests != 0 || result.DiagnosticStage != "next_part_url_or_response_unresolved" {
		t.Fatal("expired local receipt inspection")
	}
	checkRedaction(result)
	for name, previous := range files {
		path := filepath.Join(s.dir, name)
		data, err := os.ReadFile(path)
		info, se := os.Stat(path)
		if err != nil || se != nil || !bytes.Equal(data, previous.bytes) || !info.ModTime().Equal(previous.modified) {
			t.Fatal("local inspection changed original files")
		}
	}
}

func TestQuarkLiveMultipart(t *testing.T) {
	agentDir, tvDir, parentDir, dir := os.Getenv("OMC_QUARK_LIVE_AGENT_STATE"), os.Getenv("OMC_QUARK_LIVE_TV_STATE"), os.Getenv("OMC_QUARK_MULTIPART_PARENT"), os.Getenv("OMC_QUARK_MULTIPART_STATE")
	if agentDir == "" || tvDir == "" || parentDir == "" || dir == "" {
		t.Skip("explicit completed donor and independent multipart batch required")
	}
	profile := os.Getenv("OMC_QUARK_MULTIPART_PROFILE")
	if profile == "" {
		profile = multipartStandardProfile
	}
	if multipartValidateLiveProfile(profile, dir) != nil {
		t.Fatal("multipart_fixed_profile_directory_required")
	}
	check := func(stage string, e error) {
		t.Helper()
		if e != nil {
			t.Fatalf("multipart_stage=%s outcome=stopped_preserve_original_batch", stage)
		}
	}
	for _, a := range []string{agentDir, tvDir, parentDir} {
		for _, pair := range [][2]string{{a, dir}, {dir, a}} {
			rel, e := filepath.Rel(pair[0], pair[1])
			if e != nil || rel == "." || rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
				t.Fatal("independent_nonoverlapping_batch_required")
			}
		}
	}
	agent, e := openObservationAgent(agentDir)
	check("original_agent", e)
	defer agent.lock.Close()
	defer func() {
		if agent.unchanged() != nil {
			t.Error("original_agent_changed")
		}
	}()
	tv, e := openLiveTVStore(tvDir)
	check("original_tv", e)
	defer tv.lock.Close()
	defer func() {
		if tv.unchanged() != nil {
			t.Error("original_tv_changed")
		}
	}()
	if tv.checkpoint != nil || tv.attempt != nil || !tv.session.IdentityVerified {
		t.Fatal("original_tv_requires_separate_recovery")
	}
	parent, e := openFollowParent(parentDir, agent, tv)
	check("completed_donor", e)
	defer parent.store.lock.Close()
	defer func() {
		if parent.unchanged() != nil {
			t.Error("completed_donor_changed")
		}
	}()
	s, e := openMultipartStore(dir, parent, profile)
	check("new_batch", e)
	defer s.lock.Close()
	size := multipartStateSize(s.state)
	if s.state.Completed {
		t.Log("multipart_completed=true cloud_requests=0")
		return
	}
	if s.state.Stopped != "" {
		t.Fatal("multipart_not_exercised=true cloud_requests=0 preserve_batch=true")
	}
	if !time.Now().Before(s.state.Started.Add(multipartLifetime)) {
		t.Fatal("multipart_deadline_expired=true cloud_requests=0")
	}
	ctx, cancel := context.WithDeadline(context.Background(), s.state.Started.Add(multipartLifetime))
	defer cancel()
	s.cancel = cancel
	b := tv.accessOnly()
	a := agent.session
	b.Agent = &quark.Authorization{AccessToken: a.Access, DeviceID: a.Device, ClientDeviceID: a.Client, AccountID: a.User, Generation: a.Generation, IdentityVerified: true}
	b.Proofs = append([]quark.SpaceProof(nil), parent.store.state.Bundle.Proofs...)
	b.Revision = s.state.Bundle.Revision + 1
	if b.TV == nil || b.Web != nil || b.TV.RefreshToken != "" || b.TV.AccountID != s.state.Identity.TVAccount || b.TV.DeviceID != s.state.Identity.TVDevice || !b.HasProof(s.state.Identity.Proof.TVRoot, s.state.Identity.Proof.AgentRoot) {
		t.Fatal("access_identity_changed")
	}
	s.state.Bundle = b
	check("access_snapshot", s.save())
	g := newMultipartGuard(s)
	defer func() {
		st := s.state
		t.Logf("requests=%d api=%d upload_pre=%d part_puts=%d upload_finish=%d mkdir=%d original_part_queries=%d media_reads=%d upload_reserved=%d upload_actual=%d read_reserved=%d read_actual=%d metadata_reserved=%d metadata_budget=%d last_kind=%s last_http=%d", st.Requests, st.API, st.Pre, st.Put, st.Finish, st.Mkdir, st.Queries, st.Reads, st.UploadReserved, st.UploadActual, st.ReadReserved, st.ReadActual, st.Metadata, multipartMetadataBudget(st), g.lastKind, g.lastStatus)
	}()
	auth := quark.NewObservationValidationAuthenticator(cloud.Config{}, g.wrap)
	check("tv_identity", auth.Verify(ctx, quark.RouteTV, b))
	check("agent_identity", auth.Verify(ctx, quark.RouteAgent, b))
	raw, _ := json.Marshal(b)
	d, e := quark.NewObservationValidationClient(cloud.Config{Credentials: raw, Operations: s}, g.wrap)
	check("native_client", e)
	check("owned_root", d.ValidateObservationOwnedRoot(ctx, s.state.Identity.ProofRootACK, strings.TrimPrefix(s.state.Identity.Proof.Path, "/")))
	s.state.RootVerified = true
	check("owned_root_checkpoint", s.save())
	original, e := d.ResolveDirectory(ctx, s.state.Identity.Proof.Path)
	check("proof_root", e)
	if original.ID != s.state.Identity.Proof.TVRoot || !original.IsDir {
		t.Fatal("proof_root_changed")
	}
	dirctx := quark.WithOperationID(ctx, s.state.DirectoryID)
	child, e := d.CreateDirectory(dirctx, s.state.Identity.Proof.AgentRoot, s.state.DirectoryName)
	check("mkdir", e)
	tvChild, e := d.ResolveReadItem(dirctx, child.ID)
	check("mkdir_mapping", e)
	if !tvChild.IsDir || tvChild.ParentID != original.ID || tvChild.Name != s.state.DirectoryName {
		t.Fatal("child_identity_changed")
	}
	managed, e := d.ResolveManagementPath(ctx, s.state.Identity.Proof.TVRoot, s.state.Identity.Proof.AgentRoot, "/"+s.state.DirectoryName)
	check("child_management", e)
	ag, ok := followRawID(managed.ID, quark.RouteAgent, s.state.Identity.AgentAccount)
	if !ok || !managed.IsDir {
		t.Fatal("child_management_invalid")
	}
	tvID, ok := followRawID(tvChild.ID, quark.RouteTV, s.state.Identity.TVAccount)
	if !ok {
		t.Fatal("child_tv_invalid")
	}
	if s.state.ChildAgent != "" && !quark.ObservationSameAgentHandle(s.state.ChildAgent, ag) || s.state.ChildTV != "" && s.state.ChildTV != tvID {
		t.Fatal("child_changed_on_resume")
	}
	s.state.ChildAgent, s.state.ChildTV = ag, tvID
	s.state.AgentIDs[ag] = true
	s.state.AgentParents[ag] = true
	s.state.TVParents[tvID] = true
	check("child_checkpoint", s.save())
	source, e := s.source()
	check("immutable_source", e)
	defer source.Close()
	upctx := quark.WithOperationID(ctx, s.state.UploadID)
	item, e := d.Upload(upctx, cloud.UploadRequest{ParentID: managed.ID, Name: s.state.UploadName, Size: size, Reader: source})
	if s.state.Stopped != "" {
		t.Fatal("multipart_not_exercised=true accepted_pre_preserved=true")
	}
	check("multipart_upload", e)
	item, e = d.ResolveReadItem(upctx, item.ID)
	check("upload_tv_mapping", e)
	if item.ParentID != tvChild.ID || item.Name != s.state.UploadName || item.IsDir || item.Size != size {
		t.Fatal("uploaded_output_changed")
	}
	read, e := d.OpenRead(ctx, cloud.ReadRequest{FileID: item.ID})
	check("explicit_complete_readback", e)
	h := sha256.New()
	n, e := io.Copy(h, io.LimitReader(read.Body, size+1))
	read.Body.Close()
	check("readback_body", e)
	if n != size || hex.EncodeToString(h.Sum(nil)) != s.state.SourceSHA {
		t.Fatal("full_readback_mismatch")
	}
	p, ok := multipartDecode(s)
	count, eligible := multipartPlan(p)
	if !ok || !eligible || s.state.Put != count || s.state.UploadActual != size || s.state.UploadReserved != size || s.state.Pre != 1 || s.state.Finish != 1 || len(p.Parts) != count || s.state.Operations[s.state.UploadID].State != "confirmed" || s.state.Operations[s.state.DirectoryID].State != "confirmed" {
		t.Fatal("multipart_evidence_incomplete")
	}
	// Recreate the native client and query only its original confirmed receipt.
	before := s.state.Requests
	cold, e := quark.NewObservationValidationClient(cloud.Config{Credentials: raw, Operations: s}, g.wrap)
	check("cold_native_client", e)
	result, e := cold.QueryOperation(ctx, s.state.UploadID)
	check("cold_original_receipt", e)
	if !result.Complete || s.state.Requests != before {
		t.Fatal("cold_query_not_local")
	}
	check("original_agent_unchanged", agent.unchanged())
	check("original_tv_unchanged", tv.unchanged())
	check("completed_donor_unchanged", parent.unchanged())
	s.state.Completed = true
	check("completed_checkpoint", s.save())
	t.Logf("multipart_completed=true parts=%d sample_bytes=%d full_sha256_match=true cold_confirmed_query_requests=0", count, size)
}

// Offline tests use an independently generated synthetic account and an actual
// loopback TLS server. The rewrite lives below the guard, never in live input.
type multipartTestTransport func(*http.Request) (*http.Response, error)

func (f multipartTestTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func multipartFixture(t *testing.T, profiles ...string) (*multipartStore, *followParent) {
	t.Helper()
	p := quark.SpaceProof{Version: 1, Generation: 1, TVAccount: "tv-test", AgentAccount: "agent-test", TVRoot: quark.QualifyID(quark.RouteTV, "tv-test", "T"), AgentRoot: quark.QualifyID(quark.RouteAgent, "agent-test", "A"), Path: "/owned", ReceiptID: "old-proof", ContentSHA256: strings.Repeat("a", 64), VerifiedAt: time.Now().UTC().Add(-time.Hour)}
	b := quark.Credentials{Version: 1, Revision: 1, TV: &quark.Authorization{AccessToken: "tv-access-test-secret", DeviceID: "tv-device", AccountID: p.TVAccount, Generation: 1, IdentityVerified: true}, Agent: &quark.Authorization{AccessToken: "agent-access-test-secret", DeviceID: "agent-device", ClientDeviceID: "client-device", AccountID: p.AgentAccount, Generation: 1, IdentityVerified: true}, Proofs: []quark.SpaceProof{p}}
	parent := &followParent{identity: followIdentity{AgentAccount: p.AgentAccount, TVAccount: p.TVAccount, Proof: p, ProofRootACK: "A", JournalSHA: strings.Repeat("b", 64), KeySHA: strings.Repeat("c", 64)}, store: &observationStore{state: observationState{Bundle: b, Completed: true}}}
	s, e := openMultipartStore(observationOfflinePrivateDir(t), parent, profiles...)
	if e != nil {
		t.Fatal("fixture store")
	}
	t.Cleanup(func() { s.lock.Close() })
	s.state.RootVerified = true
	s.state.ChildAgent = "A"
	s.state.ChildTV = "T"
	return s, parent
}
func multipartOfflineClient(t *testing.T, s *multipartStore, lost bool, partSize int64) (*quark.Client, *multipartGuard, *int) {
	t.Helper()
	size := multipartStateSize(s.state)
	puts := 0
	complete := false
	childCreated := false
	uploadParent := "A"
	recorded := map[int]string{}
	lostOnce := lost
	agentFile := func(id, name string, dir bool, size int64) map[string]any {
		kind, parent := "1", "A"
		if dir {
			kind = "0"
			parent = "0"
			if id == "C" {
				parent = "A"
			}
		} else {
			parent = uploadParent
		}
		return map[string]any{"fid": id, "parent_fid": parent, "filename": name, "file_type": kind, "size": size}
	}
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		envelope := func(data any) {
			w.Header().Set("Content-Type", "application/json")
			raw, _ := json.Marshal(map[string]any{"status": 0, "data": data})
			w.Header().Set("Content-Length", strconv.Itoa(len(raw)))
			_, _ = w.Write(raw)
		}
		switch r.URL.Path {
		case "/open/v1/dir":
			childCreated = true
			envelope(map[string]any{"fid": "C", "finish": true})
		case "/open/v1/file/info":
			if r.URL.Query().Get("fid") == "A" {
				envelope(agentFile("A", "owned", true, 0))
			} else if r.URL.Query().Get("fid") == "C" {
				envelope(agentFile("C", s.state.DirectoryName, true, 0))
			} else {
				envelope(agentFile("created", s.state.UploadName, false, size))
			}
		case "/open/v1/file/list":
			var in struct {
				Parent string `json:"parent_fid"`
			}
			_ = json.NewDecoder(r.Body).Decode(&in)
			files := []any{}
			if in.Parent == "0" {
				files = append(files, agentFile("A", "owned", true, 0))
			} else if in.Parent == "A" && childCreated {
				files = append(files, agentFile("C", s.state.DirectoryName, true, 0))
			} else if in.Parent == uploadParent && complete {
				files = append(files, agentFile("created", s.state.UploadName, false, size))
			}
			envelope(map[string]any{"last_page": true, "file_list": files})
		case "/file":
			if r.URL.Query().Get("method") == "download" {
				envelope(map[string]any{"fid": "created-tv", "file_name": s.state.UploadName, "size": size, "download_url": "https://objects.example/read"})
				return
			}
			files := []any{}
			if r.URL.Query().Get("parent_fid") == "0" {
				files = append(files, map[string]any{"fid": "T", "filename": "owned", "size": 0, "isdir": 1})
			} else if r.URL.Query().Get("parent_fid") == "T" && childCreated {
				files = append(files, map[string]any{"fid": "CT", "filename": s.state.DirectoryName, "size": 0, "isdir": 1})
			} else if (r.URL.Query().Get("parent_fid") == "T" && !childCreated || r.URL.Query().Get("parent_fid") == "CT" && childCreated) && complete {
				files = append(files, map[string]any{"fid": "created-tv", "filename": s.state.UploadName, "size": size, "isdir": 0})
			}
			envelope(map[string]any{"total_count": len(files), "files": files})
		case "/open/v1/file/upload_pre":
			var input struct {
				Parent string `json:"pdir_fid"`
			}
			_ = json.NewDecoder(r.Body).Decode(&input)
			uploadParent = input.Parent
			if partSize == 0 {
				envelope(map[string]any{"fid": "created", "finish": true})
				return
			}
			envelope(map[string]any{"task_id": "original-upload-task", "part_size": partSize, "upload_urls": []any{}, "common_headers": map[string]string{"Content-Type": "application/octet-stream"}})
		case "/open/v1/file/get_upload_urls":
			var in struct {
				Parts []struct {
					Number int `json:"part_number"`
				} `json:"part_info_list"`
			}
			_ = json.NewDecoder(r.Body).Decode(&in)
			n := in.Parts[0].Number
			envelope(map[string]any{"upload_urls": []any{map[string]any{"part_number": n, "upload_url": "https://objects.example/part/" + strconv.Itoa(n), "signature_info": map[string]string{"signature": "object-only-signature"}}}})
		case "/open/v1/file/list_uploaded_parts":
			parts := []multipartPart{}
			for n, e := range recorded {
				parts = append(parts, multipartPart{n, e})
			}
			envelope(map[string]any{"uploaded_parts": parts})
		case "/open/v1/file/upload_finish":
			complete = true
			envelope(map[string]any{"fid": "created", "finish": true})
		case "/read":
			w.Header().Set("Content-Length", strconv.FormatInt(size, 10))
			f, e := os.Open(filepath.Join(s.dir, "sample.bin"))
			if e != nil {
				t.Error("fixture source")
				return
			}
			defer f.Close()
			_, _ = io.Copy(w, f)
		default:
			if strings.HasPrefix(r.URL.Path, "/part/") {
				n, _ := strconv.Atoi(strings.TrimPrefix(r.URL.Path, "/part/"))
				_, e := io.Copy(io.Discard, r.Body)
				if e != nil {
					t.Error("fixture PUT")
				}
				puts++
				recorded[n] = fmt.Sprintf("etag-%d", n)
				w.Header().Set("ETag", recorded[n])
				w.WriteHeader(200)
				return
			}
			t.Error("unexpected offline endpoint")
			w.WriteHeader(400)
		}
	}))
	t.Cleanup(server.Close)
	base := server.Client().Transport
	u, _ := url.Parse(server.URL)
	routed := multipartTestTransport(func(r *http.Request) (*http.Response, error) {
		rr := r.Clone(r.Context())
		copyURL := *r.URL
		copyURL.Scheme, copyURL.Host = u.Scheme, u.Host
		rr.URL = &copyURL
		rr.Host = u.Host
		resp, e := base.RoundTrip(rr)
		if lostOnce && strings.HasPrefix(r.URL.Path, "/part/") {
			lostOnce = false
			if resp != nil {
				resp.Body.Close()
			}
			return nil, errors.New("synthetic lost ACK private-url-token")
		}
		return resp, e
	})
	g := newMultipartGuard(s)
	g.delay = 0
	raw, _ := json.Marshal(s.state.Bundle)
	d, e := quark.NewObservationValidationClient(cloud.Config{Credentials: raw, Operations: s}, func(_ http.RoundTripper, media bool) http.RoundTripper { return g.wrap(routed, media) })
	if e != nil {
		t.Fatal("offline client")
	}
	return d, g, &puts
}
func TestQuarkMultipartOfflineNativeLostACK(t *testing.T) {
	multipartOfflineNativeLostACK(t, multipartStandardProfile, 16<<20)
}
func TestQuarkMultipartOfflineSmallNativeLostACK(t *testing.T) {
	multipartOfflineNativeLostACK(t, multipartSmallProfile, 4<<20)
}
func TestQuarkMultipartOfflineFinalNativeLostACK(t *testing.T) {
	multipartOfflineNativeLostACK(t, multipartFinalSmallProfile, 4<<20)
}
func multipartOfflineNativeLostACK(t *testing.T, profile string, partSize int64) {
	t.Helper()
	size := multipartProfileSize(profile)
	s, parent := multipartFixture(t, profile)
	d, _, puts := multipartOfflineClient(t, s, true, partSize)
	run := func() (cloud.Item, error) {
		f, e := s.source()
		if e != nil {
			return cloud.Item{}, e
		}
		defer f.Close()
		return d.Upload(quark.WithOperationID(context.Background(), s.state.UploadID), cloud.UploadRequest{ParentID: s.state.Identity.Proof.AgentRoot, Name: s.state.UploadName, Size: size, Reader: f})
	}
	_, e := run()
	if e == nil || s.state.Put != 1 || s.state.Operations[s.state.UploadID].State != "unknown" {
		t.Fatal("lost ACK not preserved")
	}
	// Actual encrypted cold store reload, unchanged sample and request budget.
	s.lock.Close()
	reloaded, e := openMultipartStore(s.dir, parent, profile)
	if e != nil {
		t.Fatal("cold store")
	}
	s.state = reloaded.state
	s.lock = reloaded.lock
	defer reloaded.lock.Close()
	item, e := run()
	if e != nil {
		code, _ := cloud.ErrorInfo(e)
		t.Fatalf("native resume code=%s", code)
	}
	if *puts != 3 || s.state.Put != 3 || s.state.Pre != 1 || s.state.Finish != 1 || s.state.Queries != 1 || s.state.UploadActual != size {
		t.Fatal("multipart dispatch accounting")
	}
	before := s.state.Requests
	_, e = run()
	if e != nil || s.state.Put != 3 || s.state.Pre != 1 || s.state.Finish != 1 {
		t.Fatal("confirmed upload replayed")
	}
	_ = before
	item, e = d.ResolveReadItem(quark.WithOperationID(context.Background(), s.state.UploadID), item.ID)
	if e != nil {
		t.Fatal("native TV mapping")
	}
	read, e := d.OpenRead(context.Background(), cloud.ReadRequest{FileID: item.ID})
	if e != nil {
		t.Fatal("native full read")
	}
	n, e := io.Copy(io.Discard, read.Body)
	read.Body.Close()
	if e != nil || n != size || s.state.ReadActual < size || s.state.ReadActual > 3*size {
		t.Fatal("full read accounting")
	}
	raw, _ := json.Marshal(s.state.Bundle)
	called := false
	cold, e := quark.NewObservationValidationClient(cloud.Config{Credentials: raw, Operations: s}, func(_ http.RoundTripper, _ bool) http.RoundTripper {
		return multipartTestTransport(func(*http.Request) (*http.Response, error) { called = true; return nil, multipartError() })
	})
	if e != nil {
		t.Fatal("cold client")
	}
	result, e := cold.QueryOperation(context.Background(), s.state.UploadID)
	if e != nil || !result.Complete || called {
		t.Fatal("confirmed cold query dispatched")
	}
	// No cleanup deletes the private source or cloud object. Changing the local
	// source is rejected instead of creating another operation.
	f, e := os.OpenFile(filepath.Join(s.dir, "sample.bin"), os.O_WRONLY, 0600)
	if e != nil {
		t.Fatal(e)
	}
	first := make([]byte, 1)
	original, _ := os.Open(filepath.Join(s.dir, "sample.bin"))
	_, _ = original.ReadAt(first, 0)
	original.Close()
	first[0] ^= 0xff
	_, _ = f.WriteAt(first, 0)
	f.Close()
	if _, e = s.source(); e == nil {
		t.Fatal("changed sample accepted")
	}
}

func TestQuarkMultipartOfflineOwnedDirectoryAndTwoParts(t *testing.T) {
	multipartOfflineOwnedDirectoryAndParts(t, multipartStandardProfile, 32<<20, 2)
}
func TestQuarkMultipartOfflineSmallOwnedDirectoryAndThreeParts(t *testing.T) {
	multipartOfflineOwnedDirectoryAndParts(t, multipartSmallProfile, 4<<20, 3)
}
func TestQuarkMultipartOfflineFinalOwnedDirectoryAndThreeParts(t *testing.T) {
	multipartOfflineOwnedDirectoryAndParts(t, multipartFinalSmallProfile, 4<<20, 3)
}
func multipartOfflineOwnedDirectoryAndParts(t *testing.T, profile string, partSize int64, expectedParts int) {
	t.Helper()
	size := multipartProfileSize(profile)
	s, _ := multipartFixture(t, profile)
	s.state.ChildAgent = ""
	s.state.ChildTV = ""
	d, _, puts := multipartOfflineClient(t, s, false, partSize)
	ctx := context.Background()
	dirctx := quark.WithOperationID(ctx, s.state.DirectoryID)
	child, e := d.CreateDirectory(dirctx, s.state.Identity.Proof.AgentRoot, s.state.DirectoryName)
	if e != nil {
		t.Fatal("native mkdir")
	}
	root, e := d.ResolveReadItem(dirctx, child.ID)
	if e != nil || !root.IsDir || root.Name != s.state.DirectoryName || root.ParentID != s.state.Identity.Proof.TVRoot {
		t.Fatal("native directory mapping")
	}
	managed, e := d.ResolveManagementPath(ctx, s.state.Identity.Proof.TVRoot, s.state.Identity.Proof.AgentRoot, "/"+s.state.DirectoryName)
	if e != nil {
		t.Fatal("native management mapping")
	}
	s.state.ChildAgent, _ = followRawID(managed.ID, quark.RouteAgent, s.state.Identity.AgentAccount)
	s.state.ChildTV, _ = followRawID(root.ID, quark.RouteTV, s.state.Identity.TVAccount)
	s.state.AgentIDs[s.state.ChildAgent] = true
	s.state.AgentParents[s.state.ChildAgent] = true
	s.state.TVParents[s.state.ChildTV] = true
	f, e := s.source()
	if e != nil {
		t.Fatal(e)
	}
	defer f.Close()
	upctx := quark.WithOperationID(ctx, s.state.UploadID)
	item, e := d.Upload(upctx, cloud.UploadRequest{ParentID: managed.ID, Name: s.state.UploadName, Size: size, Reader: f})
	if e != nil {
		t.Fatal("native two-part upload")
	}
	item, e = d.ResolveReadItem(upctx, item.ID)
	if e != nil || item.ParentID != root.ID {
		t.Fatal("new subdirectory TV output")
	}
	read, e := d.OpenRead(ctx, cloud.ReadRequest{FileID: item.ID})
	if e != nil {
		t.Fatal("original-file full read")
	}
	h := sha256.New()
	n, e := io.Copy(h, read.Body)
	read.Body.Close()
	if e != nil || hex.EncodeToString(h.Sum(nil)) != s.state.SourceSHA || n != size || *puts != expectedParts || s.state.Mkdir != 1 || s.state.Pre != 1 || s.state.Finish != 1 || s.state.UploadActual != size || s.state.ReadActual < size {
		t.Fatal("dedicated directory evidence")
	}
	before := s.state.Requests
	_, e = d.CreateDirectory(dirctx, s.state.Identity.Proof.AgentRoot, s.state.DirectoryName)
	if e != nil || s.state.Requests != before || s.state.Mkdir != 1 {
		t.Fatal("confirmed mkdir replayed")
	}
}

func TestQuarkMultipartOfflineExactPartAndFinish(t *testing.T) {
	s, _ := multipartFixture(t)
	g := newMultipartGuard(s)
	g.delay = 0
	p := multipartPayload{Parent: s.state.ChildAgent, Name: s.state.UploadName, Size: multipartSize, SHA256: s.state.SourceSHA, PartSize: 16 << 20, TaskID: "original", Phase: "put_inflight", InFlightPart: 3}
	payload, _ := json.Marshal(p)
	s.state.Operations[s.state.UploadID] = cloud.OperationReceipt{ID: s.state.UploadID, Provider: cloud.ProviderQuark, AccountID: s.state.Identity.AgentAccount, Route: string(quark.RouteAgent), Kind: "upload", Revision: 1, State: "accepted", Payload: payload}
	s.state.Eligible = true
	dest := multipartDestination{Number: 3, URL: "https://objects.example/part/3"}
	dest.Signature.Value = "object-signature"
	s.state.Destinations[3] = dest
	source, e := s.source()
	if e != nil {
		t.Fatal(e)
	}
	last := make([]byte, 1)
	_, _ = source.ReadAt(last, multipartSize-1)
	source.Close()
	called := 0
	base := multipartTestTransport(func(r *http.Request) (*http.Response, error) {
		called++
		_, _ = io.Copy(io.Discard, r.Body)
		return &http.Response{StatusCode: 200, ContentLength: 0, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(""))}, nil
	})
	put := func(data []byte, u, auth string) error {
		r, _ := http.NewRequest("PUT", u, bytes.NewReader(data))
		r.Header.Set("Authorization", auth)
		r.Header.Set("Accept-Encoding", "identity")
		_, e := g.wrap(base, true).RoundTrip(r)
		return e
	}
	if put([]byte{last[0] ^ 0xff}, dest.URL, dest.Signature.Value) == nil || put(append(last, 0), dest.URL, dest.Signature.Value) == nil || put(last, "https://objects.example/other", dest.Signature.Value) == nil || put(last, dest.URL, s.state.Bundle.Agent.AccessToken) == nil || called != 0 {
		t.Fatal("wrong segment, destination or account authorization accepted")
	}
	if put(last, dest.URL, dest.Signature.Value) == nil || called != 1 || s.state.ETags[3] != "" || s.state.Put != 1 || s.state.UploadReserved != 1 || s.state.UploadActual != 1 {
		t.Fatal("missing ETag did not preserve uncertain original PUT")
	}
	if put(last, dest.URL, dest.Signature.Value) == nil || called != 1 {
		t.Fatal("no-ETag PUT replayed")
	}
	p.Phase = "finish_inflight"
	p.InFlightPart = 0
	p.Parts = []multipartPart{{1, "one"}, {2, "two"}, {3, "three"}}
	payload, _ = json.Marshal(p)
	r := s.state.Operations[s.state.UploadID]
	r.Payload = payload
	s.state.Operations[s.state.UploadID] = r
	s.state.Writes["part:1"] = true
	s.state.Writes["part:2"] = true
	s.state.ETags = map[int]string{1: "one", 2: "two", 3: "three"}
	wrong := []multipartPart{{1, "one"}, {2, "changed"}, {3, "three"}}
	body, _ := json.Marshal(map[string]any{"task_id": p.TaskID, "part_info_list": wrong})
	request, _ := http.NewRequest("POST", "https://open-api-drive.quark.cn/open/v1/file/upload_finish", bytes.NewReader(body))
	if _, e = g.wrap(base, false).RoundTrip(request); e == nil || called != 1 || s.state.Finish != 0 {
		t.Fatal("finish accepted unobserved ETag")
	}
}

func TestQuarkMultipartOfflineAnonymousReadBudget(t *testing.T) {
	multipartOfflineAnonymousReadBudget(t, multipartStandardProfile)
}
func TestQuarkMultipartOfflineSmallAnonymousReadBudget(t *testing.T) {
	multipartOfflineAnonymousReadBudget(t, multipartSmallProfile)
}
func multipartOfflineAnonymousReadBudget(t *testing.T, profile string) {
	t.Helper()
	size := multipartProfileSize(profile)
	s, _ := multipartFixture(t, profile)
	g := newMultipartGuard(s)
	g.delay = 0
	u := "https://objects.example/read"
	s.state.ReadURLs[u] = size
	called := 0
	base := multipartTestTransport(func(*http.Request) (*http.Response, error) {
		called++
		return &http.Response{StatusCode: 200, ContentLength: size, Header: http.Header{}, Body: io.NopCloser(strings.NewReader("short"))}, nil
	})
	get := func(header, value string) error {
		r, _ := http.NewRequest("GET", u, nil)
		r.Header.Set("Accept-Encoding", "identity")
		if header != "" {
			r.Header.Set(header, value)
		}
		_, e := g.wrap(base, true).RoundTrip(r)
		return e
	}
	for _, h := range []string{"Cookie", "Authorization", "Referer", "Range"} {
		if get(h, "rejected") == nil {
			t.Fatal("anonymous boundary")
		}
	}
	if called != 0 {
		t.Fatal("credentialed read dispatched")
	}
	for n := 0; n < 3; n++ {
		if get("", "") == nil {
			t.Fatal("partial body passed")
		}
	}
	if s.state.ReadReserved != 3*size || s.state.ReadActual != 15 || called != 3 {
		t.Fatal("partial reads refunded")
	}
	if get("", "") == nil || called != 3 {
		t.Fatal("fourth complete-object attempt exceeded byte cap")
	}
}

func TestQuarkMultipartOfflineHardBudgetsAndSecretErrors(t *testing.T) {
	s, _ := multipartFixture(t)
	g := newMultipartGuard(s)
	g.delay = 0
	called := 0
	tr := g.wrap(multipartTestTransport(func(*http.Request) (*http.Response, error) {
		called++
		return nil, errors.New("https://private.invalid/?token=SECRET")
	}), false)
	request := func() error {
		r, _ := http.NewRequest("GET", "https://open-api-drive.quark.cn/user?method=user_info", nil)
		_, e := tr.RoundTrip(r)
		return e
	}
	s.state.API = 112
	if request() == nil || called != 0 {
		t.Fatal("API limit")
	}
	s.state.API = 0
	s.state.Requests = 128
	if request() == nil || called != 0 {
		t.Fatal("total limit")
	}
	s.state.Requests = 0
	s.state.Started = time.Now().UTC().Add(-multipartLifetime)
	if request() == nil || called != 0 {
		t.Fatal("deadline limit")
	}
	s.state.Started = time.Now().UTC()
	e := request()
	if e == nil || strings.Contains(e.Error(), "SECRET") || strings.Contains(e.Error(), "private.invalid") || called != 1 {
		t.Fatal("unsafe error exposure")
	}
	s.state.Completed = true
	if request() == nil || called != 1 {
		t.Fatal("completed batch dispatched")
	}
}

func TestQuarkMultipartOfflinePreGate(t *testing.T) {
	multipartOfflinePreGate(t, multipartStandardProfile, []int64{0, 64, 4 << 20, 64 << 20})
}

func TestQuarkMultipartOfflineSmallPreGate(t *testing.T) {
	multipartOfflinePreGate(t, multipartSmallProfile, []int64{0, 64, 16 << 20})
}
func TestQuarkMultipartOfflineFinalPreGate(t *testing.T) {
	multipartOfflinePreGate(t, multipartFinalSmallProfile, []int64{0, 64, 16 << 20})
}

func multipartOfflinePreGate(t *testing.T, profile string, partSizes []int64) {
	t.Helper()
	for _, partSize := range partSizes {
		t.Run(strconv.FormatInt(partSize, 10), func(t *testing.T) {
			s, _ := multipartFixture(t, profile)
			d, _, puts := multipartOfflineClient(t, s, false, partSize)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			s.cancel = cancel
			f, e := s.source()
			if e != nil {
				t.Fatal(e)
			}
			defer f.Close()
			_, e = d.Upload(quark.WithOperationID(ctx, s.state.UploadID), cloud.UploadRequest{ParentID: s.state.Identity.Proof.AgentRoot, Name: s.state.UploadName, Size: multipartStateSize(s.state), Reader: f})
			p, ok := multipartDecode(s)
			if e == nil || !ok || partSize != 0 && (p.TaskID == "" || p.PartSize != partSize) || partSize == 0 && p.Handle == "" || s.state.Operations[s.state.UploadID].State != "accepted" || s.state.Stopped != "multipart_not_exercised" || *puts != 0 || s.state.Pre != 1 || len(s.state.PreResponse) == 0 {
				t.Fatal("pre acceptance evidence or stop missing")
			}
		})
	}
}

func TestQuarkMultipartOfflineGuardLimits(t *testing.T) {
	s, _ := multipartFixture(t)
	d, g, _ := multipartOfflineClient(t, s, true, 16<<20)
	f, e := s.source()
	if e != nil {
		t.Fatal(e)
	}
	_, _ = d.Upload(quark.WithOperationID(context.Background(), s.state.UploadID), cloud.UploadRequest{ParentID: s.state.Identity.Proof.AgentRoot, Name: s.state.UploadName, Size: multipartSize, Reader: f})
	f.Close()
	p, ok := multipartDecode(s)
	if !ok || p.InFlightPart != 1 {
		t.Fatal("missing in-flight original")
	}
	called := 0
	tr := g.wrap(multipartTestTransport(func(*http.Request) (*http.Response, error) {
		called++
		return nil, errors.New("private upstream error")
	}), false)
	query := func(task string) error {
		b, _ := json.Marshal(map[string]string{"task_id": task})
		r, _ := http.NewRequest("POST", "https://open-api-drive.quark.cn/open/v1/file/list_uploaded_parts", bytes.NewReader(b))
		_, e := tr.RoundTrip(r)
		return e
	}
	before := s.state.Requests
	if query("another-task") == nil || called != 0 || s.state.Requests != before {
		t.Fatal("foreign task accepted")
	}
	for n := 0; n < 2; n++ {
		if query(p.TaskID) == nil {
			t.Fatal("lost query accepted")
		}
	}
	if query(p.TaskID) == nil || called != 2 || s.state.Queries != 2 {
		t.Fatal("query cap")
	}
	// Deadline and counters survive reopen; no re-PUT, even if no ETag arrived.
	media := g.wrap(multipartTestTransport(func(*http.Request) (*http.Response, error) { called++; return nil, multipartError() }), true)
	dest := s.state.Destinations[1]
	f, e = s.source()
	if e != nil {
		t.Fatal(e)
	}
	defer f.Close()
	r, _ := http.NewRequest("PUT", dest.URL, io.LimitReader(f, p.PartSize))
	r.ContentLength = p.PartSize
	r.Header.Set("Authorization", dest.Signature.Value)
	r.Header.Set("Accept-Encoding", "identity")
	if _, e = media.RoundTrip(r); e == nil || called != 2 {
		t.Fatal("lost PUT replayed")
	}
	s.state.Requests = 128
	if query(p.TaskID) == nil || called != 2 {
		t.Fatal("total budget bypass")
	}
	s.state.Requests = before
	s.state.Started = time.Now().Add(-multipartLifetime)
	if query(p.TaskID) == nil || called != 2 {
		t.Fatal("expired batch dispatched")
	}
}
