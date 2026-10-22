package quark

import (
	"bytes"
	"context"
	"crypto/md5"
	"crypto/sha1"
	"crypto/sha256"
	"encoding"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/yuanjing-hash/OhMyCine-Server/pkg/cloud"
	"golang.org/x/net/http/httpguts"
)

type uploadDestination struct {
	Number    int    `json:"part_number"`
	URL       string `json:"upload_url"`
	Signature struct {
		Value string `json:"signature"`
	} `json:"signature_info"`
}
type uploadPart struct {
	Number int    `json:"part_number"`
	ETag   string `json:"etag"`
}
type uploadPayload struct {
	Parent        string                  `json:"parent"`
	Name          string                  `json:"name"`
	Size          int64                   `json:"size"`
	SHA1          string                  `json:"sha1"`
	MD5           string                  `json:"md5"`
	SHA256        string                  `json:"sha256"`
	TaskID        string                  `json:"task_id"`
	Handle        string                  `json:"handle"`
	PartSize      int64                   `json:"part_size"`
	Headers       map[string]string       `json:"common_headers"`
	Destinations  []uploadDestination     `json:"upload_urls"`
	Parts         []uploadPart            `json:"parts"`
	InFlightPart  int                     `json:"inflight_part"`
	Phase         string                  `json:"phase"`
	Output        *cloud.Item             `json:"output,omitempty"`
	Target        *operationTarget        `json:"target,omitempty"`
	Mapping       *operationMapping       `json:"mapping,omitempty"`
	SubmitFailure *operationSubmitFailure `json:"submit_failure,omitempty"`
	Observation   *uploadTaskObservation  `json:"task_observation,omitempty"`
}
type contextReader struct {
	ctx context.Context
	r   io.Reader
}

func (r contextReader) Read(p []byte) (int, error) {
	if e := r.ctx.Err(); e != nil {
		return 0, e
	}
	return r.r.Read(p)
}
func uploadHashes(ctx context.Context, r io.ReadSeeker, size int64) (string, string, string, [5]uint32, error) {
	var h [5]uint32
	if _, e := r.Seek(0, io.SeekStart); e != nil {
		return "", "", "", h, e
	}
	m, s, z := md5.New(), sha1.New(), sha256.New()
	n, e := io.Copy(io.MultiWriter(m, s, z), io.LimitReader(contextReader{ctx, r}, size+1))
	if e != nil || n != size {
		return "", "", "", h, failure(CodeInvalid)
	}
	state, e := s.(encoding.BinaryMarshaler).MarshalBinary()
	if e != nil || len(state) < 24 || string(state[:4]) != "sha\x01" {
		return "", "", "", h, failure(CodeInvalid)
	}
	for i := range h {
		h[i] = binary.BigEndian.Uint32(state[4+i*4 : 8+i*4])
	}
	return hex.EncodeToString(m.Sum(nil)), hex.EncodeToString(s.Sum(nil)), hex.EncodeToString(z.Sum(nil)), h, nil
}
func uploadProof(reader io.ReadSeeker, size int64, account, token string) (map[string]string, error) {
	double := func(s string) string {
		a := md5.Sum([]byte(s))
		b := md5.Sum([]byte(hex.EncodeToString(a[:])))
		return hex.EncodeToString(b[:])
	}
	seed1, seed2 := double(account+token), double(strconv.FormatInt(size, 10))
	out := map[string]string{"proof_version": "v1", "proof_seed1": seed1, "proof_seed2": seed2}
	for i, seed := range []string{seed1, seed2} {
		v, _ := strconv.ParseUint(seed[:16], 16, 64)
		offset := int64(v % uint64(size))
		if _, e := reader.Seek(offset, io.SeekStart); e != nil {
			return nil, e
		}
		data := make([]byte, min(int64(8), size-offset))
		if _, e := io.ReadFull(reader, data); e != nil {
			return nil, e
		}
		out[fmt.Sprintf("proof_code%d", i+1)] = base64.StdEncoding.EncodeToString(data)
	}
	return out, nil
}
func (d *Client) uploadPre(ctx context.Context, a Authorization, p uploadPayload, h [5]uint32, reader io.ReadSeeker) (json.RawMessage, error) {
	path := "/open/v1/file/upload_pre"
	timestamp := strconv.FormatInt(d.api.clock().UnixMilli(), 10)
	signature := sha256.Sum256([]byte("POST&" + path + "&" + timestamp + "&" + agentSignKey))
	proof, e := uploadProof(reader, p.Size, a.AccountID, hex.EncodeToString(signature[:]))
	if e != nil {
		return nil, &requestNotDispatchedError{Cause: e}
	}
	body := map[string]any{"file_name": p.Name, "size": p.Size, "sha1": p.SHA1, "md5": p.MD5, "pdir_fid": p.Parent, "format_type": "application/octet-stream", "parallel_upload": true, "hash_update": false}
	if p.Size <= 8*1024*1024 {
		body["part_info_list"] = []any{map[string]any{"part_number": 1, "part_size": p.Size, "parallel_sha1_ctx": map[string]any{"part_offset": 0, "h": h}}}
	}
	for k, v := range proof {
		body[k] = v
	}
	encoded, _ := json.Marshal(body)
	q := url.Values{"req_id": {randomID()}, "platform": {"web"}, "access_token": {a.AccessToken}, "device_id": {a.DeviceID}}
	request, _ := http.NewRequestWithContext(ctx, http.MethodPost, apiOrigin+path+"?"+q.Encode(), bytes.NewReader(encoded))
	sign(request, agentClientID, agentSignKey, timestamp)
	request.Header.Set("Authorization", "Bearer "+a.AccessToken)
	request.Header.Set("X-Agent-ID", agentID)
	request.Header.Set("User-Agent", "OhMyCine/Quark")
	raw, e := d.api.call(ctx, request)
	if e != nil {
		return nil, e
	}
	return unwrap(raw)
}
func (d *Client) saveUpload(ctx context.Context, r *cloud.OperationReceipt, p uploadPayload, state string, checkpoint bool) error {
	next := *r
	next.Revision++
	next.State = state
	next.UpdatedAt = d.api.clock()
	next.Payload, _ = json.Marshal(p)
	if checkpoint {
		c, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Second)
		defer cancel()
		ctx = c
	}
	if e := d.config.Operations.Update(ctx, r.Revision, next); e != nil {
		return operationError("upload_receipt_save", e)
	}
	*r = next
	return nil
}

