package pan115

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"sync"
	"time"

	pan115sdk "github.com/SheltonZhu/115driver/pkg/driver"
	"github.com/aliyun/aliyun-oss-go-sdk/oss"
	"github.com/go-resty/resty/v2"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/cloud"
)

type observedUploadSDK interface {
	UploadObserved(context.Context, cloud.UploadRequest, *os.File) error
}

// Keep provider preparation, rapid upload, signing and callback semantics in the
// SDK. Own only the OSS body requests so cancellation and byte samples reach it.
func (s *sdkAdapter) UploadObserved(ctx context.Context, request cloud.UploadRequest, file *os.File) error {
	ctx, cancel := context.WithTimeout(ctx, 24*time.Hour)
	defer cancel()
	sdk := s.uploadClient(ctx)
	if request.Progress != nil {
		request.Progress(cloud.UploadProgress{Phase: "hashing_source"})
	}
	available, err := sdk.UploadAvailable()
	if err != nil {
		return err
	}
	if !available || sdk.UploadMetaInfo == nil {
		return errors.New("115 upload unavailable")
	}
	if request.Size > sdk.UploadMetaInfo.SizeLimit {
		return pan115sdk.ErrUploadTooLarge
	}
	digest, err := sdk.GetDigestResult(&uploadContextReader{ctx: ctx, reader: file})
	if err != nil {
		return err
	}
	if digest.Size != request.Size {
		return errors.New("115 upload source changed")
	}
	if request.Progress != nil {
		request.Progress(cloud.UploadProgress{Phase: "verifying_target"})
	}
	initial, err := sdk.RapidUpload(digest.Size, request.Name, request.ParentID, digest.PreID, digest.QuickID, &uploadContextReadSeeker{uploadContextReader: uploadContextReader{ctx: ctx, reader: file}, seeker: file})
	if err != nil {
		return err
	}
	if complete, err := initial.Ok(); complete || err != nil {
		return err
	}
	if err := uploadOSSObserved(ctx, request, file, initial, sdk.GetOSSEndpoint(sdk.UseInternalUpload), sdk.GetOSSToken); err != nil {
		return err
	}
	if request.Size <= pan115sdk.KB {
		return verifySmallUpload(sdk, request, digest.QuickID)
	}
	return nil
}

func (s *sdkAdapter) uploadClient(ctx context.Context) *pan115sdk.Pan115Client {
	sdk := *s.Pan115Client
	// Resty's Clone shares hook storage. Use the SDK's fresh-client entry point
	// while retaining the configured HTTP transport, cookies and user agent.
	sdk.SetHttpClient(s.Client.GetClient())
	sdk.Client.Header = s.Client.Header.Clone()
	sdk.Client.SetCookies(s.Client.Cookies)
	sdk.Client.OnBeforeRequest(func(_ *resty.Client, r *resty.Request) error {
		r.SetContext(ctx)
		return ctx.Err()
	})
	return &sdk
}

func verifySmallUpload(sdk *pan115sdk.Pan115Client, request cloud.UploadRequest, sha1 string) error {
	// The SDK checks the uploaded SHA1 for this PutObject path. Keep that
	// evidence in addition to the callback and outer paginated reconcile.
	files, err := pan115sdk.GetFiles(sdk.NewRequest().ForceContentType("application/json;charset=UTF-8"), request.ParentID,
		pan115sdk.WithOrder(pan115sdk.FileOrderByTime), pan115sdk.WithShowDirEnable(false), pan115sdk.WithAsc(false), pan115sdk.WithLimit(500))
	if err != nil {
		return err
	}
	for _, uploaded := range files.Files {
		if uploaded.Name == request.Name && int64(uploaded.Size) == request.Size && uploaded.Sha1 == sha1 {
			return nil
		}
	}
	return pan115sdk.ErrUploadFailed
}

