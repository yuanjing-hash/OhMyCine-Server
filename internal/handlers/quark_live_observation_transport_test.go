//go:build quark_live

package handlers_test

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
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

// These views inspect the real driver's encrypted private receipt payloads.
// They neither generate receipts nor infer success from an upstream ACK.
type observationUpload struct {
	Parent, Name, SHA1, MD5, SHA256, TaskID, Handle, Phase string
	Size                                                   int64
	InFlightPart                                           int `json:"inflight_part"`
	Destinations                                           []struct {
		URL       string `json:"upload_url"`
		Signature struct {
			Value string `json:"signature"`
		} `json:"signature_info"`
	} `json:"upload_urls"`
}

// Fields with snake-case names need explicit bindings (case folding does not
// remove underscores).
func observationDecodeUpload(raw []byte) (observationUpload, bool) {
	var p struct {
		Parent       string `json:"parent"`
		Name         string `json:"name"`
		SHA1         string `json:"sha1"`
		MD5          string `json:"md5"`
		SHA256       string `json:"sha256"`
		TaskID       string `json:"task_id"`
		Handle       string `json:"handle"`
		Phase        string `json:"phase"`
		Size         int64  `json:"size"`
		InFlightPart int    `json:"inflight_part"`
		Destinations []struct {
			URL       string `json:"upload_url"`
			Signature struct {
				Value string `json:"signature"`
			} `json:"signature_info"`
		} `json:"upload_urls"`
	}
	if json.Unmarshal(raw, &p) != nil {
		return observationUpload{}, false
	}
	return observationUpload{p.Parent, p.Name, p.SHA1, p.MD5, p.SHA256, p.TaskID, p.Handle, p.Phase, p.Size, p.InFlightPart, p.Destinations}, true
}

type observationProof struct {
	Path      string `json:"path"`
	TVRoot    string `json:"tv_root"`
	AgentRoot string `json:"agent_root"`
	Name      string `json:"name"`
	Bytes     []byte `json:"bytes"`
}
type observationMutation struct {
	Request cloud.OperationRequest `json:"request"`
	Handle  string                 `json:"handle"`
}

type observationGuard struct {
	mu                                         sync.Mutex // Covers dispatch, bounded response read and delay: concurrency one.
	store                                      *observationStore
	agentIDs, agentParents, tvParents, tvFiles map[string]bool
	readURLs                                   map[string]int64
	rootAgent, rootTV, childAgent, childTV     string
	startup                                    bool
	delay                                      time.Duration
	lastEndpoint, lastOutcome                  string
	lastStatus                                 int
}

func newObservationGuard(s *observationStore) *observationGuard {
	g := &observationGuard{store: s, agentIDs: map[string]bool{s.state.RootACK: true}, agentParents: map[string]bool{}, tvParents: map[string]bool{}, tvFiles: map[string]bool{}, readURLs: map[string]int64{}, startup: true, delay: 2 * time.Second}
	for _, r := range s.state.Operations {
		if p, ok := observationDecodeUpload(r.Payload); r.Kind == "upload" && ok && p.Handle != "" {
			g.agentIDs[p.Handle] = true
		}
		var m observationMutation
		if r.Kind == "mkdir" && json.Unmarshal(r.Payload, &m) == nil && m.Handle != "" {
			g.agentIDs[m.Handle] = true
			g.agentParents[m.Handle] = true
		}
	}
	return g
}
func (g *observationGuard) wrap(base http.RoundTripper, media bool) http.RoundTripper {
	return observationTransport{base, g, media}
}

type observationTransport struct {
	base  http.RoundTripper
	g     *observationGuard
	media bool
}

func (g *observationGuard) knownAgent(ids map[string]bool, id string) bool {
	for existing := range ids {
		if quark.ObservationSameAgentHandle(existing, id) {
			return true
		}
	}
	return false
}
func (g *observationGuard) allowedName(name string) bool {
	s := g.store.state
	if name == s.UploadName || name == s.DirectoryName {
		return true
	}
	r, ok := s.Operations[s.ProofID]
	var p observationProof
	return ok && r.Kind == "space_proof" && json.Unmarshal(r.Payload, &p) == nil && len(p.Bytes) == 32 && p.Name == name
}

