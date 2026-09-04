package medialibrary

import (
	"crypto/sha256"
	"encoding/hex"
	"path"
	"sort"
	"strconv"
	"strings"
	"time"
)

// MaxRecognitionEvidenceFiles bounds the work-level parser input. Files keeps
// the complete projection set, while EvidenceFiles is the only slice that may
// cross into remote work recognition.
const MaxRecognitionEvidenceFiles = 32

// RecognitionUnit is a provider-neutral group of file facts that represents
// one TMDB lookup. SourceKey is an opaque, library-scoped stable identifier;
// callers must not expose it as a provider item ID or path.
type RecognitionUnit struct {
	SourceKey        string
	InputFingerprint string
	PackageName      string
	MediaTypeHint    string
	Files            []File
	EvidenceFiles    []File
}

// GroupRecognitionUnits groups already-enumerated facts without performing
// network I/O. It deliberately uses only provider-relative structure and the
// existing filename parser's structural hints; final identity remains the
// responsibility of the shared recognizer.
func GroupRecognitionUnits(files []File) []RecognitionUnit {
	type bucket struct {
		key       string
		name      string
		mediaType string
		files     []File
	}
	buckets := make(map[string]*bucket)
	for _, file := range files {
		relative := normalizeFactPath(file.RelativePath)
		parsed := ParseMedia(path.Base(relative), relative)
		key, name := recognitionGroup(relative, parsed)
		bucketKey := parsed.MediaType + "\x00" + key
		current := buckets[bucketKey]
		if current == nil {
			current = &bucket{key: key, name: name, mediaType: parsed.MediaType}
			buckets[bucketKey] = current
		}
		current.files = append(current.files, file)
	}

	units := make([]RecognitionUnit, 0, len(buckets))
	for _, current := range buckets {
		sort.Slice(current.files, func(i, j int) bool {
			return normalizeFactPath(current.files[i].RelativePath) < normalizeFactPath(current.files[j].RelativePath)
		})
		units = append(units, RecognitionUnit{
			SourceKey:        recognitionSourceKey(current.key, current.files),
			InputFingerprint: recognitionInputFingerprint(current.key, current.name, current.mediaType, current.files),
			PackageName:      current.name,
			MediaTypeHint:    current.mediaType,
			Files:            append([]File(nil), current.files...),
			EvidenceFiles:    RecognitionEvidenceFiles(current.files),
		})
	}
	sort.Slice(units, func(i, j int) bool {
		if units[i].PackageName == units[j].PackageName {
			return units[i].SourceKey < units[j].SourceKey
		}
		return units[i].PackageName < units[j].PackageName
	})
	return units
}

func recognitionGroup(relative string, parsed ParsedMedia) (string, string) {
	directory := path.Dir(relative)
	if outer, ok := discOuterDirectory(relative); ok {
		return "disc:" + outer, path.Base(outer)
	}
	if parsed.MediaType == "tv" {
		seriesDirectory := directory
		if _, ok := seasonFolderNumber(path.Base(directory)); ok {
			seriesDirectory = path.Dir(directory)
		}
		if seriesDirectory == "." || seriesDirectory == "/" {
			name := strings.TrimSpace(parsed.SeriesTitle)
			if name == "" {
				name = strings.TrimSuffix(path.Base(relative), path.Ext(relative))
			}
			return "tv-root:" + strings.ToLower(name), name
		}
		return "tv-dir:" + seriesDirectory, path.Base(seriesDirectory)
	}
	if directory == "." || directory == "/" {
		name := strings.TrimSuffix(path.Base(relative), path.Ext(relative))
		return "file:" + relative, name
	}
	return "movie-dir:" + directory, path.Base(directory)
}

func discOuterDirectory(relative string) (string, bool) {
	parts := strings.Split(strings.Trim(relative, "/"), "/")
	for index, part := range parts {
		if strings.EqualFold(part, "BDMV") || strings.EqualFold(part, "VIDEO_TS") {
			if index == 0 {
				return "", false
			}
			return "/" + strings.Join(parts[:index], "/"), true
		}
	}
	return "", false
}

func recognitionSourceKey(groupKey string, files []File) string {
	stableIDs := make([]string, 0, len(files))
	allStable := len(files) > 0
	for _, file := range files {
		if !file.ProviderIDStable || strings.TrimSpace(file.ProviderID) == "" {
			allStable = false
			break
		}
		stableIDs = append(stableIDs, file.ProviderID)
	}
	identity := "path\x00" + groupKey
	// A TV directory is the work identity. Deriving it from the lexicographically
	// smallest episode provider ID made an existing series change identity when
	// a newly added episode happened to sort before the old anchor.
	if allStable && !strings.HasPrefix(groupKey, "tv-dir:") && !strings.HasPrefix(groupKey, "tv-root:") {
		sort.Strings(stableIDs)
		// One stable anchor keeps a work identity unchanged when later episodes
		// or sidecars join the same provider directory. The provider ID remains
		// inside the hash and is never exposed.
		identity = "provider-anchor\x00" + stableIDs[0]
	}
	sum := sha256.Sum256([]byte(identity))
	return hex.EncodeToString(sum[:20])
}

func recognitionInputFingerprint(groupKey, packageName, mediaType string, files []File) string {
	hash := sha256.New()
	hash.Write([]byte(mediaType))
	hash.Write([]byte{'\x00'})
	hash.Write([]byte(strings.TrimSpace(packageName)))
	// Directory-backed works are identified by their directory surface. Episode
	// membership is a per-file fact and must not invalidate the work lookup.
	// Root-level files have no directory identity, so retain their physical fact
	// fingerprint to detect replacement in place.
	if strings.HasPrefix(groupKey, "file:") {
		hash.Write([]byte{'\x00'})
		hash.Write([]byte(groupKey))
		for _, file := range files {
			hash.Write([]byte{'\x00'})
			hash.Write([]byte(normalizeFactPath(file.RelativePath)))
			hash.Write([]byte{'\x00'})
			hash.Write([]byte(strconv.FormatInt(file.Size, 10)))
			hash.Write([]byte{'\x00'})
			hash.Write([]byte(file.ModifiedAt.UTC().Format(time.RFC3339Nano)))
		}
	}
	return hex.EncodeToString(hash.Sum(nil))
}

// RecognitionEvidenceFiles returns a deterministic bounded copy suitable for
// work parsing. It is shared by initial/background recognition and manual
// retry so neither path can accidentally reintroduce an unbounded manifest.
func RecognitionEvidenceFiles(files []File) []File {
	ordered := append([]File(nil), files...)
	sort.SliceStable(ordered, func(i, j int) bool {
		left, right := normalizeFactPath(ordered[i].RelativePath), normalizeFactPath(ordered[j].RelativePath)
		if left == right {
			return ordered[i].ProviderID < ordered[j].ProviderID
		}
		return left < right
	})
	if len(ordered) > MaxRecognitionEvidenceFiles {
		ordered = ordered[:MaxRecognitionEvidenceFiles]
	}
	return ordered
}

func normalizeFactPath(value string) string {
	value = strings.ReplaceAll(strings.TrimSpace(value), "\\", "/")
	return "/" + strings.TrimLeft(path.Clean("/"+strings.TrimLeft(value, "/")), "/")
}
