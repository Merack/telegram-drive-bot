package repository

import (
	"encoding/json"
	"errors"
	"time"

	"tg-drive-bot/internal/model"

	"gorm.io/gorm"
)

// FileRepo is the GORM-backed implementation of FileRepository.
type FileRepo struct {
	db *gorm.DB
}

// NewFileRepo creates a FileRepo bound to the given DB or transaction handle.
func NewFileRepo(db *gorm.DB) *FileRepo {
	return &FileRepo{db: db}
}

// Create inserts a new file record.
func (r *FileRepo) Create(file *model.File) error {
	return r.db.Create(file).Error
}

// GetByID finds a file by internal ID. Returns nil, nil if not found.
func (r *FileRepo) GetByID(id uint) (*model.File, error) {
	var file model.File
	err := r.db.First(&file, id).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, nil
		}
		return nil, err
	}
	return &file, nil
}

// GetByIDAndUser 按主键查找文件,但要求文件归属 userID
// 用于来自 callback_data 的 fileID 取文件 -- callback_data 可被客户端伪造,
// 不能信任,必须在 SQL 层用 user_id 限定避免越权读取他人文件
// 不存在或不属于 userID 时统一返回 (nil, nil)
func (r *FileRepo) GetByIDAndUser(id, userID uint) (*model.File, error) {
	var file model.File
	err := r.db.Where("id = ? AND user_id = ?", id, userID).First(&file).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, nil
		}
		return nil, err
	}
	return &file, nil
}

// GetByUniqueIDAndUser Telegram 的 file_unique_id 和 user id查找文件
func (r *FileRepo) GetByUniqueIDAndUser(fileUniqueID string, userID uint) (*model.File, error) {
	var file model.File
	err := r.db.
		Where("file_unique_id = ? AND user_id = ?", fileUniqueID, userID).
		First(&file).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, nil
		}
		return nil, err
	}
	return &file, nil
}

// DeleteByIDAndUser deletes a file row scoped by (id, user_id) and returns the rows affected.
// Callers use rowsAffected == 0 to detect "not found or not owned" and respond without
// leaking which of the two it was.
func (r *FileRepo) DeleteByIDAndUser(id, userID uint) (int64, error) {
	res := r.db.Where("id = ? AND user_id = ?", id, userID).Delete(&model.File{})
	return res.RowsAffected, res.Error
}

// ExistsByUserAndFileUniqueID checks if a file with the given unique_id already exists for the user.
func (r *FileRepo) ExistsByUserAndFileUniqueID(userID uint, fileUniqueID string) (bool, error) {
	var count int64
	err := r.db.Model(&model.File{}).
		Where("user_id = ? AND file_unique_id = ?", userID, fileUniqueID).
		Count(&count).Error
	return count > 0, err
}

// ListByUser returns files for a user with optional type filter, paginated.
func (r *FileRepo) ListByUser(userID uint, fileType string, page, pageSize int) ([]model.File, int64, error) {
	db := r.db.Where("user_id = ?", userID)

	if fileType != "" && fileType != "all" {
		db = db.Where("file_type = ?", fileType)
	}

	var total int64
	if err := db.Model(&model.File{}).Count(&total).Error; err != nil {
		return nil, 0, err
	}

	var files []model.File
	err := db.Order("created_at DESC, id DESC").
		Offset((page - 1) * pageSize).
		Limit(pageSize).
		Find(&files).Error

	return files, total, err
}

// 返回最多不超过 `limit` 个由 userID 拥有且 file_type 在 fileTypes 中的随机文件
// 如果符合条件的文件池大小小于 limit，则返回更少的数据行（或空切片）
//
// 直接对该用户的行做 ORDER BY RANDOM(): 查询已按 user_id 过滤, 排序只发生在该用户的行上,
// 一条往返就能拿到真正分散的随机结果。旧实现在大文件池上改成主键采样 + 逐个候选取最近行,
// 一次 /rand 要跑几十次数据库往返, 远程库上延迟反而更高
// shortcut: 排序成本随该用户的文件数线性增长, 单用户超过约 10 万条时再改回采样
func (r *FileRepo) RandomByUserAndTypes(userID uint, fileTypes []string, limit int) ([]model.File, error) {
	if limit <= 0 || len(fileTypes) == 0 {
		return nil, nil
	}

	var files []model.File
	err := r.db.
		Where("user_id = ? AND file_type IN ?", userID, fileTypes).
		Order("RANDOM()").
		Limit(limit).
		Find(&files).Error

	return files, err
}

// Search performs full-text search on file_name and caption using PostgreSQL tsvector.
func (r *FileRepo) Search(userID uint, query string, fileType string, page, pageSize int) ([]model.File, int64, error) {
	db := r.db.Where("user_id = ?", userID).
		Where("search_vector @@ plainto_tsquery('simple', ?)", query)

	if fileType != "" && fileType != "all" {
		db = db.Where("file_type = ?", fileType)
	}

	var total int64
	if err := db.Model(&model.File{}).Count(&total).Error; err != nil {
		return nil, 0, err
	}

	var files []model.File
	err := db.
		Order(gorm.Expr("ts_rank(search_vector, plainto_tsquery('simple', ?)) DESC", query)).
		Order("id DESC").
		Offset((page - 1) * pageSize).
		Limit(pageSize).
		Find(&files).Error

	return files, total, err
}

