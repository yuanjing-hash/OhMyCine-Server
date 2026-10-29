// Package resourcecontent reads immutable content facts without submitting a
// download, choosing a downloader, or opening media payload storage.
package resourcecontent

import (
	"errors"
)

// Limits bounds untrusted metainfo before allocation or recursive traversal.
// Zero fields select the defaults; callers may only tighten those defaults.
type Limits struct {
	MaxMetainfoBytes int
	MaxInfoBytes     int
	MaxFiles         int
	MaxPathBytes     int
	MaxDepth         int
	MaxValues        int
}

func DefaultLimits() Limits {
	return Limits{MaxMetainfoBytes: 16 << 20, MaxInfoBytes: 4 << 20, MaxFiles: 10000, MaxPathBytes: 4096, MaxDepth: 64, MaxValues: 200000}
}

func normalizeLimits(in Limits) Limits {
	d := DefaultLimits()
	tighten := func(dst *int, requested int) {
		if requested > 0 && requested < *dst {
			*dst = requested
		}
	}
	tighten(&d.MaxMetainfoBytes, in.MaxMetainfoBytes)
	tighten(&d.MaxInfoBytes, in.MaxInfoBytes)
	tighten(&d.MaxFiles, in.MaxFiles)
	tighten(&d.MaxPathBytes, in.MaxPathBytes)
	tighten(&d.MaxDepth, in.MaxDepth)
	tighten(&d.MaxValues, in.MaxValues)
	return d
}

var (
	ErrInvalidMetainfo = errors.New("resource_content_invalid_metainfo")
	ErrLimitExceeded   = errors.New("resource_content_limit_exceeded")
	ErrUnsafePath      = errors.New("resource_content_unsafe_path")
	ErrHashMismatch    = errors.New("resource_content_hash_mismatch")
	ErrUnsupported     = errors.New("resource_content_unsupported")
	ErrUnavailable     = errors.New("resource_content_unavailable")
)

// Error contains a fixed diagnostic code only, never raw source URLs, tracker
// credentials, peer addresses, filenames, or provider response text.
type Error struct {
	Code  string
	cause error
}

func (e *Error) Error() string { return e.Code }
func (e *Error) Unwrap() error { return e.cause }
func invalid(code string, cause error) error {
	return &Error{Code: "resource_content_" + code, cause: cause}
}

// Identity uses the complete infohashes, not the truncated v2 handshake hash.
type Identity struct {
	V1 string `json:"v1,omitempty"`
	V2 string `json:"v2,omitempty"`
}

// File.Index is the original v1 files-list index, including padding positions.
// For pure v2 it is the deterministic byte-sorted file-tree traversal index.
// Path is relative to the metainfo root; a single root file uses its own name.
// Files are facts, not authorization to select or acquire those files.
type File struct {
	Index      int    `json:"index"`
	Path       string `json:"path"`
	Size       int64  `json:"size"`
	Padding    bool   `json:"padding,omitempty"`
	PiecesRoot string `json:"pieces_root,omitempty"`
}

type Manifest struct {
	Format      string   `json:"format"`
	Name        string   `json:"name"`
	Identity    Identity `json:"identity"`
	Private     bool     `json:"private"`
	MultiFile   bool     `json:"multi_file"`
	Files       []File   `json:"files"`
	TotalSize   int64    `json:"total_size"`
	PieceLength int64    `json:"piece_length"`
	// Raw input and tracker fields are private execution facts. Public DTOs
	// should project only the safe bounded display rows they actually need.
	InfoBytes     []byte     `json:"-"`
	MetainfoBytes []byte     `json:"-"`
	Trackers      [][]string `json:"-"`
}

func (m Manifest) ValidateIdentity(expected Identity) error {
	if expected.V1 != "" && m.Identity.V1 != expected.V1 || expected.V2 != "" && m.Identity.V2 != expected.V2 {
		return invalid("hash_mismatch", ErrHashMismatch)
	}
	return nil
}

func unsupportedVersion(version int64) error {
	_ = version // Never include untrusted values in a public error.
	return invalid("meta_version_unsupported", ErrUnsupported)
}
