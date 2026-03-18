package mattermost

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/neirth/openlobster/internal/domain/models"
	"github.com/neirth/openlobster/internal/infrastructure/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// noopThreadStore satisfies threadStorer for tests that don't need threading.
type noopThreadStore struct{}

func (n *noopThreadStore) set(_, _ string) {}

// captureThreadStore records the last set call.
type captureThreadStore struct {
	channelID string
	rootID    string
}

func (c *captureThreadStore) set(channelID, rootID string) {
	c.channelID = channelID
	c.rootID = rootID
}

// newTestClient creates a Client pointing to the given httptest.Server.
func newTestClient(srv *httptest.Server) *Client {
	return &Client{
		serverURL:  srv.URL,
		token:      "test-token",
		httpClient: srv.Client(),
	}
}

// makePostedEvent builds a minimal "posted" wsEvent for testing.
func makePostedEvent(post mmPost, channelType string) wsEvent {
	postJSON, _ := json.Marshal(post)
	return wsEvent{
		Event: "posted",
		Data: map[string]interface{}{
			"post":         string(postJSON),
			"channel_type": channelType,
		},
	}
}

// testProfile builds a minimal MattermostBotProfile for tests.
func testProfile(name, token string) config.MattermostBotProfile {
	return config.MattermostBotProfile{Name: name, BotToken: token}
}

// --------------------------------------------------------------------------
// handlePostedEvent — event parsing, self-echo filter, @mention filter
// --------------------------------------------------------------------------

func TestHandlePostedEvent_SelfEcho(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	defer srv.Close()

	called := false
	onMsg := func(_ context.Context, _ *models.Message) { called = true }

	post := mmPost{ID: "p1", ChannelID: "c1", UserID: "bot-id", Message: "@mybot hello"}
	evt := makePostedEvent(post, "O")

	handlePostedEvent(context.Background(), evt, "bot-id", "mybot", "mattermost:test",
		newTestClient(srv), &noopThreadStore{}, nil, onMsg)

	assert.False(t, called, "self-posted messages must not trigger onMessage")
}

func TestHandlePostedEvent_SystemMessage(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	defer srv.Close()

	called := false
	onMsg := func(_ context.Context, _ *models.Message) { called = true }

	post := mmPost{ID: "p1", ChannelID: "c1", UserID: "user-1", Message: "@mybot hello", Type: "system_join_channel"}
	evt := makePostedEvent(post, "O")

	handlePostedEvent(context.Background(), evt, "bot-id", "mybot", "mattermost:test",
		newTestClient(srv), &noopThreadStore{}, nil, onMsg)

	assert.False(t, called, "system messages must be ignored")
}

func TestHandlePostedEvent_MentionFilter_WithMention(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/api/v4/users/") {
			json.NewEncoder(w).Encode(mmUser{ID: "user-1", Username: "alice", Nickname: "Alice"})
			return
		}
		if r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/typing") {
			w.WriteHeader(http.StatusOK)
			return
		}
		if strings.HasPrefix(r.URL.Path, "/api/v4/channels/") {
			json.NewEncoder(w).Encode(mmChannel{ID: "c1", Type: "O", Name: "general", DisplayName: "General"})
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()

	var received *models.Message
	onMsg := func(_ context.Context, msg *models.Message) { received = msg }

	post := mmPost{ID: "p1", ChannelID: "c1", UserID: "user-1", Message: "@mybot what time is it?"}
	evt := makePostedEvent(post, "O") // non-DM channel

	handlePostedEvent(context.Background(), evt, "bot-id", "mybot", "mattermost:test",
		newTestClient(srv), &noopThreadStore{}, nil, onMsg)

	require.NotNil(t, received)
	assert.Equal(t, "c1", received.ChannelID)
	assert.True(t, received.IsMentioned)
	assert.True(t, received.IsGroup)
	assert.Equal(t, "what time is it?", received.Content, "bot mention should be stripped from content")
	assert.Equal(t, "mattermost:test", received.Metadata["channel_type"])
}

func TestHandlePostedEvent_MentionFilter_WithoutMention(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	defer srv.Close()

	called := false
	onMsg := func(_ context.Context, _ *models.Message) { called = true }

	post := mmPost{ID: "p1", ChannelID: "c1", UserID: "user-1", Message: "hello world"}
	evt := makePostedEvent(post, "O") // public channel, no @mention

	handlePostedEvent(context.Background(), evt, "bot-id", "mybot", "mattermost:test",
		newTestClient(srv), &noopThreadStore{}, nil, onMsg)

	assert.False(t, called, "messages without @mention in group channels must be ignored")
}

