package bot

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"sync"
	"time"

	"tg-drive-bot/internal/bot/handler"
	"tg-drive-bot/internal/bot/middleware"
	"tg-drive-bot/internal/bot/ui"
	"tg-drive-bot/internal/config"
	"tg-drive-bot/internal/service"
	"tg-drive-bot/internal/storage"
	"tg-drive-bot/internal/telegraph"
	"tg-drive-bot/internal/util"

	"golang.org/x/net/proxy"
	tele "gopkg.in/telebot.v4"
)

// newProxyClient creates an http.Client with the given proxy URL.
// Supports socks5:// and http:// (also https://) proxy schemes.
func newProxyClient(proxyURL string) (*http.Client, error) {
	parsed, err := url.Parse(proxyURL)
	if err != nil {
		return nil, fmt.Errorf("invalid proxy URL: %w", err)
	}

	switch parsed.Scheme {
	case "socks5":
		var auth *proxy.Auth
		if parsed.User != nil {
			password, _ := parsed.User.Password()
			auth = &proxy.Auth{
				User:     parsed.User.Username(),
				Password: password,
			}
		}
		dialer, err := proxy.SOCKS5("tcp", parsed.Host, auth, proxy.Direct)
		if err != nil {
			return nil, fmt.Errorf("failed to create SOCKS5 dialer: %w", err)
		}
		return &http.Client{
			Timeout: time.Minute,
			Transport: &http.Transport{
				Dial: dialer.Dial,
			},
		}, nil

	case "http", "https":
		return &http.Client{
			Timeout: time.Minute,
			Transport: &http.Transport{
				Proxy: http.ProxyURL(parsed),
			},
		}, nil

	default:
		return nil, fmt.Errorf("unsupported proxy scheme: %s (use socks5:// or http://)", parsed.Scheme)
	}
}

// asyncHandlerWG 跟踪被 async 放到 goroutine 里的 handler, 供 serve() 退出前 drain
// (与 handler 在更新循环里同步跑时的效果保持一致: 退出时会等它跑完)
var asyncHandlerWG sync.WaitGroup

// WaitAsyncHandlers 阻塞到所有异步 handler 跑完, 或 ctx 超时
// 返回 false 表示超时放弃等待(不强杀 goroutine)
func WaitAsyncHandlers(ctx context.Context) bool {
	done := make(chan struct{})
	go func() {
		asyncHandlerWG.Wait()
		close(done)
	}()
	select {
	case <-done:
		return true
	case <-ctx.Done():
		return false
	}
}

// async 把一个慢 handler 放到 goroutine 里跑并立即返回
//
// telebot 在单条更新循环里串行调用 handler, 一次 30s 的网络等待(/tph 抓文章、
// /search 调 embedding API)或一次全表 UPDATE(/cap_sync)会把所有人的更新卡住。
// 这层壳只移动执行位置: 中间件仍先同步跑完(白名单仍在更新循环里判定), 错误仍然
// 交给 bot 的 OnError, 与同步路径表现一致
func async(b *tele.Bot, h tele.HandlerFunc) tele.HandlerFunc {
	return func(c tele.Context) error {
		asyncHandlerWG.Add(1)
		go func() {
			defer asyncHandlerWG.Done()
			if err := h(c); err != nil {
				b.OnError(err, c)
			}
		}()
		return nil
	}
}

