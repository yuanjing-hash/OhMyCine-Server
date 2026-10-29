package downloader

import (
	"context"
	"encoding/hex"
	"strings"

	"github.com/yuanjing-hash/OhMyCine-Server/pkg/resourcecontent"
)

// SelectionGroup is a current, fenced view of durable references. It never
// replaces a task's immutable selection. The authority refreshes it for every
// operation; adapters must not invent their own reference registry.
type SelectionGroup struct {
	Version         int                       `json:"version"`
	ID              string                    `json:"id"`
	Tag             string                    `json:"tag"`
	Workspace       string                    `json:"workspace"`
	Revision        uint64                    `json:"revision"`
	Epoch           uint64                    `json:"epoch"`
	PreviousDigests []string                  `json:"previous_digests,omitempty"`
	Union           resourcecontent.Selection `json:"union"`
	Resume          bool                      `json:"resume"`
	// Empty represents the last released reference. Union then retains mapping
	// evidence only and does not mean that any file is wanted.
	Empty bool `json:"empty,omitempty"`
}

type SelectionGroupClient interface {
	ApplySelectionGroup(context.Context, SubmitRequest, SelectionGroup) (Task, error)
}

func (g SelectionGroup) Validate(own resourcecontent.Selection) error {
	if g.Version != 1 || len(g.ID) != 64 || g.Tag != "omc-group-"+g.ID[:32] || g.Workspace == "" || strings.ContainsAny(g.Workspace, "\x00\r\n") || g.Revision == 0 || g.Epoch == 0 || len(g.PreviousDigests) > 64 {
		return Error("downloader_selection_group_invalid", false, nil)
	}
	if _, err := hex.DecodeString(g.ID); err != nil {
		return Error("downloader_selection_group_invalid", false, nil)
	}
	for _, digest := range g.PreviousDigests {
		if len(digest) != 64 {
			return Error("downloader_selection_group_invalid", false, nil)
		}
		if _, err := hex.DecodeString(digest); err != nil {
			return Error("downloader_selection_group_invalid", false, nil)
		}
	}
	if _, err := g.Union.Digest(); err != nil || g.Union.Identity != own.Identity {
		return Error("downloader_selection_group_invalid", false, err)
	}
	if g.Empty && g.Resume {
		return Error("downloader_selection_group_invalid", false, nil)
	}
	return nil
}