func TestHandlePostedEvent_DirectMessage_NoMentionNeeded(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/api/v4/users/") {
			json.NewEncoder(w).Encode(mmUser{ID: "user-1", Username: "bob"})
			return
		}
		if r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/typing") {
			w.WriteHeader(http.StatusOK)
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()

	var received *models.Message
	onMsg := func(_ context.Context, msg *models.Message) { received = msg }

	post := mmPost{ID: "p2", ChannelID: "dm-1", UserID: "user-1", Message: "hey there"}
	evt := makePostedEvent(post, "D") // DM channel

	handlePostedEvent(context.Background(), evt, "bot-id", "mybot", "mattermost:test",
		newTestClient(srv), &noopThreadStore{}, nil, onMsg)

	require.NotNil(t, received)
	assert.False(t, received.IsGroup)
	assert.Equal(t, "hey there", received.Content)
}

func TestHandlePostedEvent_ThreadRootRecorded_ExistingThread(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(mmUser{ID: "user-1", Username: "charlie"})
	}))
	defer srv.Close()

	ts := &captureThreadStore{}
	onMsg := func(_ context.Context, _ *models.Message) {}

	// Incoming message already inside a thread (RootID set).
	post := mmPost{ID: "p3", ChannelID: "dm-2", UserID: "user-1", Message: "hi", RootID: "root-abc"}
	evt := makePostedEvent(post, "D")

	handlePostedEvent(context.Background(), evt, "bot-id", "mybot", "mattermost:test",
		newTestClient(srv), ts, nil, onMsg)

	assert.Equal(t, "dm-2", ts.channelID)
	assert.Equal(t, "root-abc", ts.rootID, "existing RootID should be used unchanged")
}

func TestHandlePostedEvent_NewTopLevelPost_InlineReply(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(mmUser{ID: "user-1", Username: "dave"})
	}))
	defer srv.Close()

	ts := &captureThreadStore{}
	onMsg := func(_ context.Context, _ *models.Message) {}

	// Incoming message with no RootID — store empty so reply is inline (not threaded).
	post := mmPost{ID: "top-post", ChannelID: "dm-3", UserID: "user-1", Message: "hello"}
	evt := makePostedEvent(post, "D")

	handlePostedEvent(context.Background(), evt, "bot-id", "mybot", "mattermost:test",
		newTestClient(srv), ts, nil, onMsg)

	assert.Equal(t, "", ts.rootID, "top-level post should store empty root so reply is inline, not threaded")
}

// --------------------------------------------------------------------------
// SendMessage — httptest server verifies correct POST /api/v4/posts body
// --------------------------------------------------------------------------

func TestSendMessage(t *testing.T) {
	var capturedBody map[string]interface{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "POST", r.Method)
		assert.Equal(t, "/api/v4/posts", r.URL.Path)
		assert.Equal(t, "Bearer test-token", r.Header.Get("Authorization"))

		body, _ := io.ReadAll(r.Body)
		json.Unmarshal(body, &capturedBody)

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(mmPost{ID: "new-post", ChannelID: "c1"})
	}))
	defer srv.Close()

	adapter := &Adapter{
		client:      newTestClient(srv),
		channelType: "mattermost:test",
		profile:     testProfile("test", "test-token"),
	}

	msg := models.NewMessage("c1", "hello world")
	err := adapter.SendMessage(context.Background(), msg)

	require.NoError(t, err)
	assert.Equal(t, "c1", capturedBody["channel_id"])
	assert.Equal(t, "hello world", capturedBody["message"])
	// No thread root stored for this channel, so root_id should not appear.
	_, hasRoot := capturedBody["root_id"]
	assert.False(t, hasRoot)
}

