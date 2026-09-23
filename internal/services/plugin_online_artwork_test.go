package services

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"
)

type testOnlineArtworkGateway struct {
	requests []string
}

func (gateway *testOnlineArtworkGateway) RegisterArtwork(_ context.Context, pluginID, connectionID, upstream string) (string, error) {
	gateway.requests = append(gateway.requests, pluginID+"|"+connectionID+"|"+upstream)
	if strings.Contains(upstream, "denied.example") {
		return "", errors.New("domain denied")
	}
	return uuid.NewSHA1(uuid.NameSpaceURL, []byte(upstream)).String(), nil
}

func TestOnlineArtworkProjectionHidesProviderURLsAcrossNestedMedia(t *testing.T) {
	gateway := &testOnlineArtworkGateway{}
	service := &PluginRepositoryService{artwork: gateway}
	raw := json.RawMessage(`{"sections":[{"items":[{"work":{"posterUrl":"https://cdn.example.test/p.jpg?token=secret","backdropUrl":"https://denied.example/b.jpg","stillUrls":["https://cdn.example.test/s.jpg","javascript:alert(1)"],"segments":[{"id":"s","versions":[{"id":"v","thumbnailUrl":"https://cdn.example.test/v.jpg"}]}]}}]}],"urlRef":"unchanged-asset"}`)
	projected, err := service.projectOnlineArtwork(context.Background(), "plugin-id", "connection-id", raw)
	if err != nil {
		t.Fatal(err)
	}
	if bytes := string(projected); strings.Contains(bytes, "cdn.example") || strings.Contains(bytes, "denied.example") || strings.Contains(bytes, "secret") || strings.Contains(bytes, "javascript:") || !strings.Contains(bytes, "/api/v1/player/artwork/") || !strings.Contains(bytes, "unchanged-asset") {
		t.Fatalf("provider image URL escaped Server boundary: %s", bytes)
	}
	if len(gateway.requests) != 5 {
		t.Fatalf("unexpected image registration count: %d", len(gateway.requests))
	}
	withoutGateway := &PluginRepositoryService{}
	stripped, err := withoutGateway.projectOnlineArtwork(context.Background(), "plugin-id", "connection-id", raw)
	if err != nil || strings.Contains(string(stripped), "cdn.example") || strings.Contains(string(stripped), "denied.example") {
		t.Fatalf("missing gateway leaked image URL: %s err=%v", stripped, err)
	}
}

func TestOnlineArtworkProjectionRejectsExcessiveDepth(t *testing.T) {
	service := &PluginRepositoryService{}
	raw := strings.Repeat("[", 22) + "null" + strings.Repeat("]", 22)
	if _, err := service.projectOnlineArtwork(context.Background(), "plugin-id", "connection-id", json.RawMessage(raw)); ErrorCode(err) != CodePluginResponseInvalid {
		t.Fatalf("deep provider image response accepted: %v", err)
	}
}
