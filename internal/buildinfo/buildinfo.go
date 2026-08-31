// Package buildinfo exposes the immutable identity injected into official
// Server binaries. Development builds intentionally remain non-comparable.
package buildinfo

import (
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// Version and Commit are overridden only by the official release workflow.
var (
	Version = "dev"
	Commit  = "unknown"
)

var (
	versionPattern = regexp.MustCompile(`^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$`)
	commitPattern  = regexp.MustCompile(`^[0-9a-f]{40}([0-9a-f]{24})?$`)
)

// SemanticVersion is the strict numeric version used by Server releases.
type SemanticVersion struct {
	Major uint64
	Minor uint64
	Patch uint64
}

func (v SemanticVersion) String() string {
	return fmt.Sprintf("%d.%d.%d", v.Major, v.Minor, v.Patch)
}

func (v SemanticVersion) Compare(other SemanticVersion) int {
	left := [...]uint64{v.Major, v.Minor, v.Patch}
	right := [...]uint64{other.Major, other.Minor, other.Patch}
	for i := range left {
		if left[i] < right[i] {
			return -1
		}
		if left[i] > right[i] {
			return 1
		}
	}
	return 0
}

func ParseVersion(value string) (SemanticVersion, error) {
	match := versionPattern.FindStringSubmatch(value)
	if match == nil {
		return SemanticVersion{}, errors.New("version is not a strict X.Y.Z semantic version")
	}
	parts := [3]uint64{}
	for i := range parts {
		parsed, err := strconv.ParseUint(match[i+1], 10, 64)
		if err != nil {
			return SemanticVersion{}, errors.New("version component is out of range")
		}
		parts[i] = parsed
	}
	return SemanticVersion{Major: parts[0], Minor: parts[1], Patch: parts[2]}, nil
}

// Info is safe to expose through an administrator status endpoint.
type Info struct {
	Version    string `json:"version"`
	Commit     string `json:"commit"`
	Official   bool   `json:"official"`
	Comparable bool   `json:"comparable"`
}

func Current() Info {
	version := strings.TrimSpace(Version)
	commit := strings.ToLower(strings.TrimSpace(Commit))
	_, versionErr := ParseVersion(version)
	comparable := versionErr == nil
	official := comparable && commitPattern.MatchString(commit)
	if !comparable {
		version = "dev"
	}
	if !commitPattern.MatchString(commit) {
		commit = "unknown"
	}
	return Info{Version: version, Commit: commit, Official: official, Comparable: comparable}
}
