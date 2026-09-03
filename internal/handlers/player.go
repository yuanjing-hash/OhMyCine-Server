package handlers

import (
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/authz"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/middleware"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/services"
)

type playerDeviceResponse struct {
	ID                string    `json:"id"`
	Name              string    `json:"name"`
	ClientKind        string    `json:"client_kind"`
	CreatedAt         time.Time `json:"created_at"`
	LastSeenAt        time.Time `json:"last_seen_at"`
	IdleExpiresAt     time.Time `json:"idle_expires_at"`
	AbsoluteExpiresAt time.Time `json:"absolute_expires_at"`
}

type playerNoStoreWriter struct{ http.ResponseWriter }

func (writer playerNoStoreWriter) WriteHeader(statusCode int) {
	writer.Header().Set("Cache-Control", "no-store")
	writer.ResponseWriter.WriteHeader(statusCode)
}

func playerDeviceDTO(device models.DeviceToken) playerDeviceResponse {
	return playerDeviceResponse{
		ID: device.ID, Name: device.DeviceName, ClientKind: device.ClientKind,
		CreatedAt: device.CreatedAt, LastSeenAt: device.LastSeenAt,
		IdleExpiresAt: device.IdleExpiresAt, AbsoluteExpiresAt: device.AbsoluteExpiresAt,
	}
}

func (a *API) PlayerLogin(c *gin.Context) {
	c.Header("Cache-Control", "no-store")
	var input struct {
		Username   string `json:"username" binding:"required"`
		Password   string `json:"password" binding:"required"`
		DeviceID   string `json:"device_id" binding:"required"`
		DeviceName string `json:"device_name" binding:"required"`
	}
	if err := c.ShouldBindJSON(&input); err != nil {
		writeError(c, a.log, invalid("请输入 Server 账号、密码和设备信息", err))
		return
	}
	actor, token, device, err := a.auth.DeviceLogin(input.Username, input.Password, input.DeviceID, input.DeviceName, middleware.RequestContextFrom(c))
	if err != nil {
		writeError(c, a.log, err)
		return
	}
	success(c, http.StatusOK, gin.H{
		"access_token": token, "token_type": "Bearer",
		"user": services.CurrentUserFromActor(actor), "device": playerDeviceDTO(device),
	})
}

func (a *API) PlayerLogout(c *gin.Context) {
	c.Header("Cache-Control", "no-store")
	actor, _ := middleware.ActorFrom(c)
	device, _ := middleware.DeviceFrom(c)
	if err := a.auth.DeviceLogout(middleware.DeviceTokenFrom(c), actor, device, middleware.RequestContextFrom(c)); err != nil {
		writeError(c, a.log, err)
		return
	}
	success(c, http.StatusOK, gin.H{"logged_out": true})
}

func (a *API) PlayerBootstrap(c *gin.Context) {
	c.Header("Cache-Control", "no-store")
	actor, _ := middleware.ActorFrom(c)
	device, _ := middleware.DeviceFrom(c)
	mediaLibraryCount := 0
	if actor.HasPermission(authz.PermissionMediaLibrariesRead) {
		libraries, err := a.libraries.PlayerLibraries(actor)
		if err != nil {
			writeError(c, a.log, err)
			return
		}
		mediaLibraryCount = len(libraries)
	}
	embyInstances := []services.PlayerEmbyInstance{}
	if a.connections != nil {
		var err error
		embyInstances, err = a.connections.PlayerEmbyInstances(actor)
		if err != nil {
			writeError(c, a.log, err)
			return
		}
	}
	success(c, http.StatusOK, gin.H{
		"server": gin.H{"name": "OhMyCine Server", "api_version": "v1"},
		"user":   services.CurrentUserFromActor(actor), "device": playerDeviceDTO(device),
		"media_library_count": mediaLibraryCount,
		"capabilities":        playerCapabilities(actor), "emby_instances": embyInstances,
	})
}

