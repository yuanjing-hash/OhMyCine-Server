//go:build quark_live

package handlers_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/yuanjing-hash/OhMyCine-Server/pkg/cloud"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/cloud/quark"
)

type proofCleanupPayload struct {
	observationProof
	Proof     *quark.SpaceProof `json:"proof"`
	Challenge *cloud.Item       `json:"challenge"`
	Cleanup   *struct {
		State     string `json:"state"`
		Agent, TV cloud.Item
	} `json:"cleanup"`
}

type proofCleanupGuard struct {
	mu                        sync.Mutex
	store                     *proofCleanupStore
	delay                     time.Duration
	agentIDs, tvFiles         map[string]bool
	readURLs                  map[string]bool
	rootAgent, rootTV         string
	lastEndpoint, lastOutcome string
	lastStatus, lastErrno     int
}

func newProofCleanupGuard(s *proofCleanupStore) *proofCleanupGuard {
	i := s.state.Identity
	a, _ := followRawID(i.Proof.AgentRoot, quark.RouteAgent, i.Proof.AgentAccount)
	tv, _ := followRawID(i.Proof.TVRoot, quark.RouteTV, i.Proof.TVAccount)
	g := &proofCleanupGuard{store: s, delay: 2 * time.Second, rootAgent: a, rootTV: tv, agentIDs: map[string]bool{i.RootACK: true, a: true}, tvFiles: map[string]bool{}, readURLs: map[string]bool{}}
	_, p, _, u, valid := g.payloads()
	if valid && u.Handle != "" {
		g.agentIDs[u.Handle] = true
	}
	if p.Challenge != nil {
		if raw, ok := followRawID(p.Challenge.ID, quark.RouteTV, i.Proof.TVAccount); ok {
			g.tvFiles[raw] = true
		}
	}
	return g
}

func (g *proofCleanupGuard) payloads() (cloud.OperationReceipt, proofCleanupPayload, cloud.OperationReceipt, observationUpload, bool) {
	s := g.store.state
	r, up := s.Operations[s.ProofID], s.Operations[s.ProofID+"/upload"]
	var p proofCleanupPayload
	u, ok := observationDecodeUpload(up.Payload)
	valid := g.store.allowed(r) && g.store.allowed(up) && json.Unmarshal(r.Payload, &p) == nil && ok && len(p.Bytes) == 32 && quark.IsVerificationName(p.Name) && p.Path == "/"+s.Identity.RootName && p.TVRoot == s.Identity.Proof.TVRoot && u.Name == p.Name && u.Size == 32 && u.SHA256 == followDigest(p.Bytes) && quark.ObservationSameAgentHandle(u.Parent, g.rootAgent)
	return r, p, up, u, valid
}

func (g *proofCleanupGuard) knownAgent(raw string) bool {
	for id := range g.agentIDs {
		if quark.ObservationSameAgentHandle(id, raw) {
			return true
		}
	}
	_, _, _, u, valid := g.payloads()
	return valid && u.Handle != "" && quark.ObservationSameAgentHandle(u.Handle, raw)
}

func (g *proofCleanupGuard) wrap(base http.RoundTripper, media bool) http.RoundTripper {
	return proofCleanupTransport{base, g, media}
}

type proofCleanupTransport struct {
	base  http.RoundTripper
	g     *proofCleanupGuard
	media bool
}

