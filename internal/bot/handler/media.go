package handler

import (
	"fmt"
	"log/slog"

	"tg-drive-bot/internal/bot/msg"
	"tg-drive-bot/internal/service"
	"tg-drive-bot/internal/storage"
	"tg-drive-bot/internal/util"

	tele "gopkg.in/telebot.v4"
)

// MediaHandler handles all media file reception.
type MediaHandler struct {
	fileService *service.FileService
	storage     storage.Storage
}

// NewMediaHandler creates a new MediaHandler.
func NewMediaHandler(fileService *service.FileService, store storage.Storage) *MediaHandler {
	return &MediaHandler{
		fileService: fileService,
		storage:     store,
	}
}

// OnMediaReceived is the unified handler for all media types.
func (h *MediaHandler) OnMediaReceived(c tele.Context) error {
	cat := Cat(c)
	lang := Lang(c)
	msgIn := c.Message()
	user, ok := RequireUser(c)
	if !ok {
		return nil
	}

	// Extract file info from message
	fileInfo := extractFileInfo(msgIn)
	if fileInfo == nil {
		return nil // Not a supported media type
	}

	// Set caption from message
	fileInfo.Caption = msgIn.Caption

	msgInfo := &service.MessageInfo{
		MessageID:    msgIn.ID,
		ChatID:       msgIn.Chat.ID,
		FromUserID:   msgIn.Sender.ID,
		FromUsername: msgIn.Sender.Username,
		Caption:      msgIn.Caption,
		MediaGroupID: msgIn.AlbumID,
		ForwardFrom:  extractForwardInfo(msgIn),
	}

	result, err := h.fileService.SaveFile(user.ID, fileInfo, msgInfo, c.Bot(), msgIn, h.storage)
	if err != nil {
		slog.Error("save file failed", "error", err, "user_id", user.ID, "file_unique_id", fileInfo.FileUniqueID)
		return c.Send(cat.FileSaveFailed)
	}

	if result.Status == "duplicate" {
		return c.Send(cat.FileAlreadyExists)
	}

	// Send confirmation
	return c.Send(formatSaveConfirmation(cat, lang, fileInfo))
}

// extractForwardInfo extracts forwarding source information from a message.
func extractForwardInfo(m *tele.Message) string {
	if m.OriginalSender != nil {
		return fmt.Sprintf("user:%d:%s", m.OriginalSender.ID, m.OriginalSender.Username)
	}
	if m.OriginalChat != nil {
		return fmt.Sprintf("chat:%d:%s", m.OriginalChat.ID, m.OriginalChat.Title)
	}
	return ""
}

// formatSaveConfirmation formats the confirmation message after saving a file.
func formatSaveConfirmation(cat *msg.Catalog, lang string, info *service.FileInfo) string {
	icon := util.FileTypeIcon(info.FileType)
	typeName := util.FileTypeName(lang, info.FileType)
	size := util.FormatFileSize(info.FileSize)

	if info.FileName != "" {
		return fmt.Sprintf(cat.SavedWithName, info.FileName, icon, typeName, size)
	}
	return fmt.Sprintf(cat.SavedTypeOnly, icon, typeName, size)
}