func playerCapabilities(actor services.Actor) []string {
	capabilities := []string{"server_connection", "playback_history_sync", "media_favorites", "media_collections"}
	if actor.HasPermission(authz.PermissionMediaLibrariesRead) {
		capabilities = append(capabilities, "canonical_playback_history_v1", "persistent_category_artwork_v1", "media_overview_v1", "media_catalog", "direct_stream")
	}
	if actor.HasPermission(authz.PermissionDiscoveryRead) {
		capabilities = append(capabilities, "discovery_search")
	}
	if actor.HasPermission(authz.PermissionDownloadsCreate) {
		capabilities = append(capabilities, "acquisition_create", "download_control")
	}
	if actor.HasPermission(authz.PermissionFollowsCreate) {
		capabilities = append(capabilities, "subscription_create")
	}
	if actor.HasPermission(authz.PermissionMediaLibrariesUpdate) || actor.HasPermission(authz.PermissionMediaLibrariesDelete) || actor.HasPermission(authz.PermissionMediaLibrariesScan) {
		capabilities = append(capabilities, "media_library_manage")
	}
	sort.Strings(capabilities)
	return capabilities
}

func (a *API) PlayerOverview(c *gin.Context) {
	if a.playerOverview == nil {
		writeError(c, a.log, &services.AppError{Code: services.CodeInvalidRequest, Message: "Server 暂不支持媒体总览"})
		return
	}
	success(c, http.StatusOK, a.playerOverview.Overview(mustActor(c)))
}

func (a *API) PlayerFavorites(c *gin.Context) {
	if a.playerMediaState == nil {
		writeError(c, a.log, &services.AppError{Code: services.CodeInvalidRequest, Message: "Server 暂不支持收藏"})
		return
	}
	items, err := a.playerMediaState.Favorites(mustActor(c))
	if err != nil {
		writeError(c, a.log, err)
		return
	}
	success(c, http.StatusOK, gin.H{"list": items, "total": len(items)})
}

func (a *API) PlayerFavoriteState(c *gin.Context) {
	if a.playerMediaState == nil {
		writeError(c, a.log, &services.AppError{Code: services.CodeInvalidRequest, Message: "Server 暂不支持收藏"})
		return
	}
	favorite, err := a.playerMediaState.FavoriteState(mustActor(c), c.Param("itemId"))
	if err != nil {
		writeError(c, a.log, err)
		return
	}
	success(c, http.StatusOK, gin.H{"favorite": favorite})
}

func (a *API) SetPlayerFavorite(c *gin.Context) {
	if a.playerMediaState == nil {
		writeError(c, a.log, &services.AppError{Code: services.CodeInvalidRequest, Message: "Server 暂不支持收藏"})
		return
	}
	var input struct {
		Favorite bool `json:"favorite"`
	}
	if err := strictJSON(c, &input); err != nil {
		writeError(c, a.log, invalid("收藏参数无效", err))
		return
	}
	favorite, err := a.playerMediaState.SetFavorite(mustActor(c), c.Param("itemId"), input.Favorite)
	if err != nil {
		writeError(c, a.log, err)
		return
	}
	success(c, http.StatusOK, gin.H{"favorite": favorite})
}

func (a *API) PlayerCollections(c *gin.Context) {
	if a.playerMediaState == nil {
		writeError(c, a.log, &services.AppError{Code: services.CodeInvalidRequest, Message: "Server 暂不支持合集"})
		return
	}
	items, err := a.playerMediaState.Collections(mustActor(c), c.Query("kind"))
	if err != nil {
		writeError(c, a.log, err)
		return
	}
	success(c, http.StatusOK, gin.H{"list": items, "total": len(items)})
}

func (a *API) CreatePlayerCollection(c *gin.Context) {
	if a.playerMediaState == nil {
		writeError(c, a.log, &services.AppError{Code: services.CodeInvalidRequest, Message: "Server 暂不支持合集"})
		return
	}
	var input struct {
		Name string `json:"name"`
		Kind string `json:"kind"`
	}
	if err := strictJSON(c, &input); err != nil {
		writeError(c, a.log, invalid("合集参数无效", err))
		return
	}
	item, err := a.playerMediaState.CreateCollection(mustActor(c), input.Name, input.Kind)
	if err != nil {
		writeError(c, a.log, err)
		return
	}
	success(c, http.StatusCreated, gin.H{"id": item.ID, "name": item.Name, "kind": item.Kind, "source": item.Source, "item_count": 0})
}