// SearchFallback performs ILIKE-based fuzzy search as a fallback when FTS is unavailable.
func (r *FileRepo) SearchFallback(userID uint, query string, fileType string, page, pageSize int) ([]model.File, int64, error) {
	pattern := "%" + query + "%"
	db := r.db.Where("user_id = ?", userID).
		Where("(file_name ILIKE ? OR caption ILIKE ?)", pattern, pattern)

	if fileType != "" && fileType != "all" {
		db = db.Where("file_type = ?", fileType)
	}

	var total int64
	if err := db.Model(&model.File{}).Count(&total).Error; err != nil {
		return nil, 0, err
	}

	var files []model.File
	err := db.Order("created_at DESC, id DESC").
		Offset((page - 1) * pageSize).
		Limit(pageSize).
		Find(&files).Error

	return files, total, err
}

// FileTypeStats holds the count and total size for a file type.
type FileTypeStats struct {
	FileType  string
	Count     int64
	TotalSize int64
}

// GetStatsByUser returns per-type file statistics for a user.
func (r *FileRepo) GetStatsByUser(userID uint) ([]FileTypeStats, error) {
	var stats []FileTypeStats
	err := r.db.Model(&model.File{}).
		Select("file_type, COUNT(*) as count, COALESCE(SUM(file_size), 0) as total_size").
		Where("user_id = ?", userID).
		Group("file_type").
		Find(&stats).Error
	return stats, err
}

// TotalStats holds aggregate stats (total count, total size, earliest, latest).
// Earliest/Latest 为 nil 表示该用户没有任何文件
type TotalStats struct {
	TotalCount int64
	TotalSize  int64
	Earliest   *time.Time
	Latest     *time.Time
}

// GetTotalStatsByUser returns aggregate stats for a user.
func (r *FileRepo) GetTotalStatsByUser(userID uint) (*TotalStats, error) {
	var stats TotalStats
	err := r.db.Model(&model.File{}).
		Select("COUNT(*) as total_count, COALESCE(SUM(file_size), 0) as total_size, MIN(created_at) as earliest, MAX(created_at) as latest").
		Where("user_id = ?", userID).
		Scan(&stats).Error
	return &stats, err
}

// VectorSearch 使用 pgvector 的余弦距离运算符 (<=>) 执行语义搜索
// 仅考虑 embedding 非空 (non-NULL) 的文件。结果按余弦距离升序排序
// (即最相似的排在最前面)。如果 threshold > 0,则排除距离大于该阈值的结果
func (r *FileRepo) VectorSearch(userID uint, queryVec []float64, fileType string, threshold float64, page, pageSize int) ([]model.File, int64, error) {
	vecJSON, err := json.Marshal(queryVec)
	if err != nil {
		return nil, 0, err
	}
	vecStr := string(vecJSON)

	db := r.db.Where("user_id = ? AND embedding IS NOT NULL", userID)

	if fileType != "" && fileType != "all" {
		db = db.Where("file_type = ?", fileType)
	}

	if threshold > 0 {
		db = db.Where("embedding <=> ?::extensions.halfvec <= ?", vecStr, threshold)
	}

	var total int64
	if err := db.Model(&model.File{}).Count(&total).Error; err != nil {
		return nil, 0, err
	}

	var files []model.File
	err = db.
		Order(gorm.Expr("embedding <=> ?::extensions.halfvec", vecStr)).
		Order("id DESC").
		Offset((page - 1) * pageSize).
		Limit(pageSize).
		Find(&files).Error

	return files, total, err
}

// ListIDsMissingEmbedding 返回 embedding 列为 NULL 的全部 file id
// 用于 owner 命令 /emb_sync 的"补缺失"路径
func (r *FileRepo) ListIDsMissingEmbedding() ([]uint, error) {
	var ids []uint
	err := r.db.Model(&model.File{}).
		Where("embedding IS NULL").
		Order("id").
		Pluck("id", &ids).Error
	return ids, err
}

// ListAllFileIDs 返回全部 file id(不分用户)
// 用于 owner 命令 /emb_re 的"全量重做"路径
func (r *FileRepo) ListAllFileIDs() ([]uint, error) {
	var ids []uint
	err := r.db.Model(&model.File{}).
		Order("id").
		Pluck("id", &ids).Error
	return ids, err
}

// EmbeddingSource 是生成 embedding 所需的文本字段子集
// 不包含数据库主键以外的字段, 调用方按 fileID 配对结果
type EmbeddingSource struct {
	FileName string
	Title    string
	Caption  string
}

// GetEmbeddingSource 读取生成 embedding 所需的文本字段
// 文件不存在时 GORM 的 Scan 不报错, 返回零值结构体 -- 调用方按"文本为空"跳过即可
func (r *FileRepo) GetEmbeddingSource(fileID uint) (*EmbeddingSource, error) {
	var src EmbeddingSource
	err := r.db.Table("files").
		Select("file_name, title, caption").
		Where("id = ?", fileID).
		Scan(&src).Error
	if err != nil {
		return nil, err
	}
	return &src, nil
}

// SaveEmbedding 将一个 fileID 的 embedding 向量写回 files 表
// vecJSON 必须是 json.Marshal 得到的 float64 数组字面量(例如 "[0.1,0.2,...]"),
// 由 service 层产出 -- 这里只负责强类型转换为 pgvector 的 halfvec
func (r *FileRepo) SaveEmbedding(fileID uint, vecJSON string) error {
	return r.db.Exec(
		"UPDATE files SET embedding = ?::extensions.halfvec WHERE id = ?",
		vecJSON, fileID,
	).Error
}
