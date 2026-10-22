//go:build quark_live

package quark

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"time"
)

// ForensicMultipartSecondURL is an acceptance-only metadata request. It does
// not enter Upload, call an identity endpoint, refresh, save credentials or
// mutate an operation. Its caller must independently bind the task and prefix
// state to the frozen receipt and wrap the transport with a one-request guard.
func (d *Client) ForensicMultipartSecondURL(ctx context.Context, task string, prefix [5]uint32) error {
	if d.config.PersistCredentials != nil || d.config.Operations != nil || !scalar(task, 4096) {
		return failure(CodeInvalid)
	}
	a := d.Credentials().Agent
	if a == nil || !a.IdentityVerified || !scalar(a.AccessToken, 32768) || a.RefreshToken != "" {
		return failure(CodeAuth)
	}
	_, err := d.api.agent(ctx, *a, http.MethodPost, "/open/v1/file/get_upload_urls", nil, map[string]any{
		"task_id": task,
		"part_info_list": []any{map[string]any{
			"part_number": 2, "part_size": int64(4 << 20),
			"parallel_sha1_ctx": map[string]any{"part_offset": int64(4 << 20), "h": prefix},
		}},
	})
	return err
}

type MultipartForensicReplay struct {
	NativeParserAccepted bool   `json:"native_parser_accepted"`
	NativePutPrepared    bool   `json:"native_put_prepared"`
	ReplayMetadataCalls  int    `json:"replay_metadata_calls"`
	FailureStage         string `json:"failure_stage"`
}
type forensicReplayTransport func(*http.Request) (*http.Response, error)

func (f forensicReplayTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// ForensicReplayMultipartSecondURL has no network transport. It passes the
// captured body through the original native parser and request preparation.
// The optional wrapper can apply an acceptance guard around the in-memory
// responder; the live caller supplies no network-capable transport.
func ForensicReplayMultipartSecondURL(ctx context.Context, response []byte, status int, auth Authorization, task string, expectedBody []byte, source io.ReadSeeker, unknownResponseLength bool, wrap func(http.RoundTripper) http.RoundTripper) MultipartForensicReplay {
	out := MultipartForensicReplay{FailureStage: "input"}
	if len(response) > 256<<10 || status < 100 || status > 599 || source == nil || !scalar(task, 4096) || !auth.IdentityVerified || auth.RefreshToken != "" {
		return out
	}
	metadata := forensicReplayTransport(func(r *http.Request) (*http.Response, error) {
		out.ReplayMetadataCalls++
		b, err := io.ReadAll(io.LimitReader(r.Body, 4097))
		r.Body.Close()
		if err != nil || out.ReplayMetadataCalls != 1 || !bytes.Equal(b, expectedBody) || r.Method != "POST" || r.URL.Host != "open-api-drive.quark.cn" || r.URL.Path != "/open/v1/file/get_upload_urls" {
			return nil, failure(CodeInvalid)
		}
		length := int64(len(response))
		if unknownResponseLength {
			length = -1
		}
		return &http.Response{StatusCode: status, Header: http.Header{}, ContentLength: length, Body: io.NopCloser(bytes.NewReader(response))}, nil
	})
	var transport http.RoundTripper = metadata
	if wrap != nil {
		transport = wrap(transport)
	}
	bundle := Credentials{Version: 1, Agent: &auth}
	d := &Client{credentials: bundle, api: &apiClient{http: &http.Client{Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}, clock: time.Now, gate: &accountGate{}}}
	d.mediaTransport = forensicReplayTransport(func(r *http.Request) (*http.Response, error) {
		defer r.Body.Close()
		// This proves construction reached the media boundary. No body is read,
		// no request is dispatched, and no response can confirm an uploaded part.
		out.NativePutPrepared = r.Method == "PUT" && r.ContentLength == 4<<20
		return nil, failure(CodeUnavailable)
	})
	p := uploadPayload{TaskID: task, Size: 8*1024*1024 + 1, PartSize: 4 << 20}
	destination, err := d.uploadPartURL(ctx, auth, &p, 2, source)
	if err != nil {
		out.FailureStage = "native_url_parser"
		return out
	}
	out.NativeParserAccepted = true
	_, _ = source.Seek(4<<20, io.SeekStart)
	_, _ = d.putPart(ctx, destination, p.Headers, source, 4<<20, auth)
	if out.NativePutPrepared {
		out.FailureStage = "none"
	} else {
		out.FailureStage = "native_put_preparation"
	}
	return out
}
