package services

import (
	"encoding/json"

	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
	downloadpkg "github.com/yuanjing-hash/OhMyCine-Server/pkg/downloader"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/nodeprotocol"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/resourcecontent"
)

func validateStoredContentSelection(source downloadSourceEnvelope) error {
	if source.SelectionMode == "" && source.ContentSelection == nil && source.ContentSelectionDigest == "" {
		return nil
	}
	if (source.SelectionMode != "files" && source.SelectionMode != "verified_all") || source.ContentSelection == nil {
		return appError(CodeDownloadSourceInvalid, "精确文件选择记录丢失，已阻止整包下载", nil)
	}
	digest, err := source.ContentSelection.Digest()
	if err != nil || digest != source.ContentSelectionDigest {
		return appError(CodeDownloadSourceInvalid, "精确文件选择记录不一致，已阻止整包下载", err)
	}
	if source.SelectionMode == "verified_all" && !downloadpkg.VerifiedAllSource(downloadpkg.Source{Kind: source.Kind, URL: source.URL, Torrent: source.Torrent, ContentSelection: source.ContentSelection, AllFilesVerified: true}) {
		return appError(CodeDownloadSourceInvalid, "完整资源选择证明已丢失", nil)
	}
	return nil
}

func (s *DownloaderService) validateNodeSelectionCapability(record models.Downloader) error {
	if normalizeExecutionLocation(record.ExecutionLocation) != models.NodeLocationRemote {
		return nil
	}
	if s.transferNodes == nil || record.NodeID == nil {
		return appError(CodeDownloaderUnavailable, "所选节点不可用", nil)
	}
	caps, known := s.registry.Capabilities(record.Type)
	if !known || caps.RemoteSelectiveFilesCapability == "" {
		return appError("downloader_selective_files_unsupported", "所选执行器尚未提供节点精确文件选择与持久回执能力", nil)
	}
	_, node, err := s.transferNodes.nodeClient(*record.NodeID)
	if err != nil {
		return err
	}
	var capabilities nodeprotocol.Capabilities
	if json.Unmarshal([]byte(node.CapabilitiesJSON), &capabilities) != nil || !capabilities.Has(caps.RemoteSelectiveFilesCapability) {
		return appError("downloader_selective_files_unsupported", "请先升级传输节点：当前节点不能执行精确文件选择", nil)
	}
	if known && caps.SelectiveFileGroups && !capabilities.Has(nodeprotocol.CapabilitySelectiveFilesUnionV1) {
		return appError("downloader_selection_group_unsupported", "请先升级传输节点：当前节点不能维护共享文件选择", nil)
	}
	return nil
}

// validateContentSelectionExecutor is shared by route recommendations and
// final admission. It only tightens acquisition; cloud share leaf selections
// use their independent share-receive capability instead.
func (s *DownloaderService) validateContentSelectionExecutor(record models.Downloader, sources ...downloadpkg.Source) error {
	capabilities, known := s.registry.Capabilities(record.Type)
	if len(sources) > 0 && s.verifiedAllExecutor(record, sources[0]) {
		return nil
	}
	if !known || !capabilities.SelectiveFiles {
		return appError("downloader_selective_files_unsupported", "所选下载器尚不支持精确文件选择，请选择兼容下载器", nil)
	}
	if normalizeExecutionLocation(record.ExecutionLocation) == models.NodeLocationRemote && len(sources) > 0 && sources[0].ContentSelection != nil {
		if err := validateNodeSelectionWireBudget(sources[0], *sources[0].ContentSelection); err != nil {
			return err
		}
	}
	if normalizeExecutionLocation(record.ExecutionLocation) == models.NodeLocationServer {
		client, err := s.clientFor(record)
		if err != nil {
			return err
		}
		if available, ok := client.(downloadpkg.SelectiveFilesAvailability); ok && !available.SupportsSelectiveFiles() {
			return appError("downloader_selective_files_unsupported", "所选执行器缺少精确文件选择所需的持久操作收据", nil)
		}
	}
	return s.validateNodeSelectionCapability(record)
}

// Conservative pre-admission allowance includes source AEAD/base64, current
// union and the larger cleanup RPC. The client still checks the exact final
// wire body before a grant/action. No provider call is made for this check.
func validateNodeSelectionWireBudget(source downloadpkg.Source, union resourcecontent.Selection) error {
	plain, err := json.Marshal(nodeprotocol.DownloaderSource{Kind: source.Kind, URL: source.URL, Torrent: source.Torrent, Filename: source.Filename, ContentSelection: source.ContentSelection})
	if err != nil {
		return err
	}
	files, err := json.Marshal(union)
	if err != nil {
		return err
	}
	if ((len(plain)+16+2)/3)*4+len(files)*4+24*1024 > nodeprotocol.MaxWireBodyBytes {
		return appError("downloader_selection_wire_limit", "所选资源元数据超出节点的精确执行容量，请减少选择或使用兼容的本机执行器", nil)
	}
	return nil
}

func (s *DownloaderService) verifiedAllExecutor(record models.Downloader, source downloadpkg.Source) bool {
	capabilities, known := s.registry.Capabilities(record.Type)
	return known && capabilities.NativeOffline && normalizeExecutionLocation(record.ExecutionLocation) == models.NodeLocationServer && downloadpkg.VerifiedAllSource(source)
}
