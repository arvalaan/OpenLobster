package mattermost

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/mattermost/mattermost/server/public/model"
	"github.com/neirth/openlobster/internal/domain/models"
)

// maxAttachmentBytes is the maximum file size (in bytes) that will be
// downloaded into memory. Larger attachments are recorded as metadata-only.
const maxAttachmentBytes int64 = 25 * 1024 * 1024 // 25 MiB

// buildWSURL converts an HTTP(S) server URL to the WebSocket endpoint.
func buildWSURL(serverURL string) string {
	if strings.HasPrefix(serverURL, "https://") {
		return "wss://" + serverURL[len("https://"):]
	}
	if strings.HasPrefix(serverURL, "http://") {
		return "ws://" + serverURL[len("http://"):]
	}
	return "wss://" + serverURL
}

// listenWithReconnect connects to the Mattermost WebSocket and calls onMessage
// for each relevant incoming post. On disconnection, it retries with exponential
// backoff until ctx is cancelled.
func listenWithReconnect(
	ctx context.Context,
	serverURL, token string,
	botUserID, botUsername, channelType string,
	apiClient *model.Client4,
	adapter *Adapter,
	sr *StickyRouter,
	onMessage func(context.Context, *models.Message),
) {
	backoff := time.Second
	maxBackoff := 60 * time.Second

	for {
		if ctx.Err() != nil {
			return
		}
		connected, err := connectAndListen(ctx, serverURL, token, botUserID, botUsername, channelType, apiClient, adapter, sr, onMessage)
		if connected {
			backoff = time.Second
		}
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
		}
	}
}

func connectAndListen(
	ctx context.Context,
	serverURL, token string,
	botUserID, botUsername, channelType string,
	apiClient *model.Client4,
	adapter *Adapter,
	sr *StickyRouter,
	onMessage func(context.Context, *models.Message),
) (connected bool, err error) {
	wsURL := buildWSURL(serverURL)
	wsClient, appErr := model.NewWebSocketClient4(wsURL, token)
	if appErr != nil {
		return false, appErr
	}
	defer wsClient.Close()

	wsClient.Listen()
	connected = true

	for {
		select {
		case <-ctx.Done():
			return true, nil
		case event, ok := <-wsClient.EventChannel:
			if !ok {
				if wsClient.ListenError != nil {
					return true, wsClient.ListenError
				}
				return true, nil
			}
			if event == nil {
				continue
			}
			if event.EventType() == model.WebsocketEventPosted {
				handlePostedEvent(ctx, event, botUserID, botUsername, channelType, apiClient, adapter, sr, onMessage)
			}
		case <-wsClient.PingTimeoutChannel:
			return true, fmt.Errorf("ping timeout")
		}
	}
}

