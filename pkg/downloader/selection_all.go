package downloader

import (
	"context"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/resourcecontent"
)

func SelectionCoversAll(manifest resourcecontent.Manifest, selection resourcecontent.Selection) bool {
	if selection.ValidateAgainst(manifest) != nil {
		return false
	}
	count := 0
	for _, file := range manifest.Files {
		if !file.Padding {
			count++
		}
	}
	return count == len(selection.Files)
}

// VerifiedAllSource keeps all persisted authority. Only the adapter call gets
// the ordinary URI shape, after complete public metainfo proof is rechecked.
func VerifiedAllSource(source Source) bool {
	if !source.AllFilesVerified || source.Kind != SourceURL {
		return false
	}
	manifest, err := ValidateContentSource(source)
	return err == nil && !manifest.Private && SelectionCoversAll(manifest, *source.ContentSelection)
}

func BindVerifiedAll(client Client, source Source) (Client, error) {
	if !VerifiedAllSource(source) {
		return nil, Error("downloader_selection_invalid", false, nil)
	}
	return &verifiedAllClient{Client: client, source: source}, nil
}

type verifiedAllClient struct {
	Client
	source Source
}

func (c *verifiedAllClient) Manifest(ctx context.Context, id string) (Manifest, error) {
	if manifest, ok := c.Client.(ManifestClient); ok {
		return manifest.Manifest(ctx, id)
	}
	return Manifest{}, Error("downloader_manifest_unsupported", false, nil)
}

func (c *verifiedAllClient) ordinaryRequest(r SubmitRequest) (SubmitRequest, error) {
	if !VerifiedAllSource(r.Source) {
		return SubmitRequest{}, Error("downloader_selection_invalid", false, nil)
	}
	a, _ := c.source.ContentSelection.Digest()
	b, _ := r.Source.ContentSelection.Digest()
	if a != b || c.source.URL != r.Source.URL {
		return SubmitRequest{}, Error("downloader_selection_invalid", false, nil)
	}
	r.Source.ContentSelection = nil
	r.Source.AllFilesVerified = false
	r.Source.Torrent = nil
	r.MetadataOnly = false
	return r, nil
}

func (c *verifiedAllClient) Submit(ctx context.Context, r SubmitRequest) (Task, error) {
	r, err := c.ordinaryRequest(r)
	if err != nil {
		return Task{}, err
	}
	return c.Client.Submit(ctx, r)
}

func (c *verifiedAllClient) ReconcileSubmission(ctx context.Context, r SubmitRequest) (Task, error) {
	r, err := c.ordinaryRequest(r)
	if err != nil {
		return Task{}, err
	}
	resolver, ok := c.Client.(SubmissionReconciler)
	if !ok {
		return Task{}, Error("downloader_submission_reconcile_unsupported", true, nil)
	}
	return resolver.ReconcileSubmission(ctx, r)
}
