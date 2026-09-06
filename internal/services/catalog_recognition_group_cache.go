package services

import (
	"time"

	"github.com/yuanjing-hash/OhMyCine-Server/internal/medialibrary"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
)

type catalogRecognitionFileEvidence struct {
	id             uint
	path, provider string
	size           int64
	modified       time.Time
}

// A worker-local cache of PURE filename grouping, not recognized identities.
// Metadata publication/compaction cannot change that grouping. Every use still
// stabilizes against the latest manual records and selects current pending E.
// Physical changes or a new source/config discard it; nothing is persisted.
type catalogRecognitionGrouping struct {
	head   models.CatalogHead
	files  []catalogRecognitionFileEvidence
	units  []medialibrary.RecognitionUnit
	loaded bool
}

func (c *catalogRecognitionGrouping) groups(baseline catalogScanBaseline) []medialibrary.RecognitionUnit {
	same := c.loaded && c.head.LibraryID == baseline.head.LibraryID && c.head.SourceEpoch == baseline.head.SourceEpoch && c.head.SourceFingerprint == baseline.head.SourceFingerprint && c.head.ConfigFingerprint == baseline.head.ConfigFingerprint && len(c.files) == len(baseline.entries)
	if same {
		for i, entry := range baseline.entries {
			old := c.files[i]
			if old.id != entry.ID || old.path != entry.RelativePath || old.provider != entry.ProviderID || old.size != entry.Size || !old.modified.Equal(entry.ModifiedAt) {
				same = false
				break
			}
		}
	}
	if !same {
		files := make([]medialibrary.File, 0, len(baseline.entries))
		c.files = make([]catalogRecognitionFileEvidence, 0, len(baseline.entries))
		for _, entry := range baseline.entries {
			files = append(files, medialibrary.File{RelativePath: entry.RelativePath, ProviderID: entry.ProviderID, ProviderIDStable: true, Size: entry.Size, ModifiedAt: entry.ModifiedAt})
			c.files = append(c.files, catalogRecognitionFileEvidence{id: entry.ID, path: entry.RelativePath, provider: entry.ProviderID, size: entry.Size, modified: entry.ModifiedAt})
		}
		c.units = medialibrary.GroupRecognitionUnits(files)
		c.head, c.loaded = baseline.head, true
	}
	return stabilizeRecognitionUnits(c.units, baseline.entries, baseline.recognitions)
}