func TestSendMessage_InThread(t *testing.T) {
	var capturedBody map[string]interface{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		json.Unmarshal(body, &capturedBody)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(mmPost{ID: "reply-post", ChannelID: "c1"})
	}))
	defer srv.Close()

	adapter := &Adapter{
		client:      newTestClient(srv),
		channelType: "mattermost:test",
		profile:     testProfile("test", "test-token"),
	}
	// Simulate a previously stored thread root.
	adapter.threadRoots.Store("c1", "root-xyz")

	msg := models.NewMessage("c1", "follow-up reply")
	err := adapter.SendMessage(context.Background(), msg)

	require.NoError(t, err)
	assert.Equal(t, "root-xyz", capturedBody["root_id"])
}

// --------------------------------------------------------------------------
// React — httptest server verifies correct POST /api/v4/reactions body
// --------------------------------------------------------------------------

func TestReact(t *testing.T) {
	var capturedBody map[string]string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "POST", r.Method)
		assert.Equal(t, "/api/v4/reactions", r.URL.Path)

		body, _ := io.ReadAll(r.Body)
		json.Unmarshal(body, &capturedBody)

		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintln(w, `{}`)
	}))
	defer srv.Close()

	adapter := &Adapter{
		client:      newTestClient(srv),
		channelType: "mattermost:test",
		botUserID:   "bot-user-id",
		profile:     testProfile("test", "test-token"),
	}

	err := adapter.React(context.Background(), "post-123", ":thumbsup:")
	require.NoError(t, err)
	assert.Equal(t, "bot-user-id", capturedBody["user_id"])
	assert.Equal(t, "post-123", capturedBody["post_id"])
	assert.Equal(t, "thumbsup", capturedBody["emoji_name"], "surrounding colons should be stripped")
}

func TestReact_BeforeStart(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	defer srv.Close()

	adapter := &Adapter{
		client:      newTestClient(srv),
		channelType: "mattermost:test",
		botUserID:   "", // Start() not called yet
		profile:     testProfile("test", "test-token"),
	}

	err := adapter.React(context.Background(), "post-1", "thumbsup")
	assert.Error(t, err)
}

// --------------------------------------------------------------------------
// NewAdapter — construction and URL conversion
// --------------------------------------------------------------------------

func TestNewAdapter_MissingServerURL(t *testing.T) {
	_, err := NewAdapter("", testProfile("test", "token"), nil)
	assert.Error(t, err)
}

func TestNewAdapter_MissingToken(t *testing.T) {
	_, err := NewAdapter("https://chat.example.com", testProfile("test", ""), nil)
	assert.Error(t, err)
}

func TestNewAdapter_OK(t *testing.T) {
	a, err := NewAdapter("https://chat.example.com", testProfile("Researcher", "xoxb-token"), nil)
	require.NoError(t, err)
	assert.Equal(t, "mattermost:researcher", a.ChannelType())
}

func TestNewAdapter_WSURLConversion(t *testing.T) {
	cases := []struct{ in, out string }{
		{"https://chat.example.com", "wss://chat.example.com/api/v4/websocket"},
		{"http://localhost:8065", "ws://localhost:8065/api/v4/websocket"},
	}
	for _, tc := range cases {
		a, err := NewAdapter(tc.in, testProfile("t", "token"), nil)
		require.NoError(t, err)
		assert.Equal(t, tc.out, a.wsURL)
	}
}

// --------------------------------------------------------------------------
// SendTyping — verifies POST /api/v4/channels/<id>/typing is called
// --------------------------------------------------------------------------

func TestSendTyping(t *testing.T) {
	var capturedReqs []struct {
		path string
		body map[string]interface{}
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "POST", r.Method)
		var body map[string]interface{}
		bodyBytes, _ := io.ReadAll(r.Body)
		json.Unmarshal(bodyBytes, &body)
		capturedReqs = append(capturedReqs, struct {
			path string
			body map[string]interface{}
		}{r.URL.Path, body})
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	adapter := &Adapter{
		client:      newTestClient(srv),
		channelType: "mattermost:test",
		botUserID:   "bot-user-id",
		profile:     testProfile("test", "test-token"),
	}

	err := adapter.SendTyping(context.Background(), "channel-1")
	require.NoError(t, err)
	require.Len(t, capturedReqs, 1)
	assert.Equal(t, "/api/v4/users/bot-user-id/typing", capturedReqs[0].path)
	assert.Equal(t, "channel-1", capturedReqs[0].body["channel_id"])
}

// --------------------------------------------------------------------------
// SendMessage — message chunking
// --------------------------------------------------------------------------