func (d *Client) Upload(ctx context.Context, request cloud.UploadRequest) (cloud.Item, error) {
	return d.upload(ctx, operationID(ctx), request, false)
}
func (d *Client) upload(ctx context.Context, id string, request cloud.UploadRequest, challenge bool) (result cloud.Item, resultErr error) {
	var p uploadPayload
	var r cloud.OperationReceipt
	defer func() { resultErr = withUploadDiagnostic(r, p, resultErr) }()
	if d.config.Operations == nil || !scalar(id, 128) || !filename(request.Name) || request.Size <= 0 || request.Reader == nil {
		return cloud.Item{}, failure(CodeInvalid)
	}
	a, e := d.authorization(ctx, RouteAgent)
	if e != nil {
		return cloud.Item{}, e
	}
	parent, e := parseID(request.ParentID, RouteAgent, a.AccountID, false)
	if e != nil {
		return cloud.Item{}, e
	}
	if challenge {
		if request.Size != 32 || !strings.HasPrefix(request.Name, ".omc-verify-") {
			return cloud.Item{}, failure(CodeInvalid)
		}
	} else {
		f, e := d.managed(ctx, request.ParentID)
		if e != nil {
			return cloud.Item{}, e
		}
		if !f.Dir {
			return cloud.Item{}, failure(CodeIdentity)
		}
	}
	md, sh, sha, h, e := uploadHashes(ctx, request.Reader, request.Size)
	if e != nil {
		return cloud.Item{}, e
	}
	p = uploadPayload{Parent: parent, Name: request.Name, Size: request.Size, MD5: md, SHA1: sh, SHA256: sha, Phase: "prepare"}
	r, e = d.config.Operations.Load(ctx, id)
	if e == nil {
		if r.Provider != cloud.ProviderQuark || r.Route != string(RouteAgent) || r.AccountID != a.AccountID || r.Kind != "upload" || json.Unmarshal(r.Payload, &p) != nil || !sameAgent(p.Parent, parent) || p.Name != request.Name || p.Size != request.Size || p.SHA256 != sha {
			return cloud.Item{}, failure(CodeIdentity)
		}
		if challenge {
			if e = d.restoreChallengeTarget(ctx, &r, &p); e != nil {
				return cloud.Item{}, e
			}
		}
		if r.State == "confirmed" && p.Output != nil {
			return *p.Output, nil
		}
		prepared := challenge && r.State == "prepared" && p.Phase == "prepare"
		if !prepared && (r.State != "accepted" && r.State != "unknown" || p.Phase == "finish_inflight" || p.Handle == "" && (p.TaskID == "" || p.PartSize <= 0)) {
			if challenge {
				return d.recoverChallengeUpload(ctx, &r, p)
			}
			return cloud.Item{}, uploadUnknownError(p)
		}
	} else {
		if !errors.Is(e, cloud.ErrOperationNotFound) {
			return cloud.Item{}, operationError("upload_receipt_load", e)
		}
		entries, e := d.agentDirectory(ctx, parent)
		if e != nil {
			return cloud.Item{}, e
		}
		for _, f := range entries {
			if f.Name == request.Name {
				return cloud.Item{}, cloud.Error(cloud.CodeConflict, false, nil)
			}
		}
		if challenge {
			p.Target, e = d.freezeChallengeTarget(ctx, id, parent, request.Name, sha)
		} else {
			p.Target, e = d.freezeCreationTarget(ctx, parent)
		}
		if e != nil {
			return cloud.Item{}, e
		}
		// Check a renewed Node grant before creating the first durable intent.
		// A denied grant has not submitted anything and must remain retryable;
		// an already prepared/in-flight receipt keeps its no-replay semantics.
		if _, e := d.uploadAuthorization(ctx, a); e != nil {
			return cloud.Item{}, e
		}
		r = cloud.OperationReceipt{ID: id, Provider: cloud.ProviderQuark, Route: string(RouteAgent), AccountID: a.AccountID, Kind: "upload", Revision: 1, State: "prepared", UpdatedAt: d.api.clock()}
		r.Payload, _ = json.Marshal(p)
		if e = d.config.Operations.Create(ctx, r); e != nil {
			return cloud.Item{}, operationError("upload_receipt_create", e)
		}
	}
	// The prepared checkpoint precedes every provider dispatch. A restart may
	// continue that same intent; pre_inflight and later phases are query-only
	// until their original acknowledgement or exact challenge is recovered.
	if r.State == "prepared" && p.Phase == "prepare" {
		if challenge {
			target, err := d.freezeChallengeTarget(ctx, r.ID, p.Parent, p.Name, p.SHA256)
			if err != nil {
				return cloud.Item{}, err
			}
			if p.Target == nil || !p.Target.Challenge || p.Target.Path != target.Path || p.Target.TVParent != target.TVParent || p.Target.Proof.TVAccount != target.Proof.TVAccount || p.Target.Proof.AgentAccount != target.Proof.AgentAccount || !sameAgent(p.Target.AgentParent, target.AgentParent) {
				return cloud.Item{}, failure(CodeIdentity)
			}
		}
		current, e := d.uploadAuthorization(ctx, a)
		if e != nil {
			return cloud.Item{}, e
		}
		p.Phase = "pre_inflight"
		if e = d.saveUpload(ctx, &r, p, "submitting", false); e != nil {
			return cloud.Item{}, e
		}
		raw, e := d.uploadPre(ctx, current, p, h, request.Reader)
		if e != nil {
			var unsent *requestNotDispatchedError
			if errors.As(e, &unsent) {
				p.Phase = "prepare"
				if err := d.saveUpload(ctx, &r, p, "prepared", true); err != nil {
					return cloud.Item{}, err
				}
				return cloud.Item{}, operationError("upload_prepare", e)
			}
			p.SubmitFailure = uploadSubmitFailure(e)
			_ = d.saveUpload(ctx, &r, p, "unknown", true)
			return cloud.Item{}, uploadUnknownError(p)
		}
		var out struct {
			ID       string              `json:"fid"`
			Finish   json.RawMessage     `json:"finish"`
			Task     string              `json:"task_id"`
			PartSize int64               `json:"part_size"`
			Headers  map[string]string   `json:"common_headers"`
			URLs     []uploadDestination `json:"upload_urls"`
		}
		if json.Unmarshal(raw, &out) != nil {
			return cloud.Item{}, failure(CodeInvalid)
		}
		done := string(out.Finish) == "true" || string(out.Finish) == "1"
		if done {
			if !scalar(out.ID, 4096) {
				return cloud.Item{}, failure(CodeInvalid)
			}
			p.Handle = out.ID
			p.Phase = "finish_received"
		} else {
			if !scalar(out.Task, 4096) || out.PartSize <= 0 || out.PartSize > 1024*1024*1024 || out.PartSize%64 != 0 && request.Size > out.PartSize {
				return cloud.Item{}, failure(CodeInvalid)
			}
			p.TaskID, p.PartSize, p.Headers, p.Destinations = out.Task, out.PartSize, out.Headers, out.URLs
			p.Phase = "pre_received"
		}
		if e = d.saveUpload(ctx, &r, p, "accepted", true); e != nil {
			return cloud.Item{}, e
		}
	}
	if ctx.Err() != nil {
		return cloud.Item{}, ctx.Err()
	}
	if p.Handle == "" {
		if p.InFlightPart != 0 {
			if e = d.recoverUploadedPart(ctx, &r, &p); e != nil {
				return cloud.Item{}, e
			}
			if p.Handle != "" {
				result, err := d.confirmUpload(ctx, &r, p)
				return result, operationError("upload_output_confirmation", err)
			}
		}
		total := 1 + (p.Size-1)/p.PartSize
		if total > 10000 {
			return cloud.Item{}, failure(CodeUnsupported)
		}
		prefix := newUploadPrefixSHA1(request.Reader)
		for n := 1; n <= int(total); n++ {
			done := false
			for _, part := range p.Parts {
				if part.Number == n {
					done = true
				}
			}
			if done {
				continue
			}
			dest, e := d.uploadPartURLWithPrefix(ctx, a, &p, n, prefix)
			if e != nil {
				return cloud.Item{}, e
			}
			offset := int64(n-1) * p.PartSize
			size := min(p.PartSize, p.Size-offset)
			position, e := request.Reader.Seek(offset, io.SeekStart)
			if e != nil || position != offset {
				return cloud.Item{}, failure(CodeInvalid)
			}
			current, e := d.uploadAuthorization(ctx, a)
			if e != nil {
				return cloud.Item{}, e
			}
			partRequest, e := prepareUploadPartRequest(ctx, dest, p.Headers, request.Reader, size, current)
			if e != nil {
				return cloud.Item{}, operationError("upload_part_prepare", e)
			}
			previousPhase := p.Phase
			p.InFlightPart = n
			p.Phase = "put_inflight"
			if e = d.saveUpload(ctx, &r, p, "accepted", false); e != nil {
				return cloud.Item{}, e
			}
			etag, e := d.sendUploadPart(ctx, partRequest)
			if e != nil {
				var unsent *requestNotDispatchedError
				if errors.As(e, &unsent) {
					p.InFlightPart, p.Phase = 0, previousPhase
					if err := d.saveUpload(ctx, &r, p, "accepted", true); err != nil {
						return cloud.Item{}, err
					}
					return cloud.Item{}, operationError("upload_part_prepare", e)
				}
				p.SubmitFailure = uploadSubmitFailure(e)
				_ = d.saveUpload(ctx, &r, p, "unknown", true)
				return cloud.Item{}, uploadUnknownError(p)
			}
			p.Parts = append(p.Parts, uploadPart{Number: n, ETag: etag})
			p.InFlightPart = 0
			p.Phase = "put_received"
			if e = d.saveUpload(ctx, &r, p, "accepted", true); e != nil {
				return cloud.Item{}, e
			}
		}
		current, e := d.uploadAuthorization(ctx, a)
		if e != nil {
			return cloud.Item{}, e
		}
		p.Phase = "finish_inflight"
		if e = d.saveUpload(ctx, &r, p, "accepted", false); e != nil {
			return cloud.Item{}, e
		}
		raw, e := d.api.agent(ctx, current, http.MethodPost, "/open/v1/file/upload_finish", nil, map[string]any{"task_id": p.TaskID, "part_info_list": p.Parts})
		if e != nil {
			var unsent *requestNotDispatchedError
			if errors.As(e, &unsent) {
				p.Phase = "put_received"
				if err := d.saveUpload(ctx, &r, p, "accepted", true); err != nil {
					return cloud.Item{}, err
				}
				return cloud.Item{}, operationError("upload_finish", e)
			}
			p.SubmitFailure = uploadSubmitFailure(e)
			_ = d.saveUpload(ctx, &r, p, "unknown", true)
			return cloud.Item{}, uploadUnknownError(p)
		}
		var out struct {
			ID     string          `json:"fid"`
			Finish json.RawMessage `json:"finish"`
		}
		if json.Unmarshal(raw, &out) != nil || !scalar(out.ID, 4096) || string(out.Finish) != "true" && string(out.Finish) != "1" {
			return cloud.Item{}, operationError("upload_finish_response", failure(cloud.CodeMutationUnknown))
		}
		p.Handle = out.ID
		p.Phase = "finish_received"
		if e = d.saveUpload(ctx, &r, p, "accepted", true); e != nil {
			return cloud.Item{}, e
		}
	}
	result, e = d.confirmUpload(ctx, &r, p)
	return result, operationError("upload_output_confirmation", e)
}

