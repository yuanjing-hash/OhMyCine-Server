package services

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/cloud"
	downloadpkg "github.com/yuanjing-hash/OhMyCine-Server/pkg/downloader"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/nodeprotocol"
	"gorm.io/gorm"
)

func quarkSourceNodeCodes() []string {
	return []string{nodeprotocol.CapabilityQuarkReadV2, nodeprotocol.CapabilityQuarkShareV2, nodeprotocol.CapabilityContentDigestV2, nodeprotocol.CapabilityRangeExport}
}

func quarkNodeAdmissionFixture(t *testing.T) (shareIngestFixture, models.TransferNode, uint) {
	t.Helper()
	f, libraryID, _ := quarkIntakeFixture(t)
	var source models.Storage
	if err := f.db.First(&source, f.storage.ID).Error; err != nil {
		t.Fatal(err)
	}
	f.downloads.downloader.connections.drivers[*source.ConnectionID] = &quarkPreviewFixtureDriver{&previewFixtureDriver{f.driver}}
	root := t.TempDir()
	local := models.Storage{Name: "Local target", NameNormalized: "local-target", Type: models.StorageTypeLocal, RootPath: root, RootPathNormalized: root, Enabled: true, Capabilities: "{}"}
	if err := f.db.Create(&local).Error; err != nil {
		t.Fatal(err)
	}
	if err := f.db.Model(&models.MediaLibrary{}).Where("id = ?", libraryID).Updates(map[string]any{"storage_id": local.ID, "relative_root": "/", "transfer_mode": models.MediaLibraryTransferCopy}).Error; err != nil {
		t.Fatal(err)
	}
	if err := f.db.Model(&models.DownloadSettings{}).Where("id = 1").Update("absolute_path", t.TempDir()).Error; err != nil {
		t.Fatal(err)
	}
	caps, _ := json.Marshal(nodeprotocol.Capabilities{Codes: quarkSourceNodeCodes(), ManagedFreeBytesKnown: true})
	free := int64(10 << 30)
	node := models.TransferNode{ID: uuid.NewString(), OwnerID: f.actor.User.ID, Name: "Quark edge", NameNormalized: "quark-edge", APIURL: "http://node.example.test:4433", Status: models.NodeStatusOnline, PublicKeyFingerprint: strings.Repeat("a", 64), EncryptionPublicKey: "fixture-key", CapabilitiesJSON: string(caps), FreeBytes: &free, CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC()}
	if err := f.db.Create(&node).Error; err != nil {
		t.Fatal(err)
	}
	nodes := NewTransferNodeService(f.db, NewAuditService(f.db), f.store)
	f.downloads.downloader.SetTransferNodeService(nodes)
	if err := f.db.Model(&models.Downloader{}).Where("id = ?", f.downloader.ID).Updates(map[string]any{"execution_location": models.NodeLocationRemote, "node_id": node.ID, "node_name": node.Name}).Error; err != nil {
		t.Fatal(err)
	}
	return f, node, libraryID
}

func quarkNodeSubmitInput(f shareIngestFixture, libraryID uint) SubmitDownloadInput {
	return SubmitDownloadInput{DownloaderID: f.downloader.ID, MediaLibraryID: &libraryID, Source: DownloadSourceInput{Kind: downloadpkg.SourceQuarkShare, URL: "https://pan.quark.cn/s/example", ShareSelection: &cloud.ShareSelection{Version: 1, Files: []cloud.ShareTreeItem{{ID: "selected-file", RelativePath: "Selected.mkv", Size: 100}}}}}
}

