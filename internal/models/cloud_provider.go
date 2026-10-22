package models

const (
	StorageTypeQuark         = "quark"
	ConnectionProviderQuark  = "quark"
	DownloaderTypeQuarkShare = "quark_share"
)

// IsNativeCloudStorage identifies providers that resolve original files through
// the authenticated cloud driver. Provider-specific mutation capabilities remain
// independent; this does not imply offline, recycle or upload support.
func IsNativeCloudStorage(kind string) bool {
	return kind == StorageTypePan115 || kind == StorageTypeQuark
}