// Long uploads must not finish with the authorization captured before hashing
// and all PUTs. Renewal may replace tokens, but never the operation's identity.
func (d *Client) uploadAuthorization(ctx context.Context, expected Authorization) (Authorization, error) {
	current, err := d.authorization(ctx, RouteAgent)
	if err != nil {
		return Authorization{}, err
	}
	if current.AccountID != expected.AccountID || current.DeviceID != expected.DeviceID || current.ClientDeviceID != expected.ClientDeviceID {
		return Authorization{}, failure(CodeIdentity)
	}
	return current, nil
}

// Querying an upload never sends another byte or repeats upload_finish. A
// saved provider output handle is sufficient to resume metadata confirmation;
// missing acknowledgements remain explicit unknown outcomes.
func (d *Client) queryUpload(ctx context.Context, r cloud.OperationReceipt) (result cloud.OperationResult, resultErr error) {
	var p uploadPayload
	defer func() { resultErr = withUploadDiagnostic(r, p, resultErr) }()
	if json.Unmarshal(r.Payload, &p) != nil {
		return cloud.OperationResult{Receipt: r}, failure(CodeInvalid)
	}
	if p.Target == nil && p.Size == 32 && IsVerificationName(p.Name) && strings.HasSuffix(r.ID, "/upload") {
		if err := d.restoreChallengeTarget(ctx, &r, &p); err != nil {
			return cloud.OperationResult{Receipt: r}, err
		}
	}
	if r.State == "confirmed" && p.Output != nil {
		return cloud.OperationResult{Receipt: r, Items: []cloud.Item{*p.Output}, Complete: true}, nil
	}
	if r.State != "accepted" && r.State != "unknown" || p.Handle == "" {
		if p.Target != nil && p.Target.Challenge && (p.Phase == "pre_inflight" || p.Phase == "finish_inflight") {
			item, err := d.recoverChallengeUpload(ctx, &r, p)
			if err != nil {
				return cloud.OperationResult{Receipt: r}, err
			}
			return cloud.OperationResult{Receipt: r, Items: []cloud.Item{item}, Complete: true}, nil
		}
		if p.TaskID != "" && (r.State == "accepted" || r.State == "unknown") {
			status, err := d.readUploadTask(ctx, &p)
			if err != nil {
				return cloud.OperationResult{Receipt: r}, err
			}
			if status.Handle != "" {
				p.Handle, p.InFlightPart, p.Phase = status.Handle, 0, "finish_received"
				if err = d.saveUpload(ctx, &r, p, "accepted", false); err != nil {
					return cloud.OperationResult{Receipt: r}, err
				}
				item, err := d.confirmUpload(ctx, &r, p)
				return cloud.OperationResult{Receipt: r, Items: []cloud.Item{item}, Complete: err == nil}, operationError("upload_output_confirmation", err)
			}
			return cloud.OperationResult{Receipt: r}, operationError("upload_part_recovery", uploadUnknownFailure(p))
		}
		return cloud.OperationResult{Receipt: r}, uploadUnknownError(p)
	}
	item, e := d.confirmUpload(ctx, &r, p)
	if e != nil {
		return cloud.OperationResult{Receipt: r}, e
	}
	return cloud.OperationResult{Receipt: r, Items: []cloud.Item{item}, Complete: true}, nil
}
func (d *Client) confirmUpload(ctx context.Context, r *cloud.OperationReceipt, p uploadPayload) (cloud.Item, error) {
	a, e := d.authorization(ctx, RouteAgent)
	if e != nil || r.AccountID != a.AccountID {
		return cloud.Item{}, failure(CodeIdentity)
	}
	item, e := d.agentInfo(ctx, p.Handle)
	if e != nil {
		return cloud.Item{}, e
	}
	if item.Name != p.Name || item.Size != p.Size || item.Dir || sameAgentDomain(item.Parent, p.Parent) && !sameAgent(item.Parent, p.Parent) {
		return cloud.Item{}, failure(CodePending)
	}
	entries, e := d.agentDirectory(ctx, p.Parent)
	if e != nil {
		return cloud.Item{}, e
	}
	matches := 0
	var listed agentItem
	for _, f := range entries {
		if f.Name == p.Name {
			matches++
			listed = f
			if f.Size != p.Size || f.Dir || sameAgentDomain(f.ID, item.ID) && !sameAgent(f.ID, item.ID) {
				return cloud.Item{}, failure(CodeIdentity)
			}
		}
	}
	if matches != 1 {
		return cloud.Item{}, failure(CodePending)
	}
	if !sameAgent(listed.ID, item.ID) || !sameAgent(item.Parent, p.Parent) {
		p.Mapping, e = d.confirmCreationMapping(ctx, *r, p.Target, item, listed, p.SHA256, p.Mapping)
		if e != nil {
			return cloud.Item{}, e
		}
	}
	result := toCloudAgent(a.AccountID, item)
	p.Output = &result
	p.Phase = "metadata_confirmed"
	if e = d.saveUpload(ctx, r, p, "confirmed", false); e != nil {
		return cloud.Item{}, e
	}
	return result, nil
}

