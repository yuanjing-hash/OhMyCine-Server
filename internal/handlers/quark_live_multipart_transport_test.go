//go:build quark_live

package handlers_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/yuanjing-hash/OhMyCine-Server/pkg/cloud/quark"
)

type multipartGuard struct {
	mu         sync.Mutex
	s          *multipartStore
	delay      time.Duration
	lastKind   string
	lastStatus int
}
type multipartTransport struct {
	base  http.RoundTripper
	g     *multipartGuard
	media bool
}

func newMultipartGuard(s *multipartStore) *multipartGuard {
	i := s.state.Identity
	if id, ok := followRawID(i.Proof.TVRoot, quark.RouteTV, i.TVAccount); ok {
		s.state.TVParents[id] = true
	}
	if id, ok := followRawID(i.Proof.AgentRoot, quark.RouteAgent, i.AgentAccount); ok {
		s.state.AgentParents[id] = true
		s.state.AgentIDs[id] = true
	}
	s.state.AgentIDs[i.ProofRootACK] = true
	return &multipartGuard{s: s, delay: 2 * time.Second}
}
func (g *multipartGuard) wrap(base http.RoundTripper, media bool) http.RoundTripper {
	if t, ok := base.(*http.Transport); ok {
		c := t.Clone()
		c.MaxResponseHeaderBytes = 64 << 10
		base = c
	}
	return multipartTransport{base, g, media}
}
func multipartKnown(ids map[string]bool, id string) bool {
	for k := range ids {
		if quark.ObservationSameAgentHandle(k, id) {
			return true
		}
	}
	return false
}
func multipartURL(s string) bool {
	u, e := url.Parse(s)
	return e == nil && u.Scheme == "https" && u.Host != "" && u.User == nil && u.Fragment == ""
}
func multipartDecode(s *multipartStore) (multipartPayload, bool) {
	var p multipartPayload
	r, ok := s.state.Operations[s.state.UploadID]
	e := json.Unmarshal(r.Payload, &p)
	return p, ok && e == nil
}
func (g *multipartGuard) valid(p multipartPayload) bool {
	return p.Name == g.s.state.UploadName && p.Size == multipartStateSize(g.s.state) && p.SHA256 == g.s.state.SourceSHA && g.s.state.ChildAgent != "" && quark.ObservationSameAgentHandle(p.Parent, g.s.state.ChildAgent)
}

type multipartCountReader struct {
	r io.Reader
	n int64
}

func (c *multipartCountReader) Read(p []byte) (int, error) {
	n, e := c.r.Read(p)
	c.n += int64(n)
	return n, e
}

