package feishu

import (
	"context"
	"encoding/json"
	"fmt"

	lark "github.com/larksuite/oapi-sdk-go/v3"
	larkcore "github.com/larksuite/oapi-sdk-go/v3/core"
	larkim "github.com/larksuite/oapi-sdk-go/v3/service/im/v1"
)

// receiveIDTypeChatID is the receive_id_type for a group chat. The pinned SDK
// version exposes no constant for it (only MsgType* constants), so it lives here.
const receiveIDTypeChatID = "chat_id"

// sender is the narrow seam over the Feishu send API so message handling can
// be unit-tested without a network or an app credential.
type sender interface {
	// SendCard posts an interactive card to a chat.
	SendCard(ctx context.Context, chatID string, card map[string]any) error
	// ReplyText replies to a specific message with plain text.
	ReplyText(ctx context.Context, messageID, text string) error
	// ReplyCard replies to a specific message with an interactive card.
	ReplyCard(ctx context.Context, messageID string, card map[string]any) error
	// PatchCard updates an existing interactive card message.
	PatchCard(ctx context.Context, messageID string, card map[string]any) error
}

// larkSender is the production sender backed by the official SDK.
type larkSender struct {
	client *lark.Client
}

func newLarkSender(appID, appSecret string) *larkSender {
	return &larkSender{
		client: lark.NewClient(appID, appSecret, lark.WithLogLevel(larkcore.LogLevelInfo)),
	}
}

// SendCard creates an interactive message in chatID.
func (s *larkSender) SendCard(ctx context.Context, chatID string, card map[string]any) error {
	content, err := json.Marshal(card)
	if err != nil {
		return fmt.Errorf("marshal card: %w", err)
	}
	body := larkim.NewCreateMessageReqBodyBuilder().
		ReceiveId(chatID).
		MsgType(larkim.MsgTypeInteractive).
		Content(string(content)).
		Build()
	req := larkim.NewCreateMessageReqBuilder().
		ReceiveIdType(receiveIDTypeChatID).
		Body(body).
		Build()

	resp, err := s.client.Im.Message.Create(ctx, req)
	if err != nil {
		return fmt.Errorf("im.message.create: %w", err)
	}
	if !resp.Success() {
		return fmt.Errorf("im.message.create: code=%d msg=%s log_id=%s", resp.Code, resp.Msg, resp.RequestId())
	}
	return nil
}

// ReplyText answers messageID with a text message.
func (s *larkSender) ReplyText(ctx context.Context, messageID, text string) error {
	content, err := json.Marshal(map[string]string{"text": text})
	if err != nil {
		return fmt.Errorf("marshal text: %w", err)
	}
	body := larkim.NewReplyMessageReqBodyBuilder().
		MsgType(larkim.MsgTypeText).
		Content(string(content)).
		Build()
	req := larkim.NewReplyMessageReqBuilder().
		MessageId(messageID).
		Body(body).
		Build()

	resp, err := s.client.Im.Message.Reply(ctx, req)
	if err != nil {
		return fmt.Errorf("im.message.reply: %w", err)
	}
	if !resp.Success() {
		return fmt.Errorf("im.message.reply: code=%d msg=%s log_id=%s", resp.Code, resp.Msg, resp.RequestId())
	}
	return nil
}

// ReplyCard answers messageID with an interactive card, the same way text
// replies are delivered.
func (s *larkSender) ReplyCard(ctx context.Context, messageID string, card map[string]any) error {
	content, err := json.Marshal(card)
	if err != nil {
		return fmt.Errorf("marshal card: %w", err)
	}
	body := larkim.NewReplyMessageReqBodyBuilder().
		MsgType(larkim.MsgTypeInteractive).
		Content(string(content)).
		Build()
	req := larkim.NewReplyMessageReqBuilder().
		MessageId(messageID).
		Body(body).
		Build()

	resp, err := s.client.Im.Message.Reply(ctx, req)
	if err != nil {
		return fmt.Errorf("im.message.reply: %w", err)
	}
	if !resp.Success() {
		return fmt.Errorf("im.message.reply: code=%d msg=%s log_id=%s", resp.Code, resp.Msg, resp.RequestId())
	}
	return nil
}

// PatchCard updates messageID with an updated interactive card.
func (s *larkSender) PatchCard(ctx context.Context, messageID string, card map[string]any) error {
	content, err := json.Marshal(card)
	if err != nil {
		return fmt.Errorf("marshal card: %w", err)
	}
	body := larkim.NewPatchMessageReqBodyBuilder().
		Content(string(content)).
		Build()
	req := larkim.NewPatchMessageReqBuilder().
		MessageId(messageID).
		Body(body).
		Build()

	resp, err := s.client.Im.Message.Patch(ctx, req)
	if err != nil {
		return fmt.Errorf("im.message.patch: %w", err)
	}
	if !resp.Success() {
		return fmt.Errorf("im.message.patch: code=%d msg=%s log_id=%s", resp.Code, resp.Msg, resp.RequestId())
	}
	return nil
}
