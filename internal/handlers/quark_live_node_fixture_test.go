//go:build quark_live

package handlers_test

import (
	"bytes"
	"crypto/md5"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/yuanjing-hash/OhMyCine-Server/pkg/cloud/quark"
)

const nodeProbeFixturePartSize int64 = 4 << 20
const nodeProbeFixtureDirectory = "opaque|listed-directory"
const nodeProbeFixtureFile = "opaque|listed-file"

// This is synthetic provider behavior, never an alternative cloud driver. It is
// only instantiated in the explicit offline child. All requests stay within
// that child's memory and all PUT bytes must match its generated local sample.
type nodeProbeFixture struct {
	mu                sync.Mutex
	spec              nodeProbeSpec
	data              []byte
	tvRoot, agentRoot string
	parts             map[int]string
	summary           nodeProbeFixtureSummary
	readOffset        int64
	readActive        bool
}

type nodeProbeFixtureSummary struct {
	Mkdir, Pre, Put, Finish, ReadRequests, ReadPasses, Rejected int
	UploadBytes, ReadBytes                                      int64
}

func newNodeProbeFixture(spec nodeProbeSpec, sourcePath string) (*nodeProbeFixture, error) {
	tv, e1 := followRawID(spec.Identity.Proof.TVRoot, quark.RouteTV, spec.Identity.TVAccount)
	agent, e2 := followRawID(spec.Identity.Proof.AgentRoot, quark.RouteAgent, spec.Identity.AgentAccount)
	if !spec.valid() || !e1 || !e2 || tv != "T" || agent != "A" || spec.Identity.Proof.Path != "/owned" || spec.Identity.ProofRootACK != "A" {
		return nil, errors.New("node_fixture_spec_invalid")
	}
	f, e := os.Open(sourcePath)
	if e != nil {
		return nil, errors.New("node_fixture_source_invalid")
	}
	defer f.Close()
	data, e := io.ReadAll(io.LimitReader(f, nodeProbeSize+1))
	sum := sha256.Sum256(data)
	if e != nil || int64(len(data)) != nodeProbeSize || hex.EncodeToString(sum[:]) != spec.SourceSHA {
		return nil, errors.New("node_fixture_source_invalid")
	}
	return &nodeProbeFixture{spec: spec, data: data, tvRoot: tv, agentRoot: agent, parts: map[int]string{}}, nil
}

func (f *nodeProbeFixture) Snapshot() nodeProbeFixtureSummary {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.summary
}
func (f *nodeProbeFixture) reject() (*http.Response, error) {
	f.summary.Rejected++
	return nil, errors.New("node_fixture_request_rejected")
}
func nodeProbeFixtureJSON(r *http.Request, data any) (*http.Response, error) {
	raw, e := json.Marshal(map[string]any{"status": 0, "data": data})
	if e != nil {
		return nil, e
	}
	return &http.Response{Request: r, StatusCode: 200, Header: http.Header{"Content-Type": {"application/json"}}, ContentLength: int64(len(raw)), Body: io.NopCloser(bytes.NewReader(raw))}, nil
}
func nodeProbeFixtureDecode(r *http.Request, out any) bool {
	if r.Body == nil {
		return false
	}
	raw, e := io.ReadAll(io.LimitReader(r.Body, 65537))
	return e == nil && len(raw) <= 65536 && json.Unmarshal(raw, out) == nil
}
func nodeProbeFixtureTV(id, parent, name string, dir bool, size int64) any {
	k := 0
	if dir {
		k = 1
	}
	return map[string]any{"fid": id, "parent_fid": parent, "filename": name, "isdir": k, "size": size, "version": "fixture-v1"}
}
func nodeProbeFixtureAgent(id, parent, name string, dir bool, size int64) any {
	k := "1"
	if dir {
		k = "0"
	}
	return map[string]any{"fid": id, "parent_fid": parent, "filename": name, "file_type": k, "size": size}
}