func TestQuarkNodeAdmissionRechecksEverySourceCapabilityAtSubmit(t *testing.T) {
	f, node, libraryID := quarkNodeAdmissionFixture(t)
	ctx := context.Background()
	previewInput := DownloadRoutePreviewInput{DownloaderID: f.downloader.ID, SourceKind: downloadpkg.SourceQuarkShare}
	preview, err := f.downloads.PreviewRoutes(ctx, f.actor, previewInput)
	if err != nil || len(preview.Options) != 1 || !preview.Options[0].Enabled || !preview.Options[0].RequiresNodeStaging {
		t.Fatalf("full-capability preview: %+v %v", preview, err)
	}
	for _, missing := range quarkSourceNodeCodes() {
		t.Run(missing, func(t *testing.T) {
			codes := []string{}
			for _, code := range quarkSourceNodeCodes() {
				if code != missing {
					codes = append(codes, code)
				}
			}
			raw, _ := json.Marshal(nodeprotocol.Capabilities{Codes: codes})
			if err := f.db.Model(&node).Update("capabilities_json", string(raw)).Error; err != nil {
				t.Fatal(err)
			}
			preview, err := f.downloads.PreviewRoutes(ctx, f.actor, previewInput)
			if err != nil || len(preview.Options) != 1 || preview.Options[0].Enabled || preview.Options[0].ReasonCode != CodeDownloaderUnavailable {
				t.Fatalf("old node preview: %+v %v", preview, err)
			}
			if _, err := f.downloads.Submit(ctx, f.actor, quarkNodeSubmitInput(f, libraryID), RequestContext{}); ErrorCode(err) != CodeDownloaderUnavailable {
				t.Fatalf("missing %s accepted: %v", missing, err)
			}
		})
	}
	var count int64
	if err := f.db.Model(&models.DownloadTask{}).Count(&count).Error; err != nil || count != 0 {
		t.Fatalf("failed admission persisted task: %d %v", count, err)
	}
	raw, _ := json.Marshal(nodeprotocol.Capabilities{Codes: quarkSourceNodeCodes()})
	if err := f.db.Model(&node).Update("capabilities_json", string(raw)).Error; err != nil {
		t.Fatal(err)
	}
	input := quarkNodeSubmitInput(f, libraryID)
	input.Source.ShareSelection = nil
	if _, err := f.downloads.Submit(ctx, f.actor, input, RequestContext{}); ErrorCode(err) != CodeDownloadSourceInvalid {
		t.Fatalf("unselected source accepted: %v", err)
	}
	accepted, err := f.downloads.Submit(ctx, f.actor, quarkNodeSubmitInput(f, libraryID), RequestContext{})
	if err != nil {
		t.Fatal(err)
	}
	var task models.DownloadTask
	if err := f.db.First(&task, "id = ?", accepted.ID).Error; err != nil {
		t.Fatal(err)
	}
	if task.ExecutionLocation != models.NodeLocationRemote || task.NodeID == nil || *task.NodeID != node.ID || task.RoutePlanDigest == "" || task.TargetLibraryID == nil || *task.TargetLibraryID != libraryID {
		t.Fatalf("route fell back or drifted: %+v", task)
	}
}

func TestQuarkNodeAdmissionFencesCapabilitiesInsideTransaction(t *testing.T) {
	f, node, libraryID := quarkNodeAdmissionFixture(t)
	input := quarkNodeSubmitInput(f, libraryID)
	input.BeforePersist = func(tx *gorm.DB) error { return tx.Model(&node).Update("capabilities_json", `{}`).Error }
	_, err := f.downloads.Submit(context.Background(), f.actor, input, RequestContext{})
	if ErrorCode(err) != CodeDownloaderUnavailable {
		t.Fatalf("capability change at commit accepted: %v", err)
	}
	var tasks, jobs int64
	f.db.Model(&models.DownloadTask{}).Count(&tasks)
	f.db.Model(&models.Job{}).Where("job_type = ?", "download").Count(&jobs)
	if tasks != 0 || jobs != 0 {
		t.Fatalf("transaction did not roll back: tasks=%d jobs=%d", tasks, jobs)
	}
}

type quarkNodeTargetDriver struct{ *shareIngestDriver }

func (d *quarkNodeTargetDriver) Capabilities() cloud.Capabilities {
	return cloud.Capabilities{DirectoryList: true, CreateDirectory: true, FileUpload: true}
}
func (d *quarkNodeTargetDriver) Upload(context.Context, cloud.UploadRequest) (cloud.Item, error) {
	panic("admission must not upload")
}