func uploadOSSObserved(ctx context.Context, request cloud.UploadRequest, file *os.File, initial *pan115sdk.UploadInitResp, endpoint string, loadToken func() (*pan115sdk.UploadOSSTokenResp, error)) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	if err := ctx.Err(); err != nil {
		return err
	}
	if complete, err := initial.Ok(); complete || err != nil {
		return err
	}
	token, err := loadToken()
	if err != nil {
		return err
	}
	newBucket := func() (*oss.Bucket, error) {
		client, err := oss.New(endpoint, token.AccessKeyID, token.AccessKeySecret, oss.EnableMD5(true), oss.EnableCRC(true))
		if err != nil {
			return nil, err
		}
		return client.Bucket(initial.Bucket)
	}
	bucket, err := newBucket()
	if err != nil {
		return err
	}
	progress := &ossUploadProgress{total: request.Size, observe: request.Progress}
	defer progress.finish()
	options := func(listener oss.ProgressListener) []oss.Option {
		return append(pan115sdk.OssOption(&initial.UploadOSSParams, token), oss.WithContext(ctx), oss.Progress(listener))
	}
	if request.Size <= pan115sdk.KB {
		var body []byte
		if err := bucket.PutObject(initial.Object, io.NewSectionReader(file, 0, request.Size), append(options(&ossPartProgress{parent: progress}), oss.CallbackResult(&body))...); err != nil {
			return err
		}
		return uploadCallbackResult(body)
	}
	chunks, err := pan115sdk.SplitFile(file.Name(), request.Size)
	if err != nil {
		return err
	}
	upload, err := bucket.InitiateMultipartUpload(initial.Object, oss.WithContext(ctx), oss.SetHeader(pan115sdk.OssSecurityTokenHeaderName, token.SecurityToken), oss.UserAgentHeader(pan115sdk.OSSUserAgent), oss.EnableSha1(), oss.Sequential())
	if err != nil {
		return err
	}
	complete := false
	defer func() {
		cancel()
		progress.finish()
		if !complete {
			cleanup, stop := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
			defer stop()
			_ = bucket.AbortMultipartUpload(upload, append(pan115sdk.OssOption(&initial.UploadOSSParams, token), oss.WithContext(cleanup))...)
		}
	}()
	parts := make([]oss.UploadPart, 0, len(chunks))
	refreshedAt := time.Now()
	refresh := func() error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if time.Since(refreshedAt) < 50*time.Minute {
			return nil
		}
		token, err = loadToken()
		if err != nil {
			return err
		}
		bucket, err = newBucket()
		if err == nil {
			refreshedAt = time.Now()
		}
		return err
	}
	for _, chunk := range chunks {
		if err := ctx.Err(); err != nil {
			return err
		}
		var part oss.UploadPart
		for attempt := 0; attempt < 3; attempt++ {
			if err := refresh(); err != nil {
				return err
			}
			listener := &ossPartProgress{parent: progress, offset: chunk.Offset}
			part, err = bucket.UploadPart(upload, io.NewSectionReader(file, chunk.Offset, chunk.Size), chunk.Size, chunk.Number, options(listener)...)
			if err == nil || ctx.Err() != nil {
				break
			}
		}
		if err != nil {
			return err
		}
		parts = append(parts, part)
	}
	var body []byte
	if err := refresh(); err != nil {
		return err
	}
	if _, err := bucket.CompleteMultipartUpload(upload, parts, append(options(nil), oss.CallbackResult(&body))...); err != nil {
		return err
	}
	if err := uploadCallbackResult(body); err != nil {
		return err
	}
	complete = true
	return nil
}

func uploadCallbackResult(body []byte) error {
	var result pan115sdk.UploadResult
	if err := json.Unmarshal(body, &result); err != nil {
		return err
	}
	return result.Err()
}

type uploadContextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (r *uploadContextReader) Read(buffer []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.reader.Read(buffer)
}

type uploadContextReadSeeker struct {
	uploadContextReader
	seeker io.Seeker
}

func (r *uploadContextReadSeeker) Seek(offset int64, whence int) (int64, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.seeker.Seek(offset, whence)
}

type ossUploadProgress struct {
	mu                   sync.Mutex
	total, sent, covered int64
	observe              func(cloud.UploadProgress)
	closed               bool
}

func (p *ossUploadProgress) finish() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.closed = true
}

type ossPartProgress struct {
	parent *ossUploadProgress
	offset int64
}

func (p *ossPartProgress) ProgressChanged(event *oss.ProgressEvent) {
	state := p.parent
	if event == nil || event.EventType != oss.TransferDataEvent || event.RwBytes <= 0 {
		return
	}
	state.mu.Lock()
	defer state.mu.Unlock()
	if state.closed {
		return
	}
	state.sent += event.RwBytes
	state.covered = min(state.total, max(state.covered, p.offset+event.ConsumedBytes))
	if state.observe != nil {
		state.observe(cloud.UploadProgress{Phase: "uploading_target", TransferredBytes: state.covered, NetworkBytes: state.sent, TotalBytes: state.total, SampledAt: time.Now().UTC()})
	}
}