func (f *nodeProbeFixture) RoundTrip(r *http.Request) (*http.Response, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if r.Context().Err() != nil {
		return nil, r.Context().Err()
	}
	if r.URL.Scheme != "https" || r.Header.Get("Cookie") != "" {
		return f.reject()
	}
	if r.URL.Host == "node-fixture.example" {
		return f.media(r)
	}
	if r.URL.Host != "open-api-drive.quark.cn" || r.URL.Query().Get("access_token") == "" {
		return f.reject()
	}
	if strings.HasPrefix(r.URL.Path, "/open/") && r.Header.Get("Authorization") != "Bearer "+r.URL.Query().Get("access_token") {
		return f.reject()
	}
	created, finished := f.summary.Mkdir == 1, f.summary.Finish == 1
	switch r.URL.Path {
	case "/file":
		if r.Method != "GET" {
			return f.reject()
		}
		q := r.URL.Query()
		if q.Get("method") == "download" {
			if !finished || q.Get("fid") != "TF" {
				return f.reject()
			}
			return nodeProbeFixtureJSON(r, map[string]any{"fid": "TF", "file_name": f.spec.UploadName, "size": nodeProbeSize, "download_url": "https://node-fixture.example/original"})
		}
		if q.Get("method") != "list" || q.Get("page_index") != "0" {
			return f.reject()
		}
		items := []any{}
		switch q.Get("parent_fid") {
		case "0":
			items = append(items, nodeProbeFixtureTV("T", "0", "owned", true, 0))
		case "T":
			if created {
				items = append(items, nodeProbeFixtureTV("TD", "T", f.spec.DirectoryName, true, 0))
			}
		case "TD":
			if !created {
				return f.reject()
			}
			if finished {
				items = append(items, nodeProbeFixtureTV("TF", "TD", f.spec.UploadName, false, nodeProbeSize))
			}
		default:
			return f.reject()
		}
		return nodeProbeFixtureJSON(r, map[string]any{"total_count": len(items), "files": items})
	case "/open/v1/file/list":
		var b struct {
			Parent string `json:"parent_fid"`
		}
		if r.Method != "POST" || !nodeProbeFixtureDecode(r, &b) {
			return f.reject()
		}
		items := []any{}
		switch b.Parent {
		case "0":
			items = append(items, nodeProbeFixtureAgent("A", "0", "owned", true, 0))
		case "A":
			if created {
				items = append(items, nodeProbeFixtureAgent(nodeProbeFixtureDirectory, "A", f.spec.DirectoryName, true, 0))
			}
		case nodeProbeFixtureDirectory:
			if !created {
				return f.reject()
			}
			if finished {
				items = append(items, nodeProbeFixtureAgent(nodeProbeFixtureFile, b.Parent, f.spec.UploadName, false, nodeProbeSize))
			}
		default:
			return f.reject()
		}
		return nodeProbeFixtureJSON(r, map[string]any{"last_page": true, "file_list": items})
	case "/open/v1/file/info":
		if r.Method != "GET" {
			return f.reject()
		}
		id := r.URL.Query().Get("fid")
		switch id {
		case "A":
			return nodeProbeFixtureJSON(r, nodeProbeFixtureAgent(id, "0", "owned", true, 0))
		case "AD", nodeProbeFixtureDirectory:
			if !created {
				return f.reject()
			}
			return nodeProbeFixtureJSON(r, nodeProbeFixtureAgent(id, "A", f.spec.DirectoryName, true, 0))
		case "AF":
			if !finished {
				return f.reject()
			}
			return nodeProbeFixtureJSON(r, nodeProbeFixtureAgent(id, "AD", f.spec.UploadName, false, nodeProbeSize))
		case nodeProbeFixtureFile:
			if !finished {
				return f.reject()
			}
			return nodeProbeFixtureJSON(r, nodeProbeFixtureAgent(id, nodeProbeFixtureDirectory, f.spec.UploadName, false, nodeProbeSize))
		default:
			return f.reject()
		}
	case "/open/v1/dir":
		var b struct {
			Name   string `json:"dir_path"`
			Parent string `json:"pdir_fid"`
		}
		if r.Method != "POST" || !nodeProbeFixtureDecode(r, &b) || b.Parent != "A" || b.Name != f.spec.DirectoryName || created {
			return f.reject()
		}
		f.summary.Mkdir++
		return nodeProbeFixtureJSON(r, map[string]any{"fid": "AD", "finish": true})
	case "/open/v1/file/upload_pre":
		var b struct {
			Name   string `json:"file_name"`
			Parent string `json:"pdir_fid"`
			Size   int64  `json:"size"`
			SHA1   string `json:"sha1"`
			MD5    string `json:"md5"`
		}
		if r.Method != "POST" || !nodeProbeFixtureDecode(r, &b) || !created || b.Name != f.spec.UploadName || b.Parent != nodeProbeFixtureDirectory || b.Size != nodeProbeSize || f.summary.Pre != 0 {
			return f.reject()
		}
		sha, md := sha1.Sum(f.data), md5.Sum(f.data)
		if b.SHA1 != hex.EncodeToString(sha[:]) || b.MD5 != hex.EncodeToString(md[:]) {
			return f.reject()
		}
		f.summary.Pre++
		return nodeProbeFixtureJSON(r, map[string]any{"task_id": "fixture-upload", "part_size": nodeProbeFixturePartSize, "upload_urls": []any{}, "common_headers": map[string]string{"Content-Type": "application/octet-stream"}})
	case "/open/v1/file/get_upload_urls":
		var b struct {
			Task  string `json:"task_id"`
			Parts []struct {
				Number int   `json:"part_number"`
				Size   int64 `json:"part_size"`
			} `json:"part_info_list"`
		}
		if r.Method != "POST" || !nodeProbeFixtureDecode(r, &b) || f.summary.Pre != 1 || b.Task != "fixture-upload" || len(b.Parts) != 1 {
			return f.reject()
		}
		n := b.Parts[0].Number
		if n < 1 || n > 3 || b.Parts[0].Size != min(nodeProbeFixturePartSize, nodeProbeSize-int64(n-1)*nodeProbeFixturePartSize) || f.parts[n] != "" {
			return f.reject()
		}
		return nodeProbeFixtureJSON(r, map[string]any{"upload_urls": []any{map[string]any{"part_number": n, "upload_url": fmt.Sprintf("https://node-fixture.example/part/%d", n), "signature_info": map[string]string{"signature": "fixture-object-signature"}}}})
	case "/open/v1/file/upload_finish":
		var b struct {
			Task  string `json:"task_id"`
			Parts []struct {
				Number int    `json:"part_number"`
				ETag   string `json:"etag"`
			} `json:"part_info_list"`
		}
		if r.Method != "POST" || !nodeProbeFixtureDecode(r, &b) || f.summary.Put != 3 || finished || b.Task != "fixture-upload" || len(b.Parts) != 3 {
			return f.reject()
		}
		seen := map[int]bool{}
		for _, p := range b.Parts {
			if p.Number < 1 || p.Number > 3 || seen[p.Number] || p.ETag != f.parts[p.Number] {
				return f.reject()
			}
			seen[p.Number] = true
		}
		f.summary.Finish++
		return nodeProbeFixtureJSON(r, map[string]any{"fid": "AF", "finish": true})
	default:
		return f.reject()
	}
}