func (tr proofCleanupTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	g := tr.g
	g.mu.Lock() // Serializes dispatch, response bytes and the persisted interval.
	defer g.mu.Unlock()
	g.lastEndpoint, g.lastOutcome, g.lastStatus, g.lastErrno = "other", "guard_rejected", 0, 0
	if r == nil || r.URL == nil || tr.base == nil || r.Context().Err() != nil || r.URL.User != nil || r.URL.Fragment != "" || r.URL.RawPath != "" || r.URL.Opaque != "" || r.Host != "" && r.Host != r.URL.Host || r.Header.Get("Cookie") != "" {
		return nil, observationError()
	}
	var body []byte
	if r.Body != nil {
		var err error
		body, err = io.ReadAll(io.LimitReader(r.Body, 32769))
		_ = r.Body.Close()
		if err != nil || len(body) > 32768 {
			return nil, observationError()
		}
		r = r.Clone(r.Context())
		r.Body, r.GetBody = io.NopCloser(bytes.NewReader(body)), nil
	}
	var in map[string]json.RawMessage
	_ = json.Unmarshal(body, &in)
	str := func(key string) string { var value string; _ = json.Unmarshal(in[key], &value); return value }
	g.store.mu.Lock()
	s := &g.store.state
	pr, p, ur, u, valid := g.payloads()
	allowed, kind, write := false, "api", ""
	if tr.media && (r.URL.Scheme == "http" || r.URL.Scheme == "https") {
		kind = "read"
		g.lastEndpoint = "challenge_read"
		allowed = valid && r.Method == http.MethodGet && len(body) == 0 && g.readURLs[r.URL.String()] && r.Header.Get("Authorization") == "" && r.Header.Get("X-Pan-Token") == "" && r.Header.Get("Range") == "bytes=0-31"
		if r.Method == http.MethodPut {
			kind, write, g.lastEndpoint = "put", "put", "challenge_put"
			for _, dest := range u.Destinations {
				if valid && ur.State == "accepted" && u.Phase == "put_inflight" && u.InFlightPart == 1 && len(body) == 32 && r.ContentLength == 32 && bytes.Equal(body, p.Bytes) && dest.URL == r.URL.String() && dest.Signature.Value != "" && r.Header.Get("Authorization") == dest.Signature.Value {
					allowed = true
				}
			}
		}
		for _, a := range []*quark.Authorization{s.Bundle.TV, s.Bundle.Agent} {
			for key, values := range r.Header {
				for _, value := range values {
					if strings.Contains(value, a.AccessToken) || strings.EqualFold(key, "X-Pan-Token") || strings.EqualFold(key, "X-Agent-ID") {
						allowed = false
					}
				}
			}
			if strings.Contains(r.URL.String(), a.AccessToken) {
				allowed = false
			}
		}
	} else if !tr.media && r.URL.Scheme == "https" && r.URL.Host == "open-api-drive.quark.cn" {
		q := r.URL.Query()
		switch r.URL.Path {
		case "/user":
			g.lastEndpoint = "tv_identity"
			allowed = r.Method == http.MethodGet && q.Get("method") == "user_info"
		case "/open/v1/user/info":
			g.lastEndpoint = "agent_identity"
			allowed = r.Method == http.MethodGet
		case "/open/v1/file/info":
			g.lastEndpoint = "agent_info"
			allowed = r.Method == http.MethodGet && g.knownAgent(q.Get("fid"))
		case "/open/v1/file/list":
			g.lastEndpoint = "agent_list"
			var size int
			allowed = s.RootVerified && r.Method == http.MethodPost && json.Unmarshal(in["size"], &size) == nil && size > 0 && size <= 100 && len(in["query_cursor"]) == 0 && (str("parent_fid") == "0" || quark.ObservationSameAgentHandle(str("parent_fid"), g.rootAgent))
		case "/file":
			switch q.Get("method") {
			case "list":
				g.lastEndpoint = "tv_list"
				size, err := strconv.Atoi(q.Get("page_size"))
				allowed = s.RootVerified && r.Method == http.MethodGet && q.Get("page_index") == "0" && err == nil && size > 0 && size <= 250 && (q.Get("parent_fid") == "0" || q.Get("parent_fid") == g.rootTV)
			case "download":
				g.lastEndpoint = "challenge_download"
				allowed = s.RootVerified && valid && r.Method == http.MethodGet && g.tvFiles[q.Get("fid")]
			}
		case "/open/v1/file/upload_pre":
			g.lastEndpoint, write = "upload_pre", "pre"
			var size int64
			allowed = valid && s.RootVerified && r.Method == http.MethodPost && ur.State == "submitting" && u.Phase == "pre_inflight" && json.Unmarshal(in["size"], &size) == nil && size == 32 && str("pdir_fid") == u.Parent && str("file_name") == p.Name && str("sha1") == u.SHA1 && str("md5") == u.MD5
		case "/open/v1/file/upload_finish":
			g.lastEndpoint, write = "upload_finish", "finish"
			var saved struct {
				Parts json.RawMessage `json:"parts"`
			}
			_ = json.Unmarshal(ur.Payload, &saved)
			var requested, parts any
			err1, err2 := json.Unmarshal(in["part_info_list"], &requested), json.Unmarshal(saved.Parts, &parts)
			requestedRaw, _ := json.Marshal(requested)
			partsRaw, _ := json.Marshal(parts)
			allowed = valid && r.Method == http.MethodPost && len(in) == 2 && u.Phase == "finish_inflight" && u.TaskID != "" && str("task_id") == u.TaskID && s.Writes["put"] && err1 == nil && err2 == nil && bytes.Equal(requestedRaw, partsRaw)
		case "/open/v1/file/list_uploaded_parts":
			g.lastEndpoint = "upload_task_query"
			var marker int
			markerOK := len(in["next_part_number_marker"]) == 0 || json.Unmarshal(in["next_part_number_marker"], &marker) == nil && marker >= 0 && marker <= 10000
			allowed = valid && r.Method == http.MethodPost && len(in) <= 2 && markerOK && u.TaskID != "" && str("task_id") == u.TaskID
		case "/open/v1/file/delete":
			g.lastEndpoint, write = "challenge_delete", "delete"
			var ids []string
			var action int
			if valid && pr.State == "confirmed" && ur.State == "confirmed" && p.Proof != nil && p.Cleanup != nil && p.Cleanup.State == "submitting" && len(s.Bundle.Proofs) == 1 && s.Bundle.Proofs[0] == *p.Proof && s.Bundle.HasProof(p.TVRoot, p.AgentRoot) && json.Unmarshal(in["fid_list"], &ids) == nil && json.Unmarshal(in["action_type"], &action) == nil && len(ids) == 1 && action == 1 && len(in) == 2 && r.Method == http.MethodPost {
				id, ok := followRawID(p.Cleanup.Agent.ID, quark.RouteAgent, s.Bundle.Agent.AccountID)
				parent, parentOK := followRawID(p.Cleanup.Agent.ParentID, quark.RouteAgent, s.Bundle.Agent.AccountID)
				expected := u.Handle
				var upload struct{ Mapping *followMapping }
				mappingOK := json.Unmarshal(ur.Payload, &upload) == nil
				if mappingOK && upload.Mapping != nil {
					m := upload.Mapping
					mappingOK = m.Version == 1 && m.Operation == ur.ID && m.ReceiptRevision == ur.Revision && m.ACK == u.Handle && m.SHA256 == u.SHA256 && m.TV.ID == p.Cleanup.TV.ID
					expected = m.Listed
				}
				allowed = ok && parentOK && mappingOK && ids[0] == id && quark.ObservationSameAgentHandle(expected, id) && p.Proof.ReceiptID == s.ProofID && p.Proof.ContentSHA256 == u.SHA256 && quark.ObservationSameAgentHandle(parent, g.rootAgent) && p.Cleanup.Agent.Name == p.Name && p.Cleanup.Agent.Size == 32 && !p.Cleanup.Agent.IsDir && p.Cleanup.TV.ParentID == p.TVRoot && p.Cleanup.TV.Name == p.Name && p.Cleanup.TV.Size == 32 && !p.Cleanup.TV.IsDir
			}
		}
	}
	if !allowed || g.store.poisoned || !proofCleanupManifestValid(*s) || s.Completed || !time.Now().Before(s.Deadline) || s.Requests >= 128 || !tr.media && s.API >= 96 || write != "" && s.Writes[write] || kind == "read" && s.ReadBytes+33 > 128 || kind != "read" && s.MetadataBytes+proofCleanupMetadataLimit+1 > proofCleanupMetadataBudget {
		g.store.mu.Unlock()
		return nil, observationError()
	}
	wait := time.Until(s.LastDispatch.Add(g.delay))
	deadline := s.Deadline
	g.store.mu.Unlock()
	ctx, cancel := context.WithDeadline(r.Context(), deadline)
	defer cancel()
	if wait > 0 {
		timer := time.NewTimer(wait)
		defer timer.Stop()
		select {
		case <-ctx.Done():
			return nil, observationError()
		case <-timer.C:
		}
	}
	g.store.mu.Lock()
	s = &g.store.state
	if ctx.Err() != nil {
		g.store.mu.Unlock()
		return nil, observationError()
	}
	s.Requests++
	if !tr.media {
		s.API++
	}
	s.LastDispatch = time.Now().UTC()
	if write != "" {
		s.Writes[write] = true
	}
	limit := proofCleanupMetadataLimit
	if kind == "read" {
		limit = 32
		s.ReadBytes += 33
	} else {
		s.MetadataBytes += limit + 1
	}
	if g.store.save() != nil {
		g.store.mu.Unlock()
		return nil, observationError()
	}
	g.store.mu.Unlock()
	response, err := tr.base.RoundTrip(r.Clone(ctx))
	if err != nil {
		g.lastOutcome = "transport_failed"
		return nil, observationError()
	}
	if response == nil || response.Body == nil {
		return nil, observationError()
	}
	defer response.Body.Close()
	g.lastStatus = response.StatusCode
	if response.Uncompressed || response.ContentLength > int64(limit) || response.Header.Get("Content-Encoding") != "" && response.Header.Get("Content-Encoding") != "identity" {
		g.lastOutcome = "response_body_rejected"
		return nil, observationError()
	}
	raw, err := io.ReadAll(io.LimitReader(response.Body, int64(limit)+1))
	if err != nil || len(raw) > limit {
		g.lastOutcome = "response_body_rejected"
		return nil, observationError()
	}
	g.lastOutcome = "response_received"
	var envelope struct {
		Status *int
		Errno  int
		Data   json.RawMessage
	}
	_ = json.Unmarshal(raw, &envelope)
	if envelope.Errno >= -999999999 && envelope.Errno <= 999999999 {
		g.lastErrno = envelope.Errno
	}
	g.store.mu.Lock()
	if kind != "read" {
		g.store.state.MetadataBytes -= limit + 1 - len(raw)
	}
	if g.store.save() != nil {
		g.store.mu.Unlock()
		return nil, observationError()
	}
	if !tr.media && response.StatusCode == http.StatusOK && envelope.Status != nil && *envelope.Status == 0 {
		g.observe(r, str("parent_fid"), envelope.Data)
	}
	g.store.mu.Unlock()
	copyResponse := new(http.Response)
	*copyResponse = *response
	copyResponse.Body, copyResponse.ContentLength = io.NopCloser(bytes.NewReader(raw)), int64(len(raw))
	return copyResponse, nil
}

