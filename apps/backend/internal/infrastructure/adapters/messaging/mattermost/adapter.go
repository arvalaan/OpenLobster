// Package mattermost provides a Mattermost messaging adapter for OpenLobster.
//
// The adapter uses the official Mattermost SDK (model.Client4 for REST,
// model.WebSocketClient for real-time events). One Adapter corresponds to one
// bot profile (one Mattermost bot account).
//
// Required Mattermost bot permissions:
//   - Read posts in all relevant channels
//   - Create posts
//   - Upload files
//   - Add reactions
//   - Read user info
package mattermost

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/neirth/openlobster/internal/domain/models"
	"github.com/neirth/openlobster/internal/domain/ports"
	"github.com/neirth/openlobster/internal/infrastructure/config"
)

// Adapter implements ports.MessagingPort for Mattermost using the official SDK.
// One Adapter corresponds to one bot profile (one Mattermost bot account).
//
// Mention-based routing: the adapter only forwards messages to onMessage when
// the bot is @mentioned (by username) or the conversation is a direct message.
type Adapter struct {
	client      *model.Client4
	serverURL   string
	botUserID   string
	botUsername string
	// channelType is the registry key used in message Metadata["channel_type"].
	// Format: "mattermost:<lowercased-profile-name>" (e.g. "mattermost:researcher").
	channelType string
	profile     config.MattermostBotProfile

	// threadRoots maps Mattermost channel IDs to the root post ID of the
	// active thread. Used to keep bot replies inside the same thread.
	threadRoots sync.Map // map[string]string

	stickyRouter *StickyRouter

	// cancel stops the WebSocket listener goroutine started by Start.
	cancel context.CancelFunc
}

// NewAdapter creates an Adapter for the given Mattermost server and bot profile.
// The adapter is not yet connected; call Start to open the WebSocket connection.
func NewAdapter(serverURL string, profile config.MattermostBotProfile, sr *StickyRouter) (*Adapter, error) {
	if serverURL == "" {
		return nil, fmt.Errorf("mattermost: serverURL is required")
	}
	serverURL = strings.TrimRight(serverURL, "/")
	if profile.BotToken == "" {
		return nil, fmt.Errorf("mattermost: profile %q: bot_token is required", profile.Name)
	}
	profileName := strings.TrimSpace(profile.Name)
	if profileName == "" {
		return nil, fmt.Errorf("mattermost: profile name is required")
	}

	client := model.NewAPIv4Client(serverURL)
	client.SetToken(profile.BotToken)

	return &Adapter{
		client:       client,
		serverURL:    serverURL,
		channelType:  "mattermost:" + strings.ToLower(profileName),
		profile:      profile,
		stickyRouter: sr,
	}, nil
}

// ChannelType returns the routing key registered in chanRegistry.
// Example: "mattermost:researcher"
func (a *Adapter) ChannelType() string {
	return a.channelType
}

// setThreadRoot implements threadStorer so the WebSocket handler can record thread roots.
func (a *Adapter) setThreadRoot(channelID, rootID string) {
	a.threadRoots.Store(channelID, rootID)
}

// Start resolves the bot user, then spawns a goroutine that maintains the
// WebSocket connection and calls onMessage for each relevant incoming post.
// Returns immediately; the goroutine runs until ctx is cancelled or Stop is called.
func (a *Adapter) Start(ctx context.Context, onMessage func(context.Context, *models.Message)) error {
	me, _, err := a.client.GetMe(ctx, "")
	if err != nil {
		return fmt.Errorf("mattermost: resolve bot user: %w", err)
	}
	a.botUserID = me.Id
	a.botUsername = me.Username

	ctx, a.cancel = context.WithCancel(ctx)
	go listenWithReconnect(ctx, a.serverURL, a.profile.BotToken, a.botUserID, a.botUsername, a.channelType, a.client, a, a.stickyRouter, onMessage)
	return nil
}

// Stop cancels the WebSocket listener goroutine. Safe to call multiple times.
func (a *Adapter) Stop() {
	if a.cancel != nil {
		a.cancel()
	}
}

// maxPostSize is the maximum character count per Mattermost post.
// Mattermost's default server limit is 16 383; 4 000 keeps messages readable.
const maxPostSize = 4000

// SendMessage posts a message to the channel specified in msg.ChannelID.
// If the content exceeds maxPostSize it is split at natural language boundaries
// and sent as sequential posts. All chunks share the same thread root.
func (a *Adapter) SendMessage(ctx context.Context, msg *models.Message) error {
	rootID := ""
	if v, ok := a.threadRoots.Load(msg.ChannelID); ok {
		rootID, _ = v.(string)
	}
	for _, chunk := range splitMessage(msg.Content, maxPostSize) {
		post := &model.Post{
			ChannelId: msg.ChannelID,
			Message:   chunk,
			RootId:    rootID,
		}
		if _, _, err := a.client.CreatePost(ctx, post); err != nil {
			return fmt.Errorf("mattermost send message: %w", err)
		}
	}
	return nil
}

