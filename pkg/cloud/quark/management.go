package quark

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/yuanjing-hash/OhMyCine-Server/pkg/cloud"
)

type agentItem struct {
	ID     string
	Parent string
	Name   string
	Dir    bool
	Size   int64
}

func (d *Client) agentInfo(ctx context.Context, id string) (agentItem, error) {
	raw, e := d.agentRead(ctx, http.MethodGet, "/open/v1/file/info", url.Values{"fid": {id}, "fetch_full_path": {"1"}}, nil)
	if e != nil {
		return agentItem{}, e
	}
	var f struct {
		ID     string `json:"fid"`
		Name   string `json:"filename"`
		Parent string `json:"parent_fid"`
		Type   string `json:"file_type"`
		Size   *int64 `json:"size"`
	}
	if json.Unmarshal(raw, &f) != nil || !scalar(f.ID, 4096) || !filename(f.Name) || !scalar(f.Parent, 4096) || f.Type != "0" && f.Type != "1" || f.Size == nil || *f.Size < 0 || !sameAgent(id, f.ID) {
		return agentItem{}, failure(CodeInvalid)
	}
	return agentItem{ID: id, Parent: f.Parent, Name: f.Name, Dir: f.Type == "0", Size: *f.Size}, nil
}
func agentKey(id string) string {
	if i := strings.LastIndexByte(id, '|'); i >= 0 && i < len(id)-1 {
		return "fingerprint:" + id[i+1:]
	}
	return "full:" + id
}
func sameAgent(a, b string) bool { return a != "" && b != "" && agentKey(a) == agentKey(b) }

