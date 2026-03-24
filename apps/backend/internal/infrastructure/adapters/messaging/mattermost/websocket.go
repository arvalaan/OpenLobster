package mattermost

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"
	"github.com/neirth/openlobster/internal/domain/models"
)

// wsEvent is the envelope for all Mattermost WebSocket events.
type wsEvent struct {
	Event    string                 `json:"event"`
	Data     map[string]interface{} `json:"data"`
	Seq      int64                  `json:"seq"`
	SeqReply int64                  `json:"seq_reply"`
}

// buildWSURL converts an HTTP(S) server URL to the WebSocket endpoint.
func buildWSURL(serverURL string) string {
	if strings.HasPrefix(serverURL, "https://") {
		return "wss://" + serverURL[len("https://"):] + "/api/v4/websocket"
	}
	if strings.HasPrefix(serverURL, "http://") {
		return "ws://" + serverURL[len("http://"):] + "/api/v4/websocket"
	}
	return "wss://" + serverURL + "/api/v4/websocket"
}

// listenWithReconnect connects to the Mattermost WebSocket and calls onMessage
// for each relevant incoming post. On disconnection, it retries with exponential
// backoff until ctx is cancelled.
func listenWithReconnect(
	ctx context.Context,
	wsURL, token string,
	botUserID, botUsername, channelType string,
	client *Client,
	threadStore threadStorer,
	sr *StickyRouter,
	onMessage func(context.Context, *models.Message),
) {
	backoff := time.Second
	maxBackoff := 60 * time.Second

	for {
		if ctx.Err() != nil {
			return
		}
		err := connectAndListen(ctx, wsURL, token, botUserID, botUsername, channelType, client, threadStore, sr, onMessage)
		if err != nil && ctx.Err() == nil {
			log.Printf("mattermost[%s] ws: %v; reconnecting in %s", botUsername, err, backoff)
			select {
			case <-ctx.Done():
				return
			case <-time.After(backoff):
			}
			backoff *= 2
			if backoff > maxBackoff {
				backoff = maxBackoff
			}
		} else {
			backoff = time.Second
		}
	}
}

func connectAndListen(
	ctx context.Context,
	wsURL, token string,
	botUserID, botUsername, channelType string,
	client *Client,
	threadStore threadStorer,
	sr *StickyRouter,
	onMessage func(context.Context, *models.Message),
) error {
	dialer := websocket.DefaultDialer
	conn, _, err := dialer.DialContext(ctx, wsURL, nil)
	if err != nil {
		return fmt.Errorf("dial: %w", err)
	}
	defer conn.Close()

	// Authenticate with the bot token.
	auth := map[string]interface{}{
		"seq":    1,
		"action": "authentication_challenge",
		"data":   map[string]string{"token": token},
	}
	if err := conn.WriteJSON(auth); err != nil {
		return fmt.Errorf("auth: %w", err)
	}

	// Close the connection when the context is cancelled.
	done := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			conn.Close()
		case <-done:
		}
	}()
	defer close(done)

	for {
		_, msgBytes, err := conn.ReadMessage()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return fmt.Errorf("read: %w", err)
		}

		var evt wsEvent
		if err := json.Unmarshal(msgBytes, &evt); err != nil {
			continue
		}

		if evt.Event == "posted" {
			handlePostedEvent(ctx, evt, botUserID, botUsername, channelType, client, threadStore, sr, onMessage)
		}
	}
}

