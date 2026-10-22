//go:build quark_live

package handlers_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"hash"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/yuanjing-hash/OhMyCine-Server/pkg/cloud/quark"
)

type largefileGuard struct {
	mu         sync.Mutex
	s          *largefileStore
	delay      time.Duration
	lastKind   string
	lastStatus int
	progress   func(string)
}
type largefileTransport struct {
	base  http.RoundTripper
	g     *largefileGuard
	media bool
}

func newLargeFileGuard(s *largefileStore) *largefileGuard {
	i := s.state.Identity
	if id, ok := followRawID(i.Proof.TVRoot, quark.RouteTV, i.TVAccount); ok {
		s.state.TVParents[id] = true
	}
	if id, ok := followRawID(i.Proof.AgentRoot, quark.RouteAgent, i.AgentAccount); ok {
		s.state.AgentParents[id] = true
		s.state.AgentIDs[id] = true
	}
	s.state.AgentIDs[i.ProofRootACK] = true
	return &largefileGuard{s: s, delay: 2 * time.Second}
}
func (g *largefileGuard) wrap(base http.RoundTripper, media bool) http.RoundTripper {
	if t, ok := base.(*http.Transport); ok {
		c := t.Clone()
		c.MaxResponseHeaderBytes = 64 << 10
		base = c
	}
	return largefileTransport{base, g, media}
}
func largefileKnown(ids map[string]bool, id string) bool {
	for k := range ids {
		if quark.ObservationSameAgentHandle(k, id) {
			return true
		}
	}
	return false
}
func largefileURL(s string) bool {
	u, e := url.Parse(s)
	return len(s) <= 8192 && e == nil && u.Scheme == "https" && u.Host != "" && u.User == nil && u.Fragment == ""
}
func largefileDecode(s *largefileStore) (largefilePayload, bool) {
	var p largefilePayload
	r, ok := s.state.Operations[s.state.UploadID]
	e := json.Unmarshal(r.Payload, &p)
	return p, ok && e == nil
}
func (g *largefileGuard) valid(p largefilePayload) bool {
	return p.Name == g.s.state.UploadName && p.Size == largefileStateSize(g.s.state) && p.SHA256 == g.s.state.SourceSHA && g.s.state.ChildAgent != "" && quark.ObservationSameAgentHandle(p.Parent, g.s.state.ChildAgent)
}

type largefileCountReader struct {
	r io.Reader
	n atomic.Int64
}

func (c *largefileCountReader) Read(p []byte) (int, error) {
	n, e := c.r.Read(p)
	c.n.Add(int64(n))
	return n, e
}