// Listing handles can renew their opaque prefix while retaining the same
// route-local fingerprint. Qualifiers still bind the account and route; this
// never equates an ACK namespace with a listing namespace.
func qualifiedAgentMatches(qualified, account, raw string) bool {
	old, err := parseID(qualified, RouteAgent, account, false)
	return err == nil && sameAgent(old, raw)
}
func (d *Client) agentDirectory(ctx context.Context, parent string) ([]agentItem, error) {
	var cursor json.RawMessage
	seen := map[string]bool{}
	cursors := map[string]bool{}
	result := []agentItem{}
	for page := 0; page < 100; page++ {
		body := map[string]any{"parent_fid": parent, "sort": "updated_at:desc", "size": 100}
		if cursor != nil {
			body["query_cursor"] = cursor
		}
		raw, e := d.agentRead(ctx, http.MethodPost, "/open/v1/file/list", nil, body)
		if e != nil {
			return nil, e
		}
		var out struct {
			Files  json.RawMessage `json:"file_list"`
			Last   *bool           `json:"last_page"`
			Cursor json.RawMessage `json:"next_query_cursor"`
		}
		if json.Unmarshal(raw, &out) != nil || out.Last == nil {
			return nil, failure(CodeInvalid)
		}
		var files []struct {
			ID   string `json:"fid"`
			Name string `json:"filename"`
			Type string `json:"file_type"`
			Size *int64 `json:"size"`
		}
		emptyTerminal := *out.Last && (len(out.Files) == 0 || string(out.Files) == "null")
		if !emptyTerminal && (len(out.Files) == 0 || out.Files[0] != '[' || json.Unmarshal(out.Files, &files) != nil) || len(files) > 100 {
			return nil, failure(CodeInvalid)
		}
		for _, f := range files {
			key := agentKey(f.ID)
			if !scalar(f.ID, 4096) || !filename(f.Name) || f.Type != "0" && f.Type != "1" || f.Size == nil || *f.Size < 0 || seen[key] {
				return nil, failure(CodeInvalid)
			}
			seen[key] = true
			result = append(result, agentItem{ID: f.ID, Parent: parent, Name: f.Name, Dir: f.Type == "0", Size: *f.Size})
		}
		if *out.Last {
			return result, nil
		}
		if len(files) == 0 || len(out.Cursor) == 0 || len(out.Cursor) > 16384 || out.Cursor[0] != '{' || cursors[string(out.Cursor)] {
			return nil, failure(CodeIncomplete)
		}
		var c struct {
			Version json.RawMessage `json:"version"`
			Token   string          `json:"token"`
		}
		if json.Unmarshal(out.Cursor, &c) != nil || len(c.Version) == 0 || !scalar(c.Token, 8192) {
			return nil, failure(CodeInvalid)
		}
		cursors[string(out.Cursor)] = true
		cursor = out.Cursor
	}
	return nil, failure(CodeIncomplete)
}
func (d *Client) tvDirectory(ctx context.Context, parent string) ([]cloud.Item, error) {
	var all []cloud.Item
	var total *int64
	seen := map[string]bool{}
	for page := int64(0); page < 100; page++ {
		p, e := d.List(ctx, parent, cloud.PageRequest{Limit: 100, Offset: page * 100})
		if e != nil {
			return nil, e
		}
		if total != nil && (p.Total == nil || *p.Total != *total) {
			return nil, failure(CodeIncomplete)
		}
		total = p.Total
		for _, f := range p.Items {
			if seen[f.ID] {
				return nil, failure(CodeIncomplete)
			}
			seen[f.ID] = true
			all = append(all, f)
		}
		if !p.HasMore {
			return all, nil
		}
	}
	return nil, failure(CodeIncomplete)
}
func pathSegments(path string) ([]string, error) {
	if path == "" || path == "/" {
		return nil, nil
	}
	if !strings.HasPrefix(path, "/") || strings.Contains(path, "\\") {
		return nil, failure(CodeInvalid)
	}
	parts := strings.Split(strings.TrimPrefix(strings.TrimSuffix(path, "/"), "/"), "/")
	if len(parts) > 128 {
		return nil, failure(CodeInvalid)
	}
	for _, s := range parts {
		if !filename(s) {
			return nil, failure(CodeInvalid)
		}
	}
	return parts, nil
}
func (d *Client) resolveTV(ctx context.Context, path string) (cloud.Item, error) {
	parts, e := pathSegments(path)
	if e != nil {
		return cloud.Item{}, e
	}
	a, e := d.authorization(ctx, RouteTV)
	if e != nil {
		return cloud.Item{}, e
	}
	item := cloud.Item{ID: QualifyID(RouteTV, a.AccountID, "0"), Name: "/", IsDir: true}
	for _, part := range parts {
		if !item.IsDir {
			return cloud.Item{}, failure(CodeIdentity)
		}
		entries, e := d.tvDirectory(ctx, item.ID)
		if e != nil {
			return cloud.Item{}, e
		}
		count := 0
		for _, f := range entries {
			if f.Name == part {
				item = f
				count++
			}
		}
		if count != 1 {
			return cloud.Item{}, failure(CodeIdentity)
		}
	}
	return item, nil
}
func (d *Client) resolveAgent(ctx context.Context, path string) (agentItem, error) {
	parts, e := pathSegments(path)
	if e != nil {
		return agentItem{}, e
	}
	item := agentItem{ID: "0", Name: "/", Dir: true}
	for _, part := range parts {
		if !item.Dir {
			return agentItem{}, failure(CodeIdentity)
		}
		entries, e := d.agentDirectory(ctx, item.ID)
		if e != nil {
			return agentItem{}, e
		}
		count := 0
		for _, f := range entries {
			if f.Name == part {
				item = f
				count++
			}
		}
		if count != 1 {
			return agentItem{}, failure(CodeIdentity)
		}
	}
	return item, nil
}
func (d *Client) ResolveDirectory(ctx context.Context, path string) (cloud.Item, error) {
	item, e := d.resolveTV(ctx, path)
	if e == nil && !item.IsDir {
		return cloud.Item{}, failure(CodeIdentity)
	}
	return item, e
}

// ResolveManagementPath independently traverses both routes from a proven
// root. The caller supplies a relative path, never substitutes TV IDs into an
// Agent request. Complete unique listings and type/size agreement are required.
func (d *Client) ResolveManagementPath(ctx context.Context, tvRoot, agentRoot, relative string) (cloud.Item, error) {
	return d.resolveManagementPath(ctx, tvRoot, agentRoot, relative, "")
}

