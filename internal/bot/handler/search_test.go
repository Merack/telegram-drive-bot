package handler

import (
	"html"
	"strings"
	"testing"

	"tg-drive-bot/internal/bot/msg"
)

// 搜索关键词会进入以 ModeHTML 发出的消息,未转义时 Telegram 直接返回 400
func TestFormatSearchTextEscapesQuery(t *testing.T) {
	cat := msg.For(msg.LangEn)
	queries := []string{"R&D", "<b>x", "a > b"}

	for _, q := range queries {
		// total == 0 与 total > 0 是两个不同分支,都要覆盖
		for _, total := range []int64{0, 3} {
			got := formatSearchText(cat, msg.LangEn, nil, total, q, 1, "mybot")
			if strings.Contains(got, q) {
				t.Errorf("total=%d query=%q: raw query leaked into HTML message: %s", total, q, got)
			}
			if !strings.Contains(got, html.EscapeString(q)) {
				t.Errorf("total=%d query=%q: escaped query missing: %s", total, q, got)
			}
		}
	}
}