func (f *nodeProbeFixture) media(r *http.Request) (*http.Response, error) {
	if r.URL.RawQuery != "" {
		return f.reject()
	}
	if r.Method == "PUT" && strings.HasPrefix(r.URL.Path, "/part/") {
		n, e := strconv.Atoi(strings.TrimPrefix(r.URL.Path, "/part/"))
		if e != nil || n < 1 || n > 3 || f.summary.Pre != 1 || f.summary.Finish != 0 || f.parts[n] != "" || r.Header.Get("Authorization") != "fixture-object-signature" {
			return f.reject()
		}
		offset := int64(n-1) * nodeProbeFixturePartSize
		size := min(nodeProbeFixturePartSize, nodeProbeSize-offset)
		if r.ContentLength != size {
			return f.reject()
		}
		raw, e := io.ReadAll(io.LimitReader(r.Body, size+1))
		if e != nil || !bytes.Equal(raw, f.data[offset:offset+size]) {
			return f.reject()
		}
		f.parts[n] = fmt.Sprintf("fixture-etag-%d", n)
		f.summary.Put++
		f.summary.UploadBytes += int64(len(raw))
		return &http.Response{Request: r, StatusCode: 200, ContentLength: 0, Header: http.Header{"Etag": {f.parts[n]}}, Body: io.NopCloser(strings.NewReader(""))}, nil
	}
	if r.Method != "GET" || r.URL.Path != "/original" || f.summary.Finish != 1 || f.readActive || f.summary.ReadPasses >= 3 || r.Header.Get("Authorization") != "" || r.Header.Get("Accept-Encoding") != "identity" {
		return f.reject()
	}
	start := f.readOffset
	size := min(int64(8<<20), nodeProbeSize-start)
	end := start + size - 1
	if r.Header.Get("Range") != fmt.Sprintf("bytes=%d-%d", start, end) {
		return f.reject()
	}
	f.summary.ReadRequests++
	f.readActive = true
	body := &nodeProbeFixtureRead{Reader: bytes.NewReader(f.data[start : end+1]), fixture: f, remaining: size}
	return &http.Response{Request: r, StatusCode: 206, ContentLength: size, Header: http.Header{"Content-Range": {fmt.Sprintf("bytes %d-%d/%d", start, end, nodeProbeSize)}}, Body: body}, nil
}

