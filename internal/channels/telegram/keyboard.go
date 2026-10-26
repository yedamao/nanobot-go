package telegram

import (
	"context"
	"fmt"
	"strconv"
	"time"
	"unicode/utf8"

	"github.com/yedamao/nanobot-go/internal/bus"
)

// maxCallbackData is Telegram's limit for an inline button's
// callback_data, in UTF-8 bytes.
const maxCallbackData = 64

// buildInlineKeyboard turns the button rows of an outbound message into
// an inline keyboard, like nanobot's _build_keyboard; nil when
// inlineKeyboards is off or there are no labels. Each button's
// callback_data is its label (see safeCallbackData), which comes back
// as the text of the tap.
func (c *Channel) buildInlineKeyboard(rows [][]string) *inlineKeyboardMarkup {
	if !c.cfg.InlineKeyboards {
		return nil
	}
	var kb [][]inlineKeyboardButton
	for _, row := range rows {
		if len(row) == 0 {
			continue
		}
		r := make([]inlineKeyboardButton, len(row))
		for i, label := range row {
			r[i] = inlineKeyboardButton{Text: label, CallbackData: safeCallbackData(label)}
		}
		kb = append(kb, r)
	}
	if kb == nil {
		return nil
	}
	return &inlineKeyboardMarkup{InlineKeyboard: kb}
}

// safeCallbackData truncates label to Telegram's 64-byte callback_data
// limit at a character boundary, like nanobot's _safe_callback_data.
func safeCallbackData(label string) string {
	if len(label) <= maxCallbackData {
		return label
	}
	cut := maxCallbackData
	for cut > 0 && !utf8.RuneStart(label[cut]) {
		cut--
	}
	return label[:cut]
}

// handleCallback turns a tap on an inline keyboard button into an
// inbound message whose text is the button's callback data, like
// nanobot's _on_callback_query: the tap is answered (so the client stops
// its spinner) and the keyboard removed from its message, so a choice is
// made once. Taps from senders who are not allowed are ignored.
func (c *Channel) handleCallback(ctx context.Context, q *callbackQuery) {
	if q.From == nil || q.From.IsBot {
		return
	}
	senderID := senderID(q.From)
	if q.Message == nil || q.Message.Chat.ID == 0 {
		c.log.Warn("telegram: callback query without a chat", "sender", senderID)
		return
	}
	m := q.Message
	if !c.isAllowed(q.From) {
		c.log.Warn("telegram: access denied for a button tap; add the sender to channels.telegram.allowFrom to grant access",
			"sender", senderID, "chat_id", m.Chat.ID)
		return
	}
	if err := c.api.call(ctx, "answerCallbackQuery", answerCallbackQueryParams{CallbackQueryID: q.ID}, nil); err != nil {
		c.log.Debug("telegram: answer callback query", "error", err)
	}
	// A message older than 48 hours comes as an InaccessibleMessage
	// (date 0) and cannot be edited.
	if m.Date != 0 {
		p := editMessageReplyMarkupParams{ChatID: m.Chat.ID, MessageID: m.MessageID}
		if err := c.api.call(ctx, "editMessageReplyMarkup", p, nil); err != nil {
			c.log.Debug("telegram: remove the inline keyboard", "chat_id", m.Chat.ID, "error", err)
		}
	}
	chatID := strconv.FormatInt(m.Chat.ID, 10)
	c.log.Debug("telegram: inline button tap", "sender", senderID, "chat_id", chatID, "label", q.Data)
	md := map[string]any{
		"callback_query_id": q.ID,
		"button_label":      q.Data,
		"user_id":           q.From.ID,
		"username":          q.From.Username,
		"first_name":        q.From.FirstName,
		"is_callback":       true,
	}
	in := bus.InboundMessage{
		Channel:   Name,
		SenderID:  senderID,
		ChatID:    chatID,
		Content:   q.Data,
		Timestamp: time.Now(),
		Metadata:  md,
	}
	// A tap in a forum topic stays in the topic's session.
	t := topicID(m)
	if t != 0 {
		md["message_thread_id"] = t
		in.SessionKeyOverride = fmt.Sprintf("%s:%s:topic:%d", Name, chatID, t)
	}
	c.startTyping(m.Chat.ID, t)
	if err := c.bus.PublishInbound(ctx, in); err != nil {
		c.stopTyping(m.Chat.ID)
		c.log.Warn("telegram: drop a button tap", "chat_id", chatID, "error", err)
	}
}

// buttonsFallback returns text with the buttons of msg written out
// (bus.WithButtonsText) when no inline keyboard carries them.
func buttonsFallback(text string, msg bus.OutboundMessage, kb *inlineKeyboardMarkup) string {
	if kb != nil || len(msg.Buttons) == 0 {
		return text
	}
	return bus.WithButtonsText(text, msg.Buttons)
}