// Candidate IDs only permit bounded metadata/read requests. They never grant
// deletion: delete admission independently requires the saved exact proof.
func (g *proofCleanupGuard) observe(r *http.Request, parent string, data json.RawMessage) {
	_, p, _, u, valid := g.payloads()
	if !valid {
		return
	}
	switch r.URL.Path {
	case "/open/v1/file/info":
		var f struct {
			ID, Parent, Name, Type string
			Size                   int64
		}
		var v struct {
			ID     string `json:"fid"`
			Parent string `json:"parent_fid"`
			Name   string `json:"filename"`
			Type   string `json:"file_type"`
			Size   int64  `json:"size"`
		}
		if json.Unmarshal(data, &v) == nil {
			f.ID, f.Parent, f.Name, f.Type, f.Size = v.ID, v.Parent, v.Name, v.Type, v.Size
		}
		if u.Handle != "" && quark.ObservationSameAgentHandle(r.URL.Query().Get("fid"), u.Handle) && quark.ObservationSameAgentHandle(f.ID, u.Handle) && f.Name == p.Name && f.Size == 32 && f.Type == "1" && f.Parent != "0" && liveTVScalar(f.Parent, 4096) {
			g.agentIDs[f.Parent] = true
		}
	case "/open/v1/file/list":
		var list struct {
			Files []struct {
				ID   string `json:"fid"`
				Name string `json:"filename"`
				Type string `json:"file_type"`
				Size int64  `json:"size"`
			} `json:"file_list"`
		}
		_ = json.Unmarshal(data, &list)
		for _, f := range list.Files {
			if quark.ObservationSameAgentHandle(parent, g.rootAgent) && f.Name == p.Name && f.Type == "1" && f.Size == 32 && liveTVScalar(f.ID, 4096) {
				g.agentIDs[f.ID] = true
			}
		}
	case "/file":
		q := r.URL.Query()
		if q.Get("method") == "list" && q.Get("parent_fid") == g.rootTV {
			var list struct {
				Files []struct {
					ID   string `json:"fid"`
					Name string `json:"filename"`
					Dir  int    `json:"isdir"`
					Size int64  `json:"size"`
				} `json:"files"`
			}
			_ = json.Unmarshal(data, &list)
			for _, f := range list.Files {
				if f.Name == p.Name && f.Size == 32 && f.Dir == 0 && liveTVScalar(f.ID, 4096) {
					g.tvFiles[f.ID] = true
				}
			}
		}
		if q.Get("method") == "download" && g.tvFiles[q.Get("fid")] {
			var out struct {
				URL  string `json:"download_url"`
				Name string `json:"file_name"`
				ID   string `json:"fid"`
				Size int64  `json:"size"`
			}
			if json.Unmarshal(data, &out) == nil && out.Size == 32 && out.Name == p.Name && out.ID == q.Get("fid") && liveTVScalar(out.URL, 32768) {
				g.readURLs[out.URL] = true
			}
		}
	}
}