type nodeProbeFixtureRead struct {
	*bytes.Reader
	fixture   *nodeProbeFixture
	remaining int64
	closed    bool
}

func (b *nodeProbeFixtureRead) Read(p []byte) (int, error) {
	f := b.fixture
	f.mu.Lock()
	defer f.mu.Unlock()
	if b.closed {
		return 0, io.ErrClosedPipe
	}
	n, e := b.Reader.Read(p)
	b.remaining -= int64(n)
	f.summary.ReadBytes += int64(n)
	f.readOffset += int64(n)
	if b.remaining == 0 && f.readActive {
		f.readActive = false
		if f.readOffset == nodeProbeSize {
			f.readOffset = 0
			f.summary.ReadPasses++
		}
	}
	return n, e
}
func (b *nodeProbeFixtureRead) Close() error {
	b.fixture.mu.Lock()
	defer b.fixture.mu.Unlock()
	b.closed = true
	return nil
}

func TestNodeProbeFixtureRejectsCorruptOrRepeatedPartAndPrematureRead(t *testing.T) {
	data := bytes.Repeat([]byte{37}, int(nodeProbeSize))
	f := &nodeProbeFixture{data: data, parts: map[int]string{}, summary: nodeProbeFixtureSummary{Pre: 1}}
	request := func(method, target string, body []byte) *http.Request {
		r, e := http.NewRequest(method, target, bytes.NewReader(body))
		if e != nil {
			t.Fatal(e)
		}
		return r
	}
	read := request("GET", "https://node-fixture.example/original", nil)
	read.Header.Set("Range", "bytes=0-8388607")
	read.Header.Set("Accept-Encoding", "identity")
	if _, e := f.RoundTrip(read); e == nil {
		t.Fatal("unfinished output was readable")
	}
	bad := request("PUT", "https://node-fixture.example/part/1", bytes.Repeat([]byte{38}, int(nodeProbeFixturePartSize)))
	bad.Header.Set("Authorization", "fixture-object-signature")
	if _, e := f.RoundTrip(bad); e == nil {
		t.Fatal("corrupt upload accepted")
	}
	if s := f.Snapshot(); s.Put != 0 || s.UploadBytes != 0 {
		t.Fatal("failed part counted as uploaded")
	}
	good := request("PUT", "https://node-fixture.example/part/1", data[:nodeProbeFixturePartSize])
	good.Header.Set("Authorization", "fixture-object-signature")
	res, e := f.RoundTrip(good)
	if e != nil {
		t.Fatal(e)
	}
	res.Body.Close()
	again := request("PUT", "https://node-fixture.example/part/1", data[:nodeProbeFixturePartSize])
	again.Header.Set("Authorization", "fixture-object-signature")
	if _, e = f.RoundTrip(again); e == nil {
		t.Fatal("duplicate part accepted")
	}
	if s := f.Snapshot(); s.Put != 1 || s.UploadBytes != nodeProbeFixturePartSize || s.Rejected != 3 {
		t.Fatal("fixture did not keep truthful counters")
	}
	if _, e = f.RoundTrip(request("GET", "https://unapproved.example/private", nil)); e == nil {
		t.Fatal("unexpected host accepted")
	}
}