func (a *API) PlayerCollectionItems(c *gin.Context) {
	if a.playerMediaState == nil {
		writeError(c, a.log, &services.AppError{Code: services.CodeInvalidRequest, Message: "Server 暂不支持合集"})
		return
	}
	items, err := a.playerMediaState.CollectionItems(mustActor(c), c.Param("id"))
	if err != nil {
		writeError(c, a.log, err)
		return
	}
	success(c, http.StatusOK, gin.H{"list": items, "total": len(items)})
}

func (a *API) AddPlayerCollectionItem(c *gin.Context) {
	if a.playerMediaState == nil {
		writeError(c, a.log, &services.AppError{Code: services.CodeInvalidRequest, Message: "Server 暂不支持合集"})
		return
	}
	var input struct {
		ItemID string `json:"item_id"`
	}
	if err := strictJSON(c, &input); err != nil {
		writeError(c, a.log, invalid("合集成员参数无效", err))
		return
	}
	if err := a.playerMediaState.AddCollectionItem(mustActor(c), c.Param("id"), input.ItemID); err != nil {
		writeError(c, a.log, err)
		return
	}
	success(c, http.StatusOK, gin.H{"added": true})
}

func (a *API) RemovePlayerCollectionItem(c *gin.Context) {
	if a.playerMediaState == nil {
		writeError(c, a.log, &services.AppError{Code: services.CodeInvalidRequest, Message: "Server 暂不支持合集"})
		return
	}
	if err := a.playerMediaState.RemoveCollectionItem(mustActor(c), c.Param("id"), c.Param("itemId")); err != nil {
		writeError(c, a.log, err)
		return
	}
	success(c, http.StatusOK, gin.H{"removed": true})
}

func (a *API) DeletePlayerCollection(c *gin.Context) {
	if a.playerMediaState == nil {
		writeError(c, a.log, &services.AppError{Code: services.CodeInvalidRequest, Message: "Server 暂不支持合集"})
		return
	}
	if err := a.playerMediaState.DeleteCollection(mustActor(c), c.Param("id")); err != nil {
		writeError(c, a.log, err)
		return
	}
	success(c, http.StatusOK, gin.H{"deleted": true})
}

func (a *API) PlayerHistorySync(c *gin.Context) {
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 2<<20)
	var payload struct {
		Cursor  uint64                         `json:"cursor"`
		Changes []services.PlayerHistoryChange `json:"changes"`
	}
	if err := strictJSON(c, &payload); err != nil {
		writeError(c, a.log, invalid("播放历史同步参数无效", err))
		return
	}
	if a.playerHistory == nil {
		writeError(c, a.log, &services.AppError{Code: services.CodeInvalidRequest, Message: "Server 暂不支持播放历史同步"})
		return
	}
	result, err := a.playerHistory.Sync(mustActor(c), payload.Cursor, payload.Changes)
	if err != nil {
		writeError(c, a.log, err)
		return
	}
	success(c, http.StatusOK, result)
}

func (a *API) PlayerHistory(c *gin.Context) {
	if a.playerHistory == nil {
		writeError(c, a.log, &services.AppError{Code: services.CodeInvalidRequest, Message: "Server 暂不支持播放历史"})
		return
	}
	page, pageSize, err := historyPageParameters(c)
	if err != nil {
		writeError(c, a.log, err)
		return
	}
	result, err := a.playerHistory.List(mustActor(c), page, pageSize, c.Query("source_kind"))
	if err != nil {
		writeError(c, a.log, err)
		return
	}
	success(c, http.StatusOK, result)
}

func (a *API) PlayerDevices(c *gin.Context) {
	c.Header("Cache-Control", "no-store")
	actor, _ := middleware.ActorFrom(c)
	devices, err := a.auth.ListDevices(actor)
	if err != nil {
		writeError(c, a.log, err)
		return
	}
	result := make([]playerDeviceResponse, 0, len(devices))
	for _, device := range devices {
		result = append(result, playerDeviceDTO(device))
	}
	success(c, http.StatusOK, gin.H{"list": result, "total": len(result)})
}

