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

// cardRefreshBudget bounds the refresh performed inside a CARD CALLBACK.
//
// Feishu requires a callback response within 3 seconds, so we refresh with a
// margin below it: on timeout we fall back to the last report or keep the
// existing card. This limit is a property of the callback protocol.
const cardRefreshBudget = 2500 * time.Millisecond

// messageRefreshBudget bounds the refresh performed for an @Bot MESSAGE.
//
// The collection now runs on a goroutine detached from the event handler, so
// this is a hard ceiling on that work rather than a deadline imposed by the
// caller: a wedged upstream must not hold the last query open forever. A real
// collection takes a few seconds, so the budget comfortably exceeds one.
const messageRefreshBudget = 30 * time.Second

// maxInflightQueries caps how many @Bot queries may be collected at once.
//
// Without a cap a burst of mentions would each spawn a goroutine and a
// concurrent upstream collection, which is how a small group can wedge the
// collector. Over the cap the query is refused with a short note instead.
const maxInflightQueries = 4

// HandleMessageV1 processes an inbound message event.
//
// It does NOT collect quota itself. Feishu re-delivers an event whose handler
// has not returned promptly, and a synchronous 5–7 second collection inside the
// handler is exactly how one user message produced two replies. The handler
// therefore only dedupes and classifies, then hands the slow work to a
// goroutine and returns. The reply is sent from that goroutine.
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

	fresh, ok := b.collectAndReply(ctx, in, intent)
	if !ok {
		return
	}
	b.audit(ctx, "query", in.openID, in.chatID, intentName(intent), auditResult(fresh), accepted)
}

// collectAndReply performs the collection and sends the answer. ok is false when
// a reply could not be sent (already logged); the caller then skips the audit
// line.
func (b *Bot) collectAndReply(ctx context.Context, in inbound, intent intent) (fresh freshness, ok bool) {
	rep, cached, err := b.collect(ctx, b.messageBudget)
	if err != nil && !cached {
		// No cached report either: tell the user instead of staying silent.
		if rerr := b.reply(ctx, in.messageID, "实时采集失败，且暂无历史数据，请稍后再试。"); rerr != nil {
			b.log.WarnContext(ctx, "feishu reply failed", "action", "query", "result", "error", "error", rerr)
		}
		return freshness{}, false
	}

	fresh = b.freshnessOf(rep, cached, time.Now())
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

// HandleCardActionTrigger processes a card button callback. It must return
// within the 3-second budget, so its only refresh is bounded by refreshBudget.
//
// Unlike a message query, this path stays synchronous: Feishu needs the
// response body within 3 seconds to update the card, so it cannot be deferred
// to a goroutine. Its refresh is therefore capped at 2.5s and falls back to the
// cached report.
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

	// Idempotency: a redelivered or double-pushed callback must not trigger a
	// second collection. Deduped callbacks are refused rather than answered,
	// so Feishu does not replace the card a second time.
	if !b.inbound.mark(cardEventKey(event)) {
		b.log.InfoContext(ctx, "feishu duplicate card action ignored",
			"action", "card_action", "operator_open_id", openID, "chat_id", chatID, "result", "duplicate")
		return &callback.CardActionTriggerResponse{Toast: &callback.Toast{
			Type: "info", Content: "该操作已处理",
		}}, nil
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

	rep, cached, err := b.collect(ctx, b.refreshBudget)
	switch {
	case err != nil && !cached:
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

	fresh := b.freshnessOf(rep, cached, started)
	msg := domain.Message{
		Kind:      render.KindQuery,
		Report:    &rep,
		Freshness: fresh.label(),
		Notice:    fresh.notice(),
	}
	card := b.renderer.Card(msg)
	toast := "额度已刷新"
	switch {
	case fresh.expired:
		toast = "未能取到新数据，展示的是可能过期的缓存"
	case !fresh.live:
		toast = "本次未实时刷新，展示的是最近一次采集结果"
	}
	b.audit(ctx, "card_action", openID, chatID, render.RefreshAction, auditResult(fresh), started)
	return &callback.CardActionTriggerResponse{
		Toast: &callback.Toast{Type: "success", Content: toast},
		Card:  &callback.Card{Type: "card_json", Data: card},
	}, nil
}

// collect runs a read-only refresh bounded by budget, falling back to the
// cached report on any failure. cached is true when the returned report is the
// fallback. err is the underlying reason and is non-nil whenever we did not
// re-collect; callers must check cached before treating err as fatal.
//
// The budget is the caller's, not a global: a card callback must answer inside
// Feishu's 3-second window, while a message reply may take as long as a real
// collection needs. The caller's context still bounds it from above.
//
// cached says only "we did not re-collect this time". Whether that matters is
// a question about the DATA's age, answered by freshnessOf, not by this flag:
// a throttled refresh one minute after a successful cycle is still current.
func (b *Bot) collect(ctx context.Context, budget time.Duration) (rep domain.Report, cached bool, err error) {
	if b.refresher == nil {
		return domain.Report{}, false, errors.New("no quota refresher configured")
	}
	rctx, cancel := context.WithTimeout(ctx, budget)
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
