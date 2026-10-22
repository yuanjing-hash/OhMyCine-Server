package quark

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
)

const originalReadChunkSize int64 = 8 * 1024 * 1024

// A transfer read uses one immutable original URL and exact sequential ranges.
// It does not retry, renew a URL, switch files or buffer a file in memory. Cloud
// operation receipts and the caller's complete digest remain the integrity gate.
type originalRangeReader struct {
	ctx         context.Context
	cancel      context.CancelFunc
	client      *http.Client
	url         string
	beforeRange func(context.Context) error
	total       int64
	next        int64
	left        int64
	err         error
	readMu      sync.Mutex
	mu          sync.Mutex // Close can interrupt a blocked Read without waiting readMu.
	body        io.ReadCloser
	closed      bool
}

type originalResponseBody struct {
	io.ReadCloser
	once sync.Once
	err  error
}

func (b *originalResponseBody) Close() error {
	b.once.Do(func() { b.err = b.ReadCloser.Close() })
	return b.err
}

func newOriginalRangeReader(ctx context.Context, transport http.RoundTripper, url string, size, offset int64, guards ...func(context.Context) error) (*originalRangeReader, error) {
	if size < 0 || offset < 0 || offset > size {
		return nil, failure(CodeInvalid)
	}
	ctx, cancel := context.WithCancel(ctx)
	r := &originalRangeReader{ctx: ctx, cancel: cancel, url: url, total: size, next: offset,
		client: &http.Client{Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
	if len(guards) > 1 {
		cancel()
		return nil, failure(CodeInvalid)
	}
	if len(guards) == 1 {
		r.beforeRange = guards[0]
	}
	if err := ctx.Err(); err != nil {
		cancel()
		return nil, err
	}
	if offset < size {
		if err := r.open(); err != nil {
			_ = r.Close()
			return nil, err
		}
	}
	return r, nil
}

func (r *originalRangeReader) open() error {
	// Restricted Node access is checked before every physical media request,
	// including the first. A fixed original URL does not outlive its grant or
	// operation lease. Ordinary Server clients have no restricted loader.
	if r.beforeRange != nil {
		if err := r.beforeRange(r.ctx); err != nil {
			return err
		}
	}
	if err := r.ctx.Err(); err != nil {
		return err
	}
	length := min(originalReadChunkSize, r.total-r.next)
	end := r.next + length - 1 // length <= total-next; never overflows int64.
	req, err := http.NewRequestWithContext(r.ctx, http.MethodGet, r.url, nil)
	if err != nil {
		return failure(CodeInvalid)
	}
	req.Header.Set("Accept-Encoding", "identity")
	req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", r.next, end))
	// No Jar or account/acquisition headers, including on later ranges.
	response, err := r.client.Do(req)
	if err != nil {
		return r.readError(err)
	}
	valid := response.StatusCode == http.StatusPartialContent && exactContentRange(response.Header, r.next, end, r.total)
	if response.StatusCode == http.StatusOK {
		// Ignoring Range is only acceptable for a complete small original.
		valid = r.next == 0 && r.total <= originalReadChunkSize && len(response.Header.Values("Content-Range")) == 0
	}
	encoding := response.Header.Get("Content-Encoding")
	if !valid || response.Uncompressed || len(response.Header.Values("Content-Encoding")) > 1 || encoding != "" && encoding != "identity" || response.ContentLength >= 0 && response.ContentLength != length {
		_ = response.Body.Close()
		return failure(CodeInvalid)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		_ = response.Body.Close()
		return io.ErrClosedPipe
	}
	r.body = &originalResponseBody{ReadCloser: response.Body}
	r.left = length
	r.next += length
	return nil
}

func exactContentRange(header http.Header, start, end, total int64) bool {
	values := header.Values("Content-Range")
	if len(values) != 1 {
		return false
	}
	// Equality with the canonical decimal representation also rejects trailing
	// text, duplicate ranges, unknown totals, signs, whitespace and overflows.
	want := "bytes " + strconv.FormatInt(start, 10) + "-" + strconv.FormatInt(end, 10) + "/" + strconv.FormatInt(total, 10)
	return strings.TrimSpace(values[0]) == want
}

func (r *originalRangeReader) readError(err error) error {
	if r.ctx.Err() != nil {
		return r.ctx.Err()
	}
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return failure(CodeInvalid)
	}
	return failure(CodeUnavailable)
}

func (r *originalRangeReader) Read(p []byte) (int, error) {
	r.readMu.Lock()
	defer r.readMu.Unlock()
	if r.err != nil {
		return 0, r.err
	}
	if r.ctx.Err() != nil {
		return 0, r.ctx.Err()
	}
	if len(p) == 0 {
		return 0, nil
	}
	r.mu.Lock()
	body := r.body
	r.mu.Unlock()
	if body == nil {
		if r.next == r.total {
			return 0, io.EOF
		}
		if r.err = r.open(); r.err != nil {
			return 0, r.err
		}
		r.mu.Lock()
		body = r.body
		r.mu.Unlock()
	}
	if int64(len(p)) > r.left {
		p = p[:int(r.left)]
	}
	n, err := body.Read(p)
	r.left -= int64(n)
	if err != nil && (err != io.EOF || r.left != 0) {
		r.err = r.readError(err)
	} else if r.left == 0 {
		// Demand actual segment EOF, even when Content-Length claims the right
		// size. No extra byte can enter a subsequent segment or the target hash.
		var extra [1]byte
		count, tailErr := io.ReadFull(body, extra[:])
		if count != 0 || tailErr != io.EOF {
			if count != 0 {
				r.err = failure(CodeInvalid)
			} else {
				r.err = r.readError(tailErr)
			}
		}
		r.mu.Lock()
		if r.body == body {
			r.body = nil
		}
		r.mu.Unlock()
		if closeErr := body.Close(); closeErr != nil && r.err == nil {
			r.err = r.readError(closeErr)
		}
	}
	if r.err != nil {
		_ = r.Close()
	}
	return n, r.err
}

func (r *originalRangeReader) Close() error {
	r.cancel()
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return nil
	}
	r.closed = true
	if r.body != nil {
		return r.body.Close()
	}
	return nil
}
