package hostapi

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/plugins/contract"
)

func TestPluginArtworkUsesOpaquePermissionBoundImageProxy(t *testing.T) {
	fixture := newHostFixture(t, []contract.Permission{{Kind: contract.PermissionNetworkHTTP, Domains: []string{"cdn.example.test"}}})
	image := []byte{0xff, 0xd8, 0xff, 0xd9}
	client := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		if request.Header.Get("Authorization") != "" || request.Header.Get("Cookie") != "" {
			t.Fatal("artwork request carried provider credentials")
		}
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"image/jpeg"}}, Body: io.NopCloser(bytes.NewReader(image)), ContentLength: int64(len(image)), Request: request}, nil
	})}
	host := New(fixture.db, fixture.credentials, zerolog.Nop(), WithHTTPClient(client), WithResolver(publicResolver))
	upstream := "https://cdn.example.test/poster.jpg?token=private"
	reference, err := host.RegisterArtwork(context.Background(), fixture.pluginID, fixture.connection.ID, upstream)
	if err != nil || reference == "" || strings.Contains(reference, "token") || strings.Contains(reference, "cdn") {
		t.Fatalf("reference=%q err=%v", reference, err)
	}
	again, err := host.RegisterArtwork(context.Background(), fixture.pluginID, fixture.connection.ID, upstream)
	if err != nil || again != reference {
		t.Fatalf("image reference was not stable within a server process: %q %q %v", reference, again, err)
	}
	if _, err := host.RegisterArtwork(context.Background(), fixture.pluginID, fixture.connection.ID, "https://other.example.test/poster.jpg"); ErrorCode(err) != "plugin_artwork_url_denied" {
		t.Fatalf("undeclared host accepted: %v", err)
	}
	if _, err := host.RegisterArtwork(context.Background(), fixture.pluginID, fixture.connection.ID, "http://cdn.example.test/poster.jpg"); ErrorCode(err) != "plugin_artwork_url_denied" {
		t.Fatalf("non-TLS image accepted: %v", err)
	}
	if _, err := host.RegisterArtwork(context.Background(), fixture.pluginID, fixture.connection.ID, "https://127.0.0.1/poster.jpg"); err == nil {
		t.Fatal("private IP image accepted")
	}
	if _, err := host.OpenAsset(context.Background(), reference, http.MethodGet, ""); ErrorCode(err) != "plugin_artwork_reference_denied" {
		t.Fatalf("artwork escaped through generic media asset transport: %v", err)
	}
	if _, err := host.OpenAssetForPluginConnection(context.Background(), fixture.pluginID, fixture.connection.ID, reference, http.MethodGet, ""); ErrorCode(err) != "plugin_artwork_reference_denied" {
		t.Fatalf("artwork escaped through connection-bound media asset transport: %v", err)
	}
	body, contentType, err := host.OpenArtwork(context.Background(), reference)
	if err != nil || contentType != "image/jpeg" || !bytes.Equal(body, image) {
		t.Fatalf("image=%x type=%q err=%v", body, contentType, err)
	}
	if err := fixture.db.Model(&models.PluginConnection{}).Where("id = ?", fixture.connection.ID).Update("enabled", false).Error; err != nil {
		t.Fatal(err)
	}
	if _, _, err := host.OpenArtwork(context.Background(), reference); ErrorCode(err) != "plugin_asset_expired" {
		t.Fatalf("disabled plugin connection could still serve artwork: %v", err)
	}
}

func TestPluginArtworkRejectsInvalidMimeSizeAndRedirect(t *testing.T) {
	fixture := newHostFixture(t, []contract.Permission{{Kind: contract.PermissionNetworkHTTP, Domains: []string{"cdn.example.test"}}})
	var body []byte
	contentType := "image/jpeg"
	status := http.StatusOK
	headers := http.Header{}
	client := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		replyHeaders := headers.Clone()
		replyHeaders.Set("Content-Type", contentType)
		return &http.Response{StatusCode: status, Header: replyHeaders, Body: io.NopCloser(bytes.NewReader(body)), ContentLength: int64(len(body)), Request: request}, nil
	})}
	host := New(fixture.db, fixture.credentials, zerolog.Nop(), WithHTTPClient(client), WithResolver(publicResolver))
	ref, err := host.RegisterArtwork(context.Background(), fixture.pluginID, fixture.connection.ID, "https://cdn.example.test/image.jpg")
	if err != nil {
		t.Fatal(err)
	}
	body = []byte("<script>alert(1)</script>")
	if _, _, err := host.OpenArtwork(context.Background(), ref); ErrorCode(err) != "plugin_artwork_body_invalid" {
		t.Fatalf("forged jpeg accepted: %v", err)
	}
	contentType = "text/html"
	if _, _, err := host.OpenArtwork(context.Background(), ref); ErrorCode(err) != "plugin_artwork_type_invalid" {
		t.Fatalf("HTML accepted: %v", err)
	}
	contentType = "image/jpeg"
	body = append([]byte{0xff, 0xd8, 0xff}, bytes.Repeat([]byte{'x'}, maxPluginArtworkBytes)...)
	if _, _, err := host.OpenArtwork(context.Background(), ref); ErrorCode(err) != "plugin_artwork_size_invalid" {
		t.Fatalf("oversize image accepted: %v", err)
	}
	body = nil
	status = http.StatusFound
	headers.Set("Location", "https://not-granted.example.test/image.jpg")
	if _, _, err := host.OpenArtwork(context.Background(), ref); err == nil {
		t.Fatal("redirect to undeclared domain accepted")
	}
	host.now = func() time.Time { return time.Now().UTC().Add(pluginArtworkTTL + time.Second) }
	if _, _, err := host.OpenArtwork(context.Background(), ref); ErrorCode(err) != "plugin_asset_expired" {
		t.Fatalf("expired artwork reference accepted: %v", err)
	}
}
