package pan115

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"

	crypto "github.com/SheltonZhu/115driver/pkg/crypto/m115"
	sdk "github.com/SheltonZhu/115driver/pkg/driver"
)

// Cookie protocol, separate from /open/offline OAuth APIs. The author's
// p115client Cookie implementation documents web torrent parsing and RSA ssp
// wanted submission. No new SDK/module/toolchain is introduced.
const selectiveTorrentEndpoint = "https://clouddownload.115.com/web/"
const selectiveSubmitEndpoint = "https://clouddownload.115.com/lixianssp/?ac=add_task_bt"
const maxSelectiveResponseBytes = 2 << 20

type selectedTorrentFile struct {
	Path string
	Size int64
}
type selectedTorrentInfo struct {
	InfoHash, Name string
	Files          []selectedTorrentFile
}
type selectedWantedRequest struct {
	InfoHash, DirectoryID, SavePath string
	Wanted                          []int
}
type selectedOfflineSDK interface {
	ParseOfflineTorrent(context.Context, string) (selectedTorrentInfo, error)
	AddOfflineWanted(context.Context, selectedWantedRequest) error
}
type selectedTorrentWire struct {
	sdk.BasicResp
	InfoHash string `json:"info_hash"`
	Name     string `json:"torrent_name"`
	Count    int    `json:"file_count"`
	Files    []struct {
		Path string        `json:"path"`
		Size sdk.IntString `json:"size"`
	} `json:"torrent_filelist"`
}

func readSelectedResponse(response *http.Response) ([]byte, error) {
	if response == nil || response.Body == nil {
		return nil, errors.New("115 response is absent")
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("115 selected offline returned HTTP %d", response.StatusCode)
	}
	raw, err := io.ReadAll(io.LimitReader(response.Body, maxSelectiveResponseBytes+1))
	if err != nil {
		return nil, err
	}
	if len(raw) > maxSelectiveResponseBytes {
		return nil, errors.New("115 selected offline response exceeded limit")
	}
	return raw, nil
}
func (s *sdkAdapter) ParseOfflineTorrent(ctx context.Context, sha1 string) (selectedTorrentInfo, error) {
	if len(sha1) != 40 {
		return selectedTorrentInfo{}, errors.New("115 torrent identity invalid")
	}
	response, err := s.NewRequest().SetContext(ctx).SetQueryParams(map[string]string{"ac": "torrent", "sha1": sha1}).SetDoNotParseResponse(true).Get(selectiveTorrentEndpoint)
	if err != nil {
		return selectedTorrentInfo{}, err
	}
	raw, err := readSelectedResponse(response.RawResponse)
	if err != nil {
		return selectedTorrentInfo{}, err
	}
	var result selectedTorrentWire
	if json.Unmarshal(raw, &result) != nil {
		return selectedTorrentInfo{}, errors.New("115 torrent table invalid")
	}
	if err = result.Err(); err != nil {
		return selectedTorrentInfo{}, err
	}
	if result.Count < 1 || result.Count > 10000 || result.Count != len(result.Files) {
		return selectedTorrentInfo{}, errors.New("115 torrent table incomplete")
	}
	info := selectedTorrentInfo{InfoHash: result.InfoHash, Name: result.Name}
	for _, row := range result.Files {
		size, e := strconv.ParseInt(string(row.Size), 10, 64)
		if e != nil || size < 0 {
			return selectedTorrentInfo{}, errors.New("115 torrent size invalid")
		}
		info.Files = append(info.Files, selectedTorrentFile{Path: row.Path, Size: size})
	}
	return info, nil
}
func (s *sdkAdapter) AddOfflineWanted(ctx context.Context, input selectedWantedRequest) error {
	if len(input.InfoHash) != 40 || input.DirectoryID == "" || input.SavePath == "" || len(input.Wanted) == 0 || len(input.Wanted) > 10000 || s.Client.RetryCount != 0 {
		return errors.New("115 wanted request or single-dispatch policy invalid")
	}
	uid := s.UserID
	if uid <= 0 {
		user, err := s.GetUser()
		if err != nil {
			return err
		}
		uid = user.UserID
	}
	wanted := append([]int(nil), input.Wanted...)
	sort.Ints(wanted)
	encoded := make([]string, len(wanted))
	for i, value := range wanted {
		if value < 0 || i > 0 && wanted[i-1] == value {
			return errors.New("115 wanted index invalid")
		}
		encoded[i] = strconv.Itoa(value)
	}
	appVersion := strings.TrimPrefix(sdk.UA115Browser, "Mozilla/5.0 115Browser/")
	params := map[string]string{"ac": "add_task_bt", "info_hash": input.InfoHash, "wanted": strings.Join(encoded, ","), "savepath": input.SavePath, "wp_path_id": input.DirectoryID, "uid": strconv.FormatInt(uid, 10), "app_ver": appVersion}
	raw, err := json.Marshal(params)
	if err != nil {
		return err
	}
	key := crypto.GenerateKey()
	payload := crypto.Encode(raw, key)
	response, err := s.NewRequest().SetContext(ctx).SetFormData(map[string]string{"data": payload}).SetDoNotParseResponse(true).Post(selectiveSubmitEndpoint)
	if err != nil {
		return err
	}
	raw, err = readSelectedResponse(response.RawResponse)
	if err != nil {
		return err
	}
	var result struct {
		sdk.BasicResp
		Data json.RawMessage `json:"data"`
	}
	if json.Unmarshal(raw, &result) != nil {
		return errors.New("115 wanted acknowledgement invalid")
	}
	if err = result.Err(); err != nil {
		return err
	}
	if len(result.Data) > 0 && string(result.Data) != "null" {
		var inner []byte
		var text string
		if json.Unmarshal(result.Data, &text) == nil {
			inner, err = crypto.Decode(text, key)
			if err != nil {
				return errors.New("115 wanted acknowledgement invalid")
			}
		} else {
			inner = result.Data
		}
		var proof struct {
			InfoHash string `json:"info_hash"`
		}
		if json.Unmarshal(inner, &proof) != nil {
			return errors.New("115 wanted acknowledgement invalid")
		}
		if proof.InfoHash != "" && !strings.EqualFold(proof.InfoHash, input.InfoHash) {
			return errors.New("115 wanted acknowledgement identity mismatch")
		}
	}
	return nil
}
