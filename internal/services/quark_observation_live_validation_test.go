//go:build quark_live

package services

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/cloud"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/cloud/quark"
)

type quarkObservationLiveFake struct {
	fakeCloudDriver
	root, sample cloud.Item
	reads        atomic.Int32
	bad          bool
}

func (d *quarkObservationLiveFake) Provider() string { return cloud.ProviderQuark }
func (d *quarkObservationLiveFake) Capabilities() cloud.Capabilities {
	return cloud.Capabilities{DirectoryList: true, TemporaryDirectURL: true, SignedProxy: true}
}
func (d *quarkObservationLiveFake) List(_ context.Context, id string, page cloud.PageRequest) (cloud.Page, error) {
	d.reads.Add(1)
	if id != d.root.ID || page.Offset != 0 {
		return cloud.Page{}, ErrObservationInvalid
	}
	total := int64(1)
	item := d.sample
	if d.bad {
		item.ID = "unrelated-private-id"
	}
	return cloud.Page{Items: []cloud.Item{item}, Offset: 0, Total: &total}, nil
}
func (d *quarkObservationLiveFake) Stat(_ context.Context, id string) (cloud.Item, error) {
	if id == d.root.ID {
		return d.root, nil
	}
	if id == d.sample.ID {
		return d.sample, nil
	}
	return cloud.Item{}, ErrObservationInvalid
}

func quarkObservationLiveFixture(t *testing.T) (QuarkObservationLiveInput, *quarkObservationLiveFake) {
	t.Helper()
	bundle := quark.NewCredentials()
	bundle.TV = &quark.Authorization{AccessToken: "synthetic-private-token", AccountID: "synthetic-account", DeviceID: "device", Generation: 1, IdentityVerified: true}
	raw, _ := json.Marshal(bundle)
	root := cloud.Item{ID: quark.QualifyID(quark.RouteTV, bundle.TV.AccountID, "root"), Name: "generated-root", IsDir: true}
	sample := cloud.Item{ID: quark.QualifyID(quark.RouteTV, bundle.TV.AccountID, "sample"), ParentID: root.ID, Name: "Native.Observation.2026.mp4", Size: 10998, ModifiedAt: time.Date(2026, 10, 22, 0, 0, 0, 0, time.UTC)}
	driver := &quarkObservationLiveFake{root: root, sample: sample}
	registry := cloud.NewRegistry()
	if err := registry.Register(cloud.ProviderQuark, func(config cloud.Config) (cloud.Driver, error) {
		if strings.Contains(string(config.Credentials), "refresh-private") {
			t.Fatal("refresh forwarded")
		}
		return driver, nil
	}); err != nil {
		t.Fatal(err)
	}
	return QuarkObservationLiveInput{StateDir: t.TempDir(), Credentials: raw, Registry: registry, Root: root, RootPath: "/generated-root", Sample: sample, BaseURL: "http://127.0.0.1:19443", RequestCount: func() int { return int(driver.reads.Load()) }}, driver
}

func TestQuarkObservationLiveAssemblerOffline(t *testing.T) {
	in, driver := quarkObservationLiveFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	report, err := RunQuarkObservationLiveValidation(ctx, in)
	if err != nil {
		t.Fatalf("stage=%s error=%v", report.Stage, err)
	}
	if !report.FirstCoverageComplete || !report.SameDBRestart || !report.RestartBeforeArtifacts || !report.PendingFollowupRecovered || !report.NoopUnchanged || report.NoopObservationPages < 1 || report.NoopRequests < 1 || report.STRMCount != 1 || report.ObservationPages < 2 || report.Requests != int(driver.reads.Load()) || report.Requests < 2 || len(report.STRMSHA256) != 64 || !report.LocalNameMetadataOnly || !report.FixtureConnectionStorage || report.PlaybackTested || report.AutomaticTimingTested {
		t.Fatalf("invalid safe report: %+v", report)
	}
	encoded, _ := json.Marshal(report)
	for _, private := range []string{"synthetic-private", "synthetic-account", "http://", in.StateDir, in.Sample.ID} {
		if strings.Contains(string(encoded), private) {
			t.Fatal("report leaked private value")
		}
	}
	// A second invocation cannot silently reinitialize the existing local DB.
	before := driver.reads.Load()
	if _, err = RunQuarkObservationLiveValidation(ctx, in); err == nil || driver.reads.Load() != before {
		t.Fatal("existing acceptance state was reused without explicit recovery")
	}
}

func TestQuarkObservationLiveInputBoundaries(t *testing.T) {
	for _, name := range []string{"refresh", "web", "remote", "host", "large", "unrelated", "occupied"} {
		t.Run(name, func(t *testing.T) {
			in, driver := quarkObservationLiveFixture(t)
			switch name {
			case "refresh":
				bundle, _ := quark.ParseCredentials(in.Credentials)
				bundle.TV.RefreshToken = "refresh-private"
				in.Credentials, _ = json.Marshal(bundle)
			case "web":
				bundle, _ := quark.ParseCredentials(in.Credentials)
				bundle.Web = &quark.WebAuthorization{Cookie: "cookie-private"}
				in.Credentials, _ = json.Marshal(bundle)
			case "remote":
				in.BaseURL = "https://example.com"
			case "host":
				in.BaseURL = "http://localhost:19443"
			case "large":
				in.Sample.Size = 1 << 20
			case "unrelated":
				in.Sample.ParentID = "other-private-root"
			case "occupied":
				if err := os.WriteFile(filepath.Join(in.StateDir, "preserve.txt"), []byte("keep"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			report, err := RunQuarkObservationLiveValidation(context.Background(), in)
			if err == nil || report.Stage != "input" || driver.reads.Load() != 0 || strings.Contains(err.Error(), "private") {
				t.Fatalf("invalid boundary: %+v %v", report, err)
			}
		})
	}
}

func TestQuarkObservationLiveProductionFence(t *testing.T) {
	in, driver := quarkObservationLiveFixture(t)
	r, err := quarkObservationLiveOpen(context.Background(), in, true)
	if err != nil {
		t.Fatal(err)
	}
	defer r.close()
	if err = r.db.Model(&models.Connection{}).Where("provider = ?", "quark").Update("enabled", false).Error; err != nil {
		t.Fatal(err)
	}
	if err = r.round(context.Background()); err == nil || driver.reads.Load() != 0 {
		t.Fatal("production source fence was bypassed")
	}
}

func TestQuarkObservationLiveUnexpectedCloudIdentity(t *testing.T) {
	in, driver := quarkObservationLiveFixture(t)
	driver.bad = true
	report, err := RunQuarkObservationLiveValidation(context.Background(), in)
	if err == nil || report.FirstCoverageComplete || strings.Contains(err.Error(), "unrelated-private") {
		t.Fatalf("unexpected identity accepted: %+v %v", report, err)
	}
}
