package quark

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type virtualSource struct {
	size, offset, read int64
}

func TestOriginalRangeStreamChecksRestrictedGrantBeforeEachRequest(t *testing.T) {
	for _, failAt := range []int{1, 2, 3, 0} {
		t.Run(fmt.Sprint(failAt), func(t *testing.T) {
			const total = 2*originalReadChunkSize + 1
			checks, calls := 0, 0
			denied := errors.New("restricted_grant_ended")
			guard := func(context.Context) error {
				checks++
				if checks == failAt {
					return denied
				}
				return nil
			}
			transport := transportFunc(func(req *http.Request) (*http.Response, error) {
				calls++
				if checks != calls {
					t.Fatal("request preceded current grant validation")
				}
				start := int64(calls-1) * originalReadChunkSize
				length := min(originalReadChunkSize, total-start)
				return &http.Response{StatusCode: 206, ContentLength: length, Header: http.Header{"Content-Range": {fmt.Sprintf("bytes %d-%d/%d", start, start+length-1, total)}}, Body: io.NopCloser(&virtualSource{size: start + length, offset: start})}, nil
			})
			r, err := newOriginalRangeReader(context.Background(), transport, "https://cdn.example/original", total, 0, guard)
			var n int64
			if err == nil {
				n, err = io.Copy(io.Discard, r)
				_ = r.Close()
			}
			if failAt == 0 {
				if err != nil || n != total || calls != 3 {
					t.Fatal("valid continuation failed", err, n, calls)
				}
			} else if !errors.Is(err, denied) || calls != failAt-1 || checks != failAt {
				t.Fatal("expired grant issued another request", err, calls, checks)
			}
		})
	}
}

func (v *virtualSource) Read(p []byte) (int, error) {
	if v.offset >= v.size {
		return 0, io.EOF
	}
	n := int(min(int64(len(p)), v.size-v.offset))
	for i := 0; i < n; i++ {
		p[i] = byte((v.offset + int64(i)) % 251)
	}
	v.offset += int64(n)
	v.read += int64(n)
	return n, nil
}
func (v *virtualSource) Seek(offset int64, whence int) (int64, error) {
	switch whence {
	case io.SeekStart:
	case io.SeekCurrent:
		offset += v.offset
	case io.SeekEnd:
		offset += v.size
	default:
		return 0, errors.New("whence")
	}
	if offset < 0 {
		return 0, errors.New("offset")
	}
	v.offset = offset
	return offset, nil
}

func TestOriginalRangeStreamSequentialAndLargeOffsets(t *testing.T) {
	for _, bounds := range [][2]int64{{0, 2*originalReadChunkSize + 37}, {1<<31 + 17, 1<<31 + 17 + originalReadChunkSize + 3}, {math.MaxInt64 - 31, math.MaxInt64}} {
		t.Run(fmt.Sprint(bounds[0]), func(t *testing.T) {
			start, total := bounds[0], bounds[1]
			next, calls := start, 0
			transport := transportFunc(func(req *http.Request) (*http.Response, error) {
				calls++
				length := min(originalReadChunkSize, total-next)
				if req.Header.Get("Range") != fmt.Sprintf("bytes=%d-%d", next, next+length-1) {
					t.Fatal("range order")
				}
				if req.URL.String() != "https://cdn.example/f?ticket=private" {
					t.Fatal("changed URL")
				}
				for _, name := range []string{"Authorization", "Cookie", "Referer", "X-Pan-Token", "X-Agent-ID"} {
					if req.Header.Get(name) != "" {
						t.Fatal("secret header")
					}
				}
				if req.Header.Get("Accept-Encoding") != "identity" {
					t.Fatal("encoding")
				}
				body := &virtualSource{size: next + length, offset: next}
				header := http.Header{"Content-Range": {fmt.Sprintf("bytes %d-%d/%d", next, next+length-1, total)}}
				next += length
				return &http.Response{StatusCode: 206, Header: header, ContentLength: length, Body: io.NopCloser(body)}, nil
			})
			r, err := newOriginalRangeReader(context.Background(), transport, "https://cdn.example/f?ticket=private", total, start)
			if err != nil {
				t.Fatal(err)
			}
			defer r.Close()
			buffer := make([]byte, 32771)
			position := start
			for {
				n, e := r.Read(buffer)
				for i := 0; i < n; i++ {
					if buffer[i] != byte((position+int64(i))%251) {
						t.Fatal("wrong content")
					}
				}
				position += int64(n)
				if e == io.EOF {
					break
				}
				if e != nil {
					t.Fatal(e)
				}
			}
			if position != total || int64(calls) != 1+(total-start-1)/originalReadChunkSize {
				t.Fatal("incomplete read")
			}
		})
	}
}