func TestSendMessage_LongMessage_Chunked(t *testing.T) {
	var postMessages []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v4/posts" {
			http.NotFound(w, r)
			return
		}
		var body map[string]interface{}
		bodyBytes, _ := io.ReadAll(r.Body)
		json.Unmarshal(bodyBytes, &body)
		postMessages = append(postMessages, body["message"].(string))
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(mmPost{ID: fmt.Sprintf("post-%d", len(postMessages)), ChannelID: "c1"})
	}))
	defer srv.Close()

	adapter := &Adapter{
		client:      newTestClient(srv),
		channelType: "mattermost:test",
		profile:     testProfile("test", "test-token"),
	}

	// Three paragraphs of 2000 chars each — total 6004 chars, well above maxPostSize.
	para := strings.Repeat("x", 2000)
	longContent := para + "\n\n" + para + "\n\n" + para
	msg := models.NewMessage("c1", longContent)
	err := adapter.SendMessage(context.Background(), msg)

	require.NoError(t, err)
	assert.Greater(t, len(postMessages), 1, "long message should produce multiple posts")
	for _, m := range postMessages {
		assert.LessOrEqual(t, len(m), maxPostSize, "each chunk must be within maxPostSize")
	}
}

func TestSendMessage_LongMessage_ThreadRootPreserved(t *testing.T) {
	var capturedRoots []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]interface{}
		bodyBytes, _ := io.ReadAll(r.Body)
		json.Unmarshal(bodyBytes, &body)
		rootID, _ := body["root_id"].(string)
		capturedRoots = append(capturedRoots, rootID)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(mmPost{ID: fmt.Sprintf("post-%d", len(capturedRoots)), ChannelID: "c1"})
	}))
	defer srv.Close()

	adapter := &Adapter{
		client:      newTestClient(srv),
		channelType: "mattermost:test",
		profile:     testProfile("test", "test-token"),
	}
	adapter.threadRoots.Store("c1", "root-xyz")

	para := strings.Repeat("y", 2000)
	longContent := para + "\n\n" + para + "\n\n" + para
	msg := models.NewMessage("c1", longContent)
	err := adapter.SendMessage(context.Background(), msg)

	require.NoError(t, err)
	require.Greater(t, len(capturedRoots), 1, "expected multiple chunks")
	for _, id := range capturedRoots {
		assert.Equal(t, "root-xyz", id, "all chunks must use the same thread root")
	}
}

// --------------------------------------------------------------------------
// splitMessage / findSplitPoint — unit tests
// --------------------------------------------------------------------------

func TestSplitMessage_ShortContent(t *testing.T) {
	result := splitMessage("hello", 100)
	assert.Equal(t, []string{"hello"}, result)
}

func TestSplitMessage_ExactSize(t *testing.T) {
	content := strings.Repeat("a", 100)
	result := splitMessage(content, 100)
	assert.Equal(t, []string{content}, result)
}

func TestSplitMessage_ParagraphBoundary(t *testing.T) {
	a := strings.Repeat("a", 60)
	b := strings.Repeat("b", 60)
	result := splitMessage(a+"\n\n"+b, 100)
	require.Len(t, result, 2)
	assert.Equal(t, a, result[0])
	assert.Equal(t, b, result[1])
}

func TestSplitMessage_LineBoundary(t *testing.T) {
	a := strings.Repeat("a", 60)
	b := strings.Repeat("b", 60)
	result := splitMessage(a+"\n"+b, 100)
	require.Len(t, result, 2)
	assert.Equal(t, a, result[0])
	assert.Equal(t, b, result[1])
}

func TestSplitMessage_SentenceBoundary(t *testing.T) {
	// 60 a's + ". " + 60 b's = 122 chars; split window is 100, ". " is at index 60.
	a := strings.Repeat("a", 60)
	b := strings.Repeat("b", 60)
	result := splitMessage(a+". "+b, 100)
	require.Len(t, result, 2)
	assert.Equal(t, a+".", result[0])
	assert.Equal(t, b, result[1])
}

func TestSplitMessage_HardCut(t *testing.T) {
	// No whitespace anywhere — must hard-cut at maxSize.
	content := strings.Repeat("a", 150)
	result := splitMessage(content, 100)
	require.Len(t, result, 2)
	assert.Equal(t, strings.Repeat("a", 100), result[0])
	assert.Equal(t, strings.Repeat("a", 50), result[1])
}
