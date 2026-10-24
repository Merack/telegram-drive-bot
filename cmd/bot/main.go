package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"tg-drive-bot/database"
	"tg-drive-bot/internal/bot"
	"tg-drive-bot/internal/config"
	"tg-drive-bot/internal/logger"
	"tg-drive-bot/internal/model"
	"tg-drive-bot/internal/repository"
	"tg-drive-bot/internal/service"
	"tg-drive-bot/internal/storage"

	tele "gopkg.in/telebot.v4"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	gormlogger "gorm.io/gorm/logger"
)

func main() {
	os.Exit(run())
}

func run() int {
	// 初始化log
	if _, err := logger.Init(os.Stderr); err != nil {
		// logger not ready yet — fall back to stderr
		slog.New(slog.NewTextHandler(os.Stderr, nil)).Error("init logger failed", "error", err)
		return 1
	}

	// 加载环境变量
	cfg, err := config.Load()
	if err != nil {
		slog.Error("load config failed", "error", err)
		return 1
	}

	// 获取db
	db, err := openDB(cfg)
	if err != nil {
		slog.Error("connect database failed", "error", err)
		return 1
	}
	defer closeDB(db)

	// 表初始化
	if err := initializeSchema(db, cfg); err != nil {
		slog.Error("schema initialization failed", "error", err)
		return 1
	}

	// 数据仓库
	repos := repository.NewRepos(db)
	uow := repository.NewGormUnitOfWork(db)

	// 各种服务
	embeddingSvc := service.NewEmbeddingService(cfg.Embedding, repos.File)
	userService := service.NewUserService(repos.User)
	fileService := service.NewFileService(repos, uow, embeddingSvc, cfg.Storage.AsyncForward, cfg.Storage.AsyncDelete)
	maintenanceSvc := service.NewMaintenanceService(repos.Maintenance)

	// 确认系统有owner
	if err := userService.EnsureOwner(cfg.Security.OwnerID); err != nil {
		slog.Error("ensure owner failed", "error", err)
		return 1
	}
	slog.Info("owner ensured", "telegram_id", cfg.Security.OwnerID)

	// 初始化存储
	store := newStorage(cfg)

	// 初始化bot
	b, err := bot.New(cfg, userService, fileService, embeddingSvc, maintenanceSvc, store)
	if err != nil {
		slog.Error("create bot failed", "error", err)
		return 1
	}

	return serve(b, fileService, embeddingSvc, maintenanceSvc, cfg.Maintenance.CapSyncInterval)
}

// 使用config里的配置连接db
func openDB(cfg *config.Config) (*gorm.DB, error) {
	gormCfg := &gorm.Config{
		// 统一日志管道为slog
		Logger: gormlogger.NewSlogLogger(slog.Default(), gormlogger.Config{
			// 打印慢sql
			SlowThreshold:             200 * time.Millisecond,
			IgnoreRecordNotFoundError: true,
			LogLevel:                  gormlogger.Warn,
		}),
	}

	db, err := gorm.Open(postgres.Open(cfg.DB.URL), gormCfg)
	if err != nil {
		return nil, err
	}

	sqlDB, err := db.DB()
	if err != nil {
		return nil, err
	}
	sqlDB.SetMaxOpenConns(cfg.DB.MaxOpenConns)
	sqlDB.SetMaxIdleConns(cfg.DB.MaxIdleConns)
	sqlDB.SetConnMaxLifetime(cfg.DB.ConnMaxLifetime)
	sqlDB.SetConnMaxIdleTime(cfg.DB.ConnMaxIdleTime)

	slog.Info("database connected",
		"max_open_conns", cfg.DB.MaxOpenConns,
		"max_idle_conns", cfg.DB.MaxIdleConns,
		"conn_max_lifetime", cfg.DB.ConnMaxLifetime,
		"conn_max_idle_time", cfg.DB.ConnMaxIdleTime)
	return db, nil
}

func closeDB(db *gorm.DB) {
	sqlDB, err := db.DB()
	if err != nil {
		slog.Warn("unwrap sql.DB for close failed", "error", err)
		return
	}
	if err := sqlDB.Close(); err != nil {
		slog.Warn("close database failed", "error", err)
		return
	}
	slog.Info("database closed")
}

