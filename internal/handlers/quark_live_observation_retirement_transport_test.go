//go:build quark_live

package handlers_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/yuanjing-hash/OhMyCine-Server/pkg/cloud/quark"
)

type retirementPreflight struct {
	OperationID, SampleTV, SampleAgent, Name, SourceTV, TargetTV, SourceAgent, TargetAgent string
	Size                                                                                   int64
	At                                                                                     time.Time
}

func retirementExpectedPreflight(st retirementState, id string) (retirementPreflight, bool) {
	i := st.Identity
	c := i.Creation
	p := retirementPreflight{OperationID: id, SampleTV: c.Sample.ID, SampleAgent: c.SampleAgent, Name: i.Name, Size: observationFixtureSize, SourceTV: i.Inside.ID, SourceAgent: i.InsideAgent, TargetTV: c.Proof.TVRoot, TargetAgent: c.ProofAgent}
	if id == st.MoveBackID {
		p.SourceTV, p.TargetTV = p.TargetTV, p.SourceTV
		p.SourceAgent, p.TargetAgent = p.TargetAgent, p.SourceAgent
	} else if id != st.MoveOutID {
		return p, false
	}
	return p, true
}

type retirementGuard struct {
	mu                        sync.Mutex
	store                     *retirementStore
	delay                     time.Duration
	startup                   bool
	agentIDs, agentParents    map[string]bool
	tvParents                 map[string]bool
	agentNext                 map[string]string
	tvNext                    map[string]bool
	lastEndpoint, lastOutcome string
	lastStatus                int
}

func newRetirementGuard(s *retirementStore) *retirementGuard {
	i, c := s.state.Identity, s.state.Identity.Creation
	g := &retirementGuard{store: s, delay: 2 * time.Second, startup: true, agentIDs: map[string]bool{}, agentParents: map[string]bool{}, tvParents: map[string]bool{c.Proof.TVRoot: true, c.Root.ID: true, i.Inside.ID: true}, agentNext: map[string]string{}, tvNext: map[string]bool{}}
	for _, id := range []string{c.RootAgent, c.ProofAgent, c.SampleAgent, c.RootACK, c.SampleACK, c.ProofRootACK, i.InsideAgent} {
		g.agentIDs[id] = true
	}
	for _, id := range []string{c.RootAgent, c.ProofAgent, i.InsideAgent} {
		g.agentParents[id] = true
	}
	return g
}
func (g *retirementGuard) known(m map[string]bool, id string) bool {
	for k := range m {
		if quark.ObservationSameAgentHandle(k, id) {
			return true
		}
	}
	return false
}
func (g *retirementGuard) wrap(base http.RoundTripper, media bool) http.RoundTripper {
	return retirementTransport{g: g, base: base, media: media}
}

type retirementTransport struct {
	g     *retirementGuard
	base  http.RoundTripper
	media bool
}

