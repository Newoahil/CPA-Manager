package feishu

import (
	"context"
	"fmt"

	larkcore "github.com/larksuite/oapi-sdk-go/v3/core"
	"github.com/larksuite/oapi-sdk-go/v3/event/dispatcher"
	larkim "github.com/larksuite/oapi-sdk-go/v3/service/im/v1"
	larkws "github.com/larksuite/oapi-sdk-go/v3/ws"
)

// Run starts the long connection and blocks until ctx is cancelled.
//
// The SDK auto-reconnects by default (unlimited retries), which is what we want
// for a long-lived bot; we only log reconnect transitions.
//
// IMPORTANT: deploy exactly ONE replica. Feishu caps an app at 50 concurrent
// long connections and delivers each event to just one of them (cluster
// delivery, not broadcast). Extra replicas would split traffic unpredictably
// across processes, and an instance that does not hold the newest report could
// answer a query.
func (b *Bot) Run(ctx context.Context) error {
	// Long-connection mode requires empty verification token and encrypt key:
	// there is no HTTP callback route to verify against.
	h := dispatcher.NewEventDispatcher("", "").
		OnP2MessageReceiveV1(b.HandleMessageV1).
		OnP2MessageReadV1(handleMessageRead).
		OnP2CardActionTrigger(b.HandleCardActionTrigger)

	client := larkws.NewClient(b.cfg.FeishuAppID, b.cfg.FeishuAppSecret,
		larkws.WithEventHandler(h),
		larkws.WithLogLevel(larkcore.LogLevelInfo),
		larkws.WithOnReconnecting(func() {
			b.log.InfoContext(ctx, "feishu websocket reconnecting")
		}),
		larkws.WithOnReconnected(func() {
			b.log.InfoContext(ctx, "feishu websocket reconnected")
		}),
	)

	b.log.InfoContext(ctx, "feishu long connection starting", "note", "requires single-replica deployment")
	if err := client.Start(ctx); err != nil {
		return fmt.Errorf("feishu: long connection: %w", err)
	}
	return nil
}

// handleMessageRead swallows the message-read receipt.
//
// The bot does not act on read receipts, but the SDK logs an "not found
// handler" error for every event type with no registered handler. Registering
// an explicit no-op keeps that routine event out of the error log. It is
// deliberately silent: a receipt is not a user action worth auditing.
func handleMessageRead(_ context.Context, _ *larkim.P2MessageReadV1) error {
	return nil
}
