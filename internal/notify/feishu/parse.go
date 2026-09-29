package feishu

import (
	"encoding/json"

	"github.com/larksuite/oapi-sdk-go/v3/event/dispatcher/callback"
	larkim "github.com/larksuite/oapi-sdk-go/v3/service/im/v1"
)

// inbound is the flattened, nil-safe view of a message event.
type inbound struct {
	chatID       string
	messageID    string
	openID       string
	text         string
	botMentioned bool
}

// parseMessage extracts the fields we act on. Every SDK field is a pointer, so
// each is nil-checked before dereferencing.
func parseMessage(event *larkim.P2MessageReceiveV1) inbound {
	var in inbound
	m := event.Event.Message
	if m.ChatId != nil {
		in.chatID = *m.ChatId
	}
	if m.MessageId != nil {
		in.messageID = *m.MessageId
	}
	if sender := event.Event.Sender; sender != nil && sender.SenderId != nil && sender.SenderId.OpenId != nil {
		in.openID = *sender.SenderId.OpenId
	}
	for _, mention := range m.Mentions {
		if mention != nil && mention.MentionedType != nil && *mention.MentionedType == "bot" {
			in.botMentioned = true
			break
		}
	}
	if m.MessageType != nil && *m.MessageType == larkim.MsgTypeText {
		if m.Content != nil {
			in.text = extractText(*m.Content)
		}
	}
	return in
}

// extractText decodes the "text" field from a text message's JSON content.
func extractText(content string) string {
	var payload struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal([]byte(content), &payload); err != nil {
		return ""
	}
	return payload.Text
}

// actionName reads the action name from the card callback. Action.Value is
// untrusted input used only to choose a branch, never as a credential.
func actionName(event *callback.CardActionTriggerEvent) string {
	if event.Event.Action == nil {
		return ""
	}
	if v, ok := event.Event.Action.Value["action"]; ok {
		if s, ok := v.(string); ok {
			return s
		}
	}
	return ""
}