// New creates and configures a new telebot Bot instance with all handlers and middleware.
func New(cfg *config.Config, userService *service.UserService, fileService *service.FileService, embeddingSvc *service.EmbeddingService, maintenanceSvc *service.MaintenanceService, store storage.Storage) (*tele.Bot, error) {
	pref := tele.Settings{
		Token:  cfg.Bot.Token,
		Poller: &tele.LongPoller{Timeout: 40 * time.Second},
		OnError: func(err error, c tele.Context) {
			if c != nil && c.Sender() != nil {
				slog.Error("handler error", "error", err, "user", c.Sender().ID, "chat", c.Chat().ID)
			} else {
				slog.Error("bot error", "error", err)
			}
		},
	}

	// Configure proxy if specified
	var httpClient *http.Client
	if cfg.Bot.ProxyURL != "" {
		client, err := newProxyClient(cfg.Bot.ProxyURL)
		if err != nil {
			return nil, fmt.Errorf("failed to configure proxy: %w", err)
		}
		httpClient = client
		pref.Client = client
		slog.Info("using proxy", "url", cfg.Bot.ProxyURL)
	}

	b, err := tele.NewBot(pref)
	if err != nil {
		return nil, err
	}

	// Initialize infrastructure
	botUsername := b.Me.Username
	searchCache := util.NewSearchCache()
	tphClient := telegraph.NewClient(httpClient)

	// Initialize handlers
	mediaHandler := handler.NewMediaHandler(fileService, store)
	listHandler := handler.NewListHandler(fileService, store, botUsername)
	searchHandler := handler.NewSearchHandler(fileService, store, searchCache, botUsername)
	startHandler := handler.NewStartHandler(fileService, store)
	statsHandler := handler.NewStatsHandler(fileService)
	randomHandler := handler.NewRandomHandler(fileService, store)
	adminHandler := handler.NewAdminUserHandler(userService)
	telegraphHandler := handler.NewTelegraphHandler(tphClient, fileService, store)
	embAdminHandler := handler.NewEmbeddingAdminHandler(embeddingSvc)
	maintAdminHandler := handler.NewMaintenanceAdminHandler(maintenanceSvc)
	langHandler := handler.NewLangHandler(userService)

	// ========== Global Middleware ==========
	b.Use(middleware.Whitelist(userService))
	b.Use(middleware.I18n())

	// ========== User Commands ==========
	b.Handle("/start", startHandler.OnStart)
	b.Handle("/list", listHandler.OnList)
	// /search 会等 embedding API, /tph 会等 telegra.ph: 两者都可能卡 30s, 放 goroutine
	b.Handle("/search", async(b, searchHandler.OnSearch))
	b.Handle("/ss", async(b, searchHandler.OnQuickSearch))
	b.Handle("/stats", statsHandler.OnStats)
	b.Handle("/rand", randomHandler.OnRand)
	b.Handle("/randv", randomHandler.OnRandVideo)
	b.Handle("/randp", randomHandler.OnRandPhoto)
	b.Handle("/tph", async(b, telegraphHandler.OnTelegraph))
	b.Handle("/lang", langHandler.OnLang)

	// ========== Media Events ==========
	mediaEvents := []string{
		tele.OnDocument,
		tele.OnPhoto,
		tele.OnVideo,
		tele.OnAudio,
		tele.OnAnimation,
		tele.OnVoice,
		tele.OnVideoNote,
	}
	for _, event := range mediaEvents {
		b.Handle(event, mediaHandler.OnMediaReceived)
	}

	// ========== Callback Handlers ==========
	// File retrieval (shared by list and search)
	btnFile := tele.InlineButton{Unique: ui.CBFile.String()}
	b.Handle(&btnFile, listHandler.OnFileCallback)

	// List pagination and type filter
	btnList := tele.InlineButton{Unique: ui.CBList.String()}
	b.Handle(&btnList, listHandler.OnListCallback)

	// Search pagination and type filter
	btnSearch := tele.InlineButton{Unique: ui.CBSearch.String()}
	b.Handle(&btnSearch, searchHandler.OnSearchCallback)

	// No-op button (page indicator)
	btnNoop := tele.InlineButton{Unique: ui.CBNoop.String()}
	b.Handle(&btnNoop, func(c tele.Context) error {
		return c.Respond()
	})

	// File deletion flow: request → confirm/cancel
	btnFileDel := tele.InlineButton{Unique: ui.CBFileDel.String()}
	b.Handle(&btnFileDel, listHandler.OnFileDelete)

	btnFileDelConfirm := tele.InlineButton{Unique: ui.CBFileDelConfirm.String()}
	b.Handle(&btnFileDelConfirm, listHandler.OnFileDeleteConfirm)

	btnFileDelCancel := tele.InlineButton{Unique: ui.CBFileDelCancel.String()}
	b.Handle(&btnFileDelCancel, listHandler.OnFileDeleteCancel)

	// ========== Admin Commands (Group with AdminOnly middleware) ==========
	admins := b.Group()
	admins.Use(middleware.AdminOnly())
	admins.Handle("/adduser", adminHandler.OnAddUser)
	admins.Handle("/removeuser", adminHandler.OnRemoveUser)
	admins.Handle("/listuser", adminHandler.OnListUser)

	// Admin callback handlers
	// 不走 AdminOnly 中间件的话,普通白名单用户构造请求即可触发管理员操作(权限提升 / 信息泄露)
	btnUserList := tele.InlineButton{Unique: ui.CBUserList.String()}
	admins.Handle(&btnUserList, adminHandler.OnUserListCallback)

	btnUserInfo := tele.InlineButton{Unique: ui.CBUserInfo.String()}
	admins.Handle(&btnUserInfo, adminHandler.OnUserInfoCallback)

	btnUserDel := tele.InlineButton{Unique: ui.CBUserDel.String()}
	admins.Handle(&btnUserDel, adminHandler.OnUserDeleteCallback)

	btnUserPromote := tele.InlineButton{Unique: ui.CBUserPromote.String()}
	admins.Handle(&btnUserPromote, adminHandler.OnUserPromoteCallback)

	btnUserDemote := tele.InlineButton{Unique: ui.CBUserDemote.String()}
	admins.Handle(&btnUserDemote, adminHandler.OnUserDemoteCallback)

	// ========== Owner Commands (Group with OwnerOnly middleware) ==========
	// 这些命令通常是耗时 / 影响范围广的运维操作:批量 embedding、caption 同步等
	owners := b.Group()
	owners.Use(middleware.OwnerOnly())
	owners.Handle("/emb_re", embAdminHandler.OnEmbRe)
	owners.Handle("/emb_sync", embAdminHandler.OnEmbSync)
	// cap_sync 是一次跨表 UPDATE, 同步跑会卡住所有人的更新
	owners.Handle("/cap_sync", async(b, maintAdminHandler.OnCapSync))

	// 注册菜单
	if err := registerCommandMenus(b); err != nil {
		slog.Warn("set command menus failed", "error", err)
	}

	return b, nil
}

