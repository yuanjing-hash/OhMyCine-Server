package pan115

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"

	pan115sdk "github.com/SheltonZhu/115driver/pkg/driver"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/cloud"
)

// ShareReadFailure contains diagnostic codes only, never the response or URL.
type ShareReadFailure struct {
	HTTPStatus   int
	ProviderCode int
}

func (e *ShareReadFailure) Error() string { return "115 share request failed" }

func ShareReadDiagnostics(err error) (httpStatus, providerCode int, coolingDown bool) {
	var failure *ShareReadFailure
	if errors.As(err, &failure) {
		httpStatus, providerCode = failure.HTTPStatus, failure.ProviderCode
	}
	return httpStatus, providerCode, errors.Is(err, errCircuitOpen)
}

// Override the SDK reader so HTTP status and provider codes are retained without
// embedding the raw response (including share secrets) in an error string.
func (s *sdkAdapter) GetShareSnapWithUA(ua, shareCode, receiveCode, dirID string, queries ...pan115sdk.Query) (*pan115sdk.ShareSnapResp, error) {
	query := map[string]string{"share_code": shareCode, "receive_code": receiveCode, "cid": dirID, "limit": "20", "offset": "0", "asc": "0", "format": "json"}
	for _, q := range queries {
		q(&query)
	}
	response, err := s.Client.R().SetQueryParams(query).
		SetHeader("referer", pan115sdk.BuildShareReferer(shareCode, receiveCode)).
		SetHeader("User-Agent", ua).SetDoNotParseResponse(true).Get(pan115sdk.ApiShareSnap)
	if err != nil {
		var urlErr *url.Error
		if errors.As(err, &urlErr) {
			err = urlErr.Err
		}
		return nil, mapError(err)
	}
	defer response.RawBody().Close()
	return decodeShareResponse(response.StatusCode(), response.RawBody())
}

func decodeShareResponse(status int, body io.Reader) (*pan115sdk.ShareSnapResp, error) {
	diagnostic := &ShareReadFailure{HTTPStatus: status}
	if status != http.StatusOK {
		code := cloud.CodeUnavailable
		if status == 405 || status == 429 {
			code = cloud.CodeRateLimited
		}
		if status == 401 {
			code = cloud.CodeAuthExpired
		}
		return nil, cloud.Error(code, code != cloud.CodeAuthExpired, diagnostic)
	}
	const limit = 4 << 20
	raw, err := io.ReadAll(io.LimitReader(body, limit+1))
	if err != nil {
		return nil, cloud.Error(cloud.CodeUnavailable, true, diagnostic)
	}
	if len(raw) > limit {
		return nil, cloud.Error(cloud.CodeResponseInvalid, false, diagnostic)
	}
	var envelope pan115sdk.BasicResp
	if json.Unmarshal(raw, &envelope) != nil {
		return nil, cloud.Error(cloud.CodeResponseInvalid, false, diagnostic)
	}
	if !envelope.State {
		diagnostic.ProviderCode = int(envelope.Errno)
		if diagnostic.ProviderCode == 0 {
			diagnostic.ProviderCode = envelope.ErrNo
		}
		code, retryable := shareProviderErrorCode(diagnostic.ProviderCode)
		return nil, cloud.Error(code, retryable, diagnostic)
	}
	var result pan115sdk.ShareSnapResp
	if json.Unmarshal(raw, &result) != nil {
		return nil, cloud.Error(cloud.CodeResponseInvalid, false, diagnostic)
	}
	return &result, nil
}

func shareProviderErrorCode(code int) (string, bool) {
	switch code {
	case 4100010, 4100026:
		return cloud.CodeShareExpired, false
	case 4100008:
		return cloud.CodeSharePassword, false
	case 4100009:
		return cloud.CodeShareInvalid, false
	case 99, 990001, 40101032, 40101035, 40101037:
		return cloud.CodeAuthExpired, false
	}
	// Unknown provider codes do not prove that a share has expired.
	return cloud.CodeUnavailable, true
}
