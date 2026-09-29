package feishu

import (
	"context"
	"errors"
	"time"

	"github.com/larksuite/oapi-sdk-go/v3/event/dispatcher/callback"
	larkim "github.com/larksuite/oapi-sdk-go/v3/service/im/v1"

	"github.com/Newoahil/CPA-Manager/internal/domain"
	"github.com/Newoahil/CPA-Manager/internal/notify/render"
)

// The long-connection callback budget is 3 seconds. We refresh with a margin
// below it so a slow collector can never make Feishu time the callback out: on
// timeout we fall back to the last report or keep the existing card.
const refreshBudget = 2500 * time.Millisecond

// HandleMessageV1 processes an inbound message event.
//
// Only messages in the single configured group are served; anything from
// another chat is ignored silently. The bot replies only when it is @-mentioned,
// and whether it was mentioned is decided from the Mentions list, never by
// matching text.
func (b *Bot) HandleMessageV1(ctx context.Context, event *larkim.P2MessageReceiveV1) error {
	if event == nil || event.Event == nil || event.Event.Message == nil {
		return nil
	}
	in := parseMessage(event)
	if in.chatID == "" || in.chatID != b.cfg.FeishuChatID {
		// Not our group: do not reply, do not error.
		return nil
	}
	if !in.botMentioned {
		return nil
	}

	intent := parseIntent(stripMentions(in.text))
	started := time.Now()

	// Help / unknown never touch upstreams.
	if intent.kind == intentHelp || intent.kind == intentUnknown {
		if err := b.reply(ctx, in.messageID, b.buildReply(intent, domain.Report{}, false)); err != nil {
			b.log.WarnContext(ctx, "feishu reply failed", "action", "query", "result", "error", "error", err)
		}
		b.audit(ctx, "query", in.openID, in.chatID, intentName(intent), "ok", started)
		return nil
	}

	rep, stale, err := b.collect(ctx)
	if err != nil && !stale {
		// No cached report either: tell the user instead of staying silent.
		if rerr := b.reply(ctx, in.messageID, "实时采集失败，且暂无历史数据，请稍后再试。"); rerr != nil {
			b.log.WarnContext(ctx, "feishu reply failed", "action", "query", "result", "error", "error", rerr)
		}
		b.audit(ctx, "query", in.openID, in.chatID, intentName(intent), "error", started)
		return nil
	}

	text := b.buildReply(intent, rep, stale)
	if serr := b.reply(ctx, in.messageID, text); serr != nil {
		b.log.WarnContext(ctx, "feishu reply failed", "action", "query", "result", "error", "error", serr)
		b.audit(ctx, "query", in.openID, in.chatID, intentName(intent), "send_error", started)
		return nil
	}
	result := "ok"
	if stale {
		result = "stale"
	}
	b.audit(ctx, "query", in.openID, in.chatID, intentName(intent), result, started)
	return nil
}