func (w multipartTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	g := w.g
	g.mu.Lock()
	defer g.mu.Unlock()
	s := g.s
	s.mu.Lock()
	deadline := s.state.Started.Add(multipartLifetime)
	s.mu.Unlock()
	if perRequest := time.Now().Add(60 * time.Second); perRequest.Before(deadline) {
		deadline = perRequest
	}
	ctx, cancel := context.WithDeadline(request.Context(), deadline)
	defer cancel()
	r := request.Clone(ctx)
	g.lastKind = "rejected"
	g.lastStatus = 0
	if ctx.Err() != nil || r.URL == nil || !multipartURL(r.URL.String()) {
		return nil, multipartError()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	st := &s.state
	size := multipartStateSize(*st)
	metadataBudget := multipartMetadataBudget(*st)
	if size == 0 || metadataBudget == 0 {
		return nil, multipartError()
	}
	p, hasUpload := multipartDecode(s)
	var body []byte
	if r.Body != nil {
		limit := int64(256 << 10)
		if w.media && r.Method == "PUT" {
			limit = size
		}
		var e error
		body, e = io.ReadAll(io.LimitReader(r.Body, limit+1))
		r.Body.Close()
		if e != nil || int64(len(body)) > limit {
			return nil, multipartError()
		}
		r.Body = io.NopCloser(bytes.NewReader(body))
	}
	var input map[string]json.RawMessage
	_ = json.Unmarshal(body, &input)
	str := func(k string) string { var v string; _ = json.Unmarshal(input[k], &v); return v }
	kind, key := "api", ""
	allowed := false
	var amount int64
	partNumber := 0
	if w.media {
		for _, a := range []*quark.Authorization{st.Bundle.TV, st.Bundle.Agent} {
			if a != nil && a.AccessToken != "" {
				for _, values := range r.Header {
					for _, v := range values {
						if strings.Contains(v, a.AccessToken) {
							return nil, multipartError()
						}
					}
				}
				if strings.Contains(r.URL.String(), a.AccessToken) {
					return nil, multipartError()
				}
			}
		}
		if r.Header.Get("Cookie") != "" || r.Header.Get("Referer") != "" || r.Header.Get("Proxy-Authorization") != "" || r.Header.Get("X-Pan-Token") != "" || r.Header.Get("X-Agent-ID") != "" || r.Header.Get("Range") != "" || r.Header.Get("Accept-Encoding") != "identity" {
			return nil, multipartError()
		}
		if r.Method == "GET" {
			amount = st.ReadURLs[r.URL.String()]
			allowed = amount == size && r.Header.Get("Authorization") == ""
			kind = "read"
		}
		if r.Method == "PUT" && hasUpload && g.valid(p) && st.Eligible && p.Phase == "put_inflight" {
			count, valid := multipartPlan(p)
			n := p.InFlightPart
			dest, found := st.Destinations[n]
			if valid && found && n >= 1 && n <= count && dest.Number == n && dest.URL == r.URL.String() && dest.Signature.Value != "" && r.Header.Get("Authorization") == dest.Signature.Value {
				offset := int64(n-1) * p.PartSize
				amount = min(p.PartSize, p.Size-offset)
				if r.ContentLength == amount && int64(len(body)) == amount {
					f, e := os.Open(filepath.Join(s.dir, "sample.bin"))
					if e != nil {
						return nil, multipartError()
					}
					expected, e := io.ReadAll(io.NewSectionReader(f, offset, amount))
					f.Close()
					allowed = e == nil && int64(len(expected)) == amount && bytes.Equal(body, expected)
					kind = "put"
					key = fmt.Sprintf("part:%d", n)
					partNumber = n
				}
			}
		}
	} else if r.URL.Host == "open-api-drive.quark.cn" {
		switch r.URL.Path {
		case "/user":
			allowed = r.Method == "GET" && r.URL.Query().Get("method") == "user_info"
		case "/open/v1/user/info":
			allowed = r.Method == "GET"
		case "/open/v1/file/info":
			allowed = r.Method == "GET" && multipartKnown(st.AgentIDs, r.URL.Query().Get("fid"))
		case "/open/v1/file/list":
			var size int
			_ = json.Unmarshal(input["size"], &size)
			allowed = st.RootVerified && r.Method == "POST" && size > 0 && size <= 100 && len(input["query_cursor"]) == 0 && (str("parent_fid") == "0" || multipartKnown(st.AgentParents, str("parent_fid")))
		case "/file":
			if st.RootVerified && r.Method == "GET" {
				switch r.URL.Query().Get("method") {
				case "list":
					n, e := strconv.Atoi(r.URL.Query().Get("page_size"))
					allowed = e == nil && n > 0 && n <= 250 && r.URL.Query().Get("page_index") == "0" && (r.URL.Query().Get("parent_fid") == "0" || st.TVParents[r.URL.Query().Get("parent_fid")])
				case "download":
					allowed = st.TVFiles[r.URL.Query().Get("fid")]
				}
			}
		case "/open/v1/dir":
			var op observationMutation
			receipt, ok := st.Operations[st.DirectoryID]
			root, _ := followRawID(st.Identity.Proof.AgentRoot, quark.RouteAgent, st.Identity.AgentAccount)
			allowed = st.RootVerified && r.Method == "POST" && ok && receipt.State == "submitting" && json.Unmarshal(receipt.Payload, &op) == nil && op.Request.Name == st.DirectoryName && str("dir_path") == st.DirectoryName && quark.ObservationSameAgentHandle(str("pdir_fid"), root)
			kind = "mkdir"
			key = "mkdir"
		case "/open/v1/task/query":
			var op struct {
				TaskID   string `json:"task_id"`
				Finished bool   `json:"finished"`
			}
			receipt, ok := st.Operations[st.DirectoryID]
			allowed = ok && receipt.Kind == "mkdir" && receipt.State == "accepted" && json.Unmarshal(receipt.Payload, &op) == nil && op.TaskID != "" && !op.Finished && r.Method == "POST" && str("task_id") == op.TaskID
		case "/open/v1/file/upload_pre":
			var size int64
			_ = json.Unmarshal(input["size"], &size)
			var parallel bool
			_ = json.Unmarshal(input["parallel_upload"], &parallel)
			allowed = st.RootVerified && r.Method == "POST" && hasUpload && g.valid(p) && p.Phase == "pre_inflight" && str("file_name") == p.Name && size == p.Size && str("pdir_fid") == p.Parent && str("sha1") == p.SHA1 && str("md5") == p.MD5 && parallel && len(input["part_info_list"]) == 0
			kind = "pre"
			key = "pre"
		case "/open/v1/file/get_upload_urls":
			var info []struct {
				Number  int   `json:"part_number"`
				Size    int64 `json:"part_size"`
				Context struct {
					Offset int64 `json:"part_offset"`
				} `json:"parallel_sha1_ctx"`
			}
			_ = json.Unmarshal(input["part_info_list"], &info)
			count, valid := multipartPlan(p)
			if len(info) == 1 {
				n := info[0].Number
				offset := int64(n-1) * p.PartSize
				allowed = st.Eligible && hasUpload && g.valid(p) && valid && r.Method == "POST" && str("task_id") == p.TaskID && p.TaskID != "" && n >= 1 && n <= count && info[0].Size == min(p.PartSize, p.Size-offset) && (n == 1 || info[0].Context.Offset == offset) && !st.Writes[fmt.Sprintf("part:%d", n)]
				partNumber = n
			}
			kind = "urls"
		case "/open/v1/file/list_uploaded_parts":
			allowed = st.Eligible && hasUpload && g.valid(p) && r.Method == "POST" && str("task_id") == p.TaskID && p.TaskID != "" && p.InFlightPart > 0 && st.Writes[fmt.Sprintf("part:%d", p.InFlightPart)]
			kind = "query"
			partNumber = p.InFlightPart
		case "/open/v1/file/upload_finish":
			var parts []multipartPart
			_ = json.Unmarshal(input["part_info_list"], &parts)
			count, valid := multipartPlan(p)
			allowed = st.Eligible && hasUpload && g.valid(p) && valid && r.Method == "POST" && p.Phase == "finish_inflight" && str("task_id") == p.TaskID && p.TaskID != "" && len(parts) == count && len(p.Parts) == count
			for n, part := range parts {
				if part.Number != n+1 || part.ETag == "" || st.ETags[n+1] != part.ETag || !st.Writes[fmt.Sprintf("part:%d", n+1)] {
					allowed = false
				}
			}
			kind = "finish"
			key = "finish"
		}
	}
	if !allowed || s.poisoned || st.Completed || st.Stopped != "" || !time.Now().Before(st.Started.Add(multipartLifetime)) || st.Requests >= 128 || !w.media && st.API >= 112 || key != "" && st.Writes[key] || kind == "mkdir" && st.Mkdir >= 1 || kind == "pre" && st.Pre >= 1 || kind == "finish" && st.Finish >= 1 || kind == "put" && (st.Put >= 8 || st.UploadReserved+amount > size) || kind == "read" && (st.Reads >= 3 || st.ReadReserved+amount > 3*size) || kind == "query" && st.Queries >= 2 {
		return nil, multipartError()
	}
	if wait := time.Until(st.LastDispatch.Add(g.delay)); wait > 0 {
		timer := time.NewTimer(wait)
		defer timer.Stop()
		select {
		case <-ctx.Done():
			return nil, multipartError()
		case <-timer.C:
		}
	}
	if ctx.Err() != nil || !time.Now().Before(st.Started.Add(multipartLifetime)) {
		return nil, multipartError()
	}
	st.Requests++
	if !w.media {
		st.API++
	}
	st.LastDispatch = time.Now().UTC()
	if key != "" {
		st.Writes[key] = true
	}
	switch kind {
	case "mkdir":
		st.Mkdir++
	case "pre":
		st.Pre++
	case "finish":
		st.Finish++
	case "query":
		st.Queries++
	case "put":
		st.Put++
		st.UploadReserved += amount
	case "read":
		st.Reads++
		st.ReadReserved += amount
	}
	if s.save() != nil {
		return nil, multipartError()
	}
	g.lastKind = kind
	counter := &multipartCountReader{r: bytes.NewReader(body)}
	if r.Body != nil {
		r.Body = io.NopCloser(counter)
	}
	resp, e := w.base.RoundTrip(r)
	if kind == "put" {
		st.UploadActual += counter.n
		if st.UploadActual > st.UploadReserved {
			s.poisoned = true
			return nil, multipartError()
		}
	}
	if e != nil {
		_ = s.save()
		return nil, multipartError()
	}
	defer resp.Body.Close()
	g.lastStatus = resp.StatusCode
	if resp.StatusCode < 200 || resp.StatusCode >= 300 || resp.Header.Get("Content-Encoding") != "" && resp.Header.Get("Content-Encoding") != "identity" {
		_ = s.save()
		return nil, multipartError()
	}
	headerBytes := 0
	for k, v := range resp.Header {
		headerBytes += len(k)
		for _, x := range v {
			headerBytes += len(x)
		}
	}
	if headerBytes > 64<<10 {
		return nil, multipartError()
	}
	limit := int64(256 << 10)
	if kind == "read" {
		limit = amount
		if resp.ContentLength != amount {
			return nil, multipartError()
		}
	} else {
		if resp.ContentLength >= 0 {
			if resp.ContentLength > limit {
				return nil, multipartError()
			}
			limit = resp.ContentLength
		}
		// The extra byte detects oversized/misdeclared bodies and is part of
		// the durable metadata budget even for a declared empty response.
		reserve := limit + 1
		if st.Metadata+reserve > metadataBudget {
			return nil, multipartError()
		}
		st.Metadata += reserve
		if s.save() != nil {
			return nil, multipartError()
		}
	}
	// A declared exact original-file length bounds media consumption without an
	// extra sentinel byte outside the pre-reserved 3N budget.
	var b []byte
	if kind == "read" {
		b, e = io.ReadAll(io.LimitReader(resp.Body, limit))
		st.ReadActual += int64(len(b))
		if e != nil || int64(len(b)) != limit {
			_ = s.save()
			return nil, multipartError()
		}
		h := sha256.Sum256(b)
		if hex.EncodeToString(h[:]) != st.SourceSHA {
			_ = s.save()
			return nil, multipartError()
		}
	} else {
		b, e = io.ReadAll(io.LimitReader(resp.Body, limit+1))
		if e != nil || int64(len(b)) > limit {
			return nil, multipartError()
		}
	}
	if kind == "put" {
		etag := resp.Header.Get("ETag")
		if !liveTVScalar(etag, 1024) {
			return nil, multipartError()
		}
		st.ETags[partNumber] = etag
	}
	if !w.media {
		var env struct {
			Status int             `json:"status"`
			Data   json.RawMessage `json:"data"`
		}
		if json.Unmarshal(b, &env) != nil {
			return nil, multipartError()
		}
		if env.Status == 0 {
			if kind == "pre" {
				st.PreResponse = append(json.RawMessage(nil), env.Data...)
				// Preserve the authenticated acknowledgement even if its geometry
				// or destinations are outside this experiment's eligible bounds.
				if s.save() != nil {
					return nil, multipartError()
				}
			}
			if kind == "pre" || kind == "urls" {
				var data struct {
					URLs []multipartDestination `json:"upload_urls"`
				}
				if json.Unmarshal(env.Data, &data) != nil {
					return nil, multipartError()
				}
				for _, d := range data.URLs {
					n := d.Number
					if kind == "pre" && n == 0 && len(data.URLs) == 1 {
						n = 1
						d.Number = n
					}
					if n < 1 || n > 8 || !multipartURL(d.URL) || !liveTVScalar(d.Signature.Value, 8192) || kind == "urls" && n != partNumber {
						if kind == "pre" {
							continue
						} // native receipt/gate must see the real pre
						return nil, multipartError()
					}
					st.Destinations[n] = d
				}
			}
			if kind == "query" {
				var data struct {
					Parts []multipartPart `json:"uploaded_parts"`
				}
				if json.Unmarshal(env.Data, &data) != nil {
					return nil, multipartError()
				}
				for _, part := range data.Parts {
					if part.Number == partNumber && liveTVScalar(part.ETag, 1024) {
						st.ETags[partNumber] = part.ETag
					}
				}
			}
			if e = g.learn(r, env.Data, input, kind); e != nil {
				return nil, e
			}
		}
	}
	if s.save() != nil {
		return nil, multipartError()
	}
	resp.Body = io.NopCloser(bytes.NewReader(b))
	return resp, nil
}
func (g *multipartGuard) learn(r *http.Request, data json.RawMessage, input map[string]json.RawMessage, kind string) error {
	st := &g.s.state
	rootAgent, _ := followRawID(st.Identity.Proof.AgentRoot, quark.RouteAgent, st.Identity.AgentAccount)
	rootTV, _ := followRawID(st.Identity.Proof.TVRoot, quark.RouteTV, st.Identity.TVAccount)
	var parent string
	_ = json.Unmarshal(input["parent_fid"], &parent)
	if r.URL.Path == "/open/v1/file/list" {
		var out struct {
			Files []struct {
				ID   string `json:"fid"`
				Name string `json:"filename"`
				Type string `json:"file_type"`
				Size int64  `json:"size"`
			} `json:"file_list"`
		}
		if json.Unmarshal(data, &out) != nil {
			return multipartError()
		}
		for _, f := range out.Files {
			if multipartKnown(map[string]bool{rootAgent: true}, parent) && f.Name == st.DirectoryName && f.Type == "0" {
				st.AgentParents[f.ID] = true
				st.AgentIDs[f.ID] = true
			}
			if st.ChildAgent != "" && quark.ObservationSameAgentHandle(parent, st.ChildAgent) && f.Name == st.UploadName && f.Type == "1" && f.Size == multipartStateSize(*st) {
				st.AgentIDs[f.ID] = true
			}
		}
	}
	if r.URL.Path == "/file" && r.URL.Query().Get("method") == "list" {
		var out struct {
			Files []struct {
				ID   string `json:"fid"`
				Name string `json:"filename"`
				Dir  int    `json:"isdir"`
				Size int64  `json:"size"`
			} `json:"files"`
		}
		if json.Unmarshal(data, &out) != nil {
			return multipartError()
		}
		parent = r.URL.Query().Get("parent_fid")
		for _, f := range out.Files {
			if parent == rootTV && f.Name == st.DirectoryName && f.Dir == 1 {
				st.TVParents[f.ID] = true
			}
			if st.ChildTV != "" && parent == st.ChildTV && f.Name == st.UploadName && f.Dir == 0 && f.Size == multipartStateSize(*st) {
				st.TVFiles[f.ID] = true
			}
		}
	}
	if r.URL.Path == "/file" && r.URL.Query().Get("method") == "download" {
		var out struct {
			URL  string `json:"download_url"`
			Size int64  `json:"size"`
		}
		if json.Unmarshal(data, &out) != nil || out.Size != multipartStateSize(*st) || !multipartURL(out.URL) {
			return multipartError()
		}
		st.ReadURLs[out.URL] = out.Size
	}
	if kind == "pre" || kind == "finish" || kind == "mkdir" {
		var out struct {
			ID string `json:"fid"`
		}
		_ = json.Unmarshal(data, &out)
		if out.ID != "" {
			st.AgentIDs[out.ID] = true
			if kind == "mkdir" {
				st.AgentParents[out.ID] = true
			}
		}
	}
	if r.URL.Path == "/open/v1/file/info" {
		var f struct {
			ID     string `json:"fid"`
			Name   string `json:"filename"`
			Parent string `json:"parent_fid"`
			Type   string `json:"file_type"`
			Size   int64  `json:"size"`
		}
		_ = json.Unmarshal(data, &f)
		p, ok := multipartDecode(g.s)
		if ok && g.valid(p) && p.Handle != "" && r.URL.Query().Get("fid") == p.Handle && quark.ObservationSameAgentHandle(f.ID, p.Handle) && f.Name == p.Name && f.Size == p.Size && f.Type == "1" && liveTVScalar(f.Parent, 4096) && f.Parent != "0" {
			st.AgentIDs[f.Parent] = true
		}
	}
	return nil
}
