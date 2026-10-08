package feishu

import (
	"encoding/json"

	larkevent "github.com/larksuite/oapi-sdk-go/v3/event"
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
	// direct is a one-to-one chat with the bot: every message there is
	// addressed to it, so no @mention is needed.
	direct bool
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
	if m.ChatType != nil && *m.ChatType == "p2p" {
		in.direct = true
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

// eventID returns the platform event id, which is stable across redeliveries
// of the same event. It is empty when the SDK did not populate the header.
func eventID(base *larkevent.EventV2Base) string {
	if base == nil || base.Header == nil {
		return ""
	}
	return base.Header.EventID
}

// messageEventKey is the dedup key for an inbound message event. The event id is
// the authoritative identity; the message id is the fallback so an event
// without a header still cannot be answered twice.
func messageEventKey(event *larkim.P2MessageReceiveV1, messageID string) string {
	if id := eventID(event.EventV2Base); id != "" {
		return "msg:" + id
	}
	if messageID != "" {
		return "msgid:" + messageID
	}
	return ""
}

// cardEventKey is the dedup key for a card callback. A callback carries no
// event id in the pinned SDK, so its identity is the message it belongs to plus
// the action: one refresh per card, regardless of how many times Feishu pushes
// the click.
func cardEventKey(event *callback.CardActionTriggerEvent) string {
	if id := eventID(event.EventV2Base); id != "" {
		return "card:" + id
	}
	msgID, action := "", ""
	if event.Event.Context != nil {
		msgID = event.Event.Context.OpenMessageID
	}
	if event.Event.Action != nil {
		if v, ok := event.Event.Action.Value["action"].(string); ok {
			action = v
		}
	}
	if msgID == "" {
		return ""
	}
	return "card:" + msgID + ":" + action
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