// HandleCardActionTrigger processes a card button callback. It must return
// within the 3-second budget, so its only refresh is bounded by refreshBudget.
//
// The refresh path is read-only. If it cannot complete in budget it returns a
// card built from the cached report (explicitly marked stale) or, failing that,
// keeps the original card via a nil Card.
func (b *Bot) HandleCardActionTrigger(ctx context.Context, event *callback.CardActionTriggerEvent) (*callback.CardActionTriggerResponse, error) {
	started := time.Now()
	if event == nil || event.Event == nil {
		return &callback.CardActionTriggerResponse{Toast: &callback.Toast{Type: "error", Content: "无效的回调"}}, nil
	}
	openID, chatID := "", ""
	if event.Event.Operator != nil {
		openID = event.Event.Operator.OpenID
	}
	if event.Event.Context != nil {
		chatID = event.Event.Context.OpenChatID
	}

	// Defense in depth: the card is only ever delivered to our group, so a
	// callback from another chat is refused. This trusts the event's chat
	// context, not Action.Value (which is untrusted user input).
	if chatID != "" && chatID != b.cfg.FeishuChatID {
		b.audit(ctx, "card_action", openID, chatID, "chat_mismatch", "denied", started)
		return &callback.CardActionTriggerResponse{Toast: &callback.Toast{Type: "error", Content: "该操作不可用"}}, nil
	}

	action := actionName(event)
	if action != render.RefreshAction {
		b.audit(ctx, "card_action", openID, chatID, action, "unsupported", started)
		return &callback.CardActionTriggerResponse{Toast: &callback.Toast{Type: "info", Content: "该操作不支持"}}, nil
	}

	rep, stale, err := b.collect(ctx)
	switch {
	case err != nil && !stale:
		// No fallback report and the refresh did not complete in budget.
		b.audit(ctx, "card_action", openID, chatID, render.RefreshAction, "unavailable", started)
		return &callback.CardActionTriggerResponse{Toast: &callback.Toast{
			Type:    "info",
			Content: "正在刷新，请稍后查看最新卡片",
		}}, nil
	case errors.Is(err, context.DeadlineExceeded):
		// Timed out but we do have a cached report: keep the original card.
		b.audit(ctx, "card_action", openID, chatID, render.RefreshAction, "timeout", started)
		return &callback.CardActionTriggerResponse{Toast: &callback.Toast{
			Type:    "info",
			Content: "正在刷新，请稍后查看最新卡片",
		}}, nil
	}

	msg := domain.Message{Kind: render.KindQuery, Report: &rep}
	card := b.renderer.Card(msg)
	result, toast := "ok", "额度已刷新"
	if stale {
		result, toast = "stale", "已展示缓存数据，可能已过期"
	}
	b.audit(ctx, "card_action", openID, chatID, render.RefreshAction, result, started)
	return &callback.CardActionTriggerResponse{
		Toast: &callback.Toast{Type: "success", Content: toast},
		Card:  &callback.Card{Type: "card_json", Data: card},
	}, nil
}

// collect runs a read-only refresh bounded by refreshBudget, falling back to the
// cached report on any failure. stale is true when the returned report is the
// fallback. err is the underlying reason and is non-nil whenever the data is not
// fresh; callers must check stale before treating err as fatal.
func (b *Bot) collect(ctx context.Context) (rep domain.Report, stale bool, err error) {
	if b.refresher == nil {
		return domain.Report{}, false, errors.New("no quota refresher configured")
	}
	rctx, cancel := context.WithTimeout(ctx, b.refreshBudget)
	defer cancel()

	rep, err = b.refresher.RefreshNow(rctx)
	if err == nil {
		return rep, false, nil
	}
	// A throttled or busy refresh returns the cached report with a sentinel
	// error; so does a timeout. Either way, fall back to the last good report
	// and label it stale rather than showing nothing.
	if last, ok := b.refresher.LastReport(); ok {
		return last, true, err
	}
	return domain.Report{}, false, err
}

func (b *Bot) reply(ctx context.Context, messageID, text string) error {
	rctx, cancel := context.WithTimeout(ctx, b.replyBudget)
	defer cancel()
	return b.sender.ReplyText(rctx, messageID, text)
}

// audit emits one structured, secret-free line per user action. It records what
// happened, who did it and how long it took; it never records message bodies,
// cookies or tokens.
func (b *Bot) audit(ctx context.Context, action, openID, chatID, detail, result string, started time.Time) {
	b.log.InfoContext(ctx, "feishu action",
		"action", action,
		"operator_open_id", openID,
		"chat_id", chatID,
		"detail", detail,
		"result", result,
		"duration_ms", time.Since(started).Milliseconds(),
	)
}

func intentName(in intent) string {
	switch in.kind {
	case intentHelp:
		return "help"
	case intentUnknown:
		return "unknown"
	case intentStatus:
		return "status"
	case intentAbnormal:
		return "abnormal"
	case intentProvider:
		return "provider:" + string(in.provider)
	default:
		return "unknown"
	}
}
