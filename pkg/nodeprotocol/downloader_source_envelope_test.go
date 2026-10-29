package nodeprotocol

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/yuanjing-hash/OhMyCine-Server/pkg/resourcecontent"
)

func TestSelectedSourceEnvelopeBindsIdentityAndRefreshDoesNotChangeOperation(t *testing.T) {
	source := DownloaderSource{Kind: "torrent", Torrent: []byte("private-metainfo-secret"), URL: "https://tracker.example/private-key", ContentSelection: &resourcecontent.Selection{Version: 1, Identity: resourcecontent.Identity{V1: strings.Repeat("a", 40)}, Files: []resourcecontent.SelectedFile{{Index: 1, Path: "movie.mkv", Size: 10}}}}
	binding := DownloaderSourceBinding{TaskID: "task-1", OperationKey: "operation-1", DownloaderID: "downloader-1", SavePath: "/managed/task", Tag: "omc-task-1", ConfigurationScope: strings.Repeat("b", 64)}
	first, key, err := SealDownloaderSource(source, binding)
	if err != nil {
		t.Fatal(err)
	}
	second, key2, err := SealDownloaderSource(source, binding)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(first)
	if bytes.Contains(raw, []byte("private-key")) || bytes.Contains(raw, []byte("private-metainfo-secret")) {
		t.Fatal("source escaped ciphertext")
	}
	opened, err := OpenDownloaderSource(first, key, binding)
	if err != nil || !bytes.Equal(opened.Torrent, source.Torrent) {
		t.Fatal(err)
	}
	for _, mutate := range []func(*DownloaderSourceBinding){func(b *DownloaderSourceBinding) { b.TaskID = "other" }, func(b *DownloaderSourceBinding) { b.OperationKey = "other" }, func(b *DownloaderSourceBinding) { b.DownloaderID = "other" }, func(b *DownloaderSourceBinding) { b.SavePath = "/other" }, func(b *DownloaderSourceBinding) { b.Tag = "other" }, func(b *DownloaderSourceBinding) { b.ConfigurationScope = strings.Repeat("c", 64) }} {
		changed := binding
		mutate(&changed)
		if _, err := OpenDownloaderSource(first, key, changed); err == nil {
			t.Fatal("AAD drift accepted")
		}
	}
	request := DownloaderActionRequest{RequestID: "request-1", OperationKey: binding.OperationKey, TaskID: binding.TaskID, DownloaderID: binding.DownloaderID, Action: DownloaderActionSubmit, GrantID: "grant-1", Submit: &DownloaderSubmit{Source: DownloaderSource{Kind: "torrent"}, SourceEnvelope: first, Tag: binding.Tag, SavePath: binding.SavePath}}
	digest, _ := request.Digest()
	request.GrantID = "grant-2"
	request.Submit.SourceEnvelope = second
	if refreshed, _ := request.Digest(); refreshed != digest || key == key2 || bytes.Equal(first.Nonce, second.Nonce) {
		t.Fatal("refresh changed business identity or reused crypto nonce")
	}
	source.Filename = "different-source.torrent"
	different, _, _ := SealDownloaderSource(source, binding)
	request.Submit.SourceEnvelope = different
	if changed, _ := request.Digest(); changed == digest {
		t.Fatal("different plaintext retained operation digest")
	}
}
