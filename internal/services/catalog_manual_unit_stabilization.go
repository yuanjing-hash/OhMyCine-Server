package services

import (
	"crypto/sha256"
	"fmt"
	"sort"
	"strings"

	"github.com/yuanjing-hash/OhMyCine-Server/internal/medialibrary"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
)

type existingRecognitionEntryIndex struct {
	byPath     map[string]models.MediaLibraryEntry
	byProvider map[string]models.MediaLibraryEntry
}

func indexExistingRecognitionEntries(entries []models.MediaLibraryEntry) existingRecognitionEntryIndex {
	index := existingRecognitionEntryIndex{byPath: map[string]models.MediaLibraryEntry{}, byProvider: map[string]models.MediaLibraryEntry{}}
	for _, entry := range entries {
		if entry.RecognitionID == nil {
			continue
		}
		index.byPath[entry.RelativePath] = entry
		if entry.ProviderID != "" {
			if previous, ok := index.byProvider[entry.ProviderID]; ok && previous.ID != entry.ID {
				index.byProvider[entry.ProviderID] = models.MediaLibraryEntry{}
			} else {
				index.byProvider[entry.ProviderID] = entry
			}
		}
	}
	return index
}

func existingRecognitionEntryID(file medialibrary.File, index existingRecognitionEntryIndex) uint {
	var entry models.MediaLibraryEntry
	if file.ProviderID != "" {
		entry = index.byProvider[file.ProviderID]
		// Provider identity takes precedence over a reused old path. An unknown
		// provider ID must never borrow the previous file's manual decision.
		if entry.ProviderID != file.ProviderID {
			return 0
		}
	} else {
		entry = index.byPath[file.RelativePath]
		if entry.ProviderID != "" {
			return 0
		}
		if entry.Size > 0 && entry.Size != file.Size {
			return 0
		}
		if !entry.ModifiedAt.IsZero() && !entry.ModifiedAt.Equal(file.ModifiedAt) {
			return 0
		}
	}
	if entry.RecognitionID == nil {
		return 0
	}
	return *entry.RecognitionID
}

// A user may correct one physical version while another version remains in the
// same directory. Preserve only exact existing manual membership, independently
// of the directory grouping used to identify newly discovered media.
func partitionExistingManualRecognitionUnits(units []medialibrary.RecognitionUnit, index existingRecognitionEntryIndex, records []models.MediaLibraryRecognition) []medialibrary.RecognitionUnit {
	manual := map[uint]models.MediaLibraryRecognition{}
	manualKeys := map[string]bool{}
	for _, record := range records {
		if record.ManualOverride {
			manual[record.ID] = record
			manualKeys[record.SourceKey] = true
		}
	}
	if len(manual) == 0 {
		return units
	}
	out := make([]medialibrary.RecognitionUnit, 0, len(units))
	manualPositions := map[uint]int{}
	// Ordinary directory-level TV overrides also cover newly added episodes.
	// File-selected reorganization overrides are different: they preserve only
	// explicit membership. An unchanged existing file must prove an unambiguous
	// one-directory/one-recognition mapping before extending the directory scope.
	extend := make([]uint, len(units))
	claims := map[uint]int{}
	for i, unit := range units {
		ids := map[uint]bool{}
		for _, file := range unit.Files {
			if id := existingRecognitionEntryID(file, index); id != 0 {
				ids[id] = true
			}
		}
		if len(ids) == 1 {
			for id := range ids {
				extend[i] = id
				claims[id]++
			}
		}
	}
	for i, unit := range units {
		remaining := make([]medialibrary.File, 0, len(unit.Files))
		for _, file := range unit.Files {
			id := existingRecognitionEntryID(file, index)
			if id == 0 {
				candidate := manual[extend[i]]
				_, reusedPath := index.byPath[file.RelativePath]
				if candidate.ID != 0 && candidate.MediaType == "tv" && claims[candidate.ID] == 1 && !strings.HasPrefix(candidate.SourceKey, "reorganization:") && !reusedPath {
					id = candidate.ID
				}
			}
			record, ok := manual[id]
			if !ok {
				remaining = append(remaining, file)
				continue
			}
			position, exists := manualPositions[id]
			if !exists {
				position = len(out)
				manualPositions[id] = position
				group := unit
				group.SourceKey = record.SourceKey
				group.Files = nil
				group.EvidenceFiles = nil
				out = append(out, group)
			}
			out[position].Files = append(out[position].Files, file)
		}
		if len(remaining) == 0 {
			continue
		}
		residual := unit
		residual.Files = remaining
		residual.EvidenceFiles = medialibrary.RecognitionEvidenceFiles(remaining)
		if manualKeys[residual.SourceKey] {
			// Merely retaining the generated directory key can hit bySource's
			// manual fast path even when no file passed the identity check.
			h := sha256.New()
			_, _ = fmt.Fprintln(h, unit.SourceKey)
			for _, file := range remaining {
				_, _ = fmt.Fprintf(h, "%s\x00%s\x00%d\x00%d\n", file.RelativePath, file.ProviderID, file.Size, file.ModifiedAt.UnixNano())
			}
			residual.SourceKey = fmt.Sprintf("unclaimed:%x", h.Sum(nil))
			residual.InputFingerprint = residual.SourceKey
		}
		out = append(out, residual)
	}
	for _, position := range manualPositions {
		group := &out[position]
		sort.Slice(group.Files, func(i, j int) bool { return group.Files[i].RelativePath < group.Files[j].RelativePath })
		group.EvidenceFiles = medialibrary.RecognitionEvidenceFiles(group.Files)
	}
	return out
}