func (w largefileTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	g := w.g
	g.mu.Lock()
	releaseGuard := true
	defer func() {
		if releaseGuard {
			g.mu.Unlock()
		}
	}()
	s := g.s
	s.mu.Lock()
	deadline := s.state.Started.Add(largefileLifetime)
	s.mu.Unlock()
	requestLimit := 60 * time.Second
	if w.media {
		requestLimit = 5 * time.Minute
	}
	if perRequest := time.Now().Add(requestLimit); perRequest.Before(deadline) {
		deadline = perRequest
	}
	ctx, cancel := context.WithDeadline(request.Context(), deadline)
	releaseCancel := true
	defer func() {
		if releaseCancel {
			cancel()
		}
	}()
	r := request.Clone(ctx)
	if r.Body != nil {
		defer r.Body.Close()
	}
	g.lastKind = "rejected"
	g.lastStatus = 0
	if ctx.Err() != nil || r.URL == nil || !largefileURL(r.URL.String()) {
		return nil, largefileError()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	st := &s.state
	size := largefileStateSize(*st)
	metadataBudget := largefileMetadataBudget(*st)
	if size == 0 || metadataBudget == 0 {
		return nil, largefileError()
	}
	p, hasUpload := largefileDecode(s)
	var body []byte
	if r.Body != nil && !(w.media && r.Method == "PUT") {
		limit := int64(256 << 10)
		var e error
		body, e = io.ReadAll(io.LimitReader(r.Body, limit+1))
		r.Body.Close()
		if e != nil || int64(len(body)) > limit {
			return nil, largefileError()
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
							return nil, largefileError()
						}
					}
				}
				if strings.Contains(r.URL.String(), a.AccessToken) {
					return nil, largefileError()
				}
			}
		}
		if r.Header.Get("Cookie") != "" || r.Header.Get("Referer") != "" || r.Header.Get("Proxy-Authorization") != "" || r.Header.Get("X-Pan-Token") != "" || r.Header.Get("X-Agent-ID") != "" || r.Header.Get("Accept-Encoding") != "identity" {
			return nil, largefileError()
		}
		if r.Method == "GET" {
			total := st.ReadURLs[r.URL.String()]
			start, end, valid := largefileRange(r.Header.Get("Range"), size)
			amount = end - start + 1
			allowed = total == size && valid && start == st.ReadOffset && !st.ReadInFlight && st.ReadPasses < 3 && r.Header.Get("Authorization") == ""
			kind = "read"
		}
		if r.Method == "PUT" && hasUpload && g.valid(p) && st.Eligible && p.Phase == "put_inflight" {
			count, valid := largefilePlan(p)
			n := p.InFlightPart
			dest, found := st.Destinations[n]
			if valid && found && n >= 1 && n <= count && dest.Number == n && dest.URL == r.URL.String() && dest.Signature.Value != "" && r.Header.Get("Authorization") == dest.Signature.Value {
				offset := int64(n-1) * p.PartSize
				amount = min(p.PartSize, p.Size-offset)
				if r.ContentLength == amount && r.Body != nil {
					f, e := os.Open(filepath.Join(s.dir, "sample.bin"))
					if e != nil {
						return nil, largefileError()
					}
					defer f.Close()
					r.Body = &largefileCompareBody{source: r.Body, expected: io.NewSectionReader(f, offset, amount), remaining: amount}
					allowed, kind, key, partNumber = true, "put", fmt.Sprintf("part:%d", n), n
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
			allowed = r.Method == "GET" && largefileKnown(st.AgentIDs, r.URL.Query().Get("fid"))
		case "/open/v1/file/list":
			var size int
			_ = json.Unmarshal(input["size"], &size)
			allowed = st.RootVerified && r.Method == "POST" && size > 0 && size <= 100 && len(input["query_cursor"]) == 0 && (str("parent_fid") == "0" || largefileKnown(st.AgentParents, str("parent_fid")))
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
			count, valid := largefilePlan(p)
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
			var parts []largefilePart
			_ = json.Unmarshal(input["part_info_list"], &parts)
			count, valid := largefilePlan(p)
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
	if !allowed || s.poisoned || st.Completed || st.Stopped != "" || !time.Now().Before(st.Started.Add(largefileLifetime)) || st.Requests >= largefileMaxRequests || !w.media && st.API >= largefileMaxAPI || key != "" && st.Writes[key] || kind == "mkdir" && st.Mkdir >= 1 || kind == "pre" && st.Pre >= 1 || kind == "finish" && st.Finish >= 1 || kind == "put" && (st.Put >= largefileMaxParts || st.UploadReserved+amount > size) || kind == "read" && (st.Reads >= largefileMaxReads || st.ReadReserved+amount > 3*size) || kind == "query" && st.Queries >= 2 {
		return nil, largefileError()
	}
	if wait := time.Until(st.LastDispatch.Add(g.delay)); wait > 0 {
		timer := time.NewTimer(wait)
		defer timer.Stop()
		select {
		case <-ctx.Done():
			return nil, largefileError()
		case <-timer.C:
		}
	}
	if ctx.Err() != nil || !time.Now().Before(st.Started.Add(largefileLifetime)) {
		return nil, largefileError()
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
		st.ReadInFlight = true
		st.ReadDetectionReserved++
	}
	if s.save() != nil {
		return nil, largefileError()
	}
	g.lastKind = kind
	counter := &largefileCountReader{}
	if r.Body != nil {
		counter.r = r.Body
		r.Body = io.NopCloser(counter)
	}
	resp, e := w.base.RoundTrip(r)
	if kind == "put" {
		st.UploadActual += counter.n.Load()
		if st.UploadActual > st.UploadReserved {
			s.poisoned = true
			return nil, largefileError()
		}
		if counter.n.Load() != amount {
			_ = s.save()
			return nil, largefileError()
		}
	}
	if e != nil {
		_ = s.save()
		return nil, largefileError()
	}
	releaseBody := true
	defer func() {
		if releaseBody {
			resp.Body.Close()
		}
	}()
	g.lastStatus = resp.StatusCode
	if resp.StatusCode < 200 || resp.StatusCode >= 300 || resp.Header.Get("Content-Encoding") != "" && resp.Header.Get("Content-Encoding") != "identity" {
		_ = s.save()
		return nil, largefileError()
	}
	headerBytes := 0
	for k, v := range resp.Header {
		headerBytes += len(k)
		for _, x := range v {
			headerBytes += len(x)
		}
	}
	if headerBytes > 64<<10 {
		return nil, largefileError()
	}
	limit := int64(256 << 10)
	if kind == "read" {
		limit = amount
		if resp.ContentLength != amount {
			return nil, largefileError()
		}
	} else {
		if resp.ContentLength >= 0 {
			if resp.ContentLength > limit {
				return nil, largefileError()
			}
			limit = resp.ContentLength
		}
		// The extra byte detects oversized/misdeclared bodies and is part of
		// the durable metadata budget even for a declared empty response.
		reserve := limit + 1
		if st.Metadata+reserve > metadataBudget {
			return nil, largefileError()
		}
		st.Metadata += reserve
		if s.save() != nil {
			return nil, largefileError()
		}
	}
	// A declared exact original-file length bounds media consumption without an
	// extra sentinel byte outside the pre-reserved 3N budget.
	var b []byte
	if kind == "read" {
		start, end, _ := largefileRange(r.Header.Get("Range"), size)
		if resp.StatusCode != 206 || resp.Header.Get("Content-Range") != fmt.Sprintf("bytes %d-%d/%d", start, end, size) {
			return nil, largefileError()
		}
		h := sha256.New()
		if start > 0 {
			if len(st.ReadHashState) == 0 || h.(encoding.BinaryUnmarshaler).UnmarshalBinary(st.ReadHashState) != nil {
				return nil, largefileError()
			}
		} else if len(st.ReadHashState) != 0 {
			return nil, largefileError()
		}
		f, err := os.Open(filepath.Join(s.dir, "sample.bin"))
		if err != nil {
			return nil, largefileError()
		}
		wrapped := &largefileReadBody{source: resp.Body, expected: io.NewSectionReader(f, start, amount), file: f, remaining: amount, h: h, s: s, g: g, cancel: cancel, ctx: ctx}
		resp.Body = wrapped
		// Read/Close now owns the live response and guard lock. The state mutex
		// is released by the ordinary defer; each bounded read takes it briefly.
		releaseGuard, releaseCancel, releaseBody = false, false, false
		return resp, nil
	}
	b, e = io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if e != nil || int64(len(b)) > limit {
		return nil, largefileError()
	}

	if kind == "put" {
		etag := resp.Header.Get("ETag")
		if !liveTVScalar(etag, 1024) {
			return nil, largefileError()
		}
		st.ETags[partNumber] = etag
	}
	if !w.media {
		var env struct {
			Status int             `json:"status"`
			Data   json.RawMessage `json:"data"`
		}
		if json.Unmarshal(b, &env) != nil {
			return nil, largefileError()
		}
		if env.Status == 0 {
			if kind == "pre" {
				st.PreResponse = append(json.RawMessage(nil), env.Data...)
				// Preserve the authenticated acknowledgement even if its geometry
				// or destinations are outside this experiment's eligible bounds.
				if s.save() != nil {
					return nil, largefileError()
				}
			}
			if kind == "pre" || kind == "urls" {
				var data struct {
					URLs []largefileDestination `json:"upload_urls"`
				}
				if json.Unmarshal(env.Data, &data) != nil {
					return nil, largefileError()
				}
				for _, d := range data.URLs {
					n := d.Number
					if kind == "pre" && n == 0 && len(data.URLs) == 1 {
						n = 1
						d.Number = n
					}
					if n < 1 || n > largefileMaxParts || !largefileURL(d.URL) || !liveTVScalar(d.Signature.Value, 8192) || kind == "urls" && n != partNumber {
						if kind == "pre" {
							continue
						} // native receipt/gate must see the real pre
						return nil, largefileError()
					}
					st.Destinations[n] = d
				}
			}
			if kind == "query" {
				var data struct {
					Parts []largefilePart `json:"uploaded_parts"`
				}
				if json.Unmarshal(env.Data, &data) != nil {
					return nil, largefileError()
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
		return nil, largefileError()
	}
	resp.Body = io.NopCloser(bytes.NewReader(b))
	if g.progress != nil && kind == "put" && st.Put%16 == 0 {
		g.progress("upload_parts")
	}
	return resp, nil
}
func (g *largefileGuard) learn(r *http.Request, data json.RawMessage, input map[string]json.RawMessage, kind string) error {
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
			return largefileError()
		}
		for _, f := range out.Files {
			if largefileKnown(map[string]bool{rootAgent: true}, parent) && f.Name == st.DirectoryName && f.Type == "0" {
				st.AgentParents[f.ID] = true
				st.AgentIDs[f.ID] = true
			}
			if st.ChildAgent != "" && quark.ObservationSameAgentHandle(parent, st.ChildAgent) && f.Name == st.UploadName && f.Type == "1" && f.Size == largefileStateSize(*st) {
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
			return largefileError()
		}
		parent = r.URL.Query().Get("parent_fid")
		for _, f := range out.Files {
			if parent == rootTV && f.Name == st.DirectoryName && f.Dir == 1 {
				st.TVParents[f.ID] = true
			}
			if st.ChildTV != "" && parent == st.ChildTV && f.Name == st.UploadName && f.Dir == 0 && f.Size == largefileStateSize(*st) {
				st.TVFiles[f.ID] = true
			}
		}
	}
	if r.URL.Path == "/file" && r.URL.Query().Get("method") == "download" {
		var out struct {
			URL  string `json:"download_url"`
			Size int64  `json:"size"`
		}
		if json.Unmarshal(data, &out) != nil || out.Size != largefileStateSize(*st) || !largefileURL(out.URL) {
			return largefileError()
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
		p, ok := largefileDecode(g.s)
		if ok && g.valid(p) && p.Handle != "" && r.URL.Query().Get("fid") == p.Handle && quark.ObservationSameAgentHandle(f.ID, p.Handle) && f.Name == p.Name && f.Size == p.Size && f.Type == "1" && liveTVScalar(f.Parent, 4096) && f.Parent != "0" {
			st.AgentIDs[f.Parent] = true
		}
	}
	return nil
}

// Only the native reader's sequential, complete 8 MiB windows are accepted.
// No ambiguous/multiple/open-ended Range or repeated window consumes a second
// reservation. Every complete pass has its own persisted rolling SHA-256.
func largefileRange(value string, total int64) (int64, int64, bool) {
	var start, end int64
	if _, err := fmt.Sscanf(value, "bytes=%d-%d", &start, &end); err != nil || start < 0 || start >= total || end != min(start+largefileReadChunk-1, total-1) || value != fmt.Sprintf("bytes=%d-%d", start, end) {
		return 0, 0, false
	}
	return start, end, true
}

// Verify each upload buffer before exposing it to the actual transport. The
// source and request remain streaming even when the provider negotiates a large
// part. Returning an error after a partial PUT preserves its unknown receipt.
type largefileCompareBody struct {
	source    io.ReadCloser
	expected  io.Reader
	remaining int64
}

func (r *largefileCompareBody) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	if r.remaining == 0 {
		var extra [1]byte
		n, err := r.source.Read(extra[:])
		if n != 0 || err != io.EOF {
			return 0, largefileError()
		}
		return 0, io.EOF
	}
	want := min(int64(len(p)), r.remaining, int64(32<<10))
	n, err := r.source.Read(p[:want])
	if n > 0 {
		var expected [32 << 10]byte
		if _, e := io.ReadFull(r.expected, expected[:n]); e != nil || !bytes.Equal(p[:n], expected[:n]) {
			return 0, largefileError()
		}
		r.remaining -= int64(n)
	}
	if err != nil && (err != io.EOF || r.remaining != 0) {
		return n, largefileError()
	}
	return n, err
}
func (r *largefileCompareBody) Close() error { return r.source.Close() }

type largefileReadBody struct {
	mu                  sync.Mutex
	source              io.ReadCloser
	expected            io.Reader
	file                *os.File
	remaining, consumed int64
	h                   hash.Hash
	s                   *largefileStore
	g                   *largefileGuard
	cancel              context.CancelFunc
	ctx                 context.Context
	closed, failed      bool
	eofVerified         bool
	detection           int64
	shutdown            sync.Once
	shutdownErr         error
	closeErr            error
}

func (r *largefileReadBody) Read(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed || r.failed || r.ctx.Err() != nil {
		r.failed = true
		return 0, largefileError()
	}
	if len(p) == 0 {
		return 0, nil
	}
	if r.remaining == 0 {
		if r.eofVerified {
			return 0, io.EOF
		}
		var extra [1]byte
		n, err := r.source.Read(extra[:])
		r.detection += int64(n)
		if n != 0 || err != io.EOF {
			r.failed = true
			return 0, largefileError()
		}
		r.eofVerified = true
		return 0, io.EOF
	}
	want := min(int64(len(p)), r.remaining, int64(32<<10))
	n, err := r.source.Read(p[:want])
	r.consumed += int64(n)
	if n > 0 {
		var expected [32 << 10]byte
		if _, e := io.ReadFull(r.expected, expected[:n]); e != nil || !bytes.Equal(p[:n], expected[:n]) {
			r.failed = true
			return 0, largefileError()
		}
		_, _ = r.h.Write(p[:n])
		r.remaining -= int64(n)
	}
	if err == io.EOF && r.remaining == 0 {
		r.eofVerified = true
	}
	if err != nil && (err != io.EOF || r.remaining != 0) {
		r.failed = true
		return n, largefileError()
	}
	return n, err
}

func (r *largefileReadBody) Close() error {
	// Abort I/O before waiting for a concurrent Read's state lock.
	r.shutdown.Do(func() { r.cancel(); r.shutdownErr = r.source.Close() })
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return r.closeErr
	}
	r.closed = true
	defer r.g.mu.Unlock()
	if r.shutdownErr != nil {
		r.failed = true
	}
	if r.file.Close() != nil {
		r.failed = true
	}
	r.s.mu.Lock()
	defer r.s.mu.Unlock()
	st := &r.s.state
	st.ReadActual += r.consumed
	st.ReadDetectionActual += r.detection
	if r.failed || !r.eofVerified || r.remaining != 0 || st.ReadActual > st.ReadReserved || st.ReadDetectionActual > st.ReadDetectionReserved || !st.ReadInFlight {
		st.Stopped = "read_incomplete_preserve_original_operation"
		r.closeErr = largefileError()
	} else {
		nextOffset := st.ReadOffset + r.consumed
		st.ReadHashState, r.closeErr = r.h.(encoding.BinaryMarshaler).MarshalBinary()
		if nextOffset == largefileSize {
			if hex.EncodeToString(r.h.Sum(nil)) != st.SourceSHA {
				st.Stopped = "read_digest_mismatch"
				r.closeErr = largefileError()
			} else {
				st.ReadPasses++
				st.ReadOffset, st.ReadHashState = 0, nil
			}
		} else {
			st.ReadOffset = nextOffset
		}
		if r.closeErr == nil {
			st.ReadInFlight = false
		}
	}
	if r.s.save() != nil {
		r.closeErr = largefileError()
	}
	if r.closeErr == nil && r.g.progress != nil && (st.ReadOffset == 0 || st.Reads%16 == 0) {
		r.g.progress("verify_readback")
	}
	return r.closeErr
}
