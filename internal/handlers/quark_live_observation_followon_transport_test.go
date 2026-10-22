//go:build quark_live

package handlers_test

import (
	"bytes"
	"encoding/base64"
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

type followOperation struct {
	Request       cloud.OperationRequest `json:"request"`
	BatchID       string                 `json:"batch_id,omitempty"`
	SubmitFailure *struct {
		HTTPStatus int  `json:"http_status"`
		Errno      *int `json:"errno"`
	} `json:"submit_failure,omitempty"`
	TaskID string `json:"task_id"`
	Handle string `json:"handle"`
	Before struct {
		ID, Parent, Name string
		Dir              bool
		Size             int64
	} `json:"before"`
	Mapping *followMapping `json:"mapping"`
	Target  *followTarget  `json:"target"`
	Outputs []cloud.Item   `json:"outputs"`
}

// Same-namespace mkdir has no alias mapping in the native receipt. Its single
// acknowledged output must itself prove the frozen input parent and identity.
func followSameDirectoryOutput(r cloud.OperationReceipt, p followOperation, account, parent, name string) bool {
	if r.Provider != cloud.ProviderQuark || r.Route != string(quark.RouteAgent) || r.AccountID != account || r.Kind != "mkdir" || r.State != "confirmed" || p.Request.ID != r.ID || p.Request.Kind != "mkdir" || p.Request.Name != name || len(p.Outputs) != 1 || !liveTVScalar(p.Handle, 4096) {
		return false
	}
	raw, valid := followRawID(p.Request.ParentID, quark.RouteAgent, account)
	out := p.Outputs[0]
	id, idValid := followRawID(out.ID, quark.RouteAgent, account)
	outParent, parentValid := followRawID(out.ParentID, quark.RouteAgent, account)
	return valid && idValid && parentValid && quark.ObservationSameAgentHandle(raw, parent) && quark.ObservationSameAgentHandle(outParent, parent) && quark.ObservationSameAgentHandle(id, p.Handle) && out.IsDir && out.Name == name && out.Size >= 0
}

func followRawID(id string, route quark.Route, account string) (string, bool) {
	p := strings.Split(id, ":")
	if len(p) != 4 {
		return "", false
	}
	b, e := base64.RawURLEncoding.DecodeString(p[3])
	raw := string(b)
	return raw, e == nil && liveTVScalar(raw, 4096) && quark.QualifyID(route, account, raw) == id
}

type followGuard struct {
	mu                        sync.Mutex
	store                     *followStore
	delay                     time.Duration
	startup                   bool
	renameQueryOnly           bool
	agentIDs, agentParents    map[string]bool
	tvParents                 map[string]bool // qualified, account-bound TV IDs
	readURLs                  map[string]bool
	lastEndpoint, lastOutcome string
	lastStatus                int
}

func newFollowGuard(s *followStore) *followGuard {
	i := s.state.Identity
	g := &followGuard{store: s, delay: 2 * time.Second, startup: true, agentIDs: map[string]bool{}, agentParents: map[string]bool{}, tvParents: map[string]bool{i.Proof.TVRoot: true, i.Root.ID: true}, readURLs: map[string]bool{}}
	for _, id := range []string{i.RootAgent, i.ProofAgent, i.SampleAgent, i.RootACK, i.SampleACK, i.ProofRootACK} {
		g.agentIDs[id] = true
	}
	for _, id := range []string{i.RootAgent, i.ProofAgent} {
		g.agentParents[id] = true
	}
	if s.state.NewDirectory.ID != "" {
		g.tvParents[s.state.NewDirectory.ID] = true
	}
	if s.state.NewDirectoryAgent != "" {
		g.agentIDs[s.state.NewDirectoryAgent] = true
		g.agentParents[s.state.NewDirectoryAgent] = true
	}
	for _, r := range s.state.Operations {
		var p followOperation
		if json.Unmarshal(r.Payload, &p) == nil && r.Kind == "mkdir" && p.Handle != "" {
			g.agentIDs[p.Handle] = true
			if p.Mapping != nil {
				g.agentIDs[p.Mapping.Listed] = true
				g.agentParents[p.Mapping.Listed] = true
				g.tvParents[p.Mapping.TV.ID] = true
			}
		}
	}
	return g
}
func (g *followGuard) known(ids map[string]bool, raw string) bool {
	for id := range ids {
		if quark.ObservationSameAgentHandle(id, raw) {
			return true
		}
	}
	return false
}
func (g *followGuard) wrap(base http.RoundTripper, media bool) http.RoundTripper {
	return followTransport{base, g, media}
}

type followTransport struct {
	base  http.RoundTripper
	g     *followGuard
	media bool
}

func (g *followGuard) operation(id, kind string) (cloud.OperationReceipt, followOperation, bool) {
	r, ok := g.store.state.Operations[id]
	var p followOperation
	valid := ok && r.Kind == kind && g.store.allowed(r) && json.Unmarshal(r.Payload, &p) == nil && p.Request.ID == id && p.Request.Kind == kind
	return r, p, valid
}
func (g *followGuard) newDirectoryConfirmed() bool {
	s := g.store.state
	i := s.Identity
	r, p, ok := g.operation(s.DirectoryID, "mkdir")
	if !ok || r.State != "confirmed" {
		return false
	}
	if p.Mapping == nil {
		_, tvValid := followRawID(s.NewDirectory.ID, quark.RouteTV, i.TVAccount)
		return followSameDirectoryOutput(r, p, i.AgentAccount, i.RootAgent, s.DirectoryName) && p.Target != nil && p.Target.Proof == i.Proof && p.Target.TVParent == i.Root.ID && p.Target.Path == i.RootPath && quark.ObservationSameAgentHandle(p.Target.AgentParent, i.RootAgent) && tvValid && s.NewDirectory.IsDir && s.NewDirectory.ParentID == i.Root.ID && s.NewDirectory.Name == s.DirectoryName && quark.ObservationSameAgentHandle(p.Handle, s.NewDirectoryAgent)
	}
	m := p.Mapping
	raw, valid := followRawID(p.Request.ParentID, quark.RouteAgent, i.AgentAccount)
	_, tvValid := followRawID(m.TV.ID, quark.RouteTV, i.TVAccount)
	return valid && tvValid && quark.ObservationSameAgentHandle(raw, i.RootAgent) && p.Request.Name == s.DirectoryName && m.Version == 1 && m.Operation == r.ID && m.ReceiptRevision == r.Revision && m.ACK != "" && m.ACK == p.Handle && m.TV.IsDir && m.TV.ParentID == i.Root.ID && m.TV.Name == s.DirectoryName && m.TV.ID == s.NewDirectory.ID && s.NewDirectory.IsDir && s.NewDirectory.ParentID == i.Root.ID && s.NewDirectory.Name == s.DirectoryName && liveTVScalar(m.Listed, 4096) && quark.ObservationSameAgentHandle(m.Listed, s.NewDirectoryAgent)
}

func (w followTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	g := w.g
	g.mu.Lock()
	defer g.mu.Unlock()
	g.lastEndpoint, g.lastOutcome, g.lastStatus = "other", "guard_rejected", 0
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
	var in map[string]json.RawMessage
	_ = json.Unmarshal(body, &in)
	str := func(k string) string { var x string; _ = json.Unmarshal(in[k], &x); return x }
	g.store.mu.Lock()
	s := &g.store.state
	i := s.Identity
	allowed, key, kind := false, "", "api"
	secure := r.URL.Scheme == "https" && r.URL.User == nil && r.URL.Fragment == ""
	if w.media && secure {
		g.lastEndpoint = "fixture_read"
		allowed = r.Method == "GET" && g.readURLs[r.URL.String()] && r.Header.Get("Authorization") == "" && r.Header.Get("Cookie") == "" && r.Header.Get("x-pan-token") == "" && r.Header.Get("Range") == ""
		kind = "read"
		for _, a := range []*quark.Authorization{s.Bundle.TV, s.Bundle.Agent} {
			if a != nil && a.AccessToken != "" && strings.Contains(r.URL.String(), a.AccessToken) {
				allowed = false
			}
		}
	} else if !w.media && secure && r.URL.Host == "api.extscreen.com" && r.URL.Path == "/quarkdrive/token" {
		g.lastEndpoint = "startup_broker"
		allowed = g.startup && r.Method == "POST" && str("refresh_token") != "" && str("code") == "" && str("device_id") == i.TVDevice
		kind, key = "broker", "tv_startup:broker"
	} else if !w.media && secure && r.URL.Host == "open-api-drive.quark.cn" {
		switch r.URL.Path {
		case "/user":
			g.lastEndpoint = "tv_identity"
			allowed = r.Method == "GET" && r.URL.Query().Get("method") == "user_info"
		case "/open/v1/user/info":
			g.lastEndpoint = "agent_identity"
			allowed = r.Method == "GET"
		case "/open/v1/file/info":
			g.lastEndpoint = "agent_info"
			allowed = r.Method == "GET" && g.known(g.agentIDs, r.URL.Query().Get("fid"))
		case "/open/v1/file/list":
			g.lastEndpoint = "agent_list"
			var size int
			valid := json.Unmarshal(in["size"], &size) == nil && size > 0 && size <= 100
			allowed = r.Method == "POST" && valid && len(in["query_cursor"]) == 0 && (str("parent_fid") == "0" || g.known(g.agentParents, str("parent_fid")))
		case "/file":
			q := r.URL.Query()
			switch q.Get("method") {
			case "list":
				g.lastEndpoint = "tv_list"
				size, e := strconv.Atoi(q.Get("page_size"))
				parent := q.Get("parent_fid")
				allowed = r.Method == "GET" && q.Get("page_index") == "0" && e == nil && size > 0 && size <= 100 && (parent == "0" || g.tvParents[quark.QualifyID(quark.RouteTV, i.TVAccount, parent)])
			case "download":
				g.lastEndpoint = "fixture_download"
				allowed = r.Method == "GET" && quark.QualifyID(quark.RouteTV, i.TVAccount, q.Get("fid")) == i.Sample.ID
			}
		case "/open/v1/dir":
			g.lastEndpoint = "mkdir"
			receipt, p, ok := g.operation(s.DirectoryID, "mkdir")
			raw, valid := followRawID(p.Request.ParentID, quark.RouteAgent, i.AgentAccount)
			allowed = r.Method == "POST" && len(in) == 2 && ok && receipt.State == "submitting" && valid && quark.ObservationSameAgentHandle(raw, i.RootAgent) && p.Request.Name == s.DirectoryName && str("dir_path") == s.DirectoryName && quark.ObservationSameAgentHandle(str("pdir_fid"), i.RootAgent)
			key = s.DirectoryID + ":mkdir"
		case "/open/v1/file/rename/batch":
			g.lastEndpoint = "rename"
			receipt, p, ok := g.operation(s.RenameID, "rename")
			var list []struct {
				ID   string `json:"fid"`
				Name string `json:"file_name"`
				Old  string `json:"origin_file_name"`
			}
			e := json.Unmarshal(in["rename_list"], &list)
			raw, valid := followRawID(p.Request.ItemID, quark.RouteAgent, i.AgentAccount)
			batch, batchErr := hex.DecodeString(p.BatchID)
			allowed = r.Method == "POST" && len(in) == 3 && ok && receipt.State == "submitting" && p.Request.Name == s.RenameName && valid && quark.ObservationSameAgentHandle(raw, i.SampleAgent) && quark.ObservationSameAgentHandle(p.Before.ID, i.SampleAgent) && p.Before.Size == observationFixtureSize && !p.Before.Dir && quark.ObservationSameAgentHandle(p.Before.Parent, i.RootAgent) && p.Before.Name == i.Sample.Name && batchErr == nil && len(batch) == 8 && len(p.BatchID) == 16 && str("batch_id") == p.BatchID && str("operation") == "RENAME" && e == nil && len(list) == 1 && quark.ObservationSameAgentHandle(list[0].ID, i.SampleAgent) && list[0].Name == s.RenameName && list[0].Old == i.Sample.Name
			key = s.RenameID + ":rename"
		case "/open/v1/file/move":
			g.lastEndpoint = "move"
			receipt, p, ok := g.operation(s.MoveID, "move")
			var ids []string
			var action int
			ei := json.Unmarshal(in["fid_list"], &ids)
			ea := json.Unmarshal(in["action_type"], &action)
			raw, valid := followRawID(p.Request.ParentID, quark.RouteAgent, i.AgentAccount)
			item, validItem := followRawID(p.Request.ItemID, quark.RouteAgent, i.AgentAccount)
			allowed = r.Method == "POST" && len(in) == 3 && ok && receipt.State == "submitting" && g.newDirectoryConfirmed() && valid && validItem && quark.ObservationSameAgentHandle(item, i.SampleAgent) && quark.ObservationSameAgentHandle(p.Before.ID, i.SampleAgent) && quark.ObservationSameAgentHandle(raw, s.NewDirectoryAgent) && !p.Before.Dir && p.Before.Size == observationFixtureSize && p.Before.Name == s.RenameName && quark.ObservationSameAgentHandle(p.Before.Parent, i.RootAgent) && ei == nil && len(ids) == 1 && quark.ObservationSameAgentHandle(ids[0], i.SampleAgent) && ea == nil && action == 1 && quark.ObservationSameAgentHandle(str("to_pdir_fid"), s.NewDirectoryAgent)
			key = s.MoveID + ":move"
		case "/open/v1/task/query", "/open/v1/file/query_task":
			g.lastEndpoint = "operation_query"
			for _, id := range []string{s.RenameID, s.MoveID, s.DirectoryID} {
				if g.renameQueryOnly && id != s.RenameID {
					continue
				}
				receipt := s.Operations[id]
				var p followOperation
				_ = json.Unmarshal(receipt.Payload, &p)
				if p.TaskID != "" && receipt.State == "accepted" {
					allowed = allowed || (r.URL.Path == "/open/v1/task/query" && r.Method == "POST" && len(in) == 1 && str("task_id") == p.TaskID) || (r.URL.Path == "/open/v1/file/query_task" && r.Method == "GET" && r.URL.Query().Get("task_id") == p.TaskID)
				}
			}
		}
	}
	if g.renameQueryOnly {
		allowed = allowed && key == "" && kind == "api" && !w.media && (g.lastEndpoint == "agent_info" || g.lastEndpoint == "agent_list" || g.lastEndpoint == "operation_query")
	}
	if key != "" && kind != "broker" && !s.InitialContentVerified {
		allowed = false
	}
	if !allowed || g.store.poisoned || s.Completed || time.Since(s.Started) >= 12*time.Minute || s.Requests >= 220 || !w.media && s.API >= 200 || key != "" && s.Writes[key] || kind == "broker" && s.BrokerWrites >= 1 || kind == "read" && (s.Reads >= 4 || s.ReadBytes+followReadLimit > 4*followReadLimit) {
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
	if kind == "broker" {
		s.BrokerWrites++
	}
	if kind == "read" {
		s.Reads++
		s.ReadBytes += followReadLimit
	}
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
	limit := int64(4 << 20)
	if w.media {
		limit = observationFixtureSize
	}
	b, e := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	_ = resp.Body.Close()
	if e != nil || int64(len(b)) > limit {
		g.lastOutcome = "response_body_rejected"
		return nil, observationError()
	}
	resp.Body = io.NopCloser(bytes.NewReader(b))
	g.lastOutcome = "response_received"
	if w.media || resp.StatusCode != 200 {
		return resp, nil
	}
	g.store.mu.Lock()
	defer g.store.mu.Unlock()
	s = &g.store.state
	var env struct{ Data json.RawMessage }
	_ = json.Unmarshal(b, &env)
	_, mkdir, hasMkdir := g.operation(s.DirectoryID, "mkdir")
	if r.URL.Path == "/open/v1/dir" {
		var out struct {
			ID string `json:"fid"`
		}
		if json.Unmarshal(env.Data, &out) == nil && liveTVScalar(out.ID, 4096) {
			g.agentIDs[out.ID] = true
		}
	}
	if r.URL.Path == "/open/v1/file/list" && hasMkdir && mkdir.Handle != "" && g.known(map[string]bool{i.RootAgent: true}, str("parent_fid")) {
		var out struct {
			Files []struct {
				ID   string `json:"fid"`
				Name string `json:"filename"`
				Type string `json:"file_type"`
			} `json:"file_list"`
		}
		_ = json.Unmarshal(env.Data, &out)
		for _, f := range out.Files {
			if f.Name == s.DirectoryName && f.Type == "0" && liveTVScalar(f.ID, 4096) {
				g.agentIDs[f.ID] = true
				g.agentParents[f.ID] = true
			}
		}
	}
	if r.URL.Path == "/file" && r.URL.Query().Get("method") == "list" && hasMkdir && mkdir.Handle != "" && quark.QualifyID(quark.RouteTV, i.TVAccount, r.URL.Query().Get("parent_fid")) == i.Root.ID {
		var out struct {
			Files []struct {
				ID   string `json:"fid"`
				Name string `json:"filename"`
				Dir  int    `json:"isdir"`
			} `json:"files"`
		}
		_ = json.Unmarshal(env.Data, &out)
		for _, f := range out.Files {
			if f.Name == s.DirectoryName && f.Dir == 1 && liveTVScalar(f.ID, 4096) {
				g.tvParents[quark.QualifyID(quark.RouteTV, i.TVAccount, f.ID)] = true
			}
		}
	}
	if r.URL.Path == "/file" && r.URL.Query().Get("method") == "download" {
		var out struct {
			URL  string `json:"download_url"`
			Size int64  `json:"size"`
		}
		if json.Unmarshal(env.Data, &out) == nil && out.Size == observationFixtureSize {
			g.readURLs[out.URL] = true
		}
	}
	return resp, nil
}
