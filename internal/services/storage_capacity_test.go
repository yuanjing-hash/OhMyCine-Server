package services

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/yuanjing-hash/OhMyCine-Server/internal/authz"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/cloud"
)

type dashboardQuotaDriver struct {
	*fakeCloudDriver
	calls  int
	quota  cloud.Quota
	err    error
	before func()
}

func (d *dashboardQuotaDriver) Quota(context.Context) (cloud.Quota, error) {
	d.calls++
	if d.before != nil {
		d.before()
	}
	return d.quota, d.err
}

func TestStorageCapacitySamplesDoNotBreakLegacyCreatesAndRemainWritable(t *testing.T) {
	base := &fakeCloudDriver{}
	db, _, connections, actor := newConnectionTestService(t, base)
	conn, err := connections.Create(actor, ConnectionInput{Name: "Legacy quota", Provider: cloud.ProviderPan115, Cookie: testPan115Cookie, Enabled: true}, RequestContext{})
	if err != nil {
		t.Fatal(err)
	}
	columns := []string{
		"last_capacity_free_bytes", "last_capacity_total_bytes", "last_capacity_checked_at", "last_capacity_sampled_at",
		"last_capacity_error_code", "last_capacity_status", "last_capacity_source_fingerprint",
	}
	for _, column := range columns {
		if err := db.Exec("ALTER TABLE storages DROP COLUMN " + column).Error; err != nil {
			t.Fatal(err)
		}
	}
	previousFree, previousTotal, previousTime := uint64(999), uint64(1000), time.Now().UTC()
	row := models.Storage{
		Name: "Legacy storage", NameNormalized: "legacy-quota", Type: cloud.ProviderPan115,
		RootPath: "root", RootDisplayPath: "/", RootPathNormalized: "legacy:root", ConnectionID: &conn.ID,
		Enabled: true, Capabilities: "{}", LastCapacityFreeBytes: &previousFree, LastCapacityTotalBytes: &previousTotal,
		LastCapacityCheckedAt: &previousTime, LastCapacitySampledAt: &previousTime, LastCapacityErrorCode: "private-old-error",
		LastCapacityStatus: "error", LastCapacitySourceFingerprint: "old-owner",
	}
	if err := db.Create(&row).Error; err != nil {
		t.Fatalf("model Create required future capacity columns: %v", err)
	}
	for _, column := range columns {
		if err := db.Migrator().AddColumn(&models.Storage{}, column); err != nil {
			t.Fatal(err)
		}
	}
	var saved models.Storage
	if err := db.First(&saved, row.ID).Error; err != nil {
		t.Fatal(err)
	}
	if saved.LastCapacityFreeBytes != nil || saved.LastCapacityCheckedAt != nil || saved.LastCapacityStatus != "" {
		t.Fatalf("ordinary create imported private sample fields: %+v", storageCapacity(saved, time.Now().UTC()))
	}
	driver := &dashboardQuotaDriver{fakeCloudDriver: base, quota: cloud.Quota{UsedBytes: 10, TotalBytes: 100}}
	connections.drivers[conn.ID] = driver
	service := NewStorageService(db, NewAuditService(db))
	service.SetConnectionService(connections)
	view, err := service.RefreshCapacity(context.Background(), actor, row.ID, RequestContext{})
	if err != nil || view.Status != "ok" || view.FreeBytes == nil || *view.FreeBytes != 90 || view.TotalBytes == nil || *view.TotalBytes != 100 || view.SampledAt == nil || driver.calls != 1 {
		t.Fatalf("capacity updates not writable after schema upgrade: %+v %v calls=%d", view, err, driver.calls)
	}
}

