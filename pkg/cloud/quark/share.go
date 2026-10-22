package quark

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/yuanjing-hash/OhMyCine-Server/pkg/cloud"
)

type shareFile struct {
	ID    string `json:"id"`
	Token string `json:"token"`
	Name  string `json:"name"`
	Dir   bool   `json:"dir"`
	Size  int64  `json:"size"`
}
type sharePrivate struct {
	Share    string      `json:"share"`
	Passcode string      `json:"passcode"`
	Token    string      `json:"token"`
	Parent   string      `json:"parent"`
	Files    []shareFile `json:"files"`
	Complete bool        `json:"complete"`
}

func parseShare(raw string) (string, string, error) {
	u, e := url.Parse(strings.TrimSpace(raw))
	if e != nil || u.Scheme != "http" && u.Scheme != "https" || u.Host != "pan.quark.cn" || u.User != nil || u.Opaque != "" || u.RawPath != "" || !strings.HasPrefix(u.Path, "/s/") {
		return "", "", failure("quark_share_invalid")
	}
	id := strings.TrimPrefix(u.Path, "/s/")
	if !shareScalar(id, 256) {
		return "", "", failure("quark_share_invalid")
	}
	pass := u.Query().Get("pwd")
	if pass != "" && !shareScalar(pass, 64) {
		return "", "", failure("quark_share_password_invalid")
	}
	return id, pass, nil
}

