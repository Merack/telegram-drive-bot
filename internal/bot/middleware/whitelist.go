package middleware

import (
	"sync"
	"time"

	"tg-drive-bot/internal/model"
	"tg-drive-bot/internal/service"

	tele "gopkg.in/telebot.v4"
)

// whitelistCacheTTL 是单条用户记录在缓存中的存活时间
// 30s 是质量与延迟的平衡:
//   - 私人 bot 量小, 30s 内的 SELECT 节省并不显著, 但能消除并发突发请求带来的 N 倍查询
//   - 管理员提权/降级 / 移除白名单最多 30s 后才在 bot 行为上生效, 私人场景可接受
// 缓存只对"已存在白名单"做正向缓存, 不缓存"不存在"——保证 /adduser 后新用户立刻生效
const whitelistCacheTTL = 30 * time.Second

type cachedUser struct {
	user      *model.User
	expiresAt time.Time
}

// whitelistCache 是一个简单的 TTL map; 没用 sync.Map 是因为 expire 比较需要 RLock 不可避免
type whitelistCache struct {
	mu    sync.RWMutex
	store map[int64]cachedUser
}

func newWhitelistCache() *whitelistCache {
	return &whitelistCache{store: make(map[int64]cachedUser)}
}

func (c *whitelistCache) get(telegramID int64) *model.User {
	c.mu.RLock()
	entry, ok := c.store[telegramID]
	c.mu.RUnlock()
	if !ok || time.Now().After(entry.expiresAt) {
		return nil
	}
	// 返回副本: handler 可能在 goroutine 里跑(见 bot.async), 而中间件会在更新循环里
	// 往同一个 *User 上写 Username/FirstName —— 共享指针就是数据竞争
	u := *entry.user
	return &u
}

func (c *whitelistCache) put(telegramID int64, user *model.User) {
	c.mu.Lock()
	c.store[telegramID] = cachedUser{
		user:      user,
		expiresAt: time.Now().Add(whitelistCacheTTL),
	}
	c.mu.Unlock()
}

// Whitelist returns a middleware that checks if the sender is in the user whitelist.
// Unauthorized users are silently ignored (no response).
//
// 为减少高频消息对 DB 的压力,白名单结果会被缓存 whitelistCacheTTL
// 仅缓存"是白名单"的正向命中;"不在白名单"不缓存,确保 /adduser 后新用户立即可用
// 缓存的副作用:管理员对已缓存用户的角色调整 / 移除最多延迟 whitelistCacheTTL 生效
func Whitelist(userService *service.UserService) tele.MiddlewareFunc {
	cache := newWhitelistCache()
	return func(next tele.HandlerFunc) tele.HandlerFunc {
		return func(c tele.Context) error {
			sender := c.Sender()
			if sender == nil {
				return nil // No sender, ignore
			}

			user := cache.get(sender.ID)
			if user == nil {
				var err error
				user, err = userService.GetByTelegramID(sender.ID)
				if err != nil || user == nil {
					return nil // Not whitelisted, silently ignore
				}
				cache.put(sender.ID, user)
			}

			// 已被 /removeuser 禁用的用户视同不在白名单,静默忽略
			// 缓存命中时最长 whitelistCacheTTL 后才生效,与角色变更的延迟一致
			if user.Blocked {
				return nil
			}

			// Update user info from Telegram if changed
			_ = userService.UpdateUserInfo(user, sender.Username, sender.FirstName)

			// Store user in context for downstream handlers
			c.Set("db_user", user)
			return next(c)
		}
	}
}
