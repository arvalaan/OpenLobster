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

	mmmodel "github.com/mattermost/mattermost/server/public/model"
	"github.com/neirth/openlobster/internal/domain/models"
	"github.com/neirth/openlobster/internal/infrastructure/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newTestAPIClient creates a model.Client4 pointing to the given httptest.Server.
func newTestAPIClient(srv *httptest.Server) *mmmodel.Client4 {
	client := mmmodel.NewAPIv4Client(srv.URL)
	client.SetToken("test-token")
	return client
}

// newTestAdapter creates a minimal Adapter backed by the given httptest.Server.
func newTestAdapter(srv *httptest.Server, name string) *Adapter {
	return &Adapter{
		client:      newTestAPIClient(srv),
		serverURL:   srv.URL,
		channelType: "mattermost:" + strings.ToLower(name),
		profile:     testProfile(name, "test-token"),
	}
}

// makePostedEvent builds a minimal "posted" WebSocketEvent for testing.
func makePostedEvent(post mmmodel.Post, channelType string) *mmmodel.WebSocketEvent {
	postJSON, _ := json.Marshal(post)
	evt := mmmodel.NewWebSocketEvent(mmmodel.WebsocketEventPosted, "", post.ChannelId, "", nil, "")
	evt = evt.SetData(map[string]any{
		"post":         string(postJSON),
		"channel_type": channelType,
	})
	return evt
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

	post := mmmodel.Post{Id: "p1", ChannelId: "c1", UserId: "bot-id", Message: "@mybot hello"}
	evt := makePostedEvent(post, "O")

	adapter := newTestAdapter(srv, "test")
	handlePostedEvent(context.Background(), evt, "bot-id", "mybot", "mattermost:test",
		adapter.client, adapter, nil, onMsg)

	assert.False(t, called, "self-posted messages must not trigger onMessage")
}

func TestHandlePostedEvent_SystemMessage(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	defer srv.Close()

	called := false
	onMsg := func(_ context.Context, _ *models.Message) { called = true }

	post := mmmodel.Post{Id: "p1", ChannelId: "c1", UserId: "user-1", Message: "@mybot hello", Type: "system_join_channel"}
	evt := makePostedEvent(post, "O")

	adapter := newTestAdapter(srv, "test")
	handlePostedEvent(context.Background(), evt, "bot-id", "mybot", "mattermost:test",
		adapter.client, adapter, nil, onMsg)

	assert.False(t, called, "system messages must be ignored")
}

