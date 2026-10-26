package service

import (
	"fmt"

	"tg-drive-bot/internal/bot/msg"
	"tg-drive-bot/internal/constants"
	"tg-drive-bot/internal/model"
	"tg-drive-bot/internal/repository"
)

// UserService handles user management business logic.
type UserService struct {
	repo repository.UserRepository
}

// NewUserService creates a new UserService instance.
func NewUserService(repo repository.UserRepository) *UserService {
	return &UserService{repo: repo}
}

// EnsureOwner creates or updates the owner user record at startup.
func (s *UserService) EnsureOwner(ownerTelegramID int64) error {
	user, err := s.repo.GetByTelegramID(ownerTelegramID)
	if err != nil {
		return err
	}
	if user == nil {
		user = &model.User{
			TelegramID: ownerTelegramID,
			Role:       constants.RoleOwner.String(),
		}
		return s.repo.Create(user)
	}
	if user.Role != constants.RoleOwner.String() {
		user.Role = constants.RoleOwner.String()
		return s.repo.Update(user)
	}
	return nil
}

// GetByTelegramID finds a user by their Telegram user_id.
func (s *UserService) GetByTelegramID(telegramID int64) (*model.User, error) {
	return s.repo.GetByTelegramID(telegramID)
}

// AddUser adds a new user to the whitelist with the 'user' role.
// Returns ("already_exists", nil) if the user is already whitelisted.
func (s *UserService) AddUser(telegramID int64) (string, error) {
	existing, err := s.repo.GetByTelegramID(telegramID)
	if err != nil {
		return "", err
	}
	if existing != nil {
		// 之前被 /removeuser 禁用的用户:重新添加即恢复访问
		if existing.Blocked {
			existing.Blocked = false
			return "created", s.repo.Update(existing)
		}
		return "already_exists", nil
	}

	user := &model.User{
		TelegramID: telegramID,
		Role:       constants.RoleUser.String(),
	}
	return "created", s.repo.Create(user)
}

// RemoveUser removes a user from the whitelist.
// Returns a status code; the caller maps it to a user-facing message.
func (s *UserService) RemoveUser(telegramID int64, operatorRole constants.Role) (string, error) {
	target, err := s.repo.GetByTelegramID(telegramID)
	if err != nil {
		return "", err
	}
	if target == nil {
		return "not_found", nil
	}
	targetRole := constants.Role(target.Role)
	if targetRole.IsOwner() {
		return "cannot_remove_owner", nil
	}
	if targetRole == constants.RoleAdmin && !operatorRole.IsOwner() {
		return "only_owner_can_remove_admin", nil
	}
	// 不硬删用户行:files.user_id 有外键指向 users.id(无 ON DELETE),硬删会被数据库拒绝;
	// 即便删成功,该用户的 files/messages/file_copies 也会变成无归属的孤儿数据。
	// 改为标记 blocked,访问由白名单中间件拒绝;/adduser 会解除标记。
	target.Blocked = true
	return "removed", s.repo.Update(target)
}

// PromoteUser promotes a user to admin role.
// operatorRole 是发起操作的用户角色,用于深度防御:即使 handler 中间件被绕过,
// 这里仍会拒绝非 admin/owner 的操作者
func (s *UserService) PromoteUser(userID uint, operatorRole constants.Role) (string, error) {
	if !operatorRole.IsAdminOrOwner() {
		return "forbidden", nil
	}
	user, err := s.repo.GetByID(userID)
	if err != nil {
		return "", err
	}
	if user == nil {
		return "not_found", nil
	}
	role := constants.Role(user.Role)
	if role.IsOwner() {
		return "cannot_modify_owner", nil
	}
	if role == constants.RoleAdmin {
		return "already_admin", nil
	}
	user.Role = constants.RoleAdmin.String()
	return "promoted", s.repo.Update(user)
}

// DemoteUser demotes an admin to regular user.
func (s *UserService) DemoteUser(userID uint, operatorRole constants.Role) (string, error) {
	user, err := s.repo.GetByID(userID)
	if err != nil {
		return "", err
	}
	if user == nil {
		return "not_found", nil
	}
	role := constants.Role(user.Role)
	if role.IsOwner() {
		return "cannot_modify_owner", nil
	}
	if role == constants.RoleUser {
		return "already_user", nil
	}
	if !operatorRole.IsOwner() {
		return "only_owner_can_demote", nil
	}
	user.Role = constants.RoleUser.String()
	return "demoted", s.repo.Update(user)
}

// ListUsers returns paginated user list.
func (s *UserService) ListUsers(page, pageSize int) ([]model.User, int64, error) {
	return s.repo.ListAll(page, pageSize)
}

// CountFilesByUserID returns the file count for a user.
func (s *UserService) CountFilesByUserID(userID uint) (int64, error) {
	return s.repo.CountFilesByUserID(userID)
}

// UpdateUserInfo updates username and first_name from Telegram sender data.
func (s *UserService) UpdateUserInfo(user *model.User, username, firstName string) error {
	changed := false
	if user.Username != username {
		user.Username = username
		changed = true
	}
	if user.FirstName != firstName {
		user.FirstName = firstName
		changed = true
	}
	if !changed {
		return nil
	}
	return s.repo.Update(user)
}

// UpdateLanguage persists a user's preferred language tag (e.g. "zh", "en").
// Caller is responsible for validating the tag via msg.IsSupported.
func (s *UserService) UpdateLanguage(user *model.User, lang string) error {
	if user.Language == lang {
		return nil
	}
	user.Language = lang
	return s.repo.Update(user)
}

// GetByID finds a user by internal ID.
func (s *UserService) GetByID(id uint) (*model.User, error) {
	return s.repo.GetByID(id)
}

// FormatRole returns a localized display name for a role. "Owner" is left
// untranslated since it's used as a proper noun in both languages.
func FormatRole(lang, role string) string {
	cat := msg.For(lang)
	switch constants.Role(role) {
	case constants.RoleOwner:
		return "Owner"
	case constants.RoleAdmin:
		return cat.RoleAdminName
	default:
		return cat.RoleUserName
	}
}

// RoleIcon returns the icon for a user: 🚫 when the account is blocked
// (i.e. removed from the whitelist), otherwise the role icon.
func RoleIcon(user *model.User) string {
	if user.Blocked {
		return "🚫"
	}
	switch constants.Role(user.Role) {
	case constants.RoleOwner:
		return "👑"
	case constants.RoleAdmin:
		return "🔧"
	default:
		return "👤"
	}
}

// DisplayName returns a user's display name (first_name or username or ID).
func DisplayName(user *model.User) string {
	if user.FirstName != "" {
		return user.FirstName
	}
	if user.Username != "" {
		return user.Username
	}
	return fmt.Sprintf("%d", user.TelegramID)
}