// splitMessage divides content into chunks no larger than maxSize characters,
// splitting at natural language boundaries (paragraphs > lines > sentences > words > hard cut).
// All operations use byte offsets but the hard-cut fallback is rune-safe.
func splitMessage(content string, maxSize int) []string {
	if utf8.RuneCountInString(content) <= maxSize {
		return []string{content}
	}
	var chunks []string
	remaining := content
	for utf8.RuneCountInString(remaining) > maxSize {
		idx := findSplitPoint(remaining, maxSize)
		chunks = append(chunks, strings.TrimSpace(remaining[:idx]))
		remaining = strings.TrimSpace(remaining[idx:])
	}
	if remaining != "" {
		chunks = append(chunks, remaining)
	}
	return chunks
}

// runeByteOffset returns the byte offset of the n-th rune in s.
func runeByteOffset(s string, n int) int {
	off := 0
	for i := 0; i < n && off < len(s); i++ {
		_, size := utf8.DecodeRuneInString(s[off:])
		off += size
	}
	return off
}

func findSplitPoint(content string, maxSize int) int {
	byteLimit := runeByteOffset(content, maxSize)
	window := content[:byteLimit]
	if i := strings.LastIndex(window, "\n\n"); i > 0 {
		return i + 2
	}
	if i := strings.LastIndex(window, "\n"); i > 0 {
		return i + 1
	}
	for _, sep := range []string{". ", "! ", "? "} {
		if i := strings.LastIndex(window, sep); i > 0 {
			return i + len(sep)
		}
	}
	if i := strings.LastIndex(window, " "); i > 0 {
		return i + 1
	}
	return byteLimit
}

// SendMedia posts a message with an optional file attachment.
// media.URL is treated as a local file path (consistent with the Telegram
// adapter). The file is read from disk, uploaded via the Mattermost API, and
// attached to the post.
func (a *Adapter) SendMedia(ctx context.Context, media *ports.Media) error {
	rootID := ""
	if v, ok := a.threadRoots.Load(media.ChatID); ok {
		rootID, _ = v.(string)
	}

	var fileIDs model.StringArray
	if media.URL != "" {
		data, err := os.ReadFile(media.URL)
		if err == nil {
			filename := media.FileName
			if filename == "" {
				filename = filepath.Base(media.URL)
			}
			if resp, _, uploadErr := a.client.UploadFile(ctx, data, media.ChatID, filename); uploadErr == nil && len(resp.FileInfos) > 0 {
				fileIDs = model.StringArray{resp.FileInfos[0].Id}
			}
		}
	}

	post := &model.Post{
		ChannelId: media.ChatID,
		Message:   media.Caption,
		RootId:    rootID,
		FileIds:   fileIDs,
	}
	if _, _, err := a.client.CreatePost(ctx, post); err != nil {
		return fmt.Errorf("mattermost send media: %w", err)
	}
	return nil
}

// SendTyping notifies the channel that the bot is typing.
func (a *Adapter) SendTyping(_ context.Context, _ string) error {
	// Typing indicators are handled by the WebSocket listener's keep-alive
	// goroutine during message processing. The SDK's UserTyping method
	// requires an active WebSocket connection which is managed internally.
	return nil
}

// HandleWebhook is a no-op; Mattermost messages arrive via WebSocket.
func (a *Adapter) HandleWebhook(_ context.Context, _ []byte) (*models.Message, error) {
	return nil, nil
}

// GetUserInfo retrieves display information for a Mattermost user by ID.
func (a *Adapter) GetUserInfo(ctx context.Context, userID string) (*ports.UserInfo, error) {
	user, _, err := a.client.GetUser(ctx, userID, "")
	if err != nil {
		return &ports.UserInfo{ID: userID, Username: userID, DisplayName: userID}, nil
	}
	displayName := user.Nickname
	if displayName == "" {
		displayName = user.Username
	}
	return &ports.UserInfo{
		ID:          userID,
		Username:    user.Username,
		DisplayName: displayName,
	}, nil
}

// React adds an emoji reaction to a post. messageID is the Mattermost post ID.
// The emoji may be wrapped in colons (":thumbsup:") or bare ("thumbsup").
func (a *Adapter) React(ctx context.Context, messageID string, emoji string) error {
	if a.botUserID == "" {
		return fmt.Errorf("mattermost react: Start() has not been called")
	}
	emoji = strings.Trim(emoji, ":")
	reaction := &model.Reaction{
		UserId:    a.botUserID,
		PostId:    messageID,
		EmojiName: emoji,
	}
	if _, _, err := a.client.SaveReaction(ctx, reaction); err != nil {
		return fmt.Errorf("mattermost react: %w", err)
	}
	return nil
}

// GetCapabilities returns capability flags for the Mattermost channel.
func (a *Adapter) GetCapabilities() ports.ChannelCapabilities {
	return ports.ChannelCapabilities{
		HasVoiceMessage: false,
		HasCallStream:   false,
		HasTextStream:   true,
		HasMediaSupport: true,
	}
}

// ConvertAudioForPlatform returns the audio data unchanged; Mattermost has no
// special audio format requirements.
func (a *Adapter) ConvertAudioForPlatform(_ context.Context, audioData []byte, format string) ([]byte, string, error) {
	return audioData, format, nil
}

var _ ports.MessagingPort = (*Adapter)(nil)
