package handler

import (
	"context"
	"fmt"
	"html"
	"log/slog"
	"strings"

	"tg-drive-bot/internal/bot/msg"
	"tg-drive-bot/internal/bot/ui"
	"tg-drive-bot/internal/constants"
	"tg-drive-bot/internal/model"
	"tg-drive-bot/internal/service"
	"tg-drive-bot/internal/storage"
	"tg-drive-bot/internal/util"

	tele "gopkg.in/telebot.v4"
)

// SearchHandler handles the /search command and its callbacks.
type SearchHandler struct {
	fileService *service.FileService
	storage     storage.Storage
	cache       *util.SearchCache
	botUsername string
}

// NewSearchHandler creates a new SearchHandler.
func NewSearchHandler(fileService *service.FileService, store storage.Storage, cache *util.SearchCache, botUsername string) *SearchHandler {
	return &SearchHandler{
		fileService: fileService,
		storage:     store,
		cache:       cache,
		botUsername: botUsername,
	}
}

// OnSearch handles the /search command (full search chain: vector → FTS → ILIKE).
func (h *SearchHandler) OnSearch(c tele.Context) error {
	return h.handleSearchCommand(c, false)
}

// OnQuickSearch handles the /ss command (FTS + ILIKE only, skips vector layer).
// 适用于明确要做关键词精确匹配、不希望走语义模糊搜索的场景
func (h *SearchHandler) OnQuickSearch(c tele.Context) error {
	return h.handleSearchCommand(c, true)
}

// handleSearchCommand 是 /search 和 /ss 共享的入口逻辑,只是是否跳过向量层的开关不同
func (h *SearchHandler) handleSearchCommand(c tele.Context, skipVector bool) error {
	cat := Cat(c)
	user, ok := RequireUser(c)
	if !ok {
		return nil
	}

	query := strings.TrimSpace(c.Message().Payload)
	if query == "" {
		return c.Send(cat.SearchEmptyQuery)
	}

	return h.sendSearchPage(c, user.ID, query, constants.FileTypeAll.String(), 1, false, skipVector)
}

// OnSearchCallback handles search pagination and type filter callbacks.
func (h *SearchHandler) OnSearchCallback(c tele.Context) error {
	cat := Cat(c)
	user, ok := RequireUser(c)
	if !ok {
		return nil
	}

	data, err := ui.Decode(c.Callback().Data)
	if err != nil || data.CacheKey == "" {
		return nil
	}

	ctx := h.cache.Get(data.CacheKey)
	if ctx == nil {
		return c.RespondText(cat.SearchExpired)
	}
	if ctx.UserID != user.ID {
		return nil
	}

	fileType := data.FileType
	if fileType == "" {
		fileType = constants.FileTypeAll.String()
	}
	page := data.Page
	if page < 1 {
		page = 1
	}

	return h.sendSearchPage(c, user.ID, ctx.Query, fileType, page, true, ctx.SkipVector)
}

// sendSearchPage sends or edits search results.
// skipVector=true 时跳过向量层,只走 FTS+ILIKE 降级链
func (h *SearchHandler) sendSearchPage(c tele.Context, userID uint, query, fileType string, page int, isEdit, skipVector bool) error {
	cat := Cat(c)
	lang := Lang(c)
	var (
		files []model.File
		total int64
		err   error
	)
	if skipVector {
		files, total, err = h.fileService.SearchFilesNoVector(context.Background(), userID, query, fileType, page, util.DefaultPageSize)
	} else {
		files, total, err = h.fileService.SearchFiles(context.Background(), userID, query, fileType, page, util.DefaultPageSize)
	}
	if err != nil {
		slog.Error("search files failed", "error", err, "user_id", userID, "query", query, "file_type", fileType, "page", page, "skip_vector", skipVector)
		return c.Send(cat.SearchFailed)
	}

	pag := &util.Pagination{Page: page, PageSize: util.DefaultPageSize, Total: total}

	cacheKey := h.cache.Set(&util.SearchContext{
		Query:      query,
		UserID:     userID,
		SkipVector: skipVector,
	})

	botUsername := h.botUsername
	text := formatSearchText(cat, lang, files, total, query, page, botUsername)
	markup := buildSearchKeyboard(cat, files, cacheKey, fileType, pag)

	return ui.EditOrSend(c, text, markup, isEdit)
}

// formatSearchText composes the search results title and body.
// query 来自用户输入,而消息以 HTML 模式发出(ModeHTML),必须先转义
// 否则含 &/</> 的关键词会让 Telegram 直接返回 400,用户什么也收不到
func formatSearchText(cat *msg.Catalog, lang string, files []model.File, total int64, query string, page int, botUsername string) string {
	escaped := html.EscapeString(query)
	if total == 0 {
		return fmt.Sprintf(cat.SearchEmpty, escaped)
	}
	header := fmt.Sprintf(cat.SearchHeader, escaped, total)
	return ui.FormatFileList(lang, header, files, page, util.DefaultPageSize, botUsername)
}

// buildSearchKeyboard assembles the search inline keyboard: file number row(s) + type filter + pagination.
func buildSearchKeyboard(cat *msg.Catalog, files []model.File, cacheKey, fileType string, pag *util.Pagination) *tele.ReplyMarkup {
	markup := &tele.ReplyMarkup{}
	var rows []tele.Row

	rows = append(rows, ui.FileNumberButtons(markup, files, pag.Page, pag.PageSize)...)

	rows = append(rows, ui.FileTypeFilterRow(markup, cat, ui.CBSearch, func(ft constants.FileType) string {
		return ui.Encode(ui.CBData{CacheKey: cacheKey, FileType: ft.String(), Page: 1})
	}))

	rows = append(rows, ui.PaginationRow(markup, cat, ui.CBSearch, pag, func(p int) string {
		return ui.Encode(ui.CBData{CacheKey: cacheKey, FileType: fileType, Page: p})
	}))

	markup.Inline(rows...)
	return markup
}