func TestStorageCapacitySamplesOptionalCapabilityPreservesSuccessAndFences(t *testing.T) {
	base := &fakeCloudDriver{}
	db, _, connections, actor := newConnectionTestService(t, base)
	conn, err := connections.Create(actor, ConnectionInput{Name: "Quota", Provider: cloud.ProviderPan115, Cookie: testPan115Cookie, Enabled: true}, RequestContext{})
	if err != nil {
		t.Fatal(err)
	}
	driver := &dashboardQuotaDriver{fakeCloudDriver: base, quota: cloud.Quota{UsedBytes: 10, TotalBytes: 100}}
	connections.drivers[conn.ID] = driver
	storage := models.Storage{Name: "Quota storage", NameNormalized: "quota-storage", Type: cloud.ProviderPan115, RootPath: "root", RootDisplayPath: "/", RootPathNormalized: "quota:root", ConnectionID: &conn.ID, Enabled: true, Capabilities: "{}"}
	if err := db.Create(&storage).Error; err != nil {
		t.Fatal(err)
	}
	service := NewStorageService(db, NewAuditService(db))
	service.SetConnectionService(connections)
	view, err := service.RefreshCapacity(context.Background(), actor, storage.ID, RequestContext{})
	if err != nil || view.Status != "ok" || view.FreeBytes == nil || *view.FreeBytes != 90 || view.SampledAt == nil || driver.calls != 1 || base.probeCalls != 0 || base.statCalls != 0 {
		t.Fatalf("sample=%+v err=%v calls=%d", view, err, driver.calls)
	}
	first := *view.SampledAt
	actor.Permissions[authz.PermissionDashboardRead] = struct{}{}
	admin := NewAdminService(db, nil, nil, nil)
	read, readErr := admin.DashboardActivity(context.Background(), actor)
	if readErr != nil || read.Capacities.Status != "ok" || len(read.Capacities.List) != 1 || *read.Capacities.List[0].FreeBytes != 90 || driver.calls != 1 {
		t.Fatalf("read projection probed provider or lost sample: %+v %v calls=%d", read.Capacities, readErr, driver.calls)
	}
	if _, err := service.RefreshCapacity(context.Background(), actor, storage.ID, RequestContext{}); ErrorCode(err) != CodeConflict || driver.calls != 1 {
		t.Fatalf("cooldown=%v calls=%d", err, driver.calls)
	}
	reset := func() {
		t.Helper()
		if err := db.Model(&models.Storage{}).Where("id=?", storage.ID).UpdateColumn("last_capacity_checked_at", time.Now().UTC().Add(-2*time.Minute)).Error; err != nil {
			t.Fatal(err)
		}
	}
	reset()
	driver.err = errors.New("private provider detail")
	view, err = service.RefreshCapacity(context.Background(), actor, storage.ID, RequestContext{})
	if err != nil || view.Status != "error" || *view.FreeBytes != 90 || !view.SampledAt.Equal(first) || view.ErrorCode != "storage_capacity_unavailable" {
		t.Fatalf("failed sample=%+v %v", view, err)
	}
	reset()
	driver.err = nil
	driver.before = func() {
		if err := db.Model(&models.Storage{}).Where("id=?", storage.ID).Updates(map[string]any{"root_path": "different-root", "updated_at": time.Now().UTC()}).Error; err != nil {
			t.Fatal(err)
		}
	}
	if _, err := service.RefreshCapacity(context.Background(), actor, storage.ID, RequestContext{}); ErrorCode(err) != CodeConflict {
		t.Fatalf("root fence=%v", err)
	}
	var saved models.Storage
	if err := db.First(&saved, storage.ID).Error; err != nil || saved.LastCapacityFreeBytes == nil || *saved.LastCapacityFreeBytes != 90 {
		t.Fatal("old sample lost", err)
	}
	projected := storageCapacity(saved, time.Now().UTC())
	if projected.Status != "not_sampled" || projected.FreeBytes != nil || projected.SampledAt != nil {
		t.Fatalf("old root sample attached to changed source: %+v", projected)
	}
	driver.before, driver.err = nil, errors.New("provider unavailable")
	view, err = service.RefreshCapacity(context.Background(), actor, storage.ID, RequestContext{})
	if err != nil || view.Status != "error" || view.FreeBytes != nil || view.SampledAt != nil {
		t.Fatalf("new scope failure exposed old values: %+v %v", view, err)
	}
}

func TestStorageCapacityUnsupportedAndLocalDoNotFabricateSpace(t *testing.T) {
	db, _, connections, actor := newConnectionTestService(t, &fakeCloudDriver{})
	conn, err := connections.Create(actor, ConnectionInput{Name: "No quota", Provider: cloud.ProviderPan115, Cookie: testPan115Cookie, Enabled: true}, RequestContext{})
	if err != nil {
		t.Fatal(err)
	}
	row := models.Storage{Name: "No quota", NameNormalized: "no-quota", Type: cloud.ProviderPan115, RootPath: "root", RootDisplayPath: "/", RootPathNormalized: "no-quota-root", ConnectionID: &conn.ID, Enabled: true, Capabilities: "{}"}
	if err := db.Create(&row).Error; err != nil {
		t.Fatal(err)
	}
	service := NewStorageService(db, NewAuditService(db))
	service.SetConnectionService(connections)
	view, err := service.RefreshCapacity(context.Background(), actor, row.ID, RequestContext{})
	if err != nil || view.Status != "unsupported" || view.FreeBytes != nil || view.TotalBytes != nil || view.SampledAt != nil {
		t.Fatalf("unsupported=%+v %v", view, err)
	}
	root := t.TempDir()
	local, err := service.Create(actor, StorageInput{Name: "Local quota", Type: models.StorageTypeLocal, RootPath: root, Enabled: true}, RequestContext{})
	if err != nil {
		t.Fatal(err)
	}
	view, err = service.RefreshCapacity(context.Background(), actor, local.ID, RequestContext{})
	if err != nil || view.Status != "ok" || view.TotalBytes == nil || *view.TotalBytes == 0 || view.FreeBytes == nil {
		t.Fatalf("local=%+v %v", view, err)
	}
	actor.Permissions = map[string]struct{}{authz.PermissionDashboardRead: {}}
	if _, err := service.RefreshCapacity(context.Background(), actor, row.ID, RequestContext{}); ErrorCode(err) != CodePermissionDenied {
		t.Fatalf("permissions=%v", err)
	}
}
