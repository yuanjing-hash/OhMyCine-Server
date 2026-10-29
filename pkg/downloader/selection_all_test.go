package downloader

import (
	"context"
	"strings"
	"testing"

	"github.com/yuanjing-hash/OhMyCine-Server/pkg/resourcecontent"
)

type allNativeFixture struct {
	Client
	submitted Source
}

func (c *allNativeFixture) Submit(_ context.Context, r SubmitRequest) (Task, error) {
	c.submitted = r.Source
	if r.MetadataOnly {
		panic("metadata request")
	}
	return Task{ID: "native"}, nil
}
func (c *allNativeFixture) Manifest(context.Context, string) (Manifest, error) {
	return Manifest{Complete: true}, nil
}

func TestVerifiedAllRequiresEveryLeafAndPreservesPrivateProof(t *testing.T) {
	info := []byte("d5:filesld6:lengthi10e4:pathl5:a.mkveed6:lengthi20e4:pathl5:b.txteee4:name4:Pack12:piece lengthi16384e6:pieces20:" + strings.Repeat("a", 20) + "e")
	raw := append(append([]byte("d4:info"), info...), 'e')
	m, err := resourcecontent.ParseTorrent(raw, resourcecontent.Limits{})
	if err != nil {
		t.Fatal(err)
	}
	sel := &resourcecontent.Selection{Version: 1, Identity: m.Identity, Files: []resourcecontent.SelectedFile{{Index: 0, Path: "a.mkv", Size: 10}}}
	source := Source{Kind: SourceURL, URL: "magnet:?xt=urn:btih:" + m.Identity.V1, Torrent: raw, ContentSelection: sel, AllFilesVerified: true}
	if VerifiedAllSource(source) {
		t.Fatal("video-only subset became full resource")
	}
	sel.Files = append(sel.Files, resourcecontent.SelectedFile{Index: 1, Path: "b.txt", Size: 20})
	if !VerifiedAllSource(source) {
		t.Fatal("complete proven source rejected")
	}
	native := &allNativeFixture{}
	bound, err := BindVerifiedAll(native, source)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := bound.(MetadataClient); ok {
		t.Fatal("native provider acquired fabricated metadata routing")
	}
	if _, err = bound.Submit(context.Background(), SubmitRequest{Source: source}); err != nil {
		t.Fatal(err)
	}
	if native.submitted.ContentSelection != nil || len(native.submitted.Torrent) != 0 || native.submitted.URL != source.URL {
		t.Fatal("ordinary execution parameters not narrowed")
	}
	if source.ContentSelection == nil || len(source.Torrent) == 0 || len(source.ContentSelection.Files) != 2 {
		t.Fatal("all optimization discarded persisted proof")
	}
	if _, err = bound.(ManifestClient).Manifest(context.Background(), "native"); err != nil {
		t.Fatal(err)
	}
	changed := source
	changed.URL = "magnet:?xt=urn:btih:" + strings.Repeat("f", 40)
	if VerifiedAllSource(changed) {
		t.Fatal("different resource passed all proof")
	}
	// The same files in a private metainfo cannot take the public native-URI
	// compatibility path, even if every file has been explicitly selected.
	privateInfo := append(append([]byte(nil), info[:len(info)-1]...), []byte("7:privatei1ee")...)
	privateRaw := append(append([]byte("d4:info"), privateInfo...), 'e')
	pm, e := resourcecontent.ParseTorrent(privateRaw, resourcecontent.Limits{})
	if e != nil {
		t.Fatal(e)
	}
	ps := *sel
	ps.Identity = pm.Identity
	private := Source{Kind: SourceURL, URL: "magnet:?xt=urn:btih:" + pm.Identity.V1, Torrent: privateRaw, ContentSelection: &ps, AllFilesVerified: true}
	if VerifiedAllSource(private) {
		t.Fatal("private source bypassed files contract")
	}
}
