package services

import (
	"context"
	"testing"

	"github.com/rs/zerolog"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/authz"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
	cloudpkg "github.com/yuanjing-hash/OhMyCine-Server/pkg/cloud"
)

type cloudCleanupSettingDriver struct {
	*fakeCloudDriver
	recyclable bool
}

func (d *cloudCleanupSettingDriver) Capabilities() cloudpkg.Capabilities {
	caps := d.fakeCloudDriver.Capabilities()
	caps.Recycle = d.recyclable
	caps.DirectoryList = true
	return caps
}

func TestCloudEmptyCleanupSettingDefaultsAndLocalDenial(t *testing.T) {
	s, _, actor, storage, profile := mediaLibraryTestService(t)
	input := testLibraryInput("cleanup", storage, profile, false)
	library, err := s.Create(context.Background(), actor, input, RequestContext{})
	if err != nil || library.CloudEmptyCleanupEnabled {
		t.Fatal("unsafe default", err)
	}
	yes := true
	input.CloudEmptyCleanupEnabled = &yes
	if _, err := s.Update(context.Background(), actor, library.ID, input, RequestContext{}); ErrorCode(err) != CodeInvalidRequest {
		t.Fatal("local cleanup enabled", err)
	}
}

func TestCloudEmptyCleanupSettingPersistsAndChecksCapabilities(t *testing.T) {
	driver := &fakeCloudDriver{}
	db, _, connections, actor := newConnectionTestService(t, driver)
	for _, permission := range []string{authz.PermissionMediaLibrariesRead, authz.PermissionMediaLibrariesCreate, authz.PermissionMediaLibrariesUpdate} {
		actor.Permissions[permission] = struct{}{}
	}
	connection, err := connections.Create(actor, ConnectionInput{Name: "cleanup", Provider: cloudpkg.ProviderPan115, Cookie: testPan115Cookie, Enabled: true}, RequestContext{})
	if err != nil {
		t.Fatal(err)
	}
	wrapped := &cloudCleanupSettingDriver{fakeCloudDriver: driver, recyclable: true}
	connections.drivers[connection.ID] = wrapped
	storage := models.Storage{Name: "cloud", NameNormalized: "cloud", Type: models.StorageTypePan115, RootPath: "root", RootPathNormalized: "root", ConnectionID: &connection.ID, Enabled: true, Capabilities: "{}"}
	if err := db.Create(&storage).Error; err != nil {
		t.Fatal(err)
	}
	var profile models.MediaClassificationProfile
	if err := db.First(&profile).Error; err != nil {
		t.Fatal(err)
	}
	s := NewMediaLibraryService(db, NewAuditService(db), zerolog.Nop())
	s.SetConnectionService(connections)
	t.Cleanup(s.Close)
	yes, no := true, false
	input := MediaLibraryInput{Name: "cloud", StorageID: storage.ID, ProfileID: profile.ID, RelativeRoot: "/", ProviderRootID: "root", TransferMode: "copy", CloudEmptyCleanupEnabled: &yes}
	library, err := s.Create(context.Background(), actor, input, RequestContext{})
	if err != nil || !library.CloudEmptyCleanupEnabled {
		t.Fatal("setting not saved", err)
	}
	input.CloudEmptyCleanupEnabled = nil
	library, err = s.Update(context.Background(), actor, library.ID, input, RequestContext{})
	if err != nil || !library.CloudEmptyCleanupEnabled {
		t.Fatal("omitted field disabled setting", err)
	}
	input.CloudEmptyCleanupEnabled = &no
	library, err = s.Update(context.Background(), actor, library.ID, input, RequestContext{})
	if err != nil || library.CloudEmptyCleanupEnabled {
		t.Fatal("disable failed", err)
	}
	wrapped.recyclable = false
	input.CloudEmptyCleanupEnabled = &yes
	if _, err := s.Update(context.Background(), actor, library.ID, input, RequestContext{}); ErrorCode(err) != CodeInvalidRequest {
		t.Fatal("unsupported recycle enabled", err)
	}
	if _, err := s.Update(context.Background(), Actor{}, library.ID, input, RequestContext{}); ErrorCode(err) != CodePermissionDenied {
		t.Fatal("missing authorization", err)
	}
}
