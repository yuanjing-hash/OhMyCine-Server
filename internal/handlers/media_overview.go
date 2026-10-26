package handlers

import (
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/middleware"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/services"
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
	options := services.BrowserHistoryOptions{GroupByWork: c.Query("group_by") == "work", WorkID: c.Query("work_id")}
	if value := c.Query("group_by"); value != "" && value != "work" {
		writeError(c, a.log, invalid("播放历史分组方式无效", nil))
		return
	}
	if value := c.Query("library_id"); value != "" {
		id, parseErr := strconv.ParseUint(value, 10, 32)
		if parseErr != nil || id == 0 {
			writeError(c, a.log, invalid("播放历史媒体库无效", nil))
			return
		}
		options.LibraryID = uint(id)
	}
	result, err := a.playerHistory.BrowserListWithOptions(actor, page, pageSize, options)
	if err != nil {
		writeError(c, a.log, err)
		return
	}
	success(c, http.StatusOK, result)
}

func (a *API) SetMediaLibraryHistoryWatched(c *gin.Context) {
	if a.playerHistory == nil {
		writeError(c, a.log, invalid("Server 暂不支持播放历史", nil))
		return
	}
	var input struct {
		Completed *bool `json:"completed"`
	}
	if err := c.ShouldBindJSON(&input); err != nil {
		writeError(c, a.log, invalid("观看状态格式无效", err))
		return
	}
	if input.Completed == nil {
		writeError(c, a.log, invalid("请选择观看完成状态", nil))
		return
	}
	actor, _ := middleware.ActorFrom(c)
	if err := a.playerHistory.SetBrowserHistoryWatched(c.Request.Context(), actor, c.Param("historyId"), *input.Completed); err != nil {
		writeError(c, a.log, err)
		return
	}
	success(c, http.StatusOK, gin.H{"completed": *input.Completed})
}

func (a *API) DeleteMediaLibraryHistory(c *gin.Context) {
	if a.playerHistory == nil {
		writeError(c, a.log, invalid("Server 暂不支持播放历史", nil))
		return
	}
	actor, _ := middleware.ActorFrom(c)
	if err := a.playerHistory.DeleteBrowserHistory(c.Request.Context(), actor, c.Param("historyId")); err != nil {
		writeError(c, a.log, err)
		return
	}
	success(c, http.StatusOK, gin.H{"deleted": true})
}

func (a *API) MediaLibraryFavorites(c *gin.Context) {
	if a.playerMediaState == nil {
		writeError(c, a.log, invalid("Server 暂不支持收藏", nil))
		return
	}
	actor, _ := middleware.ActorFrom(c)
	if c.Query("page") != "" || c.Query("page_size") != "" {
		page, size, err := historyPageParameters(c)
		if err != nil {
			writeError(c, a.log, err)
			return
		}
		result, err := a.playerMediaState.FavoritePage(actor, page, size)
		if err != nil {
			writeError(c, a.log, err)
			return
		}
		success(c, http.StatusOK, result)
		return
	}
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
	if c.Query("page") != "" || c.Query("page_size") != "" {
		page, size, err := historyPageParameters(c)
		if err != nil {
			writeError(c, a.log, err)
			return
		}
		result, err := a.playerMediaState.CollectionPage(actor, c.Query("kind"), c.Query("source"), page, size)
		if err != nil {
			writeError(c, a.log, err)
			return
		}
		success(c, http.StatusOK, result)
		return
	}
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
	if c.Query("page") != "" || c.Query("page_size") != "" {
		page, size, err := historyPageParameters(c)
		if err != nil {
			writeError(c, a.log, err)
			return
		}
		result, err := a.playerMediaState.CollectionItemPage(actor, c.Param("id"), page, size)
		if err != nil {
			writeError(c, a.log, err)
			return
		}
		success(c, http.StatusOK, result)
		return
	}
	items, err := a.playerMediaState.BrowserCollectionItems(actor, c.Param("id"))
	if err != nil {
		writeError(c, a.log, err)
		return
	}
	success(c, http.StatusOK, gin.H{"list": items, "total": len(items)})
}

func (a *API) RenameMediaCollection(c *gin.Context) {
	if a.playerMediaState == nil {
		writeError(c, a.log, invalid("Server 暂不支持合集", nil))
		return
	}
	var input struct {
		Name     string `json:"name"`
		Revision uint64 `json:"revision"`
	}
	if err := strictJSON(c, &input); err != nil {
		writeError(c, a.log, invalid("合集参数无效", err))
		return
	}
	if err := a.playerMediaState.RenameCollection(mustActor(c), c.Param("id"), input.Name, input.Revision); err != nil {
		writeError(c, a.log, err)
		return
	}
	success(c, http.StatusOK, gin.H{"updated": true})
}

func (a *API) MoveMediaCollectionItem(c *gin.Context) {
	if a.playerMediaState == nil {
		writeError(c, a.log, invalid("Server 暂不支持合集", nil))
		return
	}
	var input struct {
		ItemID   string `json:"item_id"`
		BeforeID string `json:"before_item_id"`
		Revision uint64 `json:"revision"`
	}
	if err := strictJSON(c, &input); err != nil {
		writeError(c, a.log, invalid("合集排序参数无效", err))
		return
	}
	if err := a.playerMediaState.MoveCollectionItem(mustActor(c), c.Param("id"), input.ItemID, input.BeforeID, input.Revision); err != nil {
		writeError(c, a.log, err)
		return
	}
	success(c, http.StatusOK, gin.H{"updated": true})
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