// handlePostedEvent processes a single "posted" WebSocket event.
func handlePostedEvent(
	ctx context.Context,
	event *model.WebSocketEvent,
	botUserID, botUsername, channelType string,
	apiClient *model.Client4,
	adapter *Adapter,
	sr *StickyRouter,
	onMessage func(context.Context, *models.Message),
) {
	data := event.GetData()
	postJSON, _ := data["post"].(string)
	if postJSON == "" {
		return
	}

	var post model.Post
	if err := json.Unmarshal([]byte(postJSON), &post); err != nil {
		return
	}

	// Self-echo filter: ignore events posted by this bot.
	if post.UserId == botUserID {
		return
	}

	// Skip system messages (type != "" means join/leave/header/etc.).
	if post.Type != "" {
		return
	}

	// Determine channel type: "D" = direct message.
	chType, _ := data["channel_type"].(string)
	if chType == "" {
		if ch, _, err := apiClient.GetChannel(ctx, post.ChannelId); err == nil {
			chType = string(ch.Type)
		}
	}
	isDM := chType == string(model.ChannelTypeDirect)

	// @mention filter: only process if the bot is @mentioned, it's a DM, or the
	// user has a live sticky routing entry pointing at this adapter.
	mentionRe := regexp.MustCompile(`(?:^|\s)@` + regexp.QuoteMeta(botUsername) + `\b`)
	isMentioned := mentionRe.MatchString(post.Message)
	isSticky := false
	if !isDM && !isMentioned {
		if sr == nil || sr.Get(post.ChannelId, post.UserId) != channelType {
			return
		}
		isSticky = true
	}
	if isMentioned && sr != nil {
		sr.Set(post.ChannelId, post.UserId, channelType)
	}

	// Resolve sender display name.
	senderName := post.UserId
	if user, _, err := apiClient.GetUser(ctx, post.UserId, ""); err == nil {
		if user.Nickname != "" {
			senderName = user.Nickname
		} else if user.Username != "" {
			senderName = user.Username
		}
	}

	// Resolve group name for non-DM channels.
	groupName := ""
	if !isDM {
		if ch, _, err := apiClient.GetChannel(ctx, post.ChannelId); err == nil {
			groupName = ch.DisplayName
			if groupName == "" {
				groupName = ch.Name
			}
		}
	}

	// Strip the bot mention for cleaner LLM context.
	stripRe := regexp.MustCompile(`@` + regexp.QuoteMeta(botUsername) + `\b`)
	content := strings.TrimSpace(stripRe.ReplaceAllString(post.Message, ""))

	// Resolve file attachments.
	var attachments []models.Attachment
	for _, fid := range post.FileIds {
		fi, _, err := apiClient.GetFileInfo(ctx, fid)
		if err != nil {
			log.Printf("mattermost[%s]: GetFileInfo(%s) failed: %v", botUsername, fid, err)
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
		var fileData []byte
		if fi.Size <= maxAttachmentBytes {
			var fileErr error
			fileData, _, fileErr = apiClient.GetFile(ctx, fid)
			if fileErr != nil {
				log.Printf("mattermost[%s]: GetFile(%s) failed: %v", botUsername, fid, fileErr)
			} else {
				log.Printf("mattermost[%s]: attachment %s (%s, %d bytes, %s)", botUsername, fi.Name, attType, len(fileData), mimeType)
			}
		} else {
			log.Printf("mattermost[%s]: attachment %s too large (%d bytes > %d), metadata only", botUsername, fi.Name, fi.Size, maxAttachmentBytes)
		}
		attachments = append(attachments, models.Attachment{
			Type:     attType,
			Filename: fi.Name,
			Size:     fi.Size,
			MIMEType: mimeType,
			Data:     fileData,
		})
	}

	// Update thread store so outbound replies stay in the same thread.
	adapter.setThreadRoot(post.ChannelId, post.RootId)

	msg := &models.Message{
		ID:          uuid.New(),
		ChannelID:   post.ChannelId,
		SenderID:    post.UserId,
		SenderName:  senderName,
		IsGroup:     !isDM,
		IsMentioned: isMentioned || isSticky,
		GroupName:   groupName,
		Content:     content,
		Timestamp:   time.UnixMilli(post.CreateAt),
		Attachments: attachments,
		Metadata: map[string]interface{}{
			"channel_type":       channelType,
			"mattermost_post_id": post.Id,
			"mattermost_root_id": post.RootId,
		},
	}

	// Keep-alive typing goroutine: fires an immediate indicator and refreshes
	// every 4 s (Mattermost indicators expire after ~5 s) until the handler returns.
	typingDone := make(chan struct{})
	go func() {
		ticker := time.NewTicker(4 * time.Second)
		defer ticker.Stop()
		// Use REST for typing since the WS client is shared and SendMessage isn't thread-safe.
		postTypingREST(ctx, apiClient, botUserID, post.ChannelId)
		for {
			select {
			case <-ticker.C:
				postTypingREST(ctx, apiClient, botUserID, post.ChannelId)
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

// postTypingREST sends a typing indicator via the REST API.
func postTypingREST(ctx context.Context, client *model.Client4, userID, channelID string) {
	body := map[string]string{"channel_id": channelID}
	resp, _ := client.DoAPIPostJSON(ctx, "/users/"+userID+"/typing", body)
	if resp != nil {
		resp.Body.Close()
	}
}