// handlePostedEvent processes a single "posted" WebSocket event.
// It is exported for testing purposes.
func handlePostedEvent(
	ctx context.Context,
	evt wsEvent,
	botUserID, botUsername, channelType string,
	client *Client,
	threadStore threadStorer,
	sr *StickyRouter,
	onMessage func(context.Context, *models.Message),
) {
	postJSON, _ := evt.Data["post"].(string)
	if postJSON == "" {
		return
	}

	var post mmPost
	if err := json.Unmarshal([]byte(postJSON), &post); err != nil {
		return
	}

	// Self-echo filter: ignore events posted by this bot.
	if post.UserID == botUserID {
		return
	}

	// Skip system messages (type != "" means join/leave/header/etc.).
	if post.Type != "" {
		return
	}

	// Determine channel type: "D" = direct message.
	// Fall back to a REST lookup when the event field is missing (some Mattermost versions omit it).
	chType, _ := evt.Data["channel_type"].(string)
	if chType == "" {
		if ch, err := client.GetChannel(ctx, post.ChannelID); err == nil {
			chType = ch.Type
		}
	}
	isDM := chType == "D"

	// @mention filter: only process if the bot is @mentioned, it's a DM, or the
	// user has a live sticky routing entry pointing at this adapter.
	mentionText := "@" + botUsername
	isMentioned := strings.Contains(post.Message, mentionText)
	isSticky := false
	if !isDM && !isMentioned {
		if sr == nil || sr.Get(post.ChannelID, post.UserID) != channelType {
			return
		}
		isSticky = true
	}
	// When explicitly mentioned, (re)set sticky so TTL refreshes.
	if isMentioned && sr != nil {
		sr.Set(post.ChannelID, post.UserID, channelType)
	}

	// Resolve sender display name.
	senderName := post.UserID
	if user, err := client.GetUser(ctx, post.UserID); err == nil {
		if user.Nickname != "" {
			senderName = user.Nickname
		} else if user.Username != "" {
			senderName = user.Username
		}
	}

	// Resolve group name for non-DM channels.
	groupName := ""
	if !isDM {
		if ch, err := client.GetChannel(ctx, post.ChannelID); err == nil {
			groupName = ch.DisplayName
			if groupName == "" {
				groupName = ch.Name
			}
		}
	}

	// Strip the bot mention for cleaner LLM context.
	content := strings.TrimSpace(strings.ReplaceAll(post.Message, mentionText, ""))

	// Resolve file attachments.
	var attachments []models.Attachment
	for _, fid := range post.FileIDs {
		fi, err := client.GetFileInfo(ctx, fid)
		if err != nil {
			continue
		}
		mimeType := fi.MimeType
		if mimeType == "" {
			mimeType = "application/octet-stream"
		}
		attType := "document"
		if strings.HasPrefix(mimeType, "image/") {
			attType = "image"
		} else if strings.HasPrefix(mimeType, "audio/") {
			attType = "audio"
		} else if strings.HasPrefix(mimeType, "video/") {
			attType = "video"
		}
		data, err := client.GetFileContent(ctx, fid)
		if err != nil {
			data = nil
		}
		attachments = append(attachments, models.Attachment{
			Type:     attType,
			Filename: fi.Name,
			Size:     fi.Size,
			MIMEType: mimeType,
			Data:     data,
		})
	}

	// Update thread store so outbound replies stay in the same thread.
	// For top-level posts (RootID == ""), store empty string so SendMessage
	// produces a plain inline reply rather than forcing a new thread.
	threadStore.set(post.ChannelID, post.RootID)

	msg := &models.Message{
		ID:          uuid.New(),
		ChannelID:   post.ChannelID,
		SenderID:    post.UserID,
		SenderName:  senderName,
		IsGroup:     !isDM,
		IsMentioned: isMentioned || isSticky,
		GroupName:   groupName,
		Content:     content,
		Timestamp:   time.UnixMilli(post.CreateAt),
		Attachments: attachments,
		Metadata: map[string]interface{}{
			"channel_type":       channelType,
			"mattermost_post_id": post.ID,
			"mattermost_root_id": post.RootID,
		},
	}

	// Keep-alive typing goroutine: fires an immediate indicator and refreshes
	// every 4 s (Mattermost indicators expire after ~5 s) until the handler returns.
	typingDone := make(chan struct{})
	go func() {
		ticker := time.NewTicker(4 * time.Second)
		defer ticker.Stop()
		_ = client.PostTyping(ctx, botUserID, post.ChannelID)
		for {
			select {
			case <-ticker.C:
				_ = client.PostTyping(ctx, botUserID, post.ChannelID)
			case <-typingDone:
				return
			case <-ctx.Done():
				return
			}
		}
	}()
	onMessage(ctx, msg)
	close(typingDone)
}

// threadStorer allows the WebSocket handler to record thread root IDs without
// coupling it to the full Adapter struct (useful in tests).
type threadStorer interface {
	set(channelID, rootID string)
}