func TestQuarkNodeTargetUploadCapabilitiesAndSameSourceAdmission(t *testing.T) {
	f, node, libraryID := quarkNodeAdmissionFixture(t)
	var library models.MediaLibrary
	if err := f.db.First(&library, libraryID).Error; err != nil {
		t.Fatal(err)
	}
	// Re-use a second fixture connection identity while retaining independent
	// source and target IDs; only metadata is inspected during admission.
	var connection models.Connection
	if err := f.db.Where("provider = ?", cloud.ProviderQuark).First(&connection).Error; err != nil {
		t.Fatal(err)
	}
	connection.ID, connection.Name, connection.NameNormalized = 0, "Quark target", "quark-target"
	if err := f.db.Create(&connection).Error; err != nil {
		t.Fatal(err)
	}
	target := models.Storage{Name: "Quark target", NameNormalized: "quark-target", Type: models.StorageTypeQuark, RootPath: "storage", RootPathNormalized: "quark:target", ConnectionID: &connection.ID, Enabled: true, Capabilities: "{}"}
	if err := f.db.Create(&target).Error; err != nil {
		t.Fatal(err)
	}
	f.downloads.downloader.connections.drivers[connection.ID] = &quarkNodeTargetDriver{f.driver}
	if err := f.db.Model(&library).Updates(map[string]any{"storage_id": target.ID, "provider_root_id": "library"}).Error; err != nil {
		t.Fatal(err)
	}
	allCodes := append(quarkSourceNodeCodes(), nodeprotocol.CapabilityQuarkUploadV2)
	for _, missing := range []string{nodeprotocol.CapabilityQuarkUploadV2, nodeprotocol.CapabilityContentDigestV2, nodeprotocol.CapabilityRangeExport} {
		codes := []string{}
		for _, code := range allCodes {
			if code != missing {
				codes = append(codes, code)
			}
		}
		raw, _ := json.Marshal(nodeprotocol.Capabilities{Codes: codes})
		if err := f.db.Model(&node).Update("capabilities_json", string(raw)).Error; err != nil {
			t.Fatal(err)
		}
		_, err := f.downloads.Submit(context.Background(), f.actor, quarkNodeSubmitInput(f, libraryID), RequestContext{})
		if err == nil || missing == nodeprotocol.CapabilityQuarkUploadV2 && ErrorCode(err) != CodeTransferRouteUnsupported {
			t.Fatalf("missing target %s accepted or wrong error: %v", missing, err)
		}
	}
	raw, _ := json.Marshal(nodeprotocol.Capabilities{Codes: allCodes})
	if err := f.db.Model(&node).Update("capabilities_json", string(raw)).Error; err != nil {
		t.Fatal(err)
	}
	input := quarkNodeSubmitInput(f, libraryID)
	input.BeforePersist = func(tx *gorm.DB) error {
		withoutUpload, _ := json.Marshal(nodeprotocol.Capabilities{Codes: quarkSourceNodeCodes()})
		return tx.Model(&node).Update("capabilities_json", string(withoutUpload)).Error
	}
	if _, err := f.downloads.Submit(context.Background(), f.actor, input, RequestContext{}); ErrorCode(err) != CodeTransferRouteUnsupported {
		t.Fatalf("target lost upload at commit but task was accepted: %v", err)
	}
	if _, err := f.downloads.Submit(context.Background(), f.actor, quarkNodeSubmitInput(f, libraryID), RequestContext{}); err != nil {
		t.Fatalf("complete target capability rejected: %v", err)
	}
	if err := f.db.Model(&library).Updates(map[string]any{"storage_id": f.storage.ID, "transfer_mode": models.MediaLibraryTransferMove}).Error; err != nil {
		t.Fatal(err)
	}
	_, err := f.downloads.Submit(context.Background(), f.actor, quarkNodeSubmitInput(f, libraryID), RequestContext{})
	if ErrorCode(err) != CodeTransferRouteUnsupported || !strings.Contains(ErrorMessage(err), "同一夸克") {
		t.Fatalf("same-source Node should explicitly reject: %v", err)
	}
}
