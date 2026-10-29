package pan115offline

import (
	"context"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/downloader"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/resourcecontent"
	"testing"
)

func TestUnsupportedBTSelectionCannotFallBackToWholeURL(t *testing.T) {
	driver := &fakeDriver{}
	client, err := New(downloader.Config{CloudDriver: driver, ProviderStorageRootID: "root", ProviderDirectoryID: "target"})
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.Submit(context.Background(), downloader.SubmitRequest{Source: downloader.Source{Kind: downloader.SourceURL, URL: "magnet:?xt=urn:btih:0123456789abcdef0123456789abcdef01234567", ContentSelection: &resourcecontent.Selection{Version: 1}}, Tag: "omc-test"})
	if code, _ := downloader.ErrorInfo(err); code != "downloader_selective_files_unsupported" || driver.submittedURI != "" {
		t.Fatal("selected source fell back to whole download", code, driver.submittedURI)
	}
}
