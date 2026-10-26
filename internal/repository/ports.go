package repository

import "tg-drive-bot/internal/model"

type UserRepository interface {
	Create(user *model.User) error
	GetByTelegramID(telegramID int64) (*model.User, error)
	GetByID(id uint) (*model.User, error)
	Update(user *model.User) error
	ListAll(page, pageSize int) ([]model.User, int64, error)
	CountFilesByUserID(userID uint) (int64, error)
}

// FileRepository covers CRUD + listing + search + stats for model.File.
type FileRepository interface {
	Create(file *model.File) error
	GetByID(id uint) (*model.File, error)
	GetByIDAndUser(id, userID uint) (*model.File, error)
	GetByUniqueIDAndUser(fileUniqueID string, userID uint) (*model.File, error)
	DeleteByIDAndUser(id, userID uint) (rowsAffected int64, err error)
	ExistsByUserAndFileUniqueID(userID uint, fileUniqueID string) (bool, error)
	ListByUser(userID uint, fileType string, page, pageSize int) ([]model.File, int64, error)
	RandomByUserAndTypes(userID uint, fileTypes []string, limit int) ([]model.File, error)
	Search(userID uint, query, fileType string, page, pageSize int) ([]model.File, int64, error)
	SearchFallback(userID uint, query, fileType string, page, pageSize int) ([]model.File, int64, error)
	VectorSearch(userID uint, queryVec []float64, fileType string, threshold float64, page, pageSize int) ([]model.File, int64, error)
	GetStatsByUser(userID uint) ([]FileTypeStats, error)
	GetTotalStatsByUser(userID uint) (*TotalStats, error)
	// 全库扫:用于 owner 命令 /emb_re 和 /emb_sync 的批量 embedding 任务
	ListIDsMissingEmbedding() ([]uint, error)
	ListAllFileIDs() ([]uint, error)
	// embedding 读写:供 EmbeddingService 使用,使 service 不再持有 *gorm.DB
	GetEmbeddingSource(fileID uint) (*EmbeddingSource, error)
	SaveEmbedding(fileID uint, vecJSON string) error
}

// FileCopyRepository covers the file_copies multi-copy table.
type FileCopyRepository interface {
	CreateBatch(copies []model.FileCopy) error
	ListByFile(fileID uint) ([]model.FileCopy, error)
	DeleteByFileID(fileID uint) error
}

// MessageRepository covers the messages observational-metadata table.
type MessageRepository interface {
	Create(msg *model.Message) error
	DeleteByFileID(fileID uint) error
}

// MaintenanceRepository 承载 owner 级别的跨表运维操作
// 这些操作往往涉及多张表的批量 SQL,不适合归到任何单表 repo
type MaintenanceRepository interface {
	CaptionSync() (CapSyncResult, error)
}

type Repos struct {
	User        UserRepository
	File        FileRepository
	FileCopy    FileCopyRepository
	Message     MessageRepository
	Maintenance MaintenanceRepository
}

// UnitOfWork 在数据库事务内运行一个函数，并向其传入一个绑定了事务的 *Repos，
// 该 Repos 下的所有仓储方法调用都将在同一个事务中执行
//
// 语义：
//   - 传入的闭包函数如果返回 nil 则提交事务，返回非 nil 则回滚
//   - 方法返回的 error 为该闭包返回的错误（或者是事务框架底层的错误）
type UnitOfWork interface {
	WithTx(fn func(repos *Repos) error) error
}