// NormalizeShareLink preserves the extraction code while returning a stable
// share identity. It never accepts a lookalike domain or credential-bearing URL.
func NormalizeShareLink(raw, passcode string) (string, string, error) {
	id, pass, e := parseShare(raw)
	if e != nil {
		return "", "", e
	}
	if passcode != "" {
		if !shareScalar(passcode, 64) || pass != "" && pass != passcode {
			return "", "", failure("quark_share_password_invalid")
		}
		pass = passcode
	}
	u := url.URL{Scheme: "https", Host: "pan.quark.cn", Path: "/s/" + id}
	if pass != "" {
		u.RawQuery = url.Values{"pwd": {pass}}.Encode()
	}
	return u.String(), id, nil
}
func shareScalar(s string, n int) bool {
	if s == "" || len(s) > n {
		return false
	}
	for _, r := range s {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_') {
			return false
		}
	}
	return true
}
func (d *Client) guest(ctx context.Context, path, method string, q url.Values, body any) (json.RawMessage, error) {
	if q == nil {
		q = url.Values{}
	}
	q.Set("req_id", randomID())
	q.Set("access_token", "guest_placeholder")
	q.Set("pr", "ucpro")
	q.Set("fr", "third")
	var b []byte
	if body != nil {
		b, _ = json.Marshal(body)
	}
	req, e := http.NewRequestWithContext(ctx, method, guestOrigin+path+"?"+q.Encode(), bytes.NewReader(b))
	if e != nil {
		return nil, failure(CodeInvalid)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept-Encoding", "identity")
	req.Header.Set("User-Agent", "OhMyCine/Quark")
	// No account header, cookie jar or Bearer material enters guest browsing.
	raw, e := d.api.call(ctx, req)
	if e != nil {
		return nil, e
	}
	var env struct {
		Status *int            `json:"status"`
		Code   *int            `json:"code"`
		Errno  int             `json:"errno"`
		Data   json.RawMessage `json:"data"`
	}
	if json.Unmarshal(raw, &env) != nil || env.Status == nil || *env.Status != 0 && *env.Status < 100 || *env.Status >= 100 && env.Code == nil || env.Code != nil && *env.Code != 0 || env.Errno != 0 || len(env.Data) == 0 || env.Data[0] != '{' {
		return nil, failure("quark_share_invalid")
	}
	return env.Data, nil
}
func (d *Client) sharePage(ctx context.Context, p sharePrivate, page int, first bool) ([]shareFile, string, error) {
	var raw json.RawMessage
	var e error
	if first {
		raw, e = d.guest(ctx, "/1/clouddrive/share/sharepage/v2/detail", http.MethodPost, nil, map[string]any{"pwd_id": p.Share, "passcode": p.Passcode, "page": 1, "size": 100, "fetch_total": 1, "fetch_share": 1})
	} else {
		raw, e = d.guest(ctx, "/1/clouddrive/share/sharepage/detail", http.MethodGet, url.Values{"pwd_id": {p.Share}, "stoken": {p.Token}, "pdir_fid": {p.Parent}, "_page": {strconv.Itoa(page)}, "_size": {"100"}, "_fetch_total": {"1"}, "_fetch_share": {"1"}}, nil)
	}
	if e != nil {
		return nil, "", e
	}
	var fields struct {
		Token struct {
			Value string `json:"stoken"`
		} `json:"token_info"`
		Detail struct {
			List json.RawMessage `json:"list"`
		} `json:"detail_info"`
		List json.RawMessage `json:"list"`
	}
	if json.Unmarshal(raw, &fields) != nil {
		return nil, "", failure(CodeInvalid)
	}
	list := fields.List
	token := p.Token
	if first {
		list = fields.Detail.List
		token = fields.Token.Value
	}
	if !scalar(token, 32768) || len(list) == 0 || list[0] != '[' {
		return nil, "", failure(CodeIncomplete)
	}
	var entries []struct {
		ID    numericID `json:"fid"`
		Name  string    `json:"file_name"`
		Size  *int64    `json:"size"`
		Type  numericID `json:"file_type"`
		Token string    `json:"share_fid_token"`
	}
	if json.Unmarshal(list, &entries) != nil || len(entries) > 100 {
		return nil, "", failure(CodeInvalid)
	}
	out := []shareFile{}
	for _, f := range entries {
		if !scalar(string(f.ID), 4096) || !filename(f.Name) || f.Type != "0" && f.Type != "1" || f.Type == "1" && (f.Size == nil || *f.Size < 0) || f.Token != "" && !scalar(f.Token, 32768) {
			return nil, "", failure(CodeInvalid)
		}
		size := int64(0)
		if f.Size != nil {
			size = *f.Size
		}
		out = append(out, shareFile{ID: string(f.ID), Token: f.Token, Name: f.Name, Dir: f.Type == "0", Size: size})
	}
	return out, token, nil
}
func (d *Client) InspectShare(ctx context.Context, raw string) (cloud.ShareSnapshot, error) {
	id, pass, e := parseShare(raw)
	if e != nil {
		return cloud.ShareSnapshot{}, e
	}
	return d.inspectShareDirectory(ctx, sharePrivate{Share: id, Passcode: pass, Parent: "0"}, true)
}

// InspectShareDirectory keeps share IDs in their own namespace. Authentication
// is refreshed anonymously; no account session is sent to this endpoint.
func (d *Client) InspectShareDirectory(ctx context.Context, raw, entryID string) (cloud.ShareSnapshot, error) {
	root, e := d.InspectShare(ctx, raw)
	if e != nil || entryID == "0" || entryID == "" {
		return root, e
	}
	var p sharePrivate
	if json.Unmarshal(root.Private, &p) != nil {
		return cloud.ShareSnapshot{}, failure(CodeInvalid)
	}
	p.Parent, e = parseID(entryID, Route("share"), p.Share, false)
	if e != nil {
		return cloud.ShareSnapshot{}, e
	}
	return d.inspectShareDirectory(ctx, p, false)
}
func (d *Client) inspectShareDirectory(ctx context.Context, p sharePrivate, first bool) (cloud.ShareSnapshot, error) {
	seen := map[string]bool{}
	p.Files = nil
	for page := 1; page <= 20; page++ {
		files, token, e := d.sharePage(ctx, p, page, first && page == 1)
		if e != nil {
			return cloud.ShareSnapshot{}, e
		}
		p.Token = token
		for _, f := range files {
			if seen[f.ID] {
				return cloud.ShareSnapshot{}, failure(CodeIncomplete)
			}
			seen[f.ID] = true
			p.Files = append(p.Files, f)
		}
		if len(files) < 100 {
			p.Complete = true
			break
		}
	}
	if !p.Complete {
		return cloud.ShareSnapshot{}, failure("quark_share_too_large")
	}
	out := cloud.ShareSnapshot{ShareCode: p.Share, ReceiveCode: p.Passcode, Items: []cloud.ShareItem{}}
	for _, f := range p.Files {
		out.Items = append(out.Items, cloud.ShareItem{ID: QualifyID(Route("share"), p.Share, f.ID), Name: f.Name, IsDir: f.Dir, Size: f.Size})
	}
	out.Private, _ = json.Marshal(p)
	return out, nil
}
func selectedShare(snapshot *cloud.ShareSnapshot) (sharePrivate, error) {
	var p sharePrivate
	if snapshot == nil || len(snapshot.Items) == 0 || len(snapshot.Items) > 500 || len(snapshot.Private) > 4*1024*1024 || json.Unmarshal(snapshot.Private, &p) != nil || !p.Complete || p.Share != snapshot.ShareCode || !scalar(p.Token, 32768) {
		return p, failure("quark_share_invalid")
	}
	selected := []shareFile{}
	seen := map[string]bool{}
	for _, item := range snapshot.Items {
		found := false
		for _, f := range p.Files {
			if item.ID == QualifyID(Route("share"), p.Share, f.ID) {
				if f.Dir || item.IsDir || item.Name != f.Name || item.Size != f.Size || !scalar(f.Token, 32768) || seen[f.ID] {
					return p, failure("quark_share_selection_invalid")
				}
				seen[f.ID] = true
				selected = append(selected, f)
				found = true
				break
			}
		}
		if !found {
			return p, failure("quark_share_selection_invalid")
		}
	}
	p.Files = selected
	return p, nil
}
func (d *Client) receiveSelected(ctx context.Context, a Authorization, snapshot *cloud.ShareSnapshot, parent string) (json.RawMessage, error) {
	selected, e := selectedShare(snapshot)
	if e != nil {
		return nil, e
	}
	fresh, e := d.inspectShareDirectory(ctx, sharePrivate{Share: selected.Share, Passcode: selected.Passcode, Token: selected.Token, Parent: selected.Parent}, false)
	if e != nil {
		return nil, e
	}
	var p sharePrivate
	if json.Unmarshal(fresh.Private, &p) != nil {
		return nil, failure(CodeInvalid)
	}
	ids, tokens := []string{}, []string{}
	for _, wanted := range selected.Files {
		found := false
		for _, f := range p.Files {
			if f.ID == wanted.ID && f.Name == wanted.Name && f.Size == wanted.Size && !f.Dir && scalar(f.Token, 32768) {
				ids = append(ids, f.ID)
				tokens = append(tokens, f.Token)
				found = true
				break
			}
		}
		if !found {
			return nil, failure("quark_share_changed")
		}
	}
	return d.api.agent(ctx, a, http.MethodPost, "/open/v1/share/saveas", nil, map[string]any{"pwd_id": p.Share, "stoken": p.Token, "to_pdir_fid": parent, "fid_list": ids, "fid_token_list": tokens})
}
func (d *Client) confirmReceived(ctx context.Context, r cloud.OperationReceipt, p operationPayload) (cloud.OperationResult, error) {
	a, e := d.authorization(ctx, RouteAgent)
	if e != nil {
		return cloud.OperationResult{Receipt: r}, e
	}
	parent, e := parseID(p.Request.ParentID, RouteAgent, a.AccountID, false)
	if e != nil {
		return cloud.OperationResult{Receipt: r}, e
	}
	selected, e := selectedShare(p.Request.Share)
	if e != nil {
		return cloud.OperationResult{Receipt: r}, e
	}
	files, e := d.agentDirectory(ctx, parent)
	if e != nil {
		return cloud.OperationResult{Receipt: r}, e
	}
	before := map[string]agentItem{}
	for _, f := range p.TargetBefore {
		before[agentKey(f.ID)] = f
	}
	for _, old := range p.TargetBefore {
		found := false
		for _, f := range files {
			if sameAgent(f.ID, old.ID) && f.Name == old.Name && f.Size == old.Size && f.Dir == old.Dir {
				found = true
				break
			}
		}
		if !found {
			return cloud.OperationResult{Receipt: r}, failure(CodeIdentity)
		}
	}
	outputs := []cloud.Item{}
	for _, wanted := range selected.Files {
		matches := 0
		for _, f := range files {
			_, existed := before[agentKey(f.ID)]
			if !existed && f.Name == wanted.Name && f.Size == wanted.Size && !f.Dir {
				matches++
				outputs = append(outputs, toCloudAgent(a.AccountID, f))
			}
		}
		if matches != 1 {
			return cloud.OperationResult{Receipt: r}, failure(CodePending)
		}
	}
	if len(files) != len(p.TargetBefore)+len(outputs) {
		return cloud.OperationResult{Receipt: r}, failure(CodeIdentity)
	}
	p.Outputs = outputs
	if e = d.saveReceipt(ctx, &r, p, "confirmed"); e != nil {
		return cloud.OperationResult{Receipt: r}, e
	}
	return cloud.OperationResult{Receipt: r, Items: outputs, Complete: true}, nil
}
func (d *Client) ReceiveShare(ctx context.Context, s cloud.ShareSnapshot, parent string) error {
	r, e := d.BeginOperation(ctx, cloud.OperationRequest{ID: operationID(ctx), Kind: "receive_share", ParentID: parent, Share: &s})
	if e == nil && !r.Complete {
		return failure(CodePending)
	}
	return e
}

var _ cloud.ShareReceiveDriver = (*Client)(nil)
var _ cloud.ShareBrowseDriver = (*Client)(nil)
