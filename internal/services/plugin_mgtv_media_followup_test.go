package services

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/authz"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/plugins/contract"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/plugins/hostapi"
	pluginruntime "github.com/yuanjing-hash/OhMyCine-Server/internal/plugins/runtime"
)

// Actual frozen guest bytes -> real Host -> service DTO validation. All HTTP
// responses are recorded public controls or explicitly synthetic account/dispatch
// controls. No real account, upstream write or media segment is accessed.
func TestMangoWASMFollowupPagedDetailPlaybackScopedAssetsAndSystemHistory(t *testing.T) {
	s, actor, id, _, control := mangoBrowseService(t, false)
	ctx := context.Background()
	fixtures := control.files
	cipher, err := s.credentials.Encrypt(hostapi.CredentialPurpose("org.ohmycine.mgtv", id, "mgtv.session"), "fixture_session=synthetic-only")
	mustCatalogue(t, err)
	mustCatalogue(t, s.db.Model(&models.PluginConnection{}).Where("id = ?", id).Updates(map[string]any{"credential_ciphertext": cipher, "credential_version": 1}).Error)
	history := NewPlayerHistoryService(s.db)
	history.SetPluginService(s)
	pages := []string{}
	streamCookie := false
	dispatches := 0
	mediaReads := 0
	preview := false
	cdn := "pcvideotx.titan.mgtv.com"
	client := &http.Client{Transport: authControlTransport(func(request *http.Request) (*http.Response, error) {
		if request.Method != http.MethodGet {
			t.Fatal("guest attempted an upstream write")
		}
		path := request.URL.Path
		query := request.URL.Query()
		file := ""
		body := []byte{}
		mime := "application/json"
		switch path {
		case "/video/info":
			switch query.Get("cid") {
			case "887305":
				file = "info-variety.json"
			case "778406":
				file = "info.json"
			case "687100":
				file = "info-free.json"
			default:
				t.Fatal("unobserved info identity")
			}
		case "/episode/list":
			if query.Get("video_id") == "24691407" {
				page := query.Get("page")
				pages = append(pages, page)
				if page != "1" && page != "2" {
					t.Fatal("variety paging used locate-page zero")
				}
				file = "episodes-variety-page" + page + ".json"
			} else if query.Get("video_id") == "22025165" {
				body = []byte(`{"code":200,"data":{"current_page":1,"total_page":1,"list":[],"short":[]}}`)
			} else {
				file = "episodes.json"
			}
		case "/video/streamList":
			streamCookie = request.Header.Get("Cookie") == "fixture_session=synthetic-only"
			if !streamCookie {
				t.Fatal("Host failed to attach stored account credential to streamList")
			}
			if query.Get("auth_mode") != "1" || query.Get("type") != "pch5" || query.Get("cid") != "687100" || query.Get("video_id") != "22025165" {
				t.Fatal("official authorized stream profile or exact identity changed")
			}
			if preview {
				file = "stream-preview.json"
			} else {
				file = "stream-free.json"
			}
		case "/atcl":
			dispatches++
			target := "https://unreviewed.invalid/fixture.m3u8"
			if request.URL.Hostname() == "web-disp1.titan.mgtv.com" {
				target = "https://" + cdn + "/fixture.m3u8"
			}
			body = []byte(fmt.Sprintf(`{"status":"ok","info":%q}`, target))
		case "/v2/video/title":
			body = []byte(`{"code":200,"data":{"partId":"22025165","title":[]}}`)
		case "/fixture.m3u8":
			mime = "application/vnd.apple.mpegurl"
			body = []byte("#EXTM3U\n#EXT-X-TARGETDURATION:10\n#EXTINF:10,\nsegment.ts\n#EXT-X-ENDLIST\n")
		default:
			if strings.HasSuffix(path, ".ts") {
				mediaReads++
				t.Fatal("test unexpectedly fetched media bytes")
			}
			t.Fatal("unexpected control endpoint")
		}
		if path != "/video/streamList" && request.Header.Get("Cookie") != "" {
			t.Fatal("credential leaked to anonymous info/dispatch/subtitle/CDN")
		}
		if file != "" {
			var e error
			body, e = os.ReadFile(filepath.Join(fixtures, file))
			if e != nil {
				return nil, e
			}
		}
		if file == "stream-preview.json" {
			// Explicitly synthetic identity rebinding; the observed denial/trial
			// indicators remain untouched. This is not a real VIP-account acceptance.
			var value map[string]any
			_ = json.Unmarshal(body, &value)
			data := value["data"].(map[string]any)
			info := data["info"].(map[string]any)
			info["collection_id"], info["video_id"] = "687100", "22025165"
			body, _ = json.Marshal(value)
		}
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {mime}}, Body: io.NopCloser(bytes.NewReader(body)), Request: request}, nil
	})}
	api := hostapi.New(s.db, s.credentials, zerolog.Nop(), hostapi.WithHTTPClient(client), hostapi.WithResolver(func(context.Context, string) ([]net.IPAddr, error) {
		return []net.IPAddr{{IP: net.ParseIP("203.0.113.10")}}, nil
	}))
	runtime := s.runtime.(*pluginruntime.Host)
	runtime.SetCapabilityHost(authTraceHost{api: api, t: t})
	s.artwork, s.offline = api, api
	raw, err := s.OnlineDetail(ctx, actor, id, "clip:887305")
	mustCatalogue(t, err)
	var work contract.MediaWork
	mustCatalogue(t, json.Unmarshal(raw, &work))
	if len(work.Segments) != 80 || strings.Join(pages, ",") != "1,2" || work.Segments[0].ID != "video:24485448" || work.DefaultSegmentID != "video:24691407" {
		t.Fatalf("paged official detail mismatch: segments=%d pages=%s default=%s", len(work.Segments), strings.Join(pages, ","), work.DefaultSegmentID)
	}
	for _, segment := range work.Segments {
		if segment.ID == "video:24618061" {
			if segment.EpisodeNumber != nil || len(segment.Versions) != 1 || segment.Versions[0].Edition != "超前彩蛋" {
				t.Fatal("special edition collapsed into guessed episode")
			}
		}
		if segment.ID == "video:24621496" && segment.EpisodeNumber != nil {
			t.Fatal("variety period guessed episode number")
		}
	}
	raw, err = s.OnlineDetail(ctx, actor, id, "clip:778406")
	mustCatalogue(t, err)
	mustCatalogue(t, json.Unmarshal(raw, &work))
	if len(work.Segments) != 76 {
		t.Fatal("drama official main+short count above requested size was truncated")
	}
	raw, err = s.OnlineDetail(ctx, actor, id, "clip:687100")
	mustCatalogue(t, err)
	for _, host := range []string{"pcvideotx.titan.mgtv.com", "pcvideoctyun.titan.mgtv.com"} {
		cdn = host
		before := dispatches
		raw, err = s.OnlinePlayback(ctx, actor, id, "clip:687100", "video:22025165", "mgtv:687100:22025165", "")
		mustCatalogue(t, err)
		var plan struct {
			Variant string `json:"variantId"`
			Assets  []struct {
				URL string `json:"urlRef"`
			} `json:"assets"`
		}
		mustCatalogue(t, json.Unmarshal(raw, &plan))
		if plan.Variant == "" || len(plan.Assets) != 1 || !strings.HasPrefix(plan.Assets[0].URL, "/api/v1/player/online-assets/") || dispatches != before+2 {
			t.Fatal("dispatcher fallback did not preserve exact selected stream")
		}
		ref := strings.TrimPrefix(plan.Assets[0].URL, "/api/v1/player/online-assets/")
		asset, e := api.ResolveAsset(ref)
		mustCatalogue(t, e)
		if asset.ConnectionID != id || asset.PluginID != "org.ohmycine.mgtv" || !strings.Contains(asset.URL, host) {
			t.Fatal("approved CDN asset lost exact scope")
		}
		actor.Permissions[authz.PermissionDownloadsCreate] = struct{}{}
		offline, e := s.OnlineOfflinePlan(ctx, actor, id, "clip:687100", "video:22025165", "mgtv:687100:22025165", plan.Variant)
		mustCatalogue(t, e)
		if len(offline.Tracks) != 1 || len(offline.Tracks[0].Units) != 1 {
			t.Fatal("actual guest offline HLS topology missing")
		}
	}
	_, err = s.SyncOnlineProgress(ctx, actor, id, "clip:687100", "video:22025165", "mgtv:687100:22025165", "progress", 10, floatPointer(1000), "fixture-system-progress", time.Now().Format(time.RFC3339Nano))
	mustCatalogue(t, err)
	page, err := history.List(actor, 1, 24, "server")
	mustCatalogue(t, err)
	if len(page.List) != 1 || page.List[0].Title != "浴火之路" || page.List[0].ItemToken != onlineHistoryToken(id, "clip:687100", "video:22025165", "mgtv:687100:22025165") {
		t.Fatal("actual guest exact detail failed Server-owned history")
	}
	preview = true
	if _, err = s.OnlinePlayback(ctx, actor, id, "clip:687100", "video:22025165", "mgtv:687100:22025165", ""); ErrorCode(err) != CodePluginOnlineAuthentication {
		t.Fatalf("observed anonymous trial error not mapped safely: %s", ErrorCode(err))
	}
	if mediaReads != 0 || !streamCookie {
		t.Fatal("controlled smoke crossed media/account boundary")
	}
	t.Log("actual WASM: 80 variety items, 46 main + 30 short drama items, explicit pages 1/2, provider default, edition metadata, Cookie profile, tx/ctyun scoped playback/offline controls, system history, trial denial; no media bytes or real account")
}

