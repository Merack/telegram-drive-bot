package repository

import (
	"errors"

	"tg-drive-bot/internal/model"

	"gorm.io/gorm"
)

// 所有方法都对 r.db 进行操作，r.db 可以是顶层的 *gorm.DB 实例，
// 也可以是通过 UnitOfWork.WithTx 生成的、绑定了事务的句柄
type UserRepo struct {
	db *gorm.DB
}

// NewUserRepo creates a UserRepo bound to the given DB or transaction handle.
func NewUserRepo(db *gorm.DB) *UserRepo {
	return &UserRepo{db: db}
}

// Create inserts a new user record.
func (r *UserRepo) Create(user *model.User) error {
	return r.db.Create(user).Error
}

// GetByTelegramID finds a user by their Telegram user_id.
// Returns nil, nil if not found.
func (r *UserRepo) GetByTelegramID(telegramID int64) (*model.User, error) {
	var user model.User
	err := r.db.Where("telegram_id = ?", telegramID).First(&user).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, nil
		}
		return nil, err
	}
	return &user, nil
}

// GetByID finds a user by internal ID.
func (r *UserRepo) GetByID(id uint) (*model.User, error) {
	var user model.User
	err := r.db.First(&user, id).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, nil
		}
		return nil, err
	}
	return &user, nil
}

// Update: saves changes to an existing user record.
func (r *UserRepo) Update(user *model.User) error {
	return r.db.Save(user).Error
}

// ListAll returns all users with pagination.
func (r *UserRepo) ListAll(page, pageSize int) ([]model.User, int64, error) {
	var users []model.User
	var total int64

	if err := r.db.Model(&model.User{}).Count(&total).Error; err != nil {
		return nil, 0, err
	}

	err := r.db.Order("created_at ASC, id ASC").
		Offset((page - 1) * pageSize).
		Limit(pageSize).
		Find(&users).Error

	return users, total, err
}

// CountFilesByUserID returns the number of files for a given user.
func (r *UserRepo) CountFilesByUserID(userID uint) (int64, error) {
	var count int64
	err := r.db.Model(&model.File{}).Where("user_id = ?", userID).Count(&count).Error
	return count, err
}