func (d *Client) recoverUploadedPart(ctx context.Context, r *cloud.OperationReceipt, p *uploadPayload) error {
	out, err := d.readUploadTask(ctx, p)
	if err != nil {
		return err
	}
	if out.Handle != "" {
		p.Handle, p.InFlightPart, p.Phase = out.Handle, 0, "finish_received"
		return d.saveUpload(ctx, r, *p, "accepted", false)
	}
	for _, part := range out.Parts {
		if part.Number == p.InFlightPart && scalar(part.ETag, 1024) {
			p.Parts = append(p.Parts, part)
			p.InFlightPart = 0
			p.Phase = "put_received"
			return d.saveUpload(ctx, r, *p, "accepted", false)
		}
	}
	// An incomplete or eventually consistent query cannot authorize another PUT.
	return operationError("upload_part_recovery", uploadUnknownFailure(*p))
}
func (d *Client) uploadPartURL(ctx context.Context, a Authorization, p *uploadPayload, n int, reader io.ReadSeeker) (uploadDestination, error) {
	return d.uploadPartURLWithPrefix(ctx, a, p, n, newUploadPrefixSHA1(reader))
}

func (d *Client) uploadPartURLWithPrefix(ctx context.Context, a Authorization, p *uploadPayload, n int, prefix *uploadPrefixSHA1) (uploadDestination, error) {
	if n < 1 || p.Size <= 0 || p.PartSize <= 0 || int64(n-1) > (p.Size-1)/p.PartSize {
		return uploadDestination{}, failure(CodeInvalid)
	}
	for _, dest := range p.Destinations {
		if dest.Number == n || len(p.Destinations) == 1 && n == 1 && dest.Number == 0 {
			return dest, nil
		}
	}
	offset := int64(n-1) * p.PartSize
	info := map[string]any{"part_number": n, "part_size": min(p.PartSize, p.Size-offset)}
	if n > 1 {
		words, e := prefix.at(ctx, offset)
		if e != nil {
			return uploadDestination{}, e
		}
		info["parallel_sha1_ctx"] = map[string]any{"part_offset": offset, "h": words}
	}
	raw, e := d.agentRead(ctx, http.MethodPost, "/open/v1/file/get_upload_urls", nil, map[string]any{"task_id": p.TaskID, "part_info_list": []any{info}})
	if e != nil {
		return uploadDestination{}, e
	}
	var out struct {
		URLs    []uploadDestination `json:"upload_urls"`
		Headers map[string]string   `json:"common_headers"`
	}
	if json.Unmarshal(raw, &out) != nil || len(out.URLs) != 1 || out.URLs[0].Number != n {
		return uploadDestination{}, failure(CodeInvalid)
	}
	if out.Headers != nil {
		p.Headers = out.Headers
	}
	return out.URLs[0], nil
}
func (d *Client) putPart(ctx context.Context, destination uploadDestination, common map[string]string, reader io.Reader, size int64, a Authorization) (string, error) {
	request, err := prepareUploadPartRequest(ctx, destination, common, reader, size, a)
	if err != nil {
		return "", err
	}
	return d.sendUploadPart(ctx, request)
}

