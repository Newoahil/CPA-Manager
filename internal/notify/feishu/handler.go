package feishu

import (
	"context"
	"time"

	"github.com/larksuite/oapi-sdk-go/v3/event/dispatcher/callback"
	larkim "github.com/larksuite/oapi-sdk-go/v3/service/im/v1"

	"github.com/Newoahil/CPA-Manager/internal/domain"
	"github.com/Newoahil/CPA-Manager/internal/notify/render"
)

// maxInflightQueries caps how many @Bot queries may render at once.
//
// Without a cap a burst of mentions would each spawn a goroutine; over the cap
// the query is refused with a short note instead.
const maxInflightQueries = 4

// HandleMessageV1 processes an inbound message event.
//
// It does NOT collect quota itself. Feishu re-delivers an event whose handler
// has not returned promptly, and a synchronous 5–7 second collection inside the
// handler is exactly how one user message produced two replies. The handler
// therefore only dedupes and classifies, then hands the slow work to a
// goroutine and returns. The reply is sent from that goroutine.
//
// Every chat is served: a group when the bot is @-mentioned (decided from the
// Mentions list, never by matching text), a one-to-one chat always.
func (b *Bot) HandleMessageV1(ctx context.Context, event *larkim.P2MessageReceiveV1) error {
	if event == nil || event.Event == nil || event.Event.Message == nil {
		return nil
	}
	in := parseMessage(event)
	if in.chatID == "" {
		return nil
	}
	// Any chat the bot is in is served: a group when the bot is @-mentioned,
	// a one-to-one chat always. FEISHU_CHAT_ID only decides where the
	// unprompted alerts and digests go. The answers are read-only quota
	// numbers, the same in every chat.
	if !in.botMentioned && !in.direct {
		return nil
	}
	// Idempotency: a redelivered event (long-connection reconnect, at-least-once
	// delivery) must not produce a second reply. The key is the event id, with
	// the message id as a fallback when the header is absent. The reservation
	// is released if the task cannot be started, so shutdown does not consume it.
	key := messageEventKey(event, in.messageID)
	if !b.inbound.mark(key) {
		b.log.InfoContext(ctx, "feishu duplicate message ignored",
			"action", "query", "result", "duplicate", "message_id", in.messageID)
		return nil
	}

	intent := parseIntent(stripMentions(in.text))
	if !b.dispatch(ctx, event, in, intent) {
		b.inbound.forget(key)
	}
	return nil
}

// dispatch schedules the slow half of a query asynchronously, returning false
// only when the task could not be accepted (the bot is shutting down), so the
// caller can release its dedup reservation.
func (b *Bot) dispatch(ctx context.Context, event *larkim.P2MessageReceiveV1, in inbound, intent intent) bool {
	started := time.Now()
	// "Accepted" is logged here, on the request path, before the handler
	// returns. It is a distinct event from the "replied" audit line so the two
	// timestamps cannot be confused: the gap between them is the collection.
	b.log.InfoContext(ctx, "feishu query accepted",
		"action", "query", "stage", "accepted",
		"operator_open_id", in.openID, "chat_id", in.chatID,
		"detail", intentName(intent))

	ok := b.tasks.Go(func(taskCtx context.Context) {
		b.serveQuery(taskCtx, in, intent, started)
	})
	if !ok {
		b.log.WarnContext(ctx, "feishu query dropped, bot shutting down",
			"action", "query", "stage", "dropped",
			"operator_open_id", in.openID, "chat_id", in.chatID,
			"detail", intentName(intent))
	}
	return ok
}

// serveQuery runs one query to completion on a detached goroutine: collect,
// render and reply, then write the "replied" audit line. taskCtx is the Bot's
// task context, not the (already-cancelled) event context.
func (b *Bot) serveQuery(ctx context.Context, in inbound, intent intent, accepted time.Time) {
	// A query that carries no data (help / unknown) is answered immediately and
	// never touches an upstream.
	if intent.kind == intentHelp || intent.kind == intentUnknown {
		text := b.buildReply(intent, domain.Report{}, freshness{})
		if err := b.reply(ctx, in.messageID, text); err != nil {
			b.log.WarnContext(ctx, "feishu reply failed", "action", "query", "result", "error", "error", err)
			return
		}
		b.audit(ctx, "query", in.openID, in.chatID, intentName(intent), "ok", accepted)
		return
	}

	if !b.acquireQuery() {
		// Over the concurrency cap: tell the user rather than queue unbounded
		// work, and do not start a collection.
		if err := b.reply(ctx, in.messageID, "当前查询较多，请稍后再试。"); err != nil {
			b.log.WarnContext(ctx, "feishu reply failed", "action", "query", "result", "error", "error", err)
		}
		b.audit(ctx, "query", in.openID, in.chatID, intentName(intent), "busy", accepted)
		return
	}
	defer b.releaseQuery()

	fresh, ok := b.serveLatest(ctx, in, intent)
	if !ok {
		return
	}
	b.audit(ctx, "query", in.openID, in.chatID, intentName(intent), auditResult(fresh), accepted)
}

