// Copyright (c) OpenLobster contributors. See LICENSE for details.

package handlers

import (
	"context"
	"time"
)

const loopbackChannelID = "loopback"

// buildMemoryConsolidationSystemPrompt returns the dedicated system prompt for
// the automated memory consolidation agent. The current timestamp is embedded
// at call time so the model always receives an accurate date.
func buildMemoryConsolidationSystemPrompt() string {
	return `## Role

You are an internal memory consolidation agent for OpenLobster. Your sole purpose
is to review recent conversations and extract durable knowledge about users into
the long-term memory graph. You do NOT interact with users.

## Instructions

1. Call list_conversations to get all conversations.
2. For each conversation, call get_conversation_messages to read recent messages.
3. Read the messages and identify ALL facts worth storing.
4. Store facts in small batches of 3-5 per tool-call round:
   - Call add_memory or set_user_property for 3-5 facts, then wait for results.
   - Then call add_memory or set_user_property for the next 3-5 facts, and so on.
   - Always pass for_user=<participant_name> on every call.
   - Use set_user_property for structured user attributes (real_name, occupation, city, country, language, timezone, birthday).
   - Use add_memory for all other facts. Give each fact a short, distinctive label (e.g. label="Burnout April 2024", label="Pets", label="TBAuctions CIO", label="Favorite tool n8n").
   - Do NOT call search_memory first — the storage layer deduplicates by label automatically.
5. After storing all facts from all conversations, stop. Do not send any visible reply.

## Rules

- Only store verifiable facts explicitly stated in the conversation.
- Do not store sensitive personal data (passwords, payment details, health records).
- Each fact should be concise and self-contained.
- NEVER use NO_REPLY — simply finish calling tools and return when done.

## Current Date

` + time.Now().Format("Monday, 2 January 2006 — 15:04:05 MST") + "\n"
}

// LoopbackDispatcher implements ports.TaskDispatcherPort and bridges the domain
// Scheduler with the MessageHandler.
type LoopbackDispatcher struct {
	handler *MessageHandler
}

// NewLoopbackDispatcher constructs a LoopbackDispatcher that routes task
// execution through handler.
func NewLoopbackDispatcher(handler *MessageHandler) *LoopbackDispatcher {
	return &LoopbackDispatcher{handler: handler}
}

// Dispatch sends prompt through the full agentic pipeline via the loopback channel.
// When the prompt originates from the memory consolidation scheduler, a dedicated
// system prompt is injected so the model knows its consolidation role.
func (d *LoopbackDispatcher) Dispatch(ctx context.Context, prompt string) error {
	return d.handler.Handle(ctx, HandleMessageInput{
		ChannelID:    loopbackChannelID,
		Content:      prompt,
		ChannelType:  loopbackChannelID,
		SystemPrompt: buildMemoryConsolidationSystemPrompt(),
	})
}
