package services

import downloadpkg "github.com/yuanjing-hash/OhMyCine-Server/pkg/downloader"

// A client's explicit whole-resource intent is separate from the private
// verified_all proof. This function never grants AllFilesVerified authority.
func validateSiteContentMode(input SiteDownloadInput, sourceKind string) error {
	selected := input.ContentSelectionToken != "" || input.PreviewToken != "" || len(input.SelectedEntryTokens) > 0
	switch input.ContentMode {
	case "":
		return nil // Existing clients retain their original manual contract.
	case "files":
		if !selected {
			return appError(CodeInvalidRequest, "请先选择实际文件", nil)
		}
	case "all":
		if selected || input.ReplacementIdentity != nil || input.FollowSubscriptionID != "" {
			return appError(CodeInvalidRequest, "整资源获取不能同时携带精确选择或自动替换意图", nil)
		}
		if sourceKind != "" && sourceKind != downloadpkg.SourceURL && sourceKind != downloadpkg.SourceTorrent {
			return appError(CodeDownloadSourceInvalid, "网盘分享必须选择实际文件，不能使用整资源降级", nil)
		}
	default:
		return appError(CodeInvalidRequest, "文件获取方式无效", nil)
	}
	return nil
}