func TestHandlePostedEvent_MentionFilter_WithMention(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(r.URL.Path, "/users/") && !strings.Contains(r.URL.Path, "/typing") {
			json.NewEncoder(w).Encode(mmmodel.User{Id: "user-1", Username: "alice", Nickname: "Alice"})
			return
		}
		if strings.Contains(r.URL.Path, "/typing") {
			w.WriteHeader(http.StatusOK)
			return
		}
		if strings.Contains(r.URL.Path, "/channels/") {
			json.NewEncoder(w).Encode(mmmodel.Channel{Id: "c1", Type: "O", Name: "general", DisplayName: "General"})
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()

	var received *models.Message
	onMsg := func(_ context.Context, msg *models.Message) { received = msg }

	post := mmmodel.Post{Id: "p1", ChannelId: "c1", UserId: "user-1", Message: "@mybot what time is it?"}
	evt := makePostedEvent(post, "O")

	adapter := newTestAdapter(srv, "test")
	handlePostedEvent(context.Background(), evt, "bot-id", "mybot", "mattermost:test",
		adapter.client, adapter, nil, onMsg)

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

	post := mmmodel.Post{Id: "p1", ChannelId: "c1", UserId: "user-1", Message: "hello world"}
	evt := makePostedEvent(post, "O")

	adapter := newTestAdapter(srv, "test")
	handlePostedEvent(context.Background(), evt, "bot-id", "mybot", "mattermost:test",
		adapter.client, adapter, nil, onMsg)

	assert.False(t, called, "messages without @mention in group channels must be ignored")
}

func TestHandlePostedEvent_DirectMessage_NoMentionNeeded(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(r.URL.Path, "/users/") && !strings.Contains(r.URL.Path, "/typing") {
			json.NewEncoder(w).Encode(mmmodel.User{Id: "user-1", Username: "bob"})
			return
		}
		if strings.Contains(r.URL.Path, "/typing") {
			w.WriteHeader(http.StatusOK)
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()

	var received *models.Message
	onMsg := func(_ context.Context, msg *models.Message) { received = msg }

	post := mmmodel.Post{Id: "p2", ChannelId: "dm-1", UserId: "user-1", Message: "hey there"}
	evt := makePostedEvent(post, "D")

	adapter := newTestAdapter(srv, "test")
	handlePostedEvent(context.Background(), evt, "bot-id", "mybot", "mattermost:test",
		adapter.client, adapter, nil, onMsg)

	require.NotNil(t, received)
	assert.False(t, received.IsGroup)
	assert.Equal(t, "hey there", received.Content)
}

func TestHandlePostedEvent_ThreadRootRecorded(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(r.URL.Path, "/users/") && !strings.Contains(r.URL.Path, "/typing") {
			json.NewEncoder(w).Encode(mmmodel.User{Id: "user-1", Username: "charlie"})
			return
		}
		if strings.Contains(r.URL.Path, "/typing") {
			w.WriteHeader(http.StatusOK)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	adapter := newTestAdapter(srv, "test")
	onMsg := func(_ context.Context, _ *models.Message) {}

	post := mmmodel.Post{Id: "p3", ChannelId: "dm-2", UserId: "user-1", Message: "hi", RootId: "root-abc"}
	evt := makePostedEvent(post, "D")

	handlePostedEvent(context.Background(), evt, "bot-id", "mybot", "mattermost:test",
		adapter.client, adapter, nil, onMsg)

	v, ok := adapter.threadRoots.Load("dm-2")
	require.True(t, ok)
	assert.Equal(t, "root-abc", v.(string), "existing RootID should be recorded")
}

func TestHandlePostedEvent_TopLevelPost_InlineReply(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(r.URL.Path, "/users/") && !strings.Contains(r.URL.Path, "/typing") {
			json.NewEncoder(w).Encode(mmmodel.User{Id: "user-1", Username: "dave"})
			return
		}
		if strings.Contains(r.URL.Path, "/typing") {
			w.WriteHeader(http.StatusOK)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	adapter := newTestAdapter(srv, "test")
	onMsg := func(_ context.Context, _ *models.Message) {}

	post := mmmodel.Post{Id: "top-post", ChannelId: "dm-3", UserId: "user-1", Message: "hello"}
	evt := makePostedEvent(post, "D")

	handlePostedEvent(context.Background(), evt, "bot-id", "mybot", "mattermost:test",
		adapter.client, adapter, nil, onMsg)

	v, ok := adapter.threadRoots.Load("dm-3")
	require.True(t, ok)
	assert.Equal(t, "", v.(string), "top-level post should store empty root so reply is inline")
}

// --------------------------------------------------------------------------
// SendMessage — httptest server verifies correct POST /api/v4/posts body
// --------------------------------------------------------------------------

func TestSendMessage(t *testing.T) {
	var capturedBody map[string]interface{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v4/posts" && r.Method == "POST" {
			body, _ := io.ReadAll(r.Body)
			json.Unmarshal(body, &capturedBody)
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(mmmodel.Post{Id: "new-post", ChannelId: "c1"})
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()

	adapter := newTestAdapter(srv, "test")
	msg := models.NewMessage("c1", "hello world")
	err := adapter.SendMessage(context.Background(), msg)

	require.NoError(t, err)
	assert.Equal(t, "c1", capturedBody["channel_id"])
	assert.Equal(t, "hello world", capturedBody["message"])
}

func TestSendMessage_InThread(t *testing.T) {
	var capturedBody map[string]interface{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v4/posts" && r.Method == "POST" {
			body, _ := io.ReadAll(r.Body)
			json.Unmarshal(body, &capturedBody)
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(mmmodel.Post{Id: "reply-post", ChannelId: "c1"})
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()

	adapter := newTestAdapter(srv, "test")
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
	var capturedBody map[string]interface{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v4/reactions" && r.Method == "POST" {
			body, _ := io.ReadAll(r.Body)
			json.Unmarshal(body, &capturedBody)
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintln(w, `{}`)
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()

	adapter := newTestAdapter(srv, "test")
	adapter.botUserID = "bot-user-id"

	err := adapter.React(context.Background(), "post-123", ":thumbsup:")
	require.NoError(t, err)
	assert.Equal(t, "bot-user-id", capturedBody["user_id"])
	assert.Equal(t, "post-123", capturedBody["post_id"])
	assert.Equal(t, "thumbsup", capturedBody["emoji_name"], "surrounding colons should be stripped")
}

func TestReact_BeforeStart(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	defer srv.Close()

	adapter := newTestAdapter(srv, "test")
	adapter.botUserID = "" // Start() not called

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

func TestNewAdapter_BlankName(t *testing.T) {
	_, err := NewAdapter("https://chat.example.com", testProfile("", "token"), nil)
	assert.Error(t, err)
}

func TestNewAdapter_OK(t *testing.T) {
	a, err := NewAdapter("https://chat.example.com", testProfile("Researcher", "xoxb-token"), nil)
	require.NoError(t, err)
	assert.Equal(t, "mattermost:researcher", a.ChannelType())
}

func TestNewAdapter_WSURLConversion(t *testing.T) {
	cases := []struct{ in, out string }{
		{"https://chat.example.com", "wss://chat.example.com"},
		{"http://localhost:8065", "ws://localhost:8065"},
	}
	for _, tc := range cases {
		assert.Equal(t, tc.out, buildWSURL(tc.in))
	}
}

// --------------------------------------------------------------------------
// SendMessage — message chunking
// --------------------------------------------------------------------------

func TestSendMessage_LongMessage_Chunked(t *testing.T) {
	var postMessages []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v4/posts" && r.Method == "POST" {
			var body map[string]interface{}
			bodyBytes, _ := io.ReadAll(r.Body)
			json.Unmarshal(bodyBytes, &body)
			postMessages = append(postMessages, body["message"].(string))
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(mmmodel.Post{Id: fmt.Sprintf("post-%d", len(postMessages)), ChannelId: "c1"})
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()

	adapter := newTestAdapter(srv, "test")

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
		if r.URL.Path == "/api/v4/posts" && r.Method == "POST" {
			var body map[string]interface{}
			bodyBytes, _ := io.ReadAll(r.Body)
			json.Unmarshal(bodyBytes, &body)
			rootID, _ := body["root_id"].(string)
			capturedRoots = append(capturedRoots, rootID)
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(mmmodel.Post{Id: fmt.Sprintf("post-%d", len(capturedRoots)), ChannelId: "c1"})
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()

	adapter := newTestAdapter(srv, "test")
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
	a := strings.Repeat("a", 60)
	b := strings.Repeat("b", 60)
	result := splitMessage(a+". "+b, 100)
	require.Len(t, result, 2)
	assert.Equal(t, a+".", result[0])
	assert.Equal(t, b, result[1])
}

func TestSplitMessage_HardCut(t *testing.T) {
	content := strings.Repeat("a", 150)
	result := splitMessage(content, 100)
	require.Len(t, result, 2)
	assert.Equal(t, strings.Repeat("a", 100), result[0])
	assert.Equal(t, strings.Repeat("a", 50), result[1])
}