// 数据库表的初始化
//
// 执行顺序:
//  1. PreMigrate    -- 列改名/删除(AutoMigrate 不会做这两件事;且对 NOT NULL 列改名必须先行)
//  2. AutoMigrate   -- 创建/调整表结构以匹配 GORM 模型
//  3. InitializeSchema -- generated 列、GIN 索引、pgvector
func initializeSchema(db *gorm.DB, cfg *config.Config) error {
	if err := database.PreMigrate(db); err != nil {
		return fmt.Errorf("pre-migrate: %w", err)
	}
	if err := db.AutoMigrate(&model.User{}, &model.File{}, &model.FileCopy{}, &model.Message{}); err != nil {
		return err
	}
	slog.Info("auto-migration completed")

	embDim := 0
	if cfg.Embedding.Enabled {
		embDim = cfg.Embedding.Dimensions
	}
	if err := database.InitializeSchema(db, embDim); err != nil {
		return err
	}
	slog.Info("schema initialization completed")
	return nil
}

// 初始化存储
// 决定使用direct和是channel 模式的storage
func newStorage(cfg *config.Config) storage.Storage {
	if !cfg.Storage.UseChannel {
		slog.Info("storage mode: direct (using file_id)")
		return storage.NewDirect()
	}
	slog.Info("storage mode: channel",
		"targets", cfg.Storage.ChatIDs,
		"delete_copies", cfg.Storage.DeleteCopies,
		"failure_limit", cfg.Storage.FailureLimit,
		"cooldown", cfg.Storage.CooldownPeriod)

	if cfg.Storage.AsyncForward || cfg.Storage.AsyncDelete {
		slog.Info("async operations enabled",
			"async_forward", cfg.Storage.AsyncForward,
			"async_delete", cfg.Storage.AsyncDelete)
	}
	return storage.NewChannel(
		cfg.Storage.ChatIDs,
		cfg.Storage.DeleteCopies,
		cfg.Storage.FailureLimit,
		cfg.Storage.CooldownPeriod,
	)
}

// 用goroutine来启动bot, 防止b.Start()阻塞主进程, 手动处理各种信号
//
// 退出顺序(保证数据完整性):
//  1. SIGINT/SIGTERM -> ctx.Done -> 通知 embedding/maintenance worker 与 RunBatch 退出
//  2. b.Stop() 停掉 Telegram poller, 不再接收新更新
//  3. bgWg.Wait 等 worker goroutine 退出
//  4. fileService.Shutdown(30s) 等异步 forward/delete 跑完(避免孤儿数据)
//  5. defer closeDB 关连接池
func serve(b *tele.Bot, fileService *service.FileService, embeddingSvc *service.EmbeddingService, maintenanceSvc *service.MaintenanceService, capSyncInterval time.Duration) int {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	var bgWg sync.WaitGroup

	// Start embedding worker if vector search is enabled.
	if embeddingSvc != nil {
		bgWg.Add(1)
		go func() {
			defer bgWg.Done()
			embeddingSvc.Start(ctx)
		}()
	}

	// 启动定时 cap_sync 任务; interval <= 0 时 Start 自身会立即返回, 这里无需额外判断
	bgWg.Add(1)
	go func() {
		defer bgWg.Done()
		maintenanceSvc.Start(ctx, capSyncInterval)
	}()

	started := make(chan struct{})
	done := make(chan struct{})
	go func() {
		close(started)
		b.Start() // blocks until b.Stop() is called or the poller errors out fatally
		close(done)
	}()
	<-started
	slog.Info("bot started, polling for updates")

	exitCode := 0
	select {
	case <-ctx.Done():
		slog.Info("shutdown signal received, stopping bot")
		b.Stop()
		<-done
		slog.Info("bot stopped cleanly")
	case <-done:
		slog.Error("bot polling exited unexpectedly")
		exitCode = 1
		stop() // trigger ctx cancel so background workers also wind down
	}

	// 等后台 worker 退出(Start/RunBatch 都监听同一个 ctx)
	bgWg.Wait()

	// 等放到 goroutine 里跑的慢 handler(/search /ss /tph /cap_sync)与
	// 异步 forward/delete 跑完, 避免半截事务造成孤儿数据
	drainCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if !bot.WaitAsyncHandlers(drainCtx) {
		slog.Warn("shutdown timeout, some handlers may not have finished")
	}
	fileService.Shutdown(drainCtx)

	return exitCode
}