func (d *Client) resolveManagementPath(ctx context.Context, tvRoot, agentRoot, relative, expectedTV string) (cloud.Item, error) {
	c := d.Credentials()
	if !c.HasProof(tvRoot, agentRoot) {
		return cloud.Item{}, failure(CodeProof)
	}
	var proof SpaceProof
	for _, p := range c.Proofs {
		if p.TVRoot == tvRoot && p.AgentRoot == agentRoot {
			proof = p
			break
		}
	}
	currentTV, e := d.resolveTV(ctx, proof.Path)
	if e != nil {
		return cloud.Item{}, e
	}
	currentAgent, e := d.resolveAgent(ctx, proof.Path)
	if e != nil {
		return cloud.Item{}, e
	}
	if currentTV.ID != tvRoot || !qualifiedAgentMatches(agentRoot, c.Agent.AccountID, currentAgent.ID) {
		return cloud.Item{}, failure(CodeProof)
	}
	parts, e := pathSegments("/" + strings.TrimPrefix(relative, "/"))
	if e != nil {
		return cloud.Item{}, e
	}
	path := strings.TrimSuffix(proof.Path, "/")
	if len(parts) > 0 {
		path += "/" + strings.Join(parts, "/")
	}
	if path == "" {
		path = "/"
	}
	tv, e := d.resolveTV(ctx, path)
	if e != nil {
		return cloud.Item{}, e
	}
	if expectedTV != "" && tv.ID != expectedTV {
		return cloud.Item{}, failure(CodeIdentity)
	}
	agent, e := d.resolveAgent(ctx, path)
	if e != nil {
		return cloud.Item{}, e
	}
	if tv.IsDir != agent.Dir || !tv.IsDir && tv.Size != agent.Size {
		return cloud.Item{}, failure(CodeIdentity)
	}
	a, e := d.authorization(ctx, RouteAgent)
	if e != nil {
		return cloud.Item{}, e
	}
	return toCloudAgent(a.AccountID, agent), nil
}
func toCloudAgent(account string, f agentItem) cloud.Item {
	return cloud.Item{ID: QualifyID(RouteAgent, account, f.ID), ParentID: QualifyID(RouteAgent, account, f.Parent), Name: f.Name, IsDir: f.Dir, Size: f.Size}
}
func (d *Client) managed(ctx context.Context, qualified string) (agentItem, error) {
	a, e := d.authorization(ctx, RouteAgent)
	if e != nil {
		return agentItem{}, e
	}
	id, e := parseID(qualified, RouteAgent, a.AccountID, false)
	if e != nil {
		return agentItem{}, e
	}
	c := d.Credentials()
	roots := map[string]bool{}
	for _, p := range c.Proofs {
		if c.HasProof(p.TVRoot, p.AgentRoot) {
			root, e := parseID(p.AgentRoot, RouteAgent, a.AccountID, true)
			if e == nil {
				roots[agentKey(root)] = true
			}
		}
	}
	if id == "0" && roots[agentKey("0")] {
		return agentItem{ID: "0", Name: "/", Dir: true}, nil
	}
	item, e := d.agentInfo(ctx, id)
	if e != nil {
		return agentItem{}, e
	}
	current := item
	seen := map[string]bool{}
	for depth := 0; depth < 128; depth++ {
		if roots[agentKey(current.ID)] {
			return item, nil
		}
		if current.Parent == "0" && roots[agentKey("0")] {
			return item, nil
		}
		if current.Parent == "0" || seen[agentKey(current.Parent)] {
			return agentItem{}, failure(CodeProof)
		}
		seen[agentKey(current.Parent)] = true
		current, e = d.agentInfo(ctx, current.Parent)
		if e != nil {
			return agentItem{}, e
		}
	}
	return agentItem{}, failure(CodeProof)
}

type operationPayload struct {
	Request cloud.OperationRequest `json:"request"`
	// Provider rename batches use 16 hexadecimal characters, independently of
	// our durable operation ID. Persist this before submission; legacy receipts
	// without it remain query-only and must never acquire a replacement intent.
	BatchID       string                  `json:"batch_id,omitempty"`
	SubmitFailure *operationSubmitFailure `json:"submit_failure,omitempty"`
	TaskID        string                  `json:"task_id,omitempty"`
	Handle        string                  `json:"handle,omitempty"`
	Before        agentItem               `json:"before"`
	TargetBefore  []agentItem             `json:"target_before,omitempty"`
	Finished      bool                    `json:"finished"`
	Outputs       []cloud.Item            `json:"outputs,omitempty"`
	Target        *operationTarget        `json:"target,omitempty"`
	Mapping       *operationMapping       `json:"mapping,omitempty"`
}

