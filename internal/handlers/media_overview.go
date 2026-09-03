package handlers

import (
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/middleware"
)

func (a *API) MediaLibraryOverview(c *gin.Context) {
	if a.playerOverview == nil {
		writeError(c, a.log, invalid("Server 暂不支持媒体总览", nil))
		return
	}
	actor, _ := middleware.ActorFrom(c)
	success(c, http.StatusOK, a.playerOverview.BrowserOverview(actor))
}

func (a *API) MediaLibraryHistory(c *gin.Context) {
	if a.playerHistory == nil {
		writeError(c, a.log, invalid("Server 暂不支持播放历史", nil))
		return
	}
	page, pageSize, err := historyPageParameters(c)
	if err != nil {
		writeError(c, a.log, err)
		return
	}
	actor, _ := middleware.ActorFrom(c)
	result, err := a.playerHistory.BrowserList(actor, page, pageSize)
	if err != nil {
		writeError(c, a.log, err)
		return
	}
	success(c, http.StatusOK, result)
}

func (a *API) MediaLibraryFavorites(c *gin.Context) {
	if a.playerMediaState == nil {
		writeError(c, a.log, invalid("Server 暂不支持收藏", nil))
		return
	}
	actor, _ := middleware.ActorFrom(c)
	items, err := a.playerMediaState.BrowserFavorites(actor)
	if err != nil {
		writeError(c, a.log, err)
		return
	}
	success(c, http.StatusOK, gin.H{"list": items, "total": len(items)})
}

func (a *API) MediaLibraryCollections(c *gin.Context) {
	if a.playerMediaState == nil {
		writeError(c, a.log, invalid("Server 暂不支持合集", nil))
		return
	}
	actor, _ := middleware.ActorFrom(c)
	items, err := a.playerMediaState.BrowserCollections(actor, c.Query("kind"))
	if err != nil {
		writeError(c, a.log, err)
		return
	}
	success(c, http.StatusOK, gin.H{"list": items, "total": len(items)})
}

func (a *API) MediaLibraryCollectionItems(c *gin.Context) {
	if a.playerMediaState == nil {
		writeError(c, a.log, invalid("Server 暂不支持合集", nil))
		return
	}
	actor, _ := middleware.ActorFrom(c)
	items, err := a.playerMediaState.BrowserCollectionItems(actor, c.Param("id"))
	if err != nil {
		writeError(c, a.log, err)
		return
	}
	success(c, http.StatusOK, gin.H{"list": items, "total": len(items)})
}

func historyPageParameters(c *gin.Context) (int, int, error) {
	page, err := strconv.Atoi(c.DefaultQuery("page", "1"))
	if err != nil {
		return 0, 0, invalid("播放历史页码无效", err)
	}
	pageSize, err := strconv.Atoi(c.DefaultQuery("page_size", "24"))
	if err != nil {
		return 0, 0, invalid("播放历史分页大小无效", err)
	}
	return page, pageSize, nil
}
