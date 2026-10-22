//go:build quark_live

package handlers_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/yuanjing-hash/OhMyCine-Server/pkg/cloud"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/cloud/quark"
)

type largefileReviewCloseFailure struct{ io.ReadCloser }

func (r largefileReviewCloseFailure) Close() error {
	_ = r.ReadCloser.Close()
	return errors.New("private upstream close failure")
}

// Exercise the actual product OpenRead and the acceptance guard together.
// Independent unit success for each wrapper would not prove that nested
// Close/error handling preserves the final range validation.
func TestQuarkLargeFileIndependentNativeRangeGuard(t *testing.T) {
	for _, mode := range []string{"exact_window", "overlong", "close_error", "cancel"} {
		t.Run(mode, func(t *testing.T) {
			s, _ := largefileOfflineStore(t)
			largefileSparseSource(t, s)
			s.state.TVFiles["F"] = true
			g := newLargeFileGuard(s)
			g.delay = 0
			apiCalls, mediaCalls := 0, 0
			blocked := &largefileBlockingBody{entered: make(chan struct{}), closed: make(chan struct{})}
			transport := multipartTestTransport(func(r *http.Request) (*http.Response, error) {
				if r.URL.Host == "open-api-drive.quark.cn" {
					apiCalls++
					if r.URL.Path != "/file" || r.URL.Query().Get("method") != "download" || r.URL.Query().Get("fid") != "F" {
						t.Fatal("unexpected metadata request")
					}
					body, _ := json.Marshal(map[string]any{"status": 0, "data": map[string]any{"fid": "F", "file_name": s.state.UploadName, "size": largefileSize, "download_url": "https://objects.example/original"}})
					return &http.Response{StatusCode: 200, ContentLength: int64(len(body)), Header: http.Header{}, Body: io.NopCloser(strings.NewReader(string(body)))}, nil
				}
				mediaCalls++
				length := largefileReadChunk
				if mode == "overlong" {
					length++
				}
				var body io.ReadCloser = io.NopCloser(io.LimitReader(largefileZeroReader{}, length))
				if mode == "close_error" {
					body = largefileReviewCloseFailure{body}
				}
				if mode == "cancel" {
					body = blocked
				}
				return &http.Response{StatusCode: 206, ContentLength: largefileReadChunk, Header: http.Header{"Content-Range": {fmt.Sprintf("bytes 0-%d/%d", largefileReadChunk-1, largefileSize)}}, Body: body}, nil
			})
			raw, _ := json.Marshal(s.state.Bundle)
			d, err := quark.NewObservationValidationClient(cloud.Config{Credentials: raw}, func(_ http.RoundTripper, media bool) http.RoundTripper {
				return g.wrap(transport, media)
			})
			if err != nil {
				t.Fatal("native client setup")
			}
			result, err := d.OpenRead(context.Background(), cloud.ReadRequest{FileID: quark.QualifyID(quark.RouteTV, s.state.Identity.TVAccount, "F")})
			if err != nil {
				t.Fatal("native read setup")
			}
			defer result.Body.Close()
			if mode == "cancel" {
				done := make(chan error, 1)
				go func() { _, e := io.Copy(io.Discard, result.Body); done <- e }()
				select {
				case <-blocked.entered:
				case <-time.After(3 * time.Second):
					t.Fatal("native read did not enter body")
				}
				closed := make(chan error, 1)
				go func() { closed <- result.Body.Close() }()
				for _, finished := range []<-chan error{closed, done} {
					select {
					case err = <-finished:
						if err == nil {
							t.Fatal("cancelled nested body completed successfully")
						}
					case <-time.After(3 * time.Second):
						t.Fatal("nested Close/Read remained blocked")
					}
				}
			} else if mode == "exact_window" {
				if n, e := io.CopyN(io.Discard, result.Body, largefileReadChunk); e != nil || n != largefileReadChunk {
					t.Fatal("exact window failed")
				}
			} else {
				_, err = io.Copy(io.Discard, result.Body)
				if err == nil || strings.Contains(err.Error(), "private") {
					t.Fatal("native read swallowed or exposed guard failure")
				}
			}
			if apiCalls != 1 || mediaCalls != 1 || s.state.ReadReserved != largefileReadChunk || s.state.ReadPasses != 0 {
				t.Fatal("failed/partial read retried or promoted to complete pass")
			}
			if mode == "exact_window" {
				if s.state.ReadInFlight || s.state.ReadOffset != largefileReadChunk || s.state.Stopped != "" {
					t.Fatal("complete range did not checkpoint")
				}
			} else if !s.state.ReadInFlight || s.state.Stopped == "" {
				t.Fatal("failed range lost its durable stop evidence")
			}
		})
	}
}