func TestOriginalRangeStreamRejectsBadResponses(t *testing.T) {
	for _, mode := range []string{"short", "long", "wrong_start", "wrong_end", "wrong_total", "trailing_range", "multiple_range", "length", "encoding", "redirect", "ignored_large", "ignored_offset", "transport"} {
		t.Run(mode, func(t *testing.T) {
			calls := 0
			transport := transportFunc(func(req *http.Request) (*http.Response, error) {
				calls++
				if mode == "transport" {
					return nil, errors.New("private CDN URL")
				}
				res := &http.Response{StatusCode: 206, ContentLength: 4, Header: http.Header{"Content-Range": {"bytes 0-3/4"}}, Body: io.NopCloser(strings.NewReader("abcd"))}
				switch mode {
				case "short":
					res.Body = io.NopCloser(strings.NewReader("abc"))
				case "long":
					res.Body = io.NopCloser(strings.NewReader("abcde"))
				case "wrong_start":
					res.Header.Set("Content-Range", "bytes 1-3/4")
				case "wrong_end":
					res.Header.Set("Content-Range", "bytes 0-2/4")
				case "wrong_total":
					res.Header.Set("Content-Range", "bytes 0-3/5")
				case "trailing_range":
					res.Header.Set("Content-Range", "bytes 0-3/4 extra")
				case "multiple_range":
					res.Header.Add("Content-Range", "bytes 0-3/4")
				case "length":
					res.ContentLength = 3
				case "encoding":
					res.Header.Set("Content-Encoding", "gzip")
				case "redirect":
					res.StatusCode = 302
					res.Header.Set("Location", "https://other.example/secret")
				case "ignored_large", "ignored_offset":
					res.StatusCode = 200
					res.Header.Del("Content-Range")
				}
				return res, nil
			})
			size, offset := int64(4), int64(0)
			if mode == "ignored_large" {
				size = originalReadChunkSize + 1
			}
			if mode == "ignored_offset" {
				offset = 1
			}
			r, e := newOriginalRangeReader(context.Background(), transport, "https://cdn.example/private", size, offset)
			if e == nil {
				_, e = io.Copy(io.Discard, r)
				_ = r.Close()
			}
			if e == nil || strings.Contains(e.Error(), "private") || calls != 1 {
				t.Fatalf("bad response accepted/retried: calls=%d err=%v", calls, e)
			}
		})
	}
}

func TestOriginalRangeStreamSmall200AndEmpty(t *testing.T) {
	for _, length := range []int64{-1, 4} {
		r, err := newOriginalRangeReader(context.Background(), transportFunc(func(*http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: 200, Header: http.Header{}, ContentLength: length, Body: io.NopCloser(strings.NewReader("abcd"))}, nil
		}), "https://cdn.example/f", 4, 0)
		if err != nil {
			t.Fatal(err)
		}
		b, err := io.ReadAll(r)
		_ = r.Close()
		if err != nil || string(b) != "abcd" {
			t.Fatal(err)
		}
	}
	for _, offset := range []int64{0, 4} {
		total := offset
		r, e := newOriginalRangeReader(context.Background(), transportFunc(func(*http.Request) (*http.Response, error) { t.Fatal("empty requested CDN"); return nil, nil }), "https://cdn.example/f", total, offset)
		if e != nil {
			t.Fatal(e)
		}
		b, e := io.ReadAll(r)
		_ = r.Close()
		if e != nil || len(b) != 0 {
			t.Fatal("empty")
		}
	}
	for _, bounds := range [][2]int64{{-1, 0}, {4, -1}, {4, 5}} {
		if _, e := newOriginalRangeReader(context.Background(), nil, "https://cdn.example/f", bounds[0], bounds[1]); e == nil {
			t.Fatal("bad bounds")
		}
	}
}

