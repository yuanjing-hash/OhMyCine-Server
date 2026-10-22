package services

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/authz"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/plugins/contract"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/plugins/hostapi"
)

const maxOfflineUnits = 8192

type PluginOfflineGateway interface {
	OfflineAssetTransport(context.Context, string, string, string) (hostapi.OfflineTransport, error)
	ReadOfflineControl(context.Context, string, string, string, int64) ([]byte, string, error)
	DeriveOfflineAsset(context.Context, string, string, string, string, string) (string, error)
	OpenOfflineAsset(context.Context, string, string, string, string, string) (*hostapi.AssetStream, error)
	ReleaseOfflineAsset(string, string, string)
}

func WithPluginOfflineGateway(gateway PluginOfflineGateway) PluginServiceOption {
	return func(service *PluginRepositoryService) { service.offline = gateway }
}

type OfflineByteRange struct {
	Offset int64 `json:"offset"`
	Length int64 `json:"length"`
}
type OfflineEncryption struct {
	Method    string `json:"method"`
	KeyBase64 string `json:"keyBase64"`
	IVHex     string `json:"ivHex"`
}
type OfflineUnit struct {
	ID            string             `json:"id"`
	URL           string             `json:"url"`
	Headers       map[string]string  `json:"headers,omitempty"`
	ExpectedBytes int64              `json:"expectedBytes,omitempty"`
	ByteRange     *OfflineByteRange  `json:"byteRange,omitempty"`
	Encryption    *OfflineEncryption `json:"encryption,omitempty"`
}
type OfflineTrack struct {
	ID    string        `json:"id"`
	Kind  string        `json:"kind"`
	Units []OfflineUnit `json:"units"`
}
type OfflineSidecar struct {
	ID       string            `json:"id"`
	Kind     string            `json:"kind"`
	URL      string            `json:"url"`
	Headers  map[string]string `json:"headers,omitempty"`
	Format   string            `json:"format,omitempty"`
	Language string            `json:"language,omitempty"`
}

// PlayerOfflinePlan is an ephemeral, no-store native response. Never persist or
// log this structure: signed CDN URLs and HLS keys have transport lifetimes.
type PlayerOfflinePlan struct {
	Version           int              `json:"version"`
	LibraryID         string           `json:"libraryId"`
	WorkID            string           `json:"workId"`
	SegmentID         string           `json:"segmentId"`
	VersionID         string           `json:"versionId"`
	VariantID         string           `json:"variantId"`
	SuggestedFileName string           `json:"suggestedFileName"`
	Format            string           `json:"format"`
	ExpiresAt         int64            `json:"expiresAt"`
	Tracks            []OfflineTrack   `json:"tracks"`
	Sidecars          []OfflineSidecar `json:"sidecars"`
}

func canPlayerOffline(actor Actor) bool {
	return actor.Can(authz.PermissionMediaLibrariesRead) && actor.Can(authz.PermissionDownloadsCreate)
}

func (s *PluginRepositoryService) OfflineAvailable() bool { return s.offline != nil }