// serveLatest answers a query from the last evaluated report. Collection is
// scheduler-driven, so a query never triggers a scrape; the report's own age is
// what the freshness label reports. ok is false when a reply could not be sent
// (already logged); the caller then skips the audit line.
func (b *Bot) serveLatest(ctx context.Context, in inbound, intent intent) (fresh freshness, ok bool) {
	if b.source == nil {
		if rerr := b.reply(ctx, in.messageID, "尚无额度数据，请稍后再试。"); rerr != nil {
			b.log.WarnContext(ctx, "feishu reply failed", "action", "query", "result", "error", "error", rerr)
		}
		return freshness{}, false
	}
	rep, has := b.source.LastReport()
	if !has {
		// No cycle has succeeded yet: tell the user instead of staying silent.
		if rerr := b.reply(ctx, in.messageID, "尚无额度数据，请等待首次采集完成。"); rerr != nil {
			b.log.WarnContext(ctx, "feishu reply failed", "action", "query", "result", "error", "error", rerr)
		}
		return freshness{}, false
	}

	// The report is always the cached one now; freshnessOf decides whether it is
	// still current (within one poll interval) or has aged out.
	fresh = b.freshnessOf(rep, true, time.Now())
	msg := b.queryMessage(intent, rep, fresh)
	if serr := b.replyCard(ctx, in.messageID, msg); serr != nil {
		b.log.WarnContext(ctx, "feishu card reply failed", "action", "query", "result", "error", "error", serr)
		return fresh, false
	}
	return fresh, true
}

// freshnessOf classifies the answer using the configured poll interval as the
// "still current" threshold: cached data younger than one collection cycle is
// the newest data that could exist, so it must not be reported as a failure.
func (b *Bot) freshnessOf(rep domain.Report, cached bool, now time.Time) freshness {
	return freshnessOf(rep, cached, b.cfg.PollInterval, now)
}

// auditResult keeps the operator log's three-way distinction: a throttled but
// still-current answer is not the same incident as an expired one.
func auditResult(f freshness) string {
	switch {
	case f.live:
		return "ok"
	case f.expired:
		return "expired"
	default:
		return "cached"
	}
}

// HandleCardActionTrigger processes a card button callback.
//
// The manual refresh button is gone: collection is entirely scheduler-driven.
// Cards in the wild still carry the old refresh action, so it is routed here and
// answered with a toast explaining the change rather than a scrape. Any other
// unknown action is refused, and the row-tap noop is acknowledged silently.
func (b *Bot) HandleCardActionTrigger(ctx context.Context, event *callback.CardActionTriggerEvent) (*callback.CardActionTriggerResponse, error) {
	started := time.Now()
	if event == nil || event.Event == nil {
		return &callback.CardActionTriggerResponse{Toast: &callback.Toast{Type: "error", Content: "无效的回调"}}, nil
	}
	openID, chatID, _ := "", "", ""
	if event.Event.Operator != nil {
		openID = event.Event.Operator.OpenID
	}
	if event.Event.Context != nil {
		chatID = event.Event.Context.OpenChatID
	}

	// Idempotency: a redelivered or double-pushed callback must not be acted on
	// twice. The refresh action is a no-op now, but the row-tap and future
	// (phase 4) approval actions still deserve the guarantee.
	key := cardEventKey(event)
	if !b.inbound.mark(key) {
		b.log.InfoContext(ctx, "feishu duplicate card action ignored",
			"action", "card_action", "operator_open_id", openID, "chat_id", chatID, "result", "duplicate")
		return &callback.CardActionTriggerResponse{Toast: &callback.Toast{
			Type: "info", Content: "该操作已处理",
		}}, nil
	}

	action := actionName(event)
	if action == render.NoopAction {
		// A tap on a card row (the rounded row container must declare a
		// callback). Nothing to do and nothing to say.
		return &callback.CardActionTriggerResponse{}, nil
	}
	if action == render.RefreshAction {
		// The button is removed. Existing cards still carry the action value, so
		// answer with an explanatory toast instead of silently failing.
		b.audit(ctx, "card_action", openID, chatID, render.RefreshAction, "removed", started)
		return &callback.CardActionTriggerResponse{Toast: &callback.Toast{
			Type: "info", Content: "刷新按钮已移除，额度由系统自动采集",
		}}, nil
	}

	b.audit(ctx, "card_action", openID, chatID, action, "unsupported", started)
	return &callback.CardActionTriggerResponse{Toast: &callback.Toast{Type: "info", Content: "该操作不支持"}}, nil
}

func (b *Bot) reply(ctx context.Context, messageID, text string) error {
	rctx, cancel := context.WithTimeout(ctx, b.replyBudget)
	defer cancel()
	return b.sender.ReplyText(rctx, messageID, text)
}

// acquireQuery reserves one of the bounded concurrent-query slots.
func (b *Bot) acquireQuery() bool {
	select {
	case b.querySlots <- struct{}{}:
		return true
	default:
		return false
	}
}

func (b *Bot) releaseQuery() { <-b.querySlots }

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
