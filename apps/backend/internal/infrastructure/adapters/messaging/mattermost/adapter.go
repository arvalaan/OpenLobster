package mattermost

import (
	"context"
	"fmt"
	"strings"
	"sync"

	"github.com/neirth/openlobster/internal/domain/models"
	"github.com/neirth/openlobster/internal/domain/ports"
	"github.com/neirth/openlobster/internal/infrastructure/config"
)

// Adapter implements ports.MessagingPort for Mattermost using the REST API v4
// and WebSocket event stream. One Adapter corresponds to one bot profile
// (one Mattermost bot account).
//
// Mention-based routing: the adapter only forwards messages to onMessage when
// the bot is @mentioned (by username) or the conversation is a direct message.
// Mattermost therefore acts as the routing layer for multi-profile deployments.
type Adapter struct {
	client      *Client
	wsURL       string
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
	profileName := profile.Name
	if profileName == "" {
		profileName = "default"
	}
	return &Adapter{
		client:       newClient(serverURL, profile.BotToken),
		wsURL:        buildWSURL(serverURL),
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

// set implements threadStorer so the WebSocket handler can record thread roots.
func (a *Adapter) set(channelID, rootID string) {
	a.threadRoots.Store(channelID, rootID)
}

// Start resolves the bot user, then spawns a goroutine that maintains the
// WebSocket connection and calls onMessage for each relevant incoming post.
// Returns immediately; the goroutine runs until ctx is cancelled.
func (a *Adapter) Start(ctx context.Context, onMessage func(context.Context, *models.Message)) error {
	me, err := a.client.GetMe(ctx)
	if err != nil {
		return fmt.Errorf("mattermost: resolve bot user: %w", err)
	}
	a.botUserID = me.ID
	a.botUsername = me.Username

	go listenWithReconnect(ctx, a.wsURL, a.profile.BotToken, a.botUserID, a.botUsername, a.channelType, a.client, a, a.stickyRouter, onMessage)
	return nil
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
		if _, err := a.client.CreatePost(ctx, msg.ChannelID, chunk, rootID, nil); err != nil {
			return fmt.Errorf("mattermost send message: %w", err)
		}
	}
	return nil
}

// splitMessage divides content into chunks no larger than maxSize, splitting
// at natural language boundaries (paragraphs > lines > sentences > words > hard cut).
func splitMessage(content string, maxSize int) []string {
	if len(content) <= maxSize {
		return []string{content}
	}
	var chunks []string
	remaining := content
	for len(remaining) > maxSize {
		idx := findSplitPoint(remaining, maxSize)
		chunks = append(chunks, strings.TrimSpace(remaining[:idx]))
		remaining = strings.TrimSpace(remaining[idx:])
	}
	if remaining != "" {
		chunks = append(chunks, remaining)
	}
	return chunks
}

func findSplitPoint(content string, maxSize int) int {
	window := content[:maxSize]
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
	return maxSize
}

// SendMedia posts a message with an optional file attachment.
// If media.URL is set but no binary data is available, the URL is appended to
// the caption so users can still access the resource.
func (a *Adapter) SendMedia(ctx context.Context, media *ports.Media) error {
	text := media.Caption
	if media.URL != "" && text == "" {
		text = media.URL
	} else if media.URL != "" {
		text = media.Caption + "\n" + media.URL
	}

	rootID := ""
	if v, ok := a.threadRoots.Load(media.ChatID); ok {
		rootID, _ = v.(string)
	}
	_, err := a.client.CreatePost(ctx, media.ChatID, text, rootID, nil)
	if err != nil {
		return fmt.Errorf("mattermost send media: %w", err)
	}
	return nil
}

// SendTyping notifies the channel that the bot is typing.
func (a *Adapter) SendTyping(ctx context.Context, channelID string) error {
	return a.client.PostTyping(ctx, a.botUserID, channelID)
}

// HandleWebhook is a no-op; Mattermost messages arrive via WebSocket.
func (a *Adapter) HandleWebhook(_ context.Context, _ []byte) (*models.Message, error) {
	return nil, nil
}

// GetUserInfo retrieves display information for a Mattermost user by ID.
func (a *Adapter) GetUserInfo(ctx context.Context, userID string) (*ports.UserInfo, error) {
	user, err := a.client.GetUser(ctx, userID)
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
	return a.client.AddReaction(ctx, a.botUserID, messageID, emoji)
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