func (a *API) RevokePlayerDevice(c *gin.Context) {
	c.Header("Cache-Control", "no-store")
	actor, _ := middleware.ActorFrom(c)
	id := strings.TrimSpace(c.Param("id"))
	if id == "" || len(id) > 64 {
		writeError(c, a.log, invalid("设备 ID 无效", nil))
		return
	}
	if err := a.auth.RevokeDevice(actor, id, middleware.RequestContextFrom(c)); err != nil {
		writeError(c, a.log, err)
		return
	}
	success(c, http.StatusOK, gin.H{"revoked": true})
}

func (a *API) PlayerMediaLibraries(c *gin.Context) {
	libraries, err := a.libraries.PlayerLibraries(mustActor(c))
	if err != nil {
		writeError(c, a.log, err)
		return
	}
	success(c, http.StatusOK, gin.H{"list": libraries, "total": len(libraries)})
}

func (a *API) PlayerMediaLibraryCategories(c *gin.Context) {
	id, ok := a.playerPathID(c, "id")
	if !ok {
		return
	}
	items, err := a.libraries.PlayerCategories(mustActor(c), id)
	if err != nil {
		writeError(c, a.log, err)
		return
	}
	if a.libraryArtwork != nil {
		items = a.libraryArtwork.DecorateMediaCategories(c.Request.Context(), id, items)
	}
	success(c, http.StatusOK, gin.H{"list": items, "total": len(items)})
}

func (a *API) PlayerMediaLibraryCatalog(c *gin.Context) {
	id, ok := a.playerPathID(c, "id")
	if !ok {
		return
	}
	query, err := mediaPageQuery(c, false)
	if err != nil {
		writeError(c, a.log, err)
		return
	}
	page, err := a.libraries.PlayerCatalog(mustActor(c), id, query)
	if err != nil {
		writeError(c, a.log, err)
		return
	}
	success(c, http.StatusOK, page)
}

func (a *API) PlayerMediaLibraryDetail(c *gin.Context) {
	id, ok := a.playerPathID(c, "id")
	if !ok {
		return
	}
	detail, err := a.libraries.PlayerCatalogDetail(c.Request.Context(), mustActor(c), id, c.Param("work"))
	if err != nil {
		writeError(c, a.log, err)
		return
	}
	success(c, http.StatusOK, detail)
}

func (a *API) PlayerSearch(c *gin.Context) {
	query, err := mediaPageQuery(c, false)
	if err != nil {
		writeError(c, a.log, err)
		return
	}
	page, err := a.libraries.PlayerSearch(mustActor(c), query)
	if err != nil {
		writeError(c, a.log, err)
		return
	}
	success(c, http.StatusOK, page)
}

func (a *API) PlayerMediaEntryStream(c *gin.Context) {
	id, ok := a.playerPathID(c, "id")
	if !ok {
		return
	}
	stream, err := a.signedProxy.ResolvePlayerEntry(c.Request.Context(), mustActor(c), id, c.GetHeader("User-Agent"), c.Request.RemoteAddr)
	if err != nil {
		writeError(c, a.log, err)
		return
	}
	c.Header("Cache-Control", "no-store")
	if stream.File != nil {
		defer func() { _ = stream.File.Close() }()
		http.ServeContent(playerNoStoreWriter{ResponseWriter: c.Writer}, c.Request, stream.Name, stream.ModifiedAt, stream.File)
		return
	}
	if stream.RedirectURL == "" {
		writeError(c, a.log, services.PlayerStreamUnavailableError())
		return
	}
	c.Header("Location", stream.RedirectURL)
	c.Status(http.StatusFound)
}

func mustActor(c *gin.Context) services.Actor {
	actor, _ := middleware.ActorFrom(c)
	return actor
}

func (a *API) playerPathID(c *gin.Context, name string) (uint, bool) {
	raw := strings.TrimSpace(c.Param(name))
	value, err := strconv.ParseUint(raw, 10, 32)
	if err != nil || value == 0 {
		writeError(c, a.log, invalid("资源 ID 无效", err))
		return 0, false
	}
	return uint(value), true
}