func TestMangoWASMCatalogueRestartReusesRawDTOAndMintsProtectedArtwork(t *testing.T) {
	s, actor, id, api, control := mangoBrowseService(t, false)
	ctx := context.Background()
	root, err := s.OnlineNavigation(ctx, actor, id)
	mustCatalogue(t, err)
	var firstRoot pluginNavigationResponse
	mustCatalogue(t, json.Unmarshal(root, &firstRoot))
	var oldToken string
	for _, node := range firstRoot.Nodes {
		if node.ID == "channel-2" {
			oldToken = node.NodeToken
		}
	}
	if oldToken == "" {
		t.Fatal("official category token missing")
	}
	_, err = s.OnlineNavigationChildren(ctx, actor, id, oldToken)
	mustCatalogue(t, err)
	feed, err := s.OnlineFeed(ctx, actor, id, "catalog:2", "", "")
	mustCatalogue(t, err)
	var sections []contract.FeedSection
	mustCatalogue(t, json.Unmarshal(feed, &sections))
	oldPoster := sections[0].Items[0].Work.PosterURL
	before := control.reads
	newAPI := hostapi.New(s.db, s.credentials, zerolog.Nop(), hostapi.WithHTTPClient(control.client), hostapi.WithResolver(func(context.Context, string) ([]net.IPAddr, error) {
		return []net.IPAddr{{IP: net.ParseIP("203.0.113.10")}}, nil
	}))
	s.runtime.(*pluginruntime.Host).SetCapabilityHost(authTraceHost{api: newAPI, t: t})
	restarted := NewPluginRepositoryService(s.db, s.audit, s.fetcher, s.log, WithPluginRuntimeHost(s.runtime), WithPluginArtworkGateway(newAPI), WithPluginCredentialStore(s.credentials))
	root, err = restarted.OnlineNavigation(ctx, actor, id)
	mustCatalogue(t, err)
	var currentRoot pluginNavigationResponse
	mustCatalogue(t, json.Unmarshal(root, &currentRoot))
	var token string
	for _, node := range currentRoot.Nodes {
		if node.ID == "channel-2" {
			token = node.NodeToken
		}
	}
	if token == "" || token == oldToken {
		t.Fatal("restart did not re-sign current process navigation")
	}
	_, err = restarted.OnlineNavigationChildren(ctx, actor, id, token)
	mustCatalogue(t, err)
	feed, err = restarted.OnlineFeed(ctx, actor, id, "catalog:2", "", "")
	mustCatalogue(t, err)
	mustCatalogue(t, json.Unmarshal(feed, &sections))
	poster := sections[0].Items[0].Work.PosterURL
	if control.reads != before || poster == oldPoster || !strings.HasPrefix(poster, "/api/v1/player/artwork/") {
		t.Fatal("persistent all-hit browse called provider or reused old image lease")
	}
	if _, _, err = newAPI.OpenArtwork(ctx, strings.TrimPrefix(poster, "/api/v1/player/artwork/")); err != nil {
		t.Fatal("new protected image reference unavailable")
	}
	if _, err = api.ResolveAsset(strings.TrimPrefix(poster, "/api/v1/player/artwork/")); err == nil {
		t.Fatal("new image reference accidentally owned by prior Host")
	}
	if _, err = restarted.OnlineFeed(ctx, Actor{}, id, "catalog:2", "", ""); ErrorCode(err) != CodePermissionDenied {
		t.Fatal("cached actual guest DTO bypassed actor read")
	}
	t.Log("persisted actual guest root, child and first feed caused zero upstream control HTTP after service/Host recreation; navigation and protected artwork regenerated")
}
