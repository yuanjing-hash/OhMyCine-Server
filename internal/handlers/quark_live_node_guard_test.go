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
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/yuanjing-hash/OhMyCine-Server/pkg/cloud/quark"
)

type nodeProbeTransport struct {
	g     *nodeProbeGuard
	base  http.RoundTripper
	media bool
}

func (g *nodeProbeGuard) wrap(base http.RoundTripper, media bool) http.RoundTripper {
	if tr, ok := base.(*http.Transport); ok {
		copy := tr.Clone()
		copy.MaxResponseHeaderBytes = 64 << 10
		base = copy
	}
	return &nodeProbeTransport{g: g, base: base, media: media}
}
func nodeProbePartPlan(p largefilePayload) (int, bool) {
	if p.Size != nodeProbeSize || p.PartSize <= 0 || p.PartSize > 1<<30 || p.PartSize%64 != 0 {
		return 0, false
	}
	n := (p.Size + p.PartSize - 1) / p.PartSize
	return int(n), n >= 2 && n <= 8
}
func (g *nodeProbeGuard) payload(ctx context.Context) (largefilePayload, bool) {
	r, e := g.receipt(ctx, "upload")
	var p largefilePayload
	valid := e == nil && json.Unmarshal(r.Payload, &p) == nil && p.Name == g.state.Spec.UploadName && p.Size == nodeProbeSize && p.SHA256 == g.state.Spec.SourceSHA && g.state.ChildAgent != "" && quark.ObservationSameAgentHandle(p.Parent, g.state.ChildAgent)
	return p, valid
}

