package services

import (
	"context"
	"encoding/hex"
	"path"
	"strings"

	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
	cloudpkg "github.com/yuanjing-hash/OhMyCine-Server/pkg/cloud"
)

// Only provider-reported, valid SHA1s establish byte equivalence here. Size or
// codec labels are never substituted for a digest and no video is downloaded
// merely to compute one for washing.
func upgradeContentProvablyUnchanged(oldVideoHash, newVideoHash string, oldSubtitles, newSubtitles []string) bool {
	normalize := func(value string) string {
		value = strings.ToLower(strings.TrimSpace(value))
		raw, err := hex.DecodeString(value)
		if err != nil || len(raw) != 20 {
			return ""
		}
		return value
	}
	oldHash, newHash := normalize(oldVideoHash), normalize(newVideoHash)
	if oldHash == "" || newHash == "" || oldHash != newHash {
		return false
	}
	if len(newSubtitles) == 0 {
		return true
	}
	existing := map[string]bool{}
	for _, hash := range oldSubtitles {
		if hash = normalize(hash); hash != "" {
			existing[hash] = true
		}
	}
	for _, hash := range newSubtitles {
		hash = normalize(hash)
		if hash == "" || !existing[hash] {
			return false
		}
	}
	return true
}
func upgradeSubtitlePath(relative string) bool {
	switch strings.ToLower(path.Ext(relative)) {
	case ".srt", ".ass", ".ssa", ".vtt", ".sub", ".idx":
		return true
	}
	return false
}
func (s *MediaVersionUpgradeService) rejectUnchangedCloudReplacement(ctx context.Context, snapshot mediaUpgradeSnapshot, video cloudpkg.Item, managed []models.MediaManagedItem, driver cloudpkg.Driver, library models.MediaLibrary) error {
	if len(snapshot.Items) == 0 || snapshot.Items[0].Size != video.Size || !upgradeContentProvablyUnchanged(snapshot.Items[0].SHA1, video.SHA1, nil, nil) {
		return nil
	}
	oldSubtitles := []string{}
	for _, item := range snapshot.Items[1:] {
		if upgradeSubtitlePath(item.RelativePath) {
			oldSubtitles = append(oldSubtitles, item.SHA1)
		}
	}
	newSubtitles := []string{}
	for _, item := range managed {
		if !upgradeSubtitlePath(item.RelativePath) {
			continue
		}
		actual, err := providerItemWithinRoot(ctx, driver, item.ProviderItemID, library.ProviderRootID)
		if err != nil || actual.IsDir || actual.Size != item.Size {
			return mediaUpgradeChanged()
		}
		newSubtitles = append(newSubtitles, actual.SHA1)
	}
	if upgradeContentProvablyUnchanged(snapshot.Items[0].SHA1, video.SHA1, oldSubtitles, newSubtitles) {
		return appError(CodeMediaUpgradeSelection, "新选择的视频内容与旧版本相同，且没有新增或更换的字幕；原版本保留", nil)
	}
	return nil
}
