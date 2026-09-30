package feishu

import (
	"context"

	larkim "github.com/larksuite/oapi-sdk-go/v3/service/im/v1"
)

// This file registers silent handlers for Feishu event types the bot does not
// act on. The SDK logs an "not found handler" error for every pushed event with
// no registered handler, so an unhandled read receipt or "user entered the
// bot chat" line floods the error log with noise that hides real failures.
//
// Each handler is a deliberate no-op: it returns nil without touching the
// network, the refresher or the audit log. An event we do not act on is not a
// user action worth recording, and a read receipt can arrive in bursts.
//
// Only IM (messaging) events are listed. They are the only family this app is
// plausibly subscribed to, and registering a handler for an event the app never
// receives is harmless; registering none for one it does receive is the bug
// being fixed here.

func ignoreP2ChatAccessEventBotP2pChatEntered(_ context.Context, _ *larkim.P2ChatAccessEventBotP2pChatEnteredV1) error {
	return nil
}

func ignoreP2ChatDisbandedV1(_ context.Context, _ *larkim.P2ChatDisbandedV1) error {
	return nil
}

func ignoreP2ChatUpdatedV1(_ context.Context, _ *larkim.P2ChatUpdatedV1) error {
	return nil
}

func ignoreP2ChatMemberBotAddedV1(_ context.Context, _ *larkim.P2ChatMemberBotAddedV1) error {
	return nil
}

func ignoreP2ChatMemberBotDeletedV1(_ context.Context, _ *larkim.P2ChatMemberBotDeletedV1) error {
	return nil
}

func ignoreP2ChatMemberUserAddedV1(_ context.Context, _ *larkim.P2ChatMemberUserAddedV1) error {
	return nil
}

func ignoreP2ChatMemberUserDeletedV1(_ context.Context, _ *larkim.P2ChatMemberUserDeletedV1) error {
	return nil
}

func ignoreP2ChatMemberUserWithdrawnV1(_ context.Context, _ *larkim.P2ChatMemberUserWithdrawnV1) error {
	return nil
}

func ignoreP2MessageRecalledV1(_ context.Context, _ *larkim.P2MessageRecalledV1) error {
	return nil
}

func ignoreP2MessageReactionCreatedV1(_ context.Context, _ *larkim.P2MessageReactionCreatedV1) error {
	return nil
}

func ignoreP2MessageReactionDeletedV1(_ context.Context, _ *larkim.P2MessageReactionDeletedV1) error {
	return nil
}
