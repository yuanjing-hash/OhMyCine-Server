package pan115

import (
	"context"
	"encoding/json"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/cloud"
	"io"
	"net/http"
	"strconv"
)

// This is the Cookie App schema, not web files/get_info or its undocumented
// STA field. Primary protocol references are recorded in docs/node-protocol.md.
const finalizedFilesEndpoint = "https://proapi.115.com/android/2.0/ufile/files"
const maxFinalizationPages = 25

type finalizedAppSDK interface {
	finalizedAppPage(context.Context, string, int64) ([]byte, error)
}

func (s *sdkAdapter) finalizedAppPage(ctx context.Context, parent string, offset int64) ([]byte, error) {
	response, err := s.NewRequest().SetContext(ctx).SetQueryParams(map[string]string{"aid": "1", "cid": parent, "cur": "1", "show_dir": "1", "count_folders": "1", "limit": "200", "offset": strconv.FormatInt(offset, 10), "o": "file_name", "asc": "1", "custom_order": "2", "fields": "fid,pid,fn,fc,fs,aid,fta"}).SetDoNotParseResponse(true).Get(finalizedFilesEndpoint)
	if err != nil {
		return nil, err
	}
	if response == nil || response.RawResponse == nil {
		return nil, nil
	}
	defer response.RawResponse.Body.Close()
	if response.StatusCode() == http.StatusTooManyRequests || response.StatusCode() == http.StatusMethodNotAllowed {
		return nil, cloud.Error(cloud.CodeRateLimited, true, nil)
	}
	// An App-specific rejected Cookie/schema is unavailable optional proof. It
	// must not mark otherwise usable web/offline credentials globally expired.
	if response.StatusCode() >= 500 {
		return nil, &offlineWireFailure{operation: "optional_file_status", responseKind: "http_error", httpStatus: response.StatusCode(), transient: true}
	}
	if response.StatusCode() != http.StatusOK {
		return nil, nil
	}
	raw, err := io.ReadAll(io.LimitReader(response.RawResponse.Body, maxSelectiveResponseBytes+1))
	if err != nil {
		return nil, err
	}
	if len(raw) > maxSelectiveResponseBytes {
		return nil, nil
	}
	return raw, nil
}
func appScalar(raw json.RawMessage) (string, bool) {
	if len(raw) == 0 {
		return "", false
	}
	var text string
	if json.Unmarshal(raw, &text) == nil {
		return text, true
	}
	var n json.Number
	if json.Unmarshal(raw, &n) == nil {
		if _, err := strconv.ParseInt(string(n), 10, 64); err == nil {
			return string(n), true
		}
	}
	return "", false
}

// parseFinalizedAppPage preserves field presence. Only an explicit FTA=1 on a
// normal leaf is finalized; 0/2 are pending; absent/null/unknown are unknown.
func parseFinalizedAppPage(raw []byte, parent string) ([]cloud.FinalizedFile, int64, bool) {
	var body struct {
		State *bool                        `json:"state"`
		Count json.RawMessage              `json:"count"`
		Data  []map[string]json.RawMessage `json:"data"`
	}
	if json.Unmarshal(raw, &body) != nil || body.State == nil || !*body.State || body.Data == nil {
		return nil, 0, false
	}
	countText, ok := appScalar(body.Count)
	if !ok {
		return nil, 0, false
	}
	count, err := strconv.ParseInt(countText, 10, 64)
	if err != nil || count < 0 || count > 5000 || len(body.Data) > 200 {
		return nil, 0, false
	}
	result := make([]cloud.FinalizedFile, 0, len(body.Data))
	seen := map[string]bool{}
	for _, row := range body.Data {
		id, a := appScalar(row["fid"])
		pid, b := appScalar(row["pid"])
		name, d := appScalar(row["fn"])
		fc, e := appScalar(row["fc"])
		sizeText, f := appScalar(row["fs"])
		aid, g := appScalar(row["aid"])
		if !a || !b || !d || !e || !f || !g || !selectedScalar(id) || pid != parent || !selectedName(name) || fc != "0" && fc != "1" || aid != "1" || seen[id] {
			return nil, 0, false
		}
		seen[id] = true
		size, err := strconv.ParseInt(sizeText, 10, 64)
		if err != nil || size < 0 {
			return nil, 0, false
		}
		state := cloud.OfflineFinalizationUnknown
		if fc == "1" {
			if fta, present := appScalar(row["fta"]); present {
				switch fta {
				case "1":
					state = cloud.OfflineFinalizationComplete
				case "0", "2":
					state = cloud.OfflineFinalizationPending
				}
			}
		}
		result = append(result, cloud.FinalizedFile{Item: cloud.Item{ID: id, ParentID: pid, Name: name, IsDir: fc == "0", Size: size}, State: state})
	}
	return result, count, true
}
func (c *Client) FinalizedFiles(ctx context.Context, expected []cloud.Item) ([]cloud.FinalizedFile, error) {
	sdk, ok := c.sdk.(finalizedAppSDK)
	if !ok || len(expected) == 0 || len(expected) > 1000 {
		return nil, nil
	}
	parents := map[string]map[string]cloud.Item{}
	seen := map[string]bool{}
	for _, leaf := range expected {
		if leaf.IsDir || !selectedScalar(leaf.ID) || !selectedScalar(leaf.ParentID) || !selectedName(leaf.Name) || leaf.Size < 0 || seen[leaf.ID] {
			return nil, nil
		}
		seen[leaf.ID] = true
		if parents[leaf.ParentID] == nil {
			parents[leaf.ParentID] = map[string]cloud.Item{}
		}
		parents[leaf.ParentID][leaf.ID] = leaf
	}
	var result []cloud.FinalizedFile
	pages := 0
	for parent, wanted := range parents {
		observed := map[string]bool{}
		var count int64 = -1
		for offset := int64(0); ; {
			pages++
			if pages > maxFinalizationPages {
				return nil, nil
			}
			var raw []byte
			err := c.waitReadAndCall(ctx, c.readLimiter(ctx), func() error { var err error; raw, err = sdk.finalizedAppPage(ctx, parent, offset); return err })
			if err != nil {
				return nil, mapError(err)
			}
			rows, total, valid := parseFinalizedAppPage(raw, parent)
			if !valid || count >= 0 && count != total {
				return nil, nil
			}
			count = total
			if len(rows) == 0 && offset < count {
				return nil, nil
			}
			for _, row := range rows {
				if observed[row.Item.ID] {
					return nil, nil
				}
				observed[row.Item.ID] = true
				if leaf, found := wanted[row.Item.ID]; found {
					if row.Item.IsDir || leaf.Name != row.Item.Name || leaf.Size != row.Item.Size || leaf.ParentID != row.Item.ParentID {
						return nil, nil
					}
					row.Item = leaf
					result = append(result, row)
				}
			}
			offset += int64(len(rows))
			if offset >= count {
				if offset != count {
					return nil, nil
				}
				break
			}
		}
	}
	if len(result) != len(expected) {
		return nil, nil
	}
	return result, nil
}

var _ cloud.FinalizedFileReader = (*Client)(nil)
