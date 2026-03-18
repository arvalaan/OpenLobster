// Package mattermost provides a Mattermost messaging adapter for OpenLobster.
//
// The adapter uses the Mattermost WebSocket API for receiving messages and the
// REST API v4 for sending. No external Mattermost SDK is required; all
// communication uses the standard library (net/http) and gorilla/websocket
// (already present as a transitive dependency).
//
// Required Mattermost bot permissions:
//   - Read posts in all relevant channels
//   - Create posts
//   - Add reactions
//   - Read user info
//
// # License
// See LICENSE in the root of the repository.
package mattermost

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"strings"
)

// mmUser is a subset of the Mattermost User object.
type mmUser struct {
	ID       string `json:"id"`
	Username string `json:"username"`
	Nickname string `json:"nickname"`
}

// mmPost is a subset of the Mattermost Post object.
type mmPost struct {
	ID        string   `json:"id"`
	ChannelID string   `json:"channel_id"`
	UserID    string   `json:"user_id"`
	Message   string   `json:"message"`
	RootID    string   `json:"root_id"`
	FileIDs   []string `json:"file_ids,omitempty"`
	Type      string   `json:"type"`
	CreateAt  int64    `json:"create_at"`
}

// mmChannel is a subset of the Mattermost Channel object.
type mmChannel struct {
	ID          string `json:"id"`
	Type        string `json:"type"` // "D" = DM, "O" = open, "P" = private, "G" = group
	Name        string `json:"name"`
	DisplayName string `json:"display_name"`
}

// Client wraps the Mattermost REST API v4 with the methods needed by the adapter.
type Client struct {
	serverURL  string
	token      string
	httpClient *http.Client
}

// newClient creates a Client for the given server URL and bot token.
func newClient(serverURL, token string) *Client {
	return &Client{
		serverURL:  strings.TrimSuffix(serverURL, "/"),
		token:      token,
		httpClient: &http.Client{},
	}
}

// doRequest executes an authenticated JSON request. The caller is responsible
// for closing resp.Body on success. On HTTP ≥ 400 the body is consumed and an
// error is returned.
func (c *Client) doRequest(ctx context.Context, method, path string, body interface{}) (*http.Response, error) {
	var bodyReader io.Reader
	var contentType string
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("marshal body: %w", err)
		}
		bodyReader = bytes.NewReader(data)
		contentType = "application/json"
	}
	req, err := http.NewRequestWithContext(ctx, method, c.serverURL+"/api/v4"+path, bodyReader)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 400 {
		defer resp.Body.Close()
		data, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(data)))
	}
	return resp, nil
}

// GetMe returns the bot account's user information.
func (c *Client) GetMe(ctx context.Context) (*mmUser, error) {
	resp, err := c.doRequest(ctx, "GET", "/users/me", nil)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var u mmUser
	return &u, json.NewDecoder(resp.Body).Decode(&u)
}

// GetUser returns the user with the given ID.
func (c *Client) GetUser(ctx context.Context, userID string) (*mmUser, error) {
	resp, err := c.doRequest(ctx, "GET", "/users/"+userID, nil)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var u mmUser
	return &u, json.NewDecoder(resp.Body).Decode(&u)
}

// CreatePost creates a new post in the given channel.
// rootID should be set to the thread root post ID to reply in a thread.
// fileIDs is a list of previously uploaded file IDs to attach.
func (c *Client) CreatePost(ctx context.Context, channelID, message, rootID string, fileIDs []string) (*mmPost, error) {
	body := map[string]interface{}{
		"channel_id": channelID,
		"message":    message,
	}
	if rootID != "" {
		body["root_id"] = rootID
	}
	if len(fileIDs) > 0 {
		body["file_ids"] = fileIDs
	}
	resp, err := c.doRequest(ctx, "POST", "/posts", body)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var p mmPost
	return &p, json.NewDecoder(resp.Body).Decode(&p)
}

// UploadFile uploads a file to a channel and returns the file ID.
func (c *Client) UploadFile(ctx context.Context, channelID string, data []byte, filename string) (string, error) {
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	_ = w.WriteField("channel_id", channelID)
	part, err := w.CreateFormFile("files", filename)
	if err != nil {
		return "", err
	}
	if _, err := part.Write(data); err != nil {
		return "", err
	}
	_ = w.Close()

	req, err := http.NewRequestWithContext(ctx, "POST", c.serverURL+"/api/v4/files", &buf)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Content-Type", w.FormDataContentType())

	httpResp, err := c.httpClient.Do(req)
	if err != nil {
		return "", err
	}
	defer httpResp.Body.Close()
	if httpResp.StatusCode >= 400 {
		body, _ := io.ReadAll(httpResp.Body)
		return "", fmt.Errorf("upload file HTTP %d: %s", httpResp.StatusCode, string(body))
	}

	var uploadResp struct {
		FileInfos []struct {
			ID string `json:"id"`
		} `json:"file_infos"`
	}
	if err := json.NewDecoder(httpResp.Body).Decode(&uploadResp); err != nil {
		return "", err
	}
	if len(uploadResp.FileInfos) == 0 {
		return "", fmt.Errorf("upload returned no file infos")
	}
	return uploadResp.FileInfos[0].ID, nil
}

// PostTyping notifies Mattermost users that the bot is typing in channelID.
// The indicator is visible for ~5 s; call repeatedly to maintain it.
// userID must be the bot's own user ID (obtained via GetMe).
func (c *Client) PostTyping(ctx context.Context, userID, channelID string) error {
	resp, err := c.doRequest(ctx, "POST", "/users/"+userID+"/typing", map[string]interface{}{"channel_id": channelID})
	if err != nil {
		return err
	}
	resp.Body.Close()
	return nil
}

// AddReaction adds an emoji reaction to a post.
func (c *Client) AddReaction(ctx context.Context, userID, postID, emojiName string) error {
	body := map[string]string{
		"user_id":    userID,
		"post_id":    postID,
		"emoji_name": emojiName,
	}
	resp, err := c.doRequest(ctx, "POST", "/reactions", body)
	if err != nil {
		return err
	}
	resp.Body.Close()
	return nil
}

// CreateDirectChannel creates (or retrieves an existing) DM channel between two users.
func (c *Client) CreateDirectChannel(ctx context.Context, userID1, userID2 string) (*mmChannel, error) {
	resp, err := c.doRequest(ctx, "POST", "/channels/direct", []string{userID1, userID2})
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var ch mmChannel
	return &ch, json.NewDecoder(resp.Body).Decode(&ch)
}

// GetChannel returns the channel with the given ID.
func (c *Client) GetChannel(ctx context.Context, channelID string) (*mmChannel, error) {
	resp, err := c.doRequest(ctx, "GET", "/channels/"+channelID, nil)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var ch mmChannel
	return &ch, json.NewDecoder(resp.Body).Decode(&ch)
}