type blockingReadBody struct {
	started chan struct{}
	done    chan struct{}
	once    sync.Once
}

func (b *blockingReadBody) Read([]byte) (int, error) {
	b.once.Do(func() { close(b.started) })
	<-b.done
	return 0, errors.New("closed")
}
func (b *blockingReadBody) Close() error {
	select {
	case <-b.done:
	default:
		close(b.done)
	}
	return nil
}

func TestOriginalRangeStreamCloseInterruptsRead(t *testing.T) {
	body := &blockingReadBody{started: make(chan struct{}), done: make(chan struct{})}
	r, e := newOriginalRangeReader(context.Background(), transportFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 206, ContentLength: 4, Header: http.Header{"Content-Range": {"bytes 0-3/4"}}, Body: body}, nil
	}), "https://cdn.example/f", 4, 0)
	if e != nil {
		t.Fatal(e)
	}
	done := make(chan error, 1)
	go func() { _, e := r.Read(make([]byte, 4)); done <- e }()
	<-body.started
	if e := r.Close(); e != nil {
		t.Fatal(e)
	}
	select {
	case e := <-done:
		if e == nil {
			t.Fatal("close accepted")
		}
	case <-time.After(time.Second):
		t.Fatal("Close blocked read")
	}
}

func TestOriginalRangeStreamCancellationAndSecondSegmentFailure(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, e := newOriginalRangeReader(ctx, nil, "https://cdn.example/f", 4, 0); !errors.Is(e, context.Canceled) {
		t.Fatal(e)
	}
	calls := 0
	r, e := newOriginalRangeReader(context.Background(), transportFunc(func(req *http.Request) (*http.Response, error) {
		calls++
		if calls == 2 {
			return nil, errors.New("private")
		}
		return &http.Response{StatusCode: 206, ContentLength: originalReadChunkSize, Header: http.Header{"Content-Range": {fmt.Sprintf("bytes 0-%d/%d", originalReadChunkSize-1, originalReadChunkSize+1)}}, Body: io.NopCloser(&virtualSource{size: originalReadChunkSize})}, nil
	}), "https://cdn.example/f", originalReadChunkSize+1, 0)
	if e != nil {
		t.Fatal(e)
	}
	n, e := io.Copy(io.Discard, r)
	_ = r.Close()
	if e == nil || n != originalReadChunkSize || calls != 2 {
		t.Fatalf("failed segment retried or lost bytes %d %d %v", n, calls, e)
	}
}

func TestOriginalRangeStreamHTTPRedirectAndCancellation(t *testing.T) {
	var redirected atomic.Int64
	started := make(chan struct{})
	finished := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		switch req.URL.Path {
		case "/redirect":
			http.Redirect(w, req, "/target", http.StatusFound)
		case "/target":
			redirected.Add(1)
		case "/block":
			if req.Header.Get("Range") != "bytes=0-3" {
				t.Error("wrong actual HTTP range")
			}
			w.Header().Set("Content-Range", "bytes 0-3/4")
			w.Header().Set("Content-Length", "4")
			w.WriteHeader(http.StatusPartialContent)
			w.(http.Flusher).Flush()
			close(started)
			<-req.Context().Done()
			close(finished)
		}
	}))
	defer server.Close()
	if _, err := newOriginalRangeReader(context.Background(), server.Client().Transport, server.URL+"/redirect", 4, 0); err == nil || redirected.Load() != 0 {
		t.Fatal("followed redirect")
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r, err := newOriginalRangeReader(ctx, server.Client().Transport, server.URL+"/block", 4, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	<-started
	done := make(chan error, 1)
	go func() { _, err := io.Copy(io.Discard, r); done <- err }()
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("HTTP cancellation stuck")
	}
	select {
	case <-finished:
	case <-time.After(5 * time.Second):
		t.Fatal("upstream request remained live")
	}
}
