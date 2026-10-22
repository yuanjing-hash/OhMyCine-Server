package services

import (
	"context"
	"encoding/json"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/cloud"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/cloud/quark"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/nodeprotocol"
	"strings"
	"testing"
	"time"
)

func TestQuarkNodeGrantDropsWebRefreshAndOtherScope(t *testing.T) {
	tv, agent := testQuarkAuthorization(), testQuarkAuthorization()
	p := quark.SpaceProof{Version: 1, Generation: 1, TVAccount: tv.AccountID, AgentAccount: agent.AccountID, TVRoot: "selected-root", AgentRoot: "agent-root", ContentSHA256: strings.Repeat("a", 64), VerifiedAt: time.Now()}
	other := p
	other.TVRoot = "other-root"
	bundle := quark.Credentials{Version: 1, Revision: 8, TV: tv, Agent: agent, Web: &quark.WebAuthorization{Cookie: "web-secret"}, Proofs: []quark.SpaceProof{other, p}}
	raw, e := restrictedQuarkNodeCredentials(bundle, p.TVRoot)
	if e != nil {
		t.Fatal(e)
	}
	var got quark.Credentials
	if e = json.Unmarshal(raw, &got); e != nil {
		t.Fatal(e)
	}
	if got.Web != nil || got.TV.RefreshToken != "" || got.Agent.RefreshToken != "" || len(got.Proofs) != 1 || got.Proofs[0].TVRoot != p.TVRoot || got.Revision != 8 || got.TV.AccessToken != tv.AccessToken {
		t.Fatal("grant included unrelated or refresh authorization")
	}
	for _, secret := range []string{"web-secret", "refresh-secret", "other-root"} {
		if strings.Contains(string(raw), secret) {
			t.Fatal("unexpected grant secret")
		}
	}
	if tv.RefreshToken == "" || len(bundle.Proofs) != 2 {
		t.Fatal("source bundle mutated")
	}
	if _, e = restrictedQuarkNodeCredentials(bundle, "unproven"); e == nil {
		t.Fatal("unproven root accepted")
	}
}

func TestQuarkNodeGrantFenceRejectsCredentialAndRootDrift(t *testing.T) {
	for _, change := range []string{"credential", "root", "disabled", "connection_revision"} {
		t.Run(change, func(t *testing.T) {
			connections, _, created := createQuarkConnection(t)
			bundle := saveQuarkTestBundle(t, connections, created.ID, func(c *quark.Credentials) {
				c.TV = testQuarkAuthorization()
				c.Agent = testQuarkAuthorization()
			})
			var connection models.Connection
			if err := connections.db.First(&connection, created.ID).Error; err != nil {
				t.Fatal(err)
			}
			storage := models.Storage{Name: "节点授权根", NameNormalized: "节点授权根", Type: models.StorageTypeQuark, ConnectionID: &created.ID, RootPath: "root-before", RootPathNormalized: "quark:root-before", Enabled: true, Capabilities: "{}"}
			if err := connections.db.Create(&storage).Error; err != nil {
				t.Fatal(err)
			}
			nodes := &TransferNodeService{db: connections.db, credentials: connections.credentials}
			if err := nodes.fenceQuarkGrant(connections.db, connection, storage, bundle); err != nil {
				t.Fatalf("unchanged grant rejected: %v", err)
			}
			var err error
			switch change {
			case "credential":
				saveQuarkTestBundle(t, connections, created.ID, func(c *quark.Credentials) { c.TV.AccessToken = "replacement-access" })
			case "root":
				err = connections.db.Model(&models.Storage{}).Where("id = ?", storage.ID).Update("root_path", "root-after").Error
			case "disabled":
				err = connections.db.Model(&models.Storage{}).Where("id = ?", storage.ID).Update("enabled", false).Error
			case "connection_revision":
				err = connections.db.Model(&models.Connection{}).Where("id = ?", connection.ID).Update("revision", connection.Revision+1).Error
			}
			if err != nil {
				t.Fatal(err)
			}
			if err = nodes.fenceQuarkGrant(connections.db, connection, storage, bundle); err == nil {
				t.Fatal("stale sealed grant accepted")
			}
		})
	}
}
func TestQuarkRemoteUploadRequiresDurableReadbackDigest(t *testing.T) {
	fixture := newUploadTransferFixture(t, models.MediaLibraryConflictOverwrite, false)
	if e := fixture.service.EnqueuePackage(fixture.download, fixture.manifest, fixture.manifest); e != nil {
		t.Fatal(e)
	}
	var task models.TransferTask
	if e := fixture.queue.db.Where("download_task_id = ?", fixture.download.ID).First(&task).Error; e != nil {
		t.Fatal(e)
	}
	rows := []models.RemoteUploadFile{{TransferTaskID: task.ID, DownloadTaskID: fixture.download.ID, Ordinal: 0, SourceFileToken: "file:quark", SourceRelative: "Movie.mkv", TargetRelative: "电影/Movie.mkv", Size: 10, SHA256: strings.Repeat("a", 64), ConflictAction: nodeprotocol.StorageConflictFailIfExists, Status: nodeprotocol.StorageFilePending}}
	if e := fixture.queue.db.Create(&rows).Error; e != nil {
		t.Fatal(e)
	}
	response := nodeprotocol.StorageActionResponse{Status: nodeprotocol.StorageActionCompleted, TotalFiles: 1, CompletedFiles: 1, Files: []nodeprotocol.StorageUploadFileResult{{SourceFileToken: rows[0].SourceFileToken, TargetParentID: "root", TargetItemID: "q-file", TargetName: "Movie.mkv", Status: nodeprotocol.StorageFileCompleted, Size: 10, SHA1: strings.Repeat("b", 40)}}}
	worker := NewTransferWorker(fixture.service)
	if e := worker.applyRemoteUploadResponse(context.Background(), &task, rows, rows, response, cloud.ProviderQuark); e == nil {
		t.Fatal("115 SHA1 accepted as quark readback proof")
	}
	response.Files[0].SHA1 = ""
	response.Files[0].ContentDigest = &nodeprotocol.ContentDigest{Version: 2, Algorithm: "sha256", Value: rows[0].SHA256, VerifiedBy: "provider_metadata"}
	if e := worker.applyRemoteUploadResponse(context.Background(), &task, rows, rows, response, cloud.ProviderQuark); e == nil {
		t.Fatal("unverified digest accepted")
	}
	response.Files[0].ContentDigest.VerifiedBy = "full_readback"
	if e := worker.applyRemoteUploadResponse(context.Background(), &task, rows, rows, response, cloud.ProviderQuark); e != nil {
		t.Fatal(e)
	}
	var saved models.RemoteUploadFile
	if e := fixture.queue.db.First(&saved, rows[0].ID).Error; e != nil {
		t.Fatal(e)
	}
	if !validRemoteQuarkDigest(saved.TargetDigestJSON, saved.SHA256) || saved.TargetSHA1 != "" {
		t.Fatal("versioned readback not persisted")
	}
}