func (g *observationGuard) proofName(name string) bool {
	r, ok := g.store.state.Operations[g.store.state.ProofID]
	var p observationProof
	return ok && r.Kind == "space_proof" && json.Unmarshal(r.Payload, &p) == nil && len(p.Bytes) == 32 && p.Name == name
}
func (g *observationGuard) uploads() map[string]observationUpload {
	out := map[string]observationUpload{}
	for _, id := range []string{g.store.state.ProofID + "/upload", g.store.state.UploadID} {
		r, ok := g.store.state.Operations[id]
		p, valid := observationDecodeUpload(r.Payload)
		if ok && r.Kind == "upload" && valid {
			out[id] = p
		}
	}
	return out
}
func (g *observationGuard) validUpload(id string, p observationUpload) bool {
	s := g.store.state
	if id == s.UploadID {
		return p.Name == s.UploadName && p.Size == observationFixtureSize && p.SHA256 == observationFixtureSHA && g.childAgent != "" && quark.ObservationSameAgentHandle(p.Parent, g.childAgent)
	}
	var proof observationProof
	r, ok := s.Operations[s.ProofID]
	if !ok || json.Unmarshal(r.Payload, &proof) != nil || len(proof.Bytes) != 32 || p.Name != proof.Name || p.Size != 32 || g.rootAgent == "" || !quark.ObservationSameAgentHandle(p.Parent, g.rootAgent) {
		return false
	}
	h := sha256.Sum256(proof.Bytes)
	return p.SHA256 == hex.EncodeToString(h[:])
}
func (w observationTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	g := w.g
	g.mu.Lock()
	defer g.mu.Unlock()
	g.lastEndpoint, g.lastOutcome, g.lastStatus = "other", "guard_rejected", 0
	switch r.URL.Path {
	case "/user", "/open/v1/user/info", "/open/v1/file/info", "/open/v1/file/list", "/open/v1/file/upload_pre", "/open/v1/file/upload_finish", "/open/v1/dir", "/quarkdrive/token":
		g.lastEndpoint = r.URL.Path
	case "/file":
		switch r.URL.Query().Get("method") {
		case "list":
			g.lastEndpoint = "tv_list"
		case "download":
			g.lastEndpoint = "tv_download"
		}
	}
	if w.media {
		g.lastEndpoint = "anonymous_media"
	}
	if r.Context().Err() != nil {
		return nil, observationError()
	}
	var body []byte
	if r.Body != nil {
		var e error
		body, e = io.ReadAll(io.LimitReader(r.Body, 32769))
		_ = r.Body.Close()
		r.Body = io.NopCloser(bytes.NewReader(body))
		if e != nil || len(body) > 32768 {
			return nil, observationError()
		}
	}
	var input map[string]json.RawMessage
	_ = json.Unmarshal(body, &input)
	str := func(k string) string { var v string; _ = json.Unmarshal(input[k], &v); return v }
	g.store.mu.Lock()
	s := &g.store.state
	allowed, kind, key := false, "api", ""
	readSize := int64(0)
	secure := r.URL.Scheme == "https" && r.URL.User == nil && r.URL.Fragment == ""
	if w.media && secure {
		if r.Method == "GET" {
			readSize = g.readURLs[r.URL.String()]
			allowed = readSize > 0 && r.Header.Get("Authorization") == "" && r.Header.Get("Cookie") == "" && r.Header.Get("x-pan-token") == "" && r.Header.Get("Range") == ""
			kind = "read"
		}
		if r.Method == "PUT" {
			for id, p := range g.uploads() {
				if !g.validUpload(id, p) {
					continue
				}
				for _, dest := range p.Destinations {
					h := sha256.Sum256(body)
					if p.Phase == "put_inflight" && p.InFlightPart == 1 && dest.URL == r.URL.String() && dest.Signature.Value != "" && r.Header.Get("Authorization") == dest.Signature.Value && int64(len(body)) == p.Size && hex.EncodeToString(h[:]) == p.SHA256 {
						allowed = true
						kind = "put"
						key = id + ":put"
					}
				}
			}
		}
		for _, a := range []*quark.Authorization{s.Bundle.TV, s.Bundle.Agent} {
			if a != nil && a.AccessToken != "" && (strings.Contains(r.URL.String(), a.AccessToken) || strings.Contains(r.Header.Get("Authorization"), a.AccessToken)) {
				allowed = false
			}
		}
	} else if !w.media && secure && r.URL.Host == "api.extscreen.com" && r.URL.Path == "/quarkdrive/token" {
		allowed = g.startup && r.Method == "POST" && str("refresh_token") != "" && str("code") == "" && str("device_id") == s.TVDevice
		kind = "broker"
		key = "tv_startup:broker"
	} else if !w.media && secure && r.URL.Host == "open-api-drive.quark.cn" {
		switch r.URL.Path {
		case "/user":
			allowed = r.Method == "GET" && r.URL.Query().Get("method") == "user_info"
		case "/open/v1/user/info":
			allowed = r.Method == "GET"
		case "/open/v1/file/info":
			allowed = r.Method == "GET" && g.knownAgent(g.agentIDs, r.URL.Query().Get("fid"))
		case "/open/v1/file/list":
			var size int
			validSize := json.Unmarshal(input["size"], &size) == nil && size > 0 && size <= 100
			allowed = s.RootVerified && r.Method == "POST" && validSize && (str("parent_fid") == "0" || g.knownAgent(g.agentParents, str("parent_fid"))) && len(input["query_cursor"]) == 0
		case "/file":
			if r.Method == "GET" && s.RootVerified {
				switch r.URL.Query().Get("method") {
				case "list":
					p := r.URL.Query().Get("parent_fid")
					size, e := strconv.Atoi(r.URL.Query().Get("page_size"))
					allowed = (p == "0" || g.tvParents[p]) && r.URL.Query().Get("page_index") == "0" && e == nil && size > 0 && size <= 250
				case "download":
					allowed = g.tvFiles[r.URL.Query().Get("fid")]
				}
			}
		case "/open/v1/file/upload_pre":
			for id, p := range g.uploads() {
				var size int64
				_ = json.Unmarshal(input["size"], &size)
				if s.RootVerified && r.Method == "POST" && g.validUpload(id, p) && p.Phase == "pre_inflight" && str("pdir_fid") == p.Parent && str("file_name") == p.Name && size == p.Size && str("sha1") == p.SHA1 && str("md5") == p.MD5 {
					allowed = true
					kind = "pre"
					key = id + ":pre"
				}
			}
		case "/open/v1/file/upload_finish":
			for id, p := range g.uploads() {
				if s.RootVerified && r.Method == "POST" && g.validUpload(id, p) && p.Phase == "finish_inflight" && p.TaskID != "" && str("task_id") == p.TaskID {
					allowed = true
					kind = "finish"
					key = id + ":finish"
				}
			}
		case "/open/v1/dir":
			var p observationMutation
			receipt, ok := s.Operations[s.DirectoryID]
			allowed = s.RootVerified && r.Method == "POST" && ok && receipt.Kind == "mkdir" && receipt.State == "submitting" && json.Unmarshal(receipt.Payload, &p) == nil && p.Request.Name == s.DirectoryName && str("dir_path") == s.DirectoryName && g.rootAgent != "" && quark.ObservationSameAgentHandle(str("pdir_fid"), g.rootAgent)
			kind = "mkdir"
			key = s.DirectoryID + ":mkdir"
		}
	}
	if !allowed || g.store.poisoned || s.Completed || time.Since(s.Started) >= 12*time.Minute || s.Requests >= 180 || !w.media && s.API >= 160 || key != "" && s.Writes[key] || kind == "read" && (s.Reads >= 8 || s.ReadBytes+int(readSize) > 64<<10) || kind == "pre" && s.PreWrites >= 2 || kind == "put" && s.PutWrites >= 2 || kind == "finish" && s.FinishWrites >= 2 || kind == "mkdir" && s.DirectoryWrites >= 1 || kind == "broker" && s.BrokerWrites >= 1 {
		g.store.mu.Unlock()
		return nil, observationError()
	}
	wait := time.Until(s.LastDispatch.Add(g.delay))
	g.store.mu.Unlock()
	if wait > 0 {
		timer := time.NewTimer(wait)
		defer timer.Stop()
		select {
		case <-r.Context().Done():
			return nil, observationError()
		case <-timer.C:
		}
	}
	g.store.mu.Lock()
	s = &g.store.state
	if r.Context().Err() != nil || time.Since(s.Started) >= 12*time.Minute {
		g.store.mu.Unlock()
		return nil, observationError()
	}
	s.Requests++
	if !w.media {
		s.API++
	}
	s.LastDispatch = time.Now().UTC()
	if key != "" {
		s.Writes[key] = true
	}
	switch kind {
	case "pre":
		s.PreWrites++
	case "put":
		s.PutWrites++
	case "finish":
		s.FinishWrites++
	case "mkdir":
		s.DirectoryWrites++
	case "broker":
		s.BrokerWrites++
	case "read":
		s.Reads++
		s.ReadBytes += int(readSize)
	} // reserve before I/O, including lost responses
	if g.store.save() != nil {
		g.store.mu.Unlock()
		return nil, observationError()
	}
	g.store.mu.Unlock()
	resp, e := w.base.RoundTrip(r)
	if e != nil {
		g.lastOutcome = "transport_failed"
		return nil, observationError()
	}
	g.lastStatus = resp.StatusCode
	max := int64(4 << 20)
	if kind == "read" {
		max = readSize
	}
	b, e := io.ReadAll(io.LimitReader(resp.Body, max+1))
	_ = resp.Body.Close()
	if e != nil || int64(len(b)) > max {
		g.lastOutcome = "response_body_rejected"
		return nil, observationError()
	}
	g.lastOutcome = "response_received"
	resp.Body = io.NopCloser(bytes.NewReader(b))
	if w.media {
		return resp, nil
	}
	g.store.mu.Lock()
	defer g.store.mu.Unlock()
	var env struct{ Data json.RawMessage }
	_ = json.Unmarshal(b, &env)
	if r.URL.Path == "/open/v1/file/info" && resp.StatusCode == http.StatusOK {
		// Creation ACKs and list handles have separate namespaces. The native
		// driver independently checks the parent named by our exact upload ACK.
		// Admit only that single metadata read, never a list/write or an ancestor
		// chain, and only after this receipt's file name/type/size match.
		var file struct {
			ID     string `json:"fid"`
			Name   string `json:"filename"`
			Parent string `json:"parent_fid"`
			Type   string `json:"file_type"`
			Size   *int64 `json:"size"`
		}
		if json.Unmarshal(env.Data, &file) == nil {
			for id, p := range g.uploads() {
				if g.validUpload(id, p) && p.Handle != "" && r.URL.Query().Get("fid") == p.Handle && quark.ObservationSameAgentHandle(file.ID, p.Handle) && file.Type == "1" && file.Name == p.Name && file.Size != nil && *file.Size == p.Size && liveTVScalar(file.Parent, 4096) && file.Parent != "0" {
					g.agentIDs[file.Parent] = true
				}
			}
		}
	}
	if r.URL.Path == "/open/v1/file/list" {
		var data struct {
			Files []struct {
				ID   string `json:"fid"`
				Name string `json:"filename"`
				Type string `json:"file_type"`
			} `json:"file_list"`
		}
		_ = json.Unmarshal(env.Data, &data)
		for _, f := range data.Files {
			if str("parent_fid") == "0" && f.Name == s.RootName && f.Type == "0" {
				g.rootAgent = f.ID
				g.agentIDs[f.ID] = true
				g.agentParents[f.ID] = true
			}
			if g.rootAgent != "" && quark.ObservationSameAgentHandle(str("parent_fid"), g.rootAgent) && f.Name == s.DirectoryName && f.Type == "0" {
				g.childAgent = f.ID
				g.agentIDs[f.ID] = true
				g.agentParents[f.ID] = true
			}
			proofFile := g.rootAgent != "" && quark.ObservationSameAgentHandle(str("parent_fid"), g.rootAgent) && g.proofName(f.Name)
			sampleFile := g.childAgent != "" && quark.ObservationSameAgentHandle(str("parent_fid"), g.childAgent) && f.Name == s.UploadName
			if (proofFile || sampleFile) && f.Type == "1" {
				g.agentIDs[f.ID] = true
			}
		}
	}
	if r.URL.Path == "/file" && r.URL.Query().Get("method") == "list" {
		var data struct {
			Files []struct {
				ID   string `json:"fid"`
				Name string `json:"filename"`
				Dir  int    `json:"isdir"`
				Size int64  `json:"size"`
			} `json:"files"`
		}
		_ = json.Unmarshal(env.Data, &data)
		parent := r.URL.Query().Get("parent_fid")
		for _, f := range data.Files {
			if parent == "0" && f.Name == s.RootName && f.Dir == 1 {
				g.rootTV = f.ID
				g.tvParents[f.ID] = true
			}
			if parent == g.rootTV && f.Name == s.DirectoryName && f.Dir == 1 {
				g.childTV = f.ID
				g.tvParents[f.ID] = true
			}
			proofFile := parent == g.rootTV && g.rootTV != "" && g.proofName(f.Name) && f.Size == 32
			sampleFile := parent == g.childTV && g.childTV != "" && f.Name == s.UploadName && f.Size == observationFixtureSize
			if (proofFile || sampleFile) && f.Dir == 0 {
				g.tvFiles[f.ID] = true
			}
		}
	}
	if r.URL.Path == "/file" && r.URL.Query().Get("method") == "download" {
		var data struct {
			URL  string `json:"download_url"`
			Size int64  `json:"size"`
		}
		_ = json.Unmarshal(env.Data, &data)
		if g.tvFiles[r.URL.Query().Get("fid")] && (data.Size == 32 || data.Size == observationFixtureSize) {
			g.readURLs[data.URL] = data.Size
		}
	}
	if kind == "pre" || kind == "finish" || kind == "mkdir" {
		var data struct {
			ID string `json:"fid"`
		}
		_ = json.Unmarshal(env.Data, &data)
		if data.ID != "" {
			g.agentIDs[data.ID] = true
			if kind == "mkdir" {
				g.agentParents[data.ID] = true
			}
		}
	}
	return resp, nil
}
