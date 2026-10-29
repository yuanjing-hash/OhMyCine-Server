package services

import (
	"encoding/json"

	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
)

// Model API tags deliberately suppress provider IDs/work keys. A private
// persisted execution snapshot therefore uses its OWN explicit codec instead
// of serializing public models and silently losing those safety fences.
type mediaUpgradePrivateEntry struct {
	models.MediaLibraryEntry
	PrivateProviderID string `json:"private_provider_id"`
	PrivateWorkKey    string `json:"private_work_key"`
}
type mediaUpgradePrivateAsset struct {
	models.MediaLibrarySourceAsset
	PrivateProviderID string `json:"private_provider_id"`
	PrivateParentID   string `json:"private_parent_id"`
	PrivateHash       string `json:"private_hash"`
}
type mediaUpgradeSnapshotWire struct {
	Version int                        `json:"version"`
	Fence   catalogDeletionFence       `json:"fence"`
	Entry   mediaUpgradePrivateEntry   `json:"entry"`
	Assets  []mediaUpgradePrivateAsset `json:"assets"`
	Items   []mediaUpgradeRecycleItem  `json:"items"`
}

func (s mediaUpgradeSnapshot) MarshalJSON() ([]byte, error) {
	wire := mediaUpgradeSnapshotWire{Version: s.Version, Fence: s.Fence, Entry: mediaUpgradePrivateEntry{MediaLibraryEntry: s.Entry, PrivateProviderID: s.Entry.ProviderID, PrivateWorkKey: s.Entry.WorkKey}, Items: s.Items}
	for _, asset := range s.Assets {
		wire.Assets = append(wire.Assets, mediaUpgradePrivateAsset{MediaLibrarySourceAsset: asset, PrivateProviderID: asset.ProviderID, PrivateParentID: asset.ParentProviderID, PrivateHash: asset.HashHint})
	}
	return json.Marshal(wire)
}
func (s *mediaUpgradeSnapshot) UnmarshalJSON(raw []byte) error {
	var wire mediaUpgradeSnapshotWire
	if err := json.Unmarshal(raw, &wire); err != nil {
		return err
	}
	s.Version, s.Fence, s.Items = wire.Version, wire.Fence, wire.Items
	s.Entry = wire.Entry.MediaLibraryEntry
	s.Entry.ProviderID = wire.Entry.PrivateProviderID
	s.Entry.WorkKey = wire.Entry.PrivateWorkKey
	s.Assets = nil
	for _, private := range wire.Assets {
		asset := private.MediaLibrarySourceAsset
		asset.ProviderID, asset.ParentProviderID, asset.HashHint = private.PrivateProviderID, private.PrivateParentID, private.PrivateHash
		s.Assets = append(s.Assets, asset)
	}
	return nil
}