func (s *PluginRepositoryService) OnlineOfflinePlan(ctx context.Context, actor Actor, libraryID, workID, segmentID, versionID, variantID string) (PlayerOfflinePlan, error) {
	if !canPlayerOffline(actor) {
		return PlayerOfflinePlan{}, appError(CodePermissionDenied, "无权下载在线媒体到本地", nil)
	}
	if !safeOnlineText(workID, 512) || !safeOnlineText(segmentID, 512) || !safeOnlineText(versionID, 512) || !safeOnlineText(variantID, 512) {
		return PlayerOfflinePlan{}, appError(CodeInvalidRequest, "在线媒体离线下载选择无效", nil)
	}
	_, connection, manifest, err := s.onlineLibrary(libraryID)
	if err != nil {
		return PlayerOfflinePlan{}, err
	}
	if s.offline == nil {
		return PlayerOfflinePlan{}, appError(CodePluginRuntimeUnavailable, "在线媒体本地下载服务不可用", nil)
	}
	if !manifestHasPermission(manifest, contract.PermissionDownloadPlan) || !pluginHasActivePermission(s.db, connection.PluginID, contract.PermissionDownloadPlan) {
		return PlayerOfflinePlan{}, appError(CodePermissionDenied, "插件没有下载授权", nil)
	}
	ctx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	ctx = hostapi.WithOfflineProjection(ctx)
	operation := contract.CapabilityMediaOffline
	if !manifestHasCapability(manifest, operation) {
		if !manifestHasCapability(manifest, contract.CapabilityMediaDownload) {
			return PlayerOfflinePlan{}, appError(CodePermissionDenied, "插件不支持本地下载", nil)
		}
		operation = contract.CapabilityMediaDownload
	}
	raw, err := s.invokeOnline(ctx, actor, libraryID, operation, map[string]any{"itemId": workID, "segmentId": segmentID, "versionId": versionID, "variantId": variantID})
	if err != nil {
		return PlayerOfflinePlan{}, err
	}
	var plan contract.OfflineDownloadPlan
	if operation == contract.CapabilityMediaDownload {
		var legacy contract.DownloadPlan
		if err := decodeOfflineJSON(raw, &legacy); err != nil {
			return PlayerOfflinePlan{}, offlineInvalid(err)
		}
		if err := validateDownloadPlan(legacy, downloadSourceEnvelope{PluginItemID: workID, PluginSegmentID: segmentID, PluginVersionID: versionID, PluginVariantID: variantID}); err != nil {
			return PlayerOfflinePlan{}, offlineInvalid(err)
		}
		format := "progressive"
		if legacy.Merge != nil {
			format = "dash"
		}
		// The old contract has no HLS topology. Only its stated MP4 media or
		// explicit DASH pair can be safely adapted; never infer from playback.
		for _, asset := range legacy.Assets {
			if (asset.Kind == "video" || asset.Kind == "audio") && asset.ExpectedContentType != "video/mp4" && asset.ExpectedContentType != "audio/mp4" {
				return PlayerOfflinePlan{}, offlineInvalid(nil)
			}
		}
		plan = contract.OfflineDownloadPlan{Version: 1, WorkID: legacy.WorkID, SegmentID: legacy.SegmentID, VersionID: legacy.VersionID, VariantID: legacy.VariantID, SuggestedFileName: legacy.SuggestedFileName, Format: format, Assets: legacy.Assets}
	} else if err := decodeOfflineJSON(raw, &plan); err != nil {
		return PlayerOfflinePlan{}, offlineInvalid(err)
	}
	if err := validateOfflinePlan(plan, workID, segmentID, versionID, variantID, time.Now().UTC()); err != nil {
		return PlayerOfflinePlan{}, offlineInvalid(err)
	}
	expiry := time.Now().UTC().Add(15 * time.Minute).Unix()
	if plan.ExpiresAt != 0 && plan.ExpiresAt < expiry {
		expiry = plan.ExpiresAt
	}
	result := PlayerOfflinePlan{Version: 1, LibraryID: libraryID, WorkID: workID, SegmentID: segmentID, VersionID: versionID, VariantID: variantID, SuggestedFileName: plan.SuggestedFileName, Format: plan.Format, ExpiresAt: expiry, Tracks: []OfflineTrack{}, Sidecars: []OfflineSidecar{}}
	builder := offlineBuilder{gateway: s.offline, pluginID: connection.PluginID, connectionID: connection.ID, libraryID: libraryID, expiry: &result.ExpiresAt, keys: map[string][]byte{}, derived: map[string]bool{}}
	for _, asset := range plan.Assets {
		builder.derived[asset.URLRef] = false
	}
	complete := false
	defer func() { builder.cleanup(complete) }()
	for _, asset := range plan.Assets {
		if asset.Kind == "subtitle" || asset.Kind == "danmaku" {
			unit, err := builder.unit(ctx, asset.URLRef, asset.ID, asset.ExpectedBytes, nil, nil)
			if err != nil {
				return PlayerOfflinePlan{}, offlineInvalid(err)
			}
			result.Sidecars = append(result.Sidecars, OfflineSidecar{ID: asset.ID, Kind: asset.Kind, URL: unit.URL, Headers: unit.Headers, Format: offlineSidecarFormat(asset.ExpectedContentType)})
			continue
		}
		if plan.Format == "hls" {
			tracks, err := builder.hlsTracks(ctx, asset, 0)
			if err != nil {
				return PlayerOfflinePlan{}, offlineInvalid(err)
			}
			result.Tracks = append(result.Tracks, tracks...)
		} else {
			unit, err := builder.unit(ctx, asset.URLRef, asset.ID+":0", asset.ExpectedBytes, nil, nil)
			if err != nil {
				return PlayerOfflinePlan{}, offlineInvalid(err)
			}
			result.Tracks = append(result.Tracks, OfflineTrack{ID: asset.ID, Kind: asset.Kind, Units: []OfflineUnit{unit}})
		}
	}
	video, audio := 0, 0
	for _, track := range result.Tracks {
		if track.Kind == "video" {
			video++
		} else if track.Kind == "audio" {
			audio++
		}
	}
	if video != 1 || audio > 1 || result.ExpiresAt <= time.Now().UTC().Unix() {
		return PlayerOfflinePlan{}, offlineInvalid(nil)
	}
	complete = true
	return result, nil
}