// 只注册普通用户菜单
func registerCommandMenus(b *tele.Bot) error {
	zh := []tele.Command{
		{Text: "start", Description: "开始 / 通过 deep link 获取文件"},
		{Text: "list", Description: "浏览我的文件库"},
		{Text: "search", Description: "搜索文件 (向量+FTS+模糊)"},
		{Text: "ss", Description: "精确搜索 (跳过向量层)"},
		{Text: "stats", Description: "查看统计信息"},
		{Text: "rand", Description: "随机返回媒体"},
		{Text: "randv", Description: "随机返回视频"},
		{Text: "randp", Description: "随机返回图片"},
		{Text: "tph", Description: "抓取 telegra.ph 文章"},
		{Text: "lang", Description: "查看或切换语言"},
	}
	en := []tele.Command{
		{Text: "start", Description: "Start / fetch a file via deep link"},
		{Text: "list", Description: "Browse my library"},
		{Text: "search", Description: "Search files (vector + FTS + fuzzy)"},
		{Text: "ss", Description: "Exact search (skip vector layer)"},
		{Text: "stats", Description: "Show statistics"},
		{Text: "rand", Description: "Random media"},
		{Text: "randv", Description: "Random videos"},
		{Text: "randp", Description: "Random photos"},
		{Text: "tph", Description: "Fetch a telegra.ph article"},
		{Text: "lang", Description: "View or switch language"},
	}

	if err := b.SetCommands(zh, "zh"); err != nil {
		return fmt.Errorf("set zh commands: %w", err)
	}
	if err := b.SetCommands(en, "en"); err != nil {
		return fmt.Errorf("set en commands: %w", err)
	}
	// 无法区分语言时的fallback
	if err := b.SetCommands(en); err != nil {
		return fmt.Errorf("set default commands: %w", err)
	}
	return nil
}