// Only fixed codes and numeric diagnostics survive a failed submission. Never
// retain upstream response text, URLs or headers in an error or a receipt.
type operationSubmitFailure struct {
	HTTPStatus int `json:"http_status,omitempty"`
	Errno      int `json:"errno,omitempty"`
}

func operationUnknownFailure(p operationPayload) *Failure {
	f := &Failure{Code: cloud.CodeMutationUnknown, UnknownOutcome: true}
	if p.SubmitFailure != nil {
		f.HTTPStatus, f.Errno = p.SubmitFailure.HTTPStatus, p.SubmitFailure.Errno
	}
	return f
}

func (d *Client) saveReceipt(ctx context.Context, r *cloud.OperationReceipt, p operationPayload, state string) error {
	next := *r
	next.State = state
	next.Revision++
	next.UpdatedAt = d.api.clock()
	next.Payload, _ = json.Marshal(p)
	if e := d.config.Operations.Update(ctx, r.Revision, next); e != nil {
		return e
	}
	*r = next
	return nil
}
func (d *Client) checkpointReceipt(ctx context.Context, r *cloud.OperationReceipt, p operationPayload, state string) error {
	c, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Second)
	defer cancel()
	return d.saveReceipt(c, r, p, state)
}
func (d *Client) BeginOperation(ctx context.Context, request cloud.OperationRequest) (cloud.OperationResult, error) {
	if d.config.Operations == nil || !scalar(request.ID, 128) {
		return cloud.OperationResult{}, failure(CodeUnsupported)
	}
	old, loadErr := d.config.Operations.Load(ctx, request.ID)
	if loadErr == nil {
		var p operationPayload
		if json.Unmarshal(old.Payload, &p) != nil || !sameRequest(p.Request, request, old.AccountID) {
			return cloud.OperationResult{}, failure(CodeIdentity)
		}
		return d.QueryOperation(ctx, request.ID)
	}
	a, e := d.authorization(ctx, RouteAgent)
	if e != nil {
		return cloud.OperationResult{}, e
	}
	p := operationPayload{Request: request}
	if request.Kind != "mkdir" && request.Kind != "move" && request.Kind != "rename" && request.Kind != "receive_share" {
		return cloud.OperationResult{}, failure(CodeUnsupported)
	}
	var parent agentItem
	if request.Kind == "mkdir" || request.Kind == "move" || request.Kind == "receive_share" {
		parent, e = d.managed(ctx, request.ParentID)
		if e != nil {
			return cloud.OperationResult{}, e
		}
		if !parent.Dir {
			return cloud.OperationResult{}, failure(CodeIdentity)
		}
	}
	if request.Kind == "move" || request.Kind == "rename" {
		p.Before, e = d.managed(ctx, request.ItemID)
		if e != nil {
			return cloud.OperationResult{}, e
		}
	}
	if request.Kind == "receive_share" {
		if _, e = selectedShare(request.Share); e != nil {
			return cloud.OperationResult{}, e
		}
		p.TargetBefore, e = d.agentDirectory(ctx, parent.ID)
		if e != nil {
			return cloud.OperationResult{}, e
		}
	}
	if request.Kind == "mkdir" || request.Kind == "rename" {
		if !filename(request.Name) {
			return cloud.OperationResult{}, failure(CodeInvalid)
		}
		target := parent.ID
		if request.Kind == "rename" {
			target = p.Before.Parent
		}
		files, e := d.agentDirectory(ctx, target)
		if e != nil {
			return cloud.OperationResult{}, e
		}
		for _, f := range files {
			if f.Name == request.Name && !sameAgent(f.ID, p.Before.ID) {
				err := cloud.Error(cloud.CodeConflict, false, nil)
				if errors.Is(loadErr, cloud.ErrOperationNotFound) {
					return cloud.OperationResult{}, &OperationNotSubmittedError{Err: err}
				}
				return cloud.OperationResult{}, err
			}
		}
		if request.Kind == "mkdir" {
			p.Target, e = d.freezeCreationTarget(ctx, parent.ID)
			if e != nil {
				return cloud.OperationResult{}, e
			}
		}
	}
	if request.Kind == "rename" {
		var batch [8]byte
		if _, e = rand.Read(batch[:]); e != nil {
			return cloud.OperationResult{}, failure(CodeUnavailable)
		}
		p.BatchID = hex.EncodeToString(batch[:])
	}
	r := cloud.OperationReceipt{ID: request.ID, Provider: cloud.ProviderQuark, Route: string(RouteAgent), AccountID: a.AccountID, Kind: request.Kind, Revision: 1, State: "prepared", UpdatedAt: d.api.clock()}
	r.Payload, _ = json.Marshal(p)
	if e = d.config.Operations.Create(ctx, r); e != nil {
		return cloud.OperationResult{}, e
	}
	if e = d.saveReceipt(ctx, &r, p, "submitting"); e != nil {
		return cloud.OperationResult{Receipt: r}, e
	}
	var raw json.RawMessage
	switch request.Kind {
	case "mkdir":
		raw, e = d.api.agent(ctx, a, http.MethodPost, "/open/v1/dir", nil, map[string]string{"dir_path": request.Name, "pdir_fid": parent.ID})
	case "move":
		raw, e = d.api.agent(ctx, a, http.MethodPost, "/open/v1/file/move", nil, map[string]any{"fid_list": []string{p.Before.ID}, "to_pdir_fid": parent.ID, "action_type": 1})
	case "rename":
		raw, e = d.api.agent(ctx, a, http.MethodPost, "/open/v1/file/rename/batch", nil, map[string]any{"batch_id": p.BatchID, "operation": "RENAME", "rename_list": []map[string]string{{"fid": p.Before.ID, "file_name": request.Name, "origin_file_name": p.Before.Name}}})
	case "receive_share":
		raw, e = d.receiveSelected(ctx, a, request.Share, parent.ID)
	}
	if e != nil {
		var diagnostic *Failure
		if errors.As(e, &diagnostic) && (diagnostic.HTTPStatus != 0 || diagnostic.Errno != 0) {
			p.SubmitFailure = &operationSubmitFailure{HTTPStatus: diagnostic.HTTPStatus, Errno: diagnostic.Errno}
		}
		_ = d.checkpointReceipt(ctx, &r, p, "unknown")
		return cloud.OperationResult{Receipt: r}, operationUnknownFailure(p)
	}
	var ack struct {
		ID     string          `json:"fid"`
		Task   string          `json:"task_id"`
		Finish json.RawMessage `json:"finish"`
	}
	if json.Unmarshal(raw, &ack) != nil {
		_ = d.checkpointReceipt(ctx, &r, p, "unknown")
		return cloud.OperationResult{Receipt: r}, failure(cloud.CodeMutationUnknown)
	}
	p.Handle, p.TaskID = ack.ID, ack.Task
	p.Finished = string(ack.Finish) == "true" || string(ack.Finish) == "1"
	if request.Kind == "mkdir" && !scalar(ack.ID, 4096) || request.Kind != "mkdir" && !p.Finished && !scalar(ack.Task, 4096) {
		_ = d.checkpointReceipt(ctx, &r, p, "unknown")
		return cloud.OperationResult{Receipt: r}, failure(cloud.CodeMutationUnknown)
	}
	if e = d.checkpointReceipt(ctx, &r, p, "accepted"); e != nil {
		return cloud.OperationResult{Receipt: r}, e
	}
	if ctx.Err() != nil {
		return cloud.OperationResult{Receipt: r}, ctx.Err()
	}
	return d.QueryOperation(ctx, request.ID)
}
func sameRequest(a, b cloud.OperationRequest, account string) bool {
	for _, pair := range [][2]*string{{&a.ParentID, &b.ParentID}, {&a.ItemID, &b.ItemID}} {
		if *pair[0] == *pair[1] {
			continue
		}
		raw, err := parseID(*pair[1], RouteAgent, account, false)
		if err != nil || !qualifiedAgentMatches(*pair[0], account, raw) {
			return false
		}
		*pair[1] = *pair[0]
	}
	x, _ := json.Marshal(a)
	y, _ := json.Marshal(b)
	return string(x) == string(y)
}
func (d *Client) QueryOperation(ctx context.Context, id string) (cloud.OperationResult, error) {
	if d.config.Operations == nil {
		return cloud.OperationResult{}, failure(CodeUnsupported)
	}
	r, e := d.config.Operations.Load(ctx, id)
	if e != nil {
		return cloud.OperationResult{}, e
	}
	var p operationPayload
	a, e := d.authorization(ctx, RouteAgent)
	if e != nil {
		return cloud.OperationResult{Receipt: r}, e
	}
	if r.Provider != cloud.ProviderQuark || r.Route != string(RouteAgent) || r.AccountID != a.AccountID || json.Unmarshal(r.Payload, &p) != nil {
		return cloud.OperationResult{}, failure(CodeIdentity)
	}
	if r.Kind == "upload" {
		return d.queryUpload(ctx, r)
	}
	if r.Kind != "mkdir" && r.Kind != "rename" && r.Kind != "move" && r.Kind != "receive_share" {
		return cloud.OperationResult{Receipt: r}, failure(CodeUnsupported)
	}
	if r.State == "confirmed" {
		return cloud.OperationResult{Receipt: r, Items: p.Outputs, Complete: true}, nil
	}
	// A lost write acknowledgement must never trigger a second mutation. For
	// rename/move we already know the stable input identity and frozen target,
	// so a fresh managed-root check and complete target/old-parent listings can
	// prove its actual output without the missing task handle. Creations and
	// share receipts remain unknown until their own identity evidence exists.
	recoverKnown := (r.State == "unknown" || r.State == "submitting") && (r.Kind == "rename" || r.Kind == "move") && scalar(p.Before.ID, 4096)
	if r.State != "accepted" && !recoverKnown {
		return cloud.OperationResult{Receipt: r}, operationUnknownFailure(p)
	}
	if p.TaskID != "" && !p.Finished {
		method, path, body, q := http.MethodPost, "/open/v1/task/query", any(map[string]string{"task_id": p.TaskID}), url.Values{}
		if r.Kind == "move" {
			method, path, body = http.MethodGet, "/open/v1/file/query_task", nil
			q.Set("task_id", p.TaskID)
		}
		raw, e := d.agentRead(ctx, method, path, q, body)
		if e != nil {
			return cloud.OperationResult{Receipt: r}, e
		}
		var state struct {
			Status  *int              `json:"status"`
			Finish  json.RawMessage   `json:"finish"`
			Fail    *int              `json:"fail_count"`
			Unknown *int              `json:"unknown_count"`
			Skip    *int              `json:"skip_count"`
			Failed  []json.RawMessage `json:"failed_results"`
		}
		if json.Unmarshal(raw, &state) != nil {
			return cloud.OperationResult{Receipt: r}, failure(CodeInvalid)
		}
		if state.Status != nil && *state.Status == 3 || state.Fail != nil && *state.Fail > 0 || state.Unknown != nil && *state.Unknown > 0 || state.Skip != nil && *state.Skip > 0 || len(state.Failed) > 0 {
			_ = d.saveReceipt(ctx, &r, p, "failed")
			return cloud.OperationResult{Receipt: r}, failure(CodeUnavailable)
		}
		p.Finished = state.Status != nil && *state.Status == 2 || string(state.Finish) == "true" || string(state.Finish) == "1"
		if !p.Finished {
			return cloud.OperationResult{Receipt: r}, nil
		}
		if e = d.saveReceipt(ctx, &r, p, "accepted"); e != nil {
			return cloud.OperationResult{Receipt: r}, e
		}
	}
	if r.Kind == "receive_share" {
		return d.confirmReceived(ctx, r, p)
	}
	handle := p.Handle
	if r.Kind != "mkdir" {
		handle = p.Before.ID
	}
	var item agentItem
	if recoverKnown {
		item, e = d.managed(ctx, QualifyID(RouteAgent, a.AccountID, handle))
	} else {
		item, e = d.agentInfo(ctx, handle)
	}
	if e != nil {
		return cloud.OperationResult{Receipt: r}, e
	}
	targetName := p.Before.Name
	targetParent := p.Before.Parent
	if r.Kind == "mkdir" || r.Kind == "rename" {
		targetName = p.Request.Name
	}
	if r.Kind == "mkdir" || r.Kind == "move" {
		targetParent, e = parseID(p.Request.ParentID, RouteAgent, a.AccountID, false)
		if e != nil {
			return cloud.OperationResult{Receipt: r}, e
		}
	}
	parentMismatch := !sameAgent(item.Parent, targetParent)
	if item.Name != targetName || parentMismatch && (r.Kind != "mkdir" || sameAgentDomain(item.Parent, targetParent)) || r.Kind == "mkdir" && !item.Dir || r.Kind != "mkdir" && (item.Dir != p.Before.Dir || item.Size != p.Before.Size) {
		if recoverKnown {
			return cloud.OperationResult{Receipt: r}, operationUnknownFailure(p)
		}
		return cloud.OperationResult{Receipt: r}, failure(CodePending)
	}
	entries, e := d.agentDirectory(ctx, targetParent)
	if e != nil {
		return cloud.OperationResult{Receipt: r}, e
	}
	count := 0
	var listed agentItem
	for _, f := range entries {
		if f.Name == item.Name {
			count++
			listed = f
			if f.Dir != item.Dir || f.Size != item.Size || !sameAgent(f.ID, item.ID) && (r.Kind != "mkdir" || sameAgentDomain(f.ID, item.ID)) {
				return cloud.OperationResult{Receipt: r}, failure(CodeIdentity)
			}
		}
	}
	if count != 1 {
		return cloud.OperationResult{Receipt: r}, failure(CodePending)
	}
	if r.Kind == "mkdir" && (!sameAgent(listed.ID, item.ID) || parentMismatch) {
		p.Mapping, e = d.confirmCreationMapping(ctx, r, p.Target, item, listed, "", p.Mapping)
		if e != nil {
			return cloud.OperationResult{Receipt: r}, e
		}
	}
	if r.Kind == "move" && !sameAgent(p.Before.Parent, targetParent) {
		previous, e := d.agentDirectory(ctx, p.Before.Parent)
		if e != nil {
			return cloud.OperationResult{Receipt: r}, e
		}
		for _, f := range previous {
			if sameAgent(f.ID, item.ID) {
				return cloud.OperationResult{Receipt: r}, failure(CodePending)
			}
		}
	}
	p.Outputs = []cloud.Item{toCloudAgent(a.AccountID, item)}
	if e = d.saveReceipt(ctx, &r, p, "confirmed"); e != nil {
		return cloud.OperationResult{Receipt: r}, e
	}
	return cloud.OperationResult{Receipt: r, Items: p.Outputs, Complete: true}, nil
}