func (w retirementTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	g := w.g
	g.mu.Lock()
	defer g.mu.Unlock()
	g.lastEndpoint, g.lastOutcome, g.lastStatus = "other", "guard_rejected", 0
	if w.media || r.Context().Err() != nil || r.URL.Scheme != "https" || r.URL.User != nil || r.URL.Fragment != "" {
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
	str := func(k string) string { var v string; _ = json.Unmarshal(in[k], &v); return v }
	g.store.mu.Lock()
	s := &g.store.state
	c := s.Identity.Creation
	allowed, key, broker := false, "", false
	parentAgent, parentTV := "", ""
	secondAgent, secondTV := false, false
	if r.URL.Host == "api.extscreen.com" && r.URL.Path == "/quarkdrive/token" {
		g.lastEndpoint = "startup_broker"
		broker = true
		key = "tv_startup:broker"
		allowed = g.startup && r.Method == "POST" && str("refresh_token") != "" && str("code") == "" && str("device_id") == c.TVDevice
	} else if r.URL.Host == "open-api-drive.quark.cn" {
		switch r.URL.Path {
		case "/user":
			g.lastEndpoint = "tv_identity"
			allowed = g.startup && r.Method == "GET" && r.URL.Query().Get("method") == "user_info"
		case "/open/v1/user/info":
			g.lastEndpoint = "agent_identity"
			allowed = r.Method == "GET"
		case "/open/v1/file/info":
			g.lastEndpoint = "agent_info"
			allowed = r.Method == "GET" && g.known(g.agentIDs, r.URL.Query().Get("fid"))
		case "/open/v1/file/list":
			g.lastEndpoint = "agent_list"
			parentAgent = str("parent_fid")
			var size int
			secondAgent = len(in["query_cursor"]) > 0
			allowed = r.Method == "POST" && json.Unmarshal(in["size"], &size) == nil && size > 0 && size <= 100 && (parentAgent == "0" || g.known(g.agentParents, parentAgent))
			if secondAgent {
				allowed = allowed && parentAgent != "0" && g.agentNext[parentAgent] != "" && g.agentNext[parentAgent] == string(in["query_cursor"])
			}
		case "/file":
			g.lastEndpoint = "tv_list"
			q := r.URL.Query()
			parentTV = q.Get("parent_fid")
			page, e := strconv.Atoi(q.Get("page_index"))
			size, se := strconv.Atoi(q.Get("page_size"))
			secondTV = page == 1
			allowed = r.Method == "GET" && q.Get("method") == "list" && e == nil && page >= 0 && page <= 1 && se == nil && size > 0 && size <= 100 && (parentTV == "0" || g.tvParents[quark.QualifyID(quark.RouteTV, c.TVAccount, parentTV)])
			if secondTV {
				allowed = allowed && parentTV != "0" && g.tvNext[parentTV]
			}
		case "/open/v1/file/move":
			g.lastEndpoint = "move"
			var ids []string
			var action int
			if json.Unmarshal(in["fid_list"], &ids) == nil && len(ids) == 1 && json.Unmarshal(in["action_type"], &action) == nil && action == 1 && len(in) == 3 && r.Method == "POST" {
				for _, id := range []string{s.MoveOutID, s.MoveBackID} {
					receipt, exists := s.Operations[id]
					var p followOperation
					expected, _ := retirementExpectedPreflight(*s, id)
					pre := s.Preflights[id]
					at := pre.At
					pre.At = time.Time{}
					if !exists || !g.store.allowed(receipt) || receipt.State != "submitting" || json.Unmarshal(receipt.Payload, &p) != nil || p.Request.ID != id || p.Request.Kind != "move" {
						continue
					}
					raw, valid := followRawID(p.Request.ItemID, quark.RouteAgent, c.AgentAccount)
					target, tvalid := followRawID(p.Request.ParentID, quark.RouteAgent, c.AgentAccount)
					if pre != expected || at.IsZero() || time.Since(at) < 0 || time.Since(at) > time.Minute || !valid || !tvalid || !quark.ObservationSameAgentHandle(raw, c.SampleAgent) || !quark.ObservationSameAgentHandle(target, expected.TargetAgent) || !quark.ObservationSameAgentHandle(p.Before.ID, c.SampleAgent) || !quark.ObservationSameAgentHandle(p.Before.Parent, expected.SourceAgent) || p.Before.Name != expected.Name || p.Before.Size != expected.Size || p.Before.Dir {
						continue
					}
					if id == s.MoveBackID {
						out, ok := s.Operations[s.MoveOutID]
						if !ok || out.State != "confirmed" {
							continue
						}
					}
					if quark.ObservationSameAgentHandle(ids[0], c.SampleAgent) && quark.ObservationSameAgentHandle(str("to_pdir_fid"), expected.TargetAgent) {
						allowed = true
						key = id + ":move"
					}
				}
			}
		case "/open/v1/file/query_task":
			g.lastEndpoint = "operation_query"
			for _, id := range []string{s.MoveOutID, s.MoveBackID} {
				receipt, exists := s.Operations[id]
				var p followOperation
				if exists && g.store.allowed(receipt) && json.Unmarshal(receipt.Payload, &p) == nil && p.TaskID != "" && (receipt.State == "accepted" || receipt.State == "unknown") && r.Method == "GET" && r.URL.Query().Get("task_id") == p.TaskID {
					allowed = true
				}
			}
		}
	}
	if !allowed || g.store.poisoned || s.Completed || !time.Now().Before(s.Deadline) || s.Requests >= 220 || s.API >= 200 || key != "" && s.Writes[key] || broker && s.BrokerWrites >= 1 {
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
	if r.Context().Err() != nil || !time.Now().Before(s.Deadline) || s.Completed || g.store.poisoned || s.Requests >= 220 || s.API >= 200 || key != "" && s.Writes[key] {
		g.store.mu.Unlock()
		return nil, observationError()
	}
	if key != "" && !broker {
		id := s.MoveOutID
		if key == s.MoveBackID+":move" {
			id = s.MoveBackID
		}
		pre := s.Preflights[id]
		at := pre.At
		pre.At = time.Time{}
		want, valid := retirementExpectedPreflight(*s, id)
		receipt := s.Operations[id]
		if !valid || pre != want || at.IsZero() || time.Since(at) < 0 || time.Since(at) > time.Minute || receipt.State != "submitting" {
			g.store.mu.Unlock()
			return nil, observationError()
		}
	}
	s.Requests++
	s.API++
	s.LastDispatch = time.Now().UTC()
	if key != "" {
		s.Writes[key] = true
	}
	if broker {
		s.BrokerWrites++
	}
	if g.store.save() != nil {
		g.store.mu.Unlock()
		return nil, observationError()
	}
	g.store.mu.Unlock()
	if parentAgent != "" {
		delete(g.agentNext, parentAgent)
	}
	if parentTV != "" {
		delete(g.tvNext, parentTV)
	}
	requestDeadline := time.Now().Add(30 * time.Second)
	if s.Deadline.Before(requestDeadline) {
		requestDeadline = s.Deadline
	}
	ctx, cancel := context.WithDeadline(r.Context(), requestDeadline)
	defer cancel()
	resp, e := w.base.RoundTrip(r.WithContext(ctx))
	if e != nil {
		g.lastOutcome = "transport_failed"
		return nil, observationError()
	}
	g.lastStatus = resp.StatusCode
	b, e := io.ReadAll(io.LimitReader(resp.Body, (4<<20)+1))
	_ = resp.Body.Close()
	if e != nil || len(b) > 4<<20 {
		g.lastOutcome = "response_body_rejected"
		return nil, observationError()
	}
	resp.Body = io.NopCloser(bytes.NewReader(b))
	g.lastOutcome = "response_received"
	if resp.StatusCode == 200 {
		var env struct {
			Status *int
			Data   json.RawMessage
		}
		if json.Unmarshal(b, &env) == nil && env.Status != nil && *env.Status == 0 {
			if parentAgent != "" && !secondAgent && parentAgent != "0" {
				var data struct {
					Last   *bool             `json:"last_page"`
					Cursor json.RawMessage   `json:"next_query_cursor"`
					Files  []json.RawMessage `json:"file_list"`
				}
				if json.Unmarshal(env.Data, &data) == nil && data.Last != nil && !*data.Last && len(data.Files) <= 100 && len(data.Files) > 0 && len(data.Cursor) > 0 && len(data.Cursor) <= 16384 {
					g.agentNext[parentAgent] = string(data.Cursor)
				}
			}
			if parentTV != "" && !secondTV && parentTV != "0" {
				g.tvNext[parentTV] = true
			}
		}
	}
	return resp, nil
}
