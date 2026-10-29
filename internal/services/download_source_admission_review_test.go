package services

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
	downloadpkg "github.com/yuanjing-hash/OhMyCine-Server/pkg/downloader"
)

func TestRawTorrentAdmissionKeepsKnownOrdinaryAndRejectsNativeExecutors(t *testing.T) {
	sites, _, actor, _, downloads, downloaders := siteFixture(t)
	const nativeType = models.DownloaderTypeFake
	builds := 0
	if err := downloaders.registry.Register(nativeType, downloadpkg.Capabilities{NativeOffline: true}, func(downloadpkg.Config) (downloadpkg.Client, error) {
		builds++
		return &stubDownloadClient{}, nil
	}); err != nil {
		t.Fatal(err)
	}
	native := models.Downloader{ID: "review-native", Name: "Native", NameNormalized: "review-native", Type: nativeType, Enabled: true}
	if err := sites.db.Create(&native).Error; err != nil {
		t.Fatal(err)
	}
	source := DownloadSourceInput{Kind: downloadpkg.SourceTorrent, Filename: "movie.torrent", Torrent: []byte("d4:infod6:lengthi10e4:name9:movie.mkv12:piece lengthi16384e6:pieces20:aaaaaaaaaaaaaaaaaaaaee")}
	if _, err := downloads.Submit(context.Background(), actor, SubmitDownloadInput{DownloaderID: native.ID, Source: source}, RequestContext{}); ErrorCode(err) != CodeDownloadSourceInvalid {
		t.Fatalf("known native executor admitted a raw whole torrent: %v", err)
	}
	var count int64
	if err := sites.db.Model(&models.DownloadTask{}).Count(&count).Error; err != nil || count != 0 || builds != 0 {
		t.Fatal("denied source built a provider or created a task")
	}
	ordinary, err := downloaders.Create(actor, DownloaderInput{Name: "Ordinary qBit", Type: models.DownloaderTypeQBittorrent, BaseURL: "http://qbit.example.test", Enabled: true}, RequestContext{})
	if err != nil {
		t.Fatal(err)
	}
	created, err := downloads.Submit(context.Background(), actor, SubmitDownloadInput{DownloaderID: ordinary.ID, Source: source}, RequestContext{})
	if err != nil {
		t.Fatalf("known ordinary executor lost its legacy raw torrent entry: %v", err)
	}
	var task models.DownloadTask
	if err := sites.db.First(&task, "id = ?", created.ID).Error; err != nil {
		t.Fatal(err)
	}
	plaintext, err := downloads.credentials.Decrypt(downloadSourcePurpose(task.ID), task.SourceCiphertext)
	var frozen downloadSourceEnvelope
	if err != nil || json.Unmarshal([]byte(plaintext), &frozen) != nil || frozen.Kind != downloadpkg.SourceTorrent || string(frozen.Torrent) != string(source.Torrent) || frozen.ContentSelection != nil || task.ContentSelectionRequired {
		t.Fatal("ordinary torrent admission changed the original source or selection mode")
	}
}
