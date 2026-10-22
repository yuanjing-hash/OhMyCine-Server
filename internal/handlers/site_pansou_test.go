package handlers

import (
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
)

func TestMixedDiscoveryQueryValidation(t *testing.T) {
	for _, query := range []string{"type_policy=bogus", "type_policy=manual", "cloud_types=untrusted", "refresh=yes", "page=2&refresh=true", "snapshot_id=short", "snapshot_id=0123456789abcdef0123456789abcdef0123456789abcdef"} {
		t.Run(query, func(t *testing.T) {
			c, _ := gin.CreateTestContext(httptest.NewRecorder())
			c.Request = httptest.NewRequest("GET", "/api/v1/discovery/torrent-search?keyword=Show&"+query, nil)
			if _, err := parsePTSearch(c); err == nil {
				t.Fatal("invalid query accepted")
			}
		})
	}
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest("GET", "/api/v1/discovery/torrent-search?keyword=Show&site_id=1&page=2&client_capabilities=mixed_discovery_v1&cloud_types=115,quark&cloud_types=magnet&type_policy=manual&snapshot_id=0123456789abcdef0123456789abcdef0123456789abcdef", nil)
	input, err := parsePTSearch(c)
	if err != nil || len(input.CloudTypes) != 3 || len(input.ClientCapabilities) != 1 || input.Page != 2 {
		t.Fatalf("input=%+v err=%v", input, err)
	}
}