func decodeOfflineJSON(raw []byte, target any) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return errors.New("trailing offline plan JSON")
	}
	return nil
}

func validateOfflinePlan(plan contract.OfflineDownloadPlan, workID, segmentID, versionID, variantID string, now time.Time) error {
	if plan.Version != 1 || plan.WorkID != workID || plan.SegmentID != segmentID || plan.VersionID != versionID || plan.VariantID != variantID || len(plan.Assets) < 1 || len(plan.Assets) > 18 {
		return errors.New("offline identity invalid")
	}
	if _, err := safeSuggestedFilename(plan.SuggestedFileName, variantID); err != nil {
		return err
	}
	if plan.ExpiresAt != 0 && (plan.ExpiresAt <= now.Unix() || plan.ExpiresAt > now.Add(24*time.Hour).Unix()) {
		return errors.New("offline expiry invalid")
	}
	ids, refs := map[string]bool{}, map[string]bool{}
	video, audio, sidecars := 0, 0, 0
	for _, asset := range plan.Assets {
		if !safeOnlineText(asset.ID, 128) || ids[asset.ID] || refs[asset.URLRef] || asset.HeadersRef != "" || !safeOptionalOnlineText(asset.ExpectedContentType, 128) || asset.ExpectedBytes < 0 || asset.ExpectedBytes > pluginDownloadMaxMediaBytes {
			return errors.New("offline asset invalid")
		}
		if _, err := uuid.Parse(asset.URLRef); err != nil {
			return errors.New("offline asset reference invalid")
		}
		ids[asset.ID], refs[asset.URLRef] = true, true
		switch asset.Kind {
		case "video":
			video++
		case "audio":
			audio++
		case "subtitle", "danmaku":
			sidecars++
			if asset.ExpectedBytes > pluginDownloadMaxSidecarBytes {
				return errors.New("offline sidecar too large")
			}
		default:
			return errors.New("offline asset kind invalid")
		}
	}
	if video != 1 || audio > 1 || sidecars > 16 {
		return errors.New("offline topology invalid")
	}
	switch plan.Format {
	case "progressive":
		if audio != 0 {
			return errors.New("progressive audio invalid")
		}
	case "dash":
		if audio != 1 {
			return errors.New("DASH audio missing")
		}
	case "hls":
	default:
		return errors.New("offline format unsupported")
	}
	return nil
}

func offlineInvalid(_ error) error {
	return appError(CodePluginResponseInvalid, "插件离线下载方案无效或媒体格式不受支持", nil)
}

func offlineSidecarFormat(contentType string) string {
	switch strings.ToLower(strings.TrimSpace(strings.Split(contentType, ";")[0])) {
	case "text/vtt":
		return "vtt"
	case "application/x-subrip":
		return "srt"
	case "text/x-ass", "text/x-ssa":
		return "ass"
	case "application/json":
		return "json"
	case "application/xml", "text/xml":
		return "xml"
	default:
		return ""
	}
}

func (s *PluginRepositoryService) OpenOnlineOfflineAsset(ctx context.Context, actor Actor, libraryID, reference, method, rangeHeader string) (*hostapi.AssetStream, error) {
	if !canPlayerOffline(actor) {
		return nil, appError(CodePermissionDenied, "无权下载在线媒体到本地", nil)
	}
	_, connection, manifest, err := s.onlineLibrary(libraryID)
	if err != nil {
		return nil, err
	}
	if s.offline == nil || !manifestHasPermission(manifest, contract.PermissionDownloadPlan) || (!manifestHasCapability(manifest, contract.CapabilityMediaOffline) && !manifestHasCapability(manifest, contract.CapabilityMediaDownload)) {
		return nil, appError(CodePermissionDenied, "插件不支持本地下载", nil)
	}
	if method != http.MethodGet && method != http.MethodHead {
		return nil, appError(CodeInvalidRequest, "本地下载请求无效", nil)
	}
	stream, err := s.offline.OpenOfflineAsset(ctx, connection.PluginID, connection.ID, reference, method, rangeHeader)
	if err != nil {
		return nil, appError(CodePluginAssetExpired, "本地下载资源不可用，请重新解析", nil)
	}
	return stream, nil
}