// Legacy wrappers preserve explicit pending outcomes; a durable operation ID
// must be supplied by the pipeline for restart-safe orchestration.
type operationIDKey struct{}

func WithOperationID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, operationIDKey{}, id)
}
func operationID(ctx context.Context) string {
	id, _ := ctx.Value(operationIDKey{}).(string)
	return id
}
func (d *Client) CreateDirectory(ctx context.Context, parent, name string) (cloud.Item, error) {
	r, e := d.BeginOperation(ctx, cloud.OperationRequest{ID: operationID(ctx), Kind: "mkdir", ParentID: parent, Name: name})
	if e != nil {
		return cloud.Item{}, e
	}
	if !r.Complete || len(r.Items) != 1 {
		return cloud.Item{}, failure(CodePending)
	}
	return r.Items[0], nil
}
func (d *Client) Move(ctx context.Context, id, parent string) error {
	r, e := d.BeginOperation(ctx, cloud.OperationRequest{ID: operationID(ctx), Kind: "move", ItemID: id, ParentID: parent})
	if e == nil && !r.Complete {
		return failure(CodePending)
	}
	return e
}
func (d *Client) Rename(ctx context.Context, id, name string) error {
	r, e := d.BeginOperation(ctx, cloud.OperationRequest{ID: operationID(ctx), Kind: "rename", ItemID: id, Name: name})
	if e == nil && !r.Complete {
		return failure(CodePending)
	}
	return e
}
func (d *Client) Copy(context.Context, string, string) error { return failure(CodeUnsupported) }
func (d *Client) Recycle(context.Context, string) error      { return failure(CodeUnsupported) }

var _ cloud.MutationDriver = (*Client)(nil)
var _ cloud.AsyncMutationDriver = (*Client)(nil)
var _ cloud.DirectoryPathResolver = (*Client)(nil)