func prepareUploadPartRequest(ctx context.Context, destination uploadDestination, common map[string]string, reader io.Reader, size int64, a Authorization) (*http.Request, error) {
	u, e := directURL(destination.URL)
	if e != nil || !scalar(destination.Signature.Value, 8192) || len(common) > 32 {
		return nil, failure(CodeInvalid)
	}
	// Upload credentials belong only to the provider-issued object endpoint.
	// Reject core account credentials, cookies and control/signature headers.
	headers := http.Header{}
	for k, v := range common {
		lower := strings.ToLower(k)
		if !httpguts.ValidHeaderFieldName(k) || !httpguts.ValidHeaderFieldValue(v) || len(v) > 8192 {
			return nil, failure(CodeInvalid)
		}
		switch lower {
		case "cookie", "authorization", "proxy-authorization", "host", "x-pan-token", "x-pan-client-id", "x-agent-id", "content-length", "connection", "transfer-encoding":
			return nil, failure(CodeInvalid)
		}
		if a.AccessToken != "" && strings.Contains(v, a.AccessToken) || a.RefreshToken != "" && strings.Contains(v, a.RefreshToken) {
			return nil, failure(CodeInvalid)
		}
		headers.Set(k, v)
	}
	if a.AccessToken != "" && strings.Contains(destination.Signature.Value, a.AccessToken) || a.RefreshToken != "" && strings.Contains(destination.Signature.Value, a.RefreshToken) {
		return nil, failure(CodeInvalid)
	}
	headers.Set("Authorization", destination.Signature.Value)
	headers.Set("Accept-Encoding", "identity")
	request, e := http.NewRequestWithContext(ctx, http.MethodPut, u.String(), io.LimitReader(contextReader{ctx, reader}, size))
	if e != nil {
		return nil, failure(CodeInvalid)
	}
	request.Header = headers
	request.ContentLength = size
	request.GetBody = nil
	return request, nil
}

func (d *Client) sendUploadPart(ctx context.Context, request *http.Request) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", &requestNotDispatchedError{Cause: err}
	}
	client := &http.Client{Transport: d.mediaTransport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	response, e := client.Do(request)
	if e != nil {
		return "", failure(CodeUnavailable)
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return "", &Failure{Code: CodeUnavailable, HTTPStatus: response.StatusCode}
	}
	n, e := io.Copy(io.Discard, io.LimitReader(response.Body, apiLimit+1))
	if e != nil || n > apiLimit {
		return "", failure(CodeUnavailable)
	}
	etag := response.Header.Get("ETag")
	if !scalar(etag, 1024) {
		return "", failure(CodeInvalid)
	}
	return etag, nil
}

var _ cloud.UploadDriver = (*Client)(nil)