func (w *nodeProbeTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	g := w.g
	g.mu.Lock()
	release := true
	defer func() {
		if release {
			g.mu.Unlock()
		}
	}()
	s := &g.state
	requestLimit := 60 * time.Second
	if w.media {
		requestLimit = 5 * time.Minute
	}
	deadline := minTime(s.Spec.Started.Add(nodeProbeLifetime), time.Now().Add(requestLimit))
	ctx, cancel := context.WithDeadline(request.Context(), deadline)
	releaseCancel := true
	defer func() {
		if releaseCancel {
			cancel()
		}
	}()
	r := request.Clone(ctx)
	if r.URL == nil || !multipartURL(r.URL.String()) || r.URL.Opaque != "" || r.URL.RawPath != "" || r.Host != "" && r.Host != r.URL.Host || ctx.Err() != nil || s.Stopped != "" || !g.validState() {
		return nil, nodeProbeError()
	}
	p, hasUpload := g.payload(ctx)
	var body []byte
	if r.Body != nil && !(w.media && r.Method == http.MethodPut) {
		var e error
		body, e = io.ReadAll(io.LimitReader(r.Body, (256<<10)+1))
		r.Body.Close()
		if e != nil || len(body) > 256<<10 {
			return nil, nodeProbeError()
		}
		r.Body = io.NopCloser(bytes.NewReader(body))
	}
	var input map[string]json.RawMessage
	_ = json.Unmarshal(body, &input)
	str := func(k string) string { var v string; _ = json.Unmarshal(input[k], &v); return v }
	allowed := false
	kind, key := "api", ""
	var amount, offset int64
	part := 0
	if w.media {
		for _, a := range []*quark.Authorization{g.bundle.TV, g.bundle.Agent} {
			if a == nil {
				return nil, nodeProbeError()
			}
			if strings.Contains(r.URL.String(), a.AccessToken) {
				return nil, nodeProbeError()
			}
			for _, values := range r.Header {
				for _, v := range values {
					if strings.Contains(v, a.AccessToken) {
						return nil, nodeProbeError()
					}
				}
			}
		}
		if r.Header.Get("Cookie") != "" || r.Header.Get("Referer") != "" || r.Header.Get("Proxy-Authorization") != "" || r.Header.Get("X-Pan-Token") != "" || r.Header.Get("X-Agent-ID") != "" || r.Header.Get("Accept-Encoding") != "identity" {
			return nil, nodeProbeError()
		}
		if r.Method == http.MethodGet {
			start, end, ok := largefileRange(r.Header.Get("Range"), nodeProbeSize)
			amount = end - start + 1
			offset = start
			allowed = ok && s.ReadURLs[r.URL.String()] == nodeProbeSize && start == s.ReadOffset && !s.ReadInFlight && s.ReadPasses < 3 && r.Header.Get("Authorization") == ""
			kind = "read"
		}
		if r.Method == http.MethodPut && hasUpload && p.Phase == "put_inflight" {
			count, ok := nodeProbePartPlan(p)
			part = p.InFlightPart
			d, exists := s.Destinations[part]
			offset = int64(part-1) * p.PartSize
			amount = min(p.PartSize, nodeProbeSize-offset)
			allowed = ok && part >= 1 && part <= count && exists && d.URL == r.URL.String() && d.Signature.Value != "" && r.Header.Get("Authorization") == d.Signature.Value && r.Header.Get("Range") == "" && r.ContentLength == amount && r.Body != nil
			kind = "put"
			key = fmt.Sprintf("part:%d", part)
			if allowed {
				f, e := os.Open(filepath.Join(g.root, "source", "sample.bin"))
				if e != nil {
					return nil, nodeProbeError()
				}
				defer f.Close()
				r.Body = &largefileCompareBody{source: r.Body, expected: io.NewSectionReader(f, offset, amount), remaining: amount}
			}
		}
	} else if r.URL.Host == "open-api-drive.quark.cn" {
		q := r.URL.Query()
		auth := g.bundle.Agent
		if r.URL.Path == "/file" || r.URL.Path == "/user" {
			auth = g.bundle.TV
		}
		if auth == nil || q.Get("access_token") != auth.AccessToken || r.Header.Get("Cookie") != "" {
			return nil, nodeProbeError()
		}
		switch r.URL.Path {
		case "/user":
			allowed = r.Method == "GET" && q.Get("method") == "user_info"
		case "/open/v1/user/info":
			allowed = r.Method == "GET"
		case "/open/v1/file/info":
			allowed = r.Method == "GET" && multipartKnown(s.AgentIDs, q.Get("fid"))
		case "/open/v1/file/list":
			var page int
			_ = json.Unmarshal(input["size"], &page)
			allowed = r.Method == "POST" && page > 0 && page <= 100 && len(input["query_cursor"]) == 0 && (str("parent_fid") == "0" || multipartKnown(s.AgentParents, str("parent_fid")))
		case "/file":
			if r.Method == "GET" {
				if q.Get("method") == "list" {
					n, e := strconv.Atoi(q.Get("page_size"))
					allowed = e == nil && n > 0 && n <= 250 && q.Get("page_index") == "0" && (q.Get("parent_fid") == "0" || s.TVParents[q.Get("parent_fid")])
				} else if q.Get("method") == "download" {
					allowed = s.TVFiles[q.Get("fid")]
				}
			}
		case "/open/v1/dir":
			rc, e := g.receipt(ctx, "mkdir")
			var op observationMutation
			a, _ := followRawID(s.Spec.Identity.Proof.AgentRoot, quark.RouteAgent, s.Spec.Identity.AgentAccount)
			allowed = e == nil && rc.State == "submitting" && json.Unmarshal(rc.Payload, &op) == nil && op.Request.Name == s.Spec.DirectoryName && r.Method == "POST" && str("dir_path") == s.Spec.DirectoryName && quark.ObservationSameAgentHandle(str("pdir_fid"), a)
			kind, key = "mkdir", "mkdir"
		case "/open/v1/task/query":
			rc, e := g.receipt(ctx, "mkdir")
			var p struct {
				TaskID   string `json:"task_id"`
				Finished bool   `json:"finished"`
			}
			allowed = e == nil && rc.State == "accepted" && json.Unmarshal(rc.Payload, &p) == nil && !p.Finished && p.TaskID != "" && r.Method == "POST" && str("task_id") == p.TaskID
		case "/open/v1/file/upload_pre":
			var size int64
			var parallel bool
			_ = json.Unmarshal(input["size"], &size)
			_ = json.Unmarshal(input["parallel_upload"], &parallel)
			allowed = hasUpload && r.Method == "POST" && p.Phase == "pre_inflight" && str("file_name") == p.Name && size == p.Size && str("pdir_fid") == p.Parent && str("sha1") == p.SHA1 && str("md5") == p.MD5 && parallel && len(input["part_info_list"]) == 0
			kind, key = "pre", "pre"
		case "/open/v1/file/get_upload_urls":
			var list []struct {
				Number  int   `json:"part_number"`
				Size    int64 `json:"part_size"`
				Context struct {
					Offset int64 `json:"part_offset"`
				} `json:"parallel_sha1_ctx"`
			}
			_ = json.Unmarshal(input["part_info_list"], &list)
			count, ok := nodeProbePartPlan(p)
			if len(list) == 1 {
				part = list[0].Number
				offset = int64(part-1) * p.PartSize
				allowed = hasUpload && ok && r.Method == "POST" && p.TaskID != "" && str("task_id") == p.TaskID && part >= 1 && part <= count && list[0].Size == min(p.PartSize, nodeProbeSize-offset) && (part == 1 || list[0].Context.Offset == offset) && !s.Writes[fmt.Sprintf("part:%d", part)]
			}
			kind = "urls"
		case "/open/v1/file/list_uploaded_parts":
			allowed = hasUpload && r.Method == "POST" && p.TaskID != "" && str("task_id") == p.TaskID && p.InFlightPart > 0 && s.Writes[fmt.Sprintf("part:%d", p.InFlightPart)]
			kind = "query"
			part = p.InFlightPart
		case "/open/v1/file/upload_finish":
			var parts []largefilePart
			_ = json.Unmarshal(input["part_info_list"], &parts)
			count, ok := nodeProbePartPlan(p)
			allowed = hasUpload && ok && r.Method == "POST" && p.Phase == "finish_inflight" && p.TaskID != "" && str("task_id") == p.TaskID && len(parts) == count && len(p.Parts) == count
			for n, v := range parts {
				if v.Number != n+1 || v.ETag == "" || s.ETags[n+1] != v.ETag || !s.Writes[fmt.Sprintf("part:%d", n+1)] {
					allowed = false
				}
			}
			kind, key = "finish", "finish"
		}
	}
	if !allowed || s.Requests >= nodeProbeRequestBudget || !w.media && s.API >= nodeProbeAPIBudget || key != "" && s.Writes[key] || kind == "mkdir" && s.Mkdir >= 1 || kind == "pre" && s.Pre >= 1 || kind == "finish" && s.Finish >= 1 || kind == "query" && s.Queries >= 2 || kind == "put" && (s.Put >= 8 || s.UploadReserved+amount > nodeProbeSize) || kind == "read" && (s.Reads >= 6 || s.ReadReserved+amount > 3*nodeProbeSize) {
		return nil, nodeProbeError()
	}
	if wait := time.Until(s.LastDispatch.Add(g.delay)); wait > 0 {
		timer := time.NewTimer(wait)
		defer timer.Stop()
		select {
		case <-ctx.Done():
			return nil, nodeProbeError()
		case <-timer.C:
		}
	}
	if ctx.Err() != nil {
		return nil, nodeProbeError()
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
	case "mkdir":
		s.Mkdir++
	case "pre":
		s.Pre++
	case "finish":
		s.Finish++
	case "query":
		s.Queries++
	case "put":
		s.Put++
		s.UploadReserved += amount
	case "read":
		s.Reads++
		s.ReadReserved += amount
		s.ReadInFlight = true
		s.Detection++
	}
	if g.journal.save(*s) != nil {
		return nil, nodeProbeError()
	}
	counter := &largefileCountReader{}
	if r.Body != nil {
		counter.r = r.Body
		r.Body = io.NopCloser(counter)
	}
	resp, e := w.base.RoundTrip(r)
	releaseBody := true
	var originalBody io.ReadCloser
	if resp != nil {
		originalBody = resp.Body
	}
	defer func() {
		if releaseBody && originalBody != nil {
			originalBody.Close()
		}
	}()
	if kind == "put" {
		s.UploadActual += counter.n.Load()
		if s.UploadActual > s.UploadReserved || counter.n.Load() != amount {
			s.Stopped = "put_bytes_unknown"
			_ = g.journal.save(*s)
			return nil, nodeProbeError()
		}
	}
	if e != nil {
		_ = g.journal.save(*s)
		return nil, nodeProbeError()
	}
	if resp == nil || resp.Body == nil {
		return nil, nodeProbeError()
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 || resp.Header.Get("Content-Encoding") != "" && resp.Header.Get("Content-Encoding") != "identity" {
		_ = g.journal.save(*s)
		return nil, nodeProbeError()
	}
	if kind == "read" {
		if resp.StatusCode != 206 || resp.ContentLength != amount || resp.Header.Get("Content-Range") != fmt.Sprintf("bytes %d-%d/%d", offset, offset+amount-1, nodeProbeSize) {
			return nil, nodeProbeError()
		}
		h := sha256.New()
		if offset > 0 {
			if h.(encoding.BinaryUnmarshaler).UnmarshalBinary(s.ReadHash) != nil {
				return nil, nodeProbeError()
			}
		} else if len(s.ReadHash) != 0 {
			return nil, nodeProbeError()
		}
		f, e := os.Open(filepath.Join(g.root, "source", "sample.bin"))
		if e != nil {
			return nil, nodeProbeError()
		}
		resp.Body = &nodeProbeReadBody{g: g, body: &largefileCompareBody{source: resp.Body, expected: io.NewSectionReader(f, offset, amount), remaining: amount}, file: f, h: h, cancel: cancel, expected: amount}
		release, releaseCancel, releaseBody = false, false, false
		return resp, nil
	}
	limit := int64(256 << 10)
	if resp.ContentLength >= 0 {
		if resp.ContentLength > limit {
			return nil, nodeProbeError()
		}
		limit = resp.ContentLength
	}
	if s.Metadata+limit+1 > nodeProbeMetadataBudget {
		return nil, nodeProbeError()
	}
	s.Metadata += limit + 1
	if g.journal.save(*s) != nil {
		return nil, nodeProbeError()
	}
	data, e := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if e != nil || int64(len(data)) > limit {
		return nil, nodeProbeError()
	}
	if kind == "put" {
		if !liveTVScalar(resp.Header.Get("ETag"), 1024) {
			return nil, nodeProbeError()
		}
		s.ETags[part] = resp.Header.Get("ETag")
	}
	if !w.media && g.learn(r, data, input, kind, part) != nil {
		return nil, nodeProbeError()
	}
	if g.journal.save(*s) != nil {
		return nil, nodeProbeError()
	}
	resp.Body = io.NopCloser(bytes.NewReader(data))
	return resp, nil
}

func minTime(a, b time.Time) time.Time {
	if a.Before(b) {
		return a
	}
	return b
}

func (g *nodeProbeGuard) learn(r *http.Request, body []byte, input map[string]json.RawMessage, kind string, part int) error {
	s := &g.state
	var env struct {
		Status int             `json:"status"`
		Data   json.RawMessage `json:"data"`
	}
	if json.Unmarshal(body, &env) != nil {
		return nodeProbeError()
	}
	if env.Status != 0 {
		return nil
	}
	data := env.Data
	rootA, _ := followRawID(s.Spec.Identity.Proof.AgentRoot, quark.RouteAgent, s.Spec.Identity.AgentAccount)
	rootT, _ := followRawID(s.Spec.Identity.Proof.TVRoot, quark.RouteTV, s.Spec.Identity.TVAccount)
	if kind == "pre" || kind == "urls" {
		var out struct {
			TaskID   string                 `json:"task_id"`
			PartSize int64                  `json:"part_size"`
			Finish   json.RawMessage        `json:"finish"`
			URLs     []largefileDestination `json:"upload_urls"`
		}
		if json.Unmarshal(data, &out) != nil {
			return nodeProbeError()
		}
		if kind == "pre" {
			count, ok := nodeProbePartPlan(largefilePayload{Size: nodeProbeSize, PartSize: out.PartSize})
			if !ok || out.TaskID == "" || string(out.Finish) == "true" || string(out.Finish) == "1" {
				s.Stopped = "native_multipart_geometry_not_exercised"
			} else {
				s.PartCount = count
			}
		}
		for _, d := range out.URLs {
			if kind == "pre" && d.Number == 0 && len(out.URLs) == 1 {
				d.Number = 1
			}
			if d.Number < 1 || d.Number > 8 || kind == "urls" && d.Number != part || !multipartURL(d.URL) || !liveTVScalar(d.Signature.Value, 8192) {
				return nodeProbeError()
			}
			s.Destinations[d.Number] = d
		}
	}
	if kind == "mkdir" || kind == "pre" || kind == "finish" {
		var out struct {
			ID string `json:"fid"`
		}
		_ = json.Unmarshal(data, &out)
		if out.ID != "" {
			s.AgentIDs[out.ID] = true
			if kind == "mkdir" {
				s.AgentParents[out.ID] = true
			}
		}
	}
	if kind == "query" {
		var out struct {
			Parts []largefilePart `json:"uploaded_parts"`
		}
		if json.Unmarshal(data, &out) != nil {
			return nodeProbeError()
		}
		for _, p := range out.Parts {
			if p.Number == part && liveTVScalar(p.ETag, 1024) {
				s.ETags[p.Number] = p.ETag
			}
		}
	}
	if r.URL.Path == "/open/v1/file/list" {
		var parent string
		_ = json.Unmarshal(input["parent_fid"], &parent)
		var out struct {
			Files []struct {
				ID   string `json:"fid"`
				Name string `json:"filename"`
				Type string `json:"file_type"`
				Size int64  `json:"size"`
			} `json:"file_list"`
		}
		if json.Unmarshal(data, &out) != nil {
			return nodeProbeError()
		}
		for _, f := range out.Files {
			if quark.ObservationSameAgentHandle(parent, rootA) && f.Name == s.Spec.DirectoryName && f.Type == "0" {
				if s.ChildAgent != "" && !quark.ObservationSameAgentHandle(s.ChildAgent, f.ID) {
					return nodeProbeError()
				}
				s.ChildAgent = f.ID
				s.AgentParents[f.ID] = true
				s.AgentIDs[f.ID] = true
			}
			if s.ChildAgent != "" && quark.ObservationSameAgentHandle(parent, s.ChildAgent) && f.Name == s.Spec.UploadName && f.Type == "1" && f.Size == nodeProbeSize {
				s.AgentIDs[f.ID] = true
			}
		}
	}
	if r.URL.Path == "/file" && r.URL.Query().Get("method") == "list" {
		parent := r.URL.Query().Get("parent_fid")
		var out struct {
			Files []struct {
				ID   string `json:"fid"`
				Name string `json:"filename"`
				Dir  int    `json:"isdir"`
				Size int64  `json:"size"`
			} `json:"files"`
		}
		if json.Unmarshal(data, &out) != nil {
			return nodeProbeError()
		}
		for _, f := range out.Files {
			if parent == rootT && f.Name == s.Spec.DirectoryName && f.Dir == 1 {
				if s.ChildTV != "" && s.ChildTV != f.ID {
					return nodeProbeError()
				}
				s.ChildTV = f.ID
				s.TVParents[f.ID] = true
			}
			if s.ChildTV != "" && parent == s.ChildTV && f.Name == s.Spec.UploadName && f.Dir == 0 && f.Size == nodeProbeSize {
				s.TVFiles[f.ID] = true
			}
		}
	}
	if r.URL.Path == "/file" && r.URL.Query().Get("method") == "download" {
		var out struct {
			URL  string `json:"download_url"`
			Size int64  `json:"size"`
		}
		if json.Unmarshal(data, &out) != nil || out.Size != nodeProbeSize || !multipartURL(out.URL) {
			return nodeProbeError()
		}
		s.ReadURLs[out.URL] = out.Size
	}
	if r.URL.Path == "/open/v1/file/info" {
		var f struct {
			ID     string `json:"fid"`
			Parent string `json:"parent_fid"`
			Name   string `json:"filename"`
			Size   int64  `json:"size"`
		}
		_ = json.Unmarshal(data, &f)
		if s.ChildAgent != "" && f.Name == s.Spec.UploadName && f.Size == nodeProbeSize && f.Parent != "" && f.Parent != "0" {
			s.AgentIDs[f.Parent] = true
		}
	}
	return nil
}

type nodeProbeReadBody struct {
	mu             sync.Mutex
	once, shutdown sync.Once
	g              *nodeProbeGuard
	body           *largefileCompareBody
	file           *os.File
	h              hash.Hash
	cancel         context.CancelFunc
	expected, read int64
	eof, failed    bool
	closeErr       error
	shutdownErr    error
}

func (b *nodeProbeReadBody) Read(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.failed {
		return 0, nodeProbeError()
	}
	n, e := b.body.Read(p)
	if n > 0 {
		b.h.Write(p[:n])
		b.read += int64(n)
	}
	if e == io.EOF {
		b.eof = true
	} else if e != nil {
		b.failed = true
	}
	return n, e
}
func (b *nodeProbeReadBody) Close() error {
	b.shutdown.Do(func() { b.cancel(); b.shutdownErr = b.body.Close() })
	b.once.Do(func() {
		b.mu.Lock()
		defer b.mu.Unlock()
		defer b.g.mu.Unlock()
		defer b.file.Close()
		s := &b.g.state
		s.ReadActual += b.read
		if b.failed || b.shutdownErr != nil || !b.eof || b.read != b.expected {
			s.Stopped = "readback_interrupted"
			b.closeErr = nodeProbeError()
		} else {
			s.ReadOffset += b.read
			if s.ReadOffset == nodeProbeSize {
				if hex.EncodeToString(b.h.Sum(nil)) != s.Spec.SourceSHA {
					s.Stopped = "readback_hash_mismatch"
					b.closeErr = nodeProbeError()
				} else {
					s.ReadPasses++
					s.ReadOffset = 0
					s.ReadHash = nil
				}
			} else {
				s.ReadHash, _ = b.h.(encoding.BinaryMarshaler).MarshalBinary()
			}
			if b.closeErr == nil {
				s.ReadInFlight = false
			}
		}
		if b.g.journal.save(*s) != nil {
			b.closeErr = nodeProbeError()
		}
	})
	return b.closeErr
}
