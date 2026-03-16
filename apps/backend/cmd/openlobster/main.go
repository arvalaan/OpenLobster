// openlobster daemon entry point.
//
// Loads configuration, wires all infrastructure adapters, starts the GraphQL
// dashboard server, the heartbeat loop, and any enabled messaging channel
// adapters.
//
// # License
// See LICENSE in the root of the repository.
package main

import (
	"context"
	"embed"
	"fmt"
	"io/fs"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	appmcp "github.com/neirth/openlobster/internal/application/mcp"
	"github.com/neirth/openlobster/internal/application/graphql/dto"
	"github.com/neirth/openlobster/internal/application/graphql/resolvers"
	"github.com/neirth/openlobster/internal/application/graphql/subscriptions"
	"github.com/neirth/openlobster/internal/application/registry"
	"github.com/neirth/openlobster/internal/application/webhooks"
	appcontext "github.com/neirth/openlobster/internal/domain/context"
	"github.com/neirth/openlobster/internal/domain/events"
	domainhandlers "github.com/neirth/openlobster/internal/domain/handlers"
	"github.com/neirth/openlobster/internal/domain/models"
	"github.com/neirth/openlobster/internal/domain/ports"
	domainservices "github.com/neirth/openlobster/internal/domain/services"
	"github.com/neirth/openlobster/internal/domain/services/mcp"
	"github.com/neirth/openlobster/internal/domain/services/permissions"
	"github.com/neirth/openlobster/internal/domain/repositories"
	aifactory "github.com/neirth/openlobster/internal/infrastructure/adapters/ai/factory"
	browser "github.com/neirth/openlobster/internal/infrastructure/adapters/browser/chromedp"
	"github.com/neirth/openlobster/internal/infrastructure/adapters/filesystem"
	inframc "github.com/neirth/openlobster/internal/infrastructure/adapters/mcp"
	memfile "github.com/neirth/openlobster/internal/infrastructure/adapters/memory/file"
	memneo4j "github.com/neirth/openlobster/internal/infrastructure/adapters/memory/neo4j"
	"github.com/neirth/openlobster/internal/infrastructure/adapters/messaging/discord"
	msgrouter "github.com/neirth/openlobster/internal/infrastructure/adapters/messaging/router"
	slackadapter "github.com/neirth/openlobster/internal/infrastructure/adapters/messaging/slack"
	"github.com/neirth/openlobster/internal/infrastructure/adapters/messaging/telegram"
	"github.com/neirth/openlobster/internal/infrastructure/adapters/messaging/twilio"
	"github.com/neirth/openlobster/internal/infrastructure/adapters/messaging/whatsapp"
	"github.com/neirth/openlobster/internal/infrastructure/adapters/terminal"
	"github.com/neirth/openlobster/internal/infrastructure/config"
	"github.com/neirth/openlobster/internal/infrastructure/logging"
	"github.com/neirth/openlobster/internal/infrastructure/persistence"
	"github.com/neirth/openlobster/internal/infrastructure/secrets"

	"github.com/99designs/gqlgen/graphql/handler"
	"github.com/neirth/openlobster/internal/application/graphql"
	"github.com/neirth/openlobster/internal/application/graphql/generated"
	"github.com/neirth/openlobster/internal/application/health"
	"github.com/neirth/openlobster/internal/application/metrics"
	"github.com/spf13/viper"
)

// version is set at build time via ldflags (-X main.version=...)
var version = "dev"

// public is the single embedded FS containing:
//
//	public/assets/     — compiled frontend (Vite outDir)
//	public/*           — any other static resources served at /public/
//
//go:embed all:public
var public embed.FS

func main() {
	// Disable Ollama SDK's key-based auth (~/.ollama/id_ed25519). We use Bearer
	// token (ollamaApiKey) via our own transport; the SDK auth is for ollama.com.
	if os.Getenv("OLLAMA_AUTH") == "" {
		os.Setenv("OLLAMA_AUTH", "false")
	}

	// -----------------------------------------------------------------------
	// Configuration
	// -----------------------------------------------------------------------
	cfgPath := "data/openlobster.yaml"
	if v := os.Getenv("OPENLOBSTER_CONFIG"); v != "" {
		cfgPath = v
	}
	cfgPathAbs, err := filepath.Abs(cfgPath)
	if err != nil {
		log.Fatalf("failed to resolve config path: %v", err)
	}

	cfg, err := config.Load(cfgPath)
	if err != nil {
		log.Fatalf("failed to load configuration from %s: %v", cfgPath, err)
	}
	if err := cfg.Validate(); err != nil {
		log.Fatalf("%v", err)
	}

	// -----------------------------------------------------------------------
	// Create required directories
	// -----------------------------------------------------------------------
	for _, dir := range []string{"data", "logs", "workspace"} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			log.Fatalf("failed to create %s directory: %v", dir, err)
		}
	}

	logFile := filepath.Join(cfg.Logging.Path, "openlobster.log")
	if !filepath.IsAbs(logFile) {
		if abs, err := filepath.Abs(logFile); err == nil {
			logFile = abs
		}
	}
	if err := logging.Init(logFile, cfg.Logging.Level); err != nil {
		log.Fatalf("failed to initialize logger: %v", err)
	}
	defer logging.Close()

	// -----------------------------------------------------------------------
	// Workspace files (created only on first boot)
	// -----------------------------------------------------------------------
	workspaceFiles := map[string]string{
		"AGENTS.md": `# AGENTS.md - Behavioral Guidelines

## Overview
You are an autonomous messaging agent running on the OpenLobster platform.

## Workflow
1. Receive incoming messages from configured channels (Telegram, Discord, etc.)
2. Process message using AI provider
3. Maintain conversation context in memory
4. Execute tools as needed to fulfill user requests
5. Respond to user

## Capabilities
- Messaging via Telegram, Discord, WhatsApp, Slack, Twilio, and other channels
- Long-term memory storage and retrieval
- MCP tool execution
- Task scheduling via heartbeat
- Browser automation
- Terminal command execution
- Filesystem access
- Subagent orchestration

## Workspace Files

Your workspace files define who you are and how you behave. You can and should
edit them at any time using the filesystem tools (read_file, write_file):

- **SOUL.md** — Your personality, values, and communication style. Edit this to
  refine your character, adjust your tone, or update your values.
- **IDENTITY.md** — Your name, role, version, and metadata. Edit this to update
  your identity after bootstrap or when your purpose evolves.
- **AGENTS.md** — This file. Your behavioral guidelines and workflow. Edit this
  to record new learned behaviors, refine your workflow, or document new rules.
- **BOOTSTRAP.md** — Present only during first boot. Once you complete initial
  setup, rewrite it with a single "Bootstrap complete" message so it no longer
  triggers re-initialization on subsequent conversations.

To read a workspace file: use read_file with the path workspace/<filename>.
To update a workspace file: use write_file with the path workspace/<filename>.

Always rewrite the full file content — append the changed section and keep the rest.

## Communication Style
- Be concise and helpful
- Prioritize safety and harmlessness
- Ask for clarification when needed
- After every tool call, ALWAYS send a follow-up message to the user
`,
		"SOUL.md": `# SOUL.md - Personality & Values

## Identity
You are openlobster, an autonomous messaging agent designed to assist users with their daily tasks.

## Core Values
- **Helpfulness**: Always strive to be useful and assistance-oriented
- **Honesty**: Be truthful, never hallucinate or fabricate information
- **Harmlessness**: Refuse requests that could cause harm

## Communication Style
- Be friendly and approachable
- Keep responses concise and relevant
- Adapt tone to the user's style

## Decision Framework
1. Understand the user's intent
2. Consider safety implications
3. Choose the most effective approach
4. Execute and confirm results

## Boundaries
- Do not execute harmful commands
- Do not share sensitive information
- Ask for confirmation on destructive actions
`,
		"IDENTITY.md": `# IDENTITY.md - Agent Metadata

## Core
- Name:
- Version: ` + version + `
- Created:

## Presentation
- Title: Autonomous Assistant
- Role: Messaging Agent
- Greeting: Hello! I'm your AI assistant.

## Traits
- Language: English
- Timezone: UTC
- Availability: 24/7

## Notes
`,
		"BOOTSTRAP.md": `# BOOTSTRAP.md - Startup Initialization

You are starting your journey as an autonomous agent. Before fulfilling your purpose, discover and define your identity.

## Your Mission

Read the workspace files to understand who you are:
- **SOUL.md**: Your personality, values, and communication style
- **IDENTITY.md**: Your name, role, and presentation
- **AGENTS.md**: Your behavioral guidelines and workflow

## What to Do

1. Read all workspace files
2. Ask the user questions to clarify your identity
3. Fill in your identity details in IDENTITY.md
4. **When all steps below are complete**: Rewrite this BOOTSTRAP.md file with a single message stating that bootstrap is complete and no further action is needed (see "When You Finish" below).

## Questions to Ask

- What should I call you (my name)?
- What is my primary purpose?
- How should I address users?
- What tone should I use?
- What are my working hours?

## Important

DO NOT rewrite this file until you have:
1. Read all workspace files
2. Filled in IDENTITY.md
3. Had a conversation with the user to clarify uncertainties

Ask the user what they want you to be!

## When You Finish

Once you have completed all steps above, **rewrite this entire BOOTSTRAP.md file** replacing its contents with something like:

# BOOTSTRAP.md - Complete

Bootstrap has been completed. No further action is done.

This signals that initialization is done and you should no longer treat bootstrap as a pending task.
`,
	}

	for filename, content := range workspaceFiles {
		if filename == "BOOTSTRAP.md" && cfg.Wizard.Completed {
			continue
		}
		fp := filepath.Join(cfg.Workspace.Path, filename)
		if _, err := os.Stat(fp); os.IsNotExist(err) {
			if err := os.WriteFile(fp, []byte(content), 0o644); err != nil {
				log.Printf("warn: failed to create %s: %v", fp, err)
			} else {
				log.Printf("created workspace file: %s", fp)
			}
		}
	}

	// -----------------------------------------------------------------------
	// Database
	// -----------------------------------------------------------------------
	db, err := persistence.Open(cfg.Database.Driver, cfg.Database.DSN)
	if err != nil {
		log.Fatalf("failed to open database: %v", err)
	}
	defer db.Close()

	if err := persistence.Migrate(db.GormDB(), cfg.Database.Driver); err != nil {
		log.Fatalf("failed to migrate database schema: %v", err)
	}
	log.Println("database schema up to date")

	gormDB := db.GormDB()
	taskRepo := repositories.NewTaskRepository(gormDB)
	messageRepo := repositories.NewMessageRepository(gormDB)
	sessionRepo := repositories.NewSessionRepository(gormDB)
	userRepo := repositories.NewUserRepository(gormDB)
	convRepo := repositories.NewConversationRepository(gormDB)
	dashMsgRepo := repositories.NewDashboardMessageRepository(messageRepo)

	// -----------------------------------------------------------------------
	// AI Provider
	// -----------------------------------------------------------------------
	aiProvider := aifactory.BuildFromConfig(cfg)
	if aiProvider == nil {
		log.Println("warn: no AI provider configured — agent will not respond to messages")
	} else {
		log.Printf("ai provider: %s", aifactory.ProviderName(cfg))
	}

	// -----------------------------------------------------------------------
	// Memory backend
	// -----------------------------------------------------------------------
	var memoryAdapter ports.MemoryPort
	switch cfg.Memory.Backend {
	case "neo4j":
		neo4jAdapter, err := memneo4j.NewNeo4jMemoryBackend(
			cfg.Memory.Neo4j.URI,
			cfg.Memory.Neo4j.User,
			cfg.Memory.Neo4j.Password,
		)
		if err != nil {
			log.Fatalf("failed to connect to neo4j memory backend: %v", err)
		}
		memoryAdapter = neo4jAdapter
		log.Println("memory backend: neo4j")
	default:
		gmlBackend := memfile.NewGMLBackend(cfg.Memory.File.Path)
		if err := gmlBackend.Load(); err != nil {
			log.Fatalf("failed to load file memory backend from %s: %v", cfg.Memory.File.Path, err)
		}
		memoryAdapter = gmlBackend
		log.Printf("memory backend: file (%s)", cfg.Memory.File.Path)
	}

	// -----------------------------------------------------------------------
	// Messaging channels
	// -----------------------------------------------------------------------
	log.Println("channels: initializing messaging adapters...")
	var messagingAdapters []ports.MessagingPort

	if !cfg.Channels.Telegram.Enabled {
		log.Println("channel: telegram — disabled (skipping)")
	} else if t := cfg.Channels.Telegram.BotToken; t == "" || t == "YOUR_BOT_TOKEN_HERE" {
		log.Println("channel: telegram — no credentials configured (skipping)")
	} else if a, err := telegram.NewAdapter(t); err != nil {
		log.Printf("channel: telegram — failed to initialize: %v", err)
	} else {
		messagingAdapters = append(messagingAdapters, a)
		log.Println("channel: telegram — registered OK")
	}

	if !cfg.Channels.Discord.Enabled {
		log.Println("channel: discord — disabled (skipping)")
	} else if t := cfg.Channels.Discord.BotToken; t == "" || t == "YOUR_BOT_TOKEN_HERE" {
		log.Println("channel: discord — no credentials configured (skipping)")
	} else if a, err := discord.NewAdapter(t); err != nil {
		log.Printf("channel: discord — failed to initialize: %v", err)
	} else {
		messagingAdapters = append(messagingAdapters, a)
		log.Println("channel: discord — registered OK")
	}

	if !cfg.Channels.Slack.Enabled {
		log.Println("channel: slack — disabled (skipping)")
	} else if bt := cfg.Channels.Slack.BotToken; bt == "" || bt == "YOUR_BOT_TOKEN_HERE" {
		log.Println("channel: slack — no bot token configured (skipping)")
	} else if at := cfg.Channels.Slack.AppToken; at == "" || at == "YOUR_APP_TOKEN_HERE" {
		log.Println("channel: slack — no app-level token configured (skipping)")
	} else if a, err := slackadapter.NewAdapter(bt, at); err != nil {
		log.Printf("channel: slack — failed to initialize: %v", err)
	} else {
		messagingAdapters = append(messagingAdapters, a)
		log.Println("channel: slack — registered OK")
	}

	if !cfg.Channels.WhatsApp.Enabled {
		log.Println("channel: whatsapp — disabled (skipping)")
	} else if pid, tok := cfg.Channels.WhatsApp.PhoneID, cfg.Channels.WhatsApp.APIToken; pid == "" || tok == "" || tok == "YOUR_API_TOKEN_HERE" {
		log.Println("channel: whatsapp — no phone_id or api_token configured (skipping)")
	} else if a, err := whatsapp.NewAdapter(pid, tok); err != nil {
		log.Printf("channel: whatsapp — failed to initialize: %v", err)
	} else {
		messagingAdapters = append(messagingAdapters, a)
		log.Println("channel: whatsapp — registered OK")
	}

	if !cfg.Channels.Twilio.Enabled {
		log.Println("channel: twilio — disabled (skipping)")
	} else if sid, tok, from := cfg.Channels.Twilio.AccountSID, cfg.Channels.Twilio.AuthToken, cfg.Channels.Twilio.FromNumber; sid == "" || tok == "" || from == "" {
		log.Println("channel: twilio — no account_sid, auth_token or from_number configured (skipping)")
	} else {
		messagingAdapters = append(messagingAdapters, twilio.NewAdapter(sid, tok, from))
		log.Println("channel: twilio — registered OK")
	}

	log.Printf("channels: %d adapter(s) active", len(messagingAdapters))

	chanReg := msgrouter.New()
	for _, a := range messagingAdapters {
		switch a.(type) {
		case *telegram.Adapter:
			chanReg.Set("telegram", a)
		case *discord.Adapter:
			chanReg.Set("discord", a)
		case *slackadapter.Adapter:
			chanReg.Set("slack", a)
		case *whatsapp.Adapter:
			chanReg.Set("whatsapp", a)
		case *twilio.Adapter:
			chanReg.Set("twilio", a)
		}
	}

	// -----------------------------------------------------------------------
	// Domain services
	// -----------------------------------------------------------------------
	eventBus := domainservices.NewEventBus()
	subManager := subscriptions.NewSubscriptionManager(eventBus)

	pairingRepo := repositories.NewPairingRepository(gormDB)
	pairingService := domainservices.NewPairingService(pairingRepo)
	userChannelRepo := repositories.NewUserChannelRepository(gormDB)

	broadcastToSubs := func(ctx context.Context, e events.Event) error {
		subManager.Broadcast(e)
		return nil
	}
	for _, et := range []string{
		events.EventMessageReceived, events.EventMessageSent, events.EventMessageProcessed,
		events.EventSessionStarted, events.EventSessionEnded,
		events.EventUserPaired, events.EventUserUnpaired,
		events.EventPairingRequested, events.EventPairingApproved, events.EventPairingDenied,
		events.EventTaskAdded, events.EventTaskCompleted, events.EventCronJobExecuted,
		events.EventMCPServerConnected, events.EventMCPServerDisconnected,
		events.EventMemoryUpdated, events.EventCompactionTriggered, events.EventCompactionCompleted,
	} {
		eventBus.Subscribe(et, broadcastToSubs)
	}

	permManager := permissions.Default()
	toolRegistry := mcp.NewToolRegistry(true, permManager)

	toolPermRepo := repositories.NewToolPermissionRepository(gormDB)
	mcpServerRepo := repositories.NewMCPServerRepository(gormDB)

	for toolName, permCfg := range cfg.Permissions.ToolPermissions {
		userID := "*"
		if permCfg.User != "" {
			userID = permCfg.User
		}
		if permCfg.Mode == "deny" {
			permManager.SetPermission(userID, toolName, permissions.PermissionDeny)
		} else {
			permManager.SetPermission(userID, toolName, permissions.PermissionAlways)
		}
	}
	if len(cfg.Permissions.ToolPermissions) > 0 {
		log.Printf("permissions: loaded %d global entries from config", len(cfg.Permissions.ToolPermissions))
	}

	if savedPerms, err := toolPermRepo.ListAll(context.Background()); err == nil {
		for _, p := range savedPerms {
			if p.Mode == "allow" {
				permManager.SetPermission(p.UserID, p.ToolName, permissions.PermissionAlways)
			} else {
				permManager.SetPermission(p.UserID, p.ToolName, permissions.PermissionDeny)
			}
		}
		if len(savedPerms) > 0 {
			log.Printf("permissions: loaded %d entries from database", len(savedPerms))
		}
	} else {
		log.Printf("permissions: failed to load from database: %v", err)
	}

	compactionSvc := domainservices.NewMessageCompactionService(messageRepo, aiProvider)
	subAgentSvc := domainservices.NewSubAgentService(
		aiProvider,
		cfg.SubAgents.MaxConcurrent,
		cfg.SubAgents.DefaultTimeout,
	)
	subAgentAdapter := dto.NewSubAgentAdapter(subAgentSvc)

	skillsAdapter := filesystem.NewSkillsAdapter(cfg.Workspace.Path)

	var schedulerNotify func()

	msgRouter := msgrouter.NewRouter(chanReg)
	mcp.RegisterAllInternalTools(toolRegistry, mcp.InternalTools{
		Messaging:           &inframc.MessagingAdapter{Port: msgRouter},
		LastChannelResolver: userChannelRepo,
		Memory:              &inframc.MemoryAdapter{Port: memoryAdapter},
		Tasks: &inframc.TaskAdapter{Repo: taskRepo, Notify: func() {
			if schedulerNotify != nil {
				schedulerNotify()
			}
		}},
		SubAgents: subAgentSvc,
		Terminal:  terminal.NewHostAdapter(),
		Browser: &inframc.BrowserAdapter{
			Port: browser.NewChromeDPAdapter(browser.ChromeDPConfig{Headless: true}),
		},
		Cron: &inframc.CronAdapter{Repo: taskRepo, Notify: func() {
			if schedulerNotify != nil {
				schedulerNotify()
			}
		}},
		Filesystem:    filesystem.NewAdapter(cfgPath),
		Conversations: &inframc.ConversationAdapter{ConvRepo: convRepo, MsgRepo: messageRepo},
		Skills:        skillsAdapter,
		ConfigPath:    cfgPath,
	})
	log.Printf("tools: registered %d internal tools", len(toolRegistry.AllTools()))

	ctxInjector := appcontext.NewContextInjector(
		cfg.Agent.Name,
		filepath.Join(cfg.Workspace.Path, "AGENTS.md"),
		filepath.Join(cfg.Workspace.Path, "SOUL.md"),
		filepath.Join(cfg.Workspace.Path, "IDENTITY.md"),
		filepath.Join(cfg.Workspace.Path, "BOOTSTRAP.md"),
		memoryAdapter,
		toolRegistry,
	)

	msgHandler := domainhandlers.NewMessageHandler(
		aiProvider,
		msgRouter,
		memoryAdapter,
		toolRegistry,
		permManager,
		sessionRepo,
		messageRepo,
		userRepo,
		eventBus,
		ctxInjector,
		compactionSvc,
		userChannelRepo,
		pairingService,
	)
	msgHandler.SetGroupRegistrar(repositories.NewGroupRepository(gormDB))
	msgHandler.SetPlatformEnsurer(repositories.NewChannelRepository(gormDB))
	msgHandler.SetSkillsProvider(skillsAdapter)
	msgHandler.SetPermissionLoader(func(ctx context.Context, userID string) map[string]string {
		records, err := toolPermRepo.ListByUser(ctx, userID)
		if err != nil {
			return nil
		}
		m := make(map[string]string, len(records))
		for _, r := range records {
			m[r.ToolName] = r.Mode
		}
		return m
	})

	// -----------------------------------------------------------------------
	// GraphQL dashboard
	// -----------------------------------------------------------------------
	agentRegistry := registry.NewAgentRegistry()

	isRealCredential := func(s string) bool {
		return s != "" &&
			s != "YOUR_BOT_TOKEN_HERE" &&
			s != "YOUR_ACCOUNT_SID" &&
			s != "YOUR_ACCOUNT_SID_HERE" &&
			s != "YOUR_AUTH_TOKEN" &&
			s != "YOUR_API_KEY_HERE"
	}
	var channels []dto.ChannelStatus
	if cfg.Channels.Discord.Enabled && isRealCredential(cfg.Channels.Discord.BotToken) {
		channels = append(channels, dto.ChannelStatus{
			ID: "discord", Name: "Discord", Type: "discord", Status: "online",
			Enabled: cfg.Channels.Discord.Enabled,
			Capabilities: dto.ChannelCapabilities{
				HasVoiceMessage: true, HasCallStream: false, HasTextStream: true, HasMediaSupport: true,
			},
		})
	}
	if cfg.Channels.Telegram.Enabled && isRealCredential(cfg.Channels.Telegram.BotToken) {
		channels = append(channels, dto.ChannelStatus{
			ID: "telegram", Name: "Telegram", Type: "telegram", Status: "online",
			Enabled: cfg.Channels.Telegram.Enabled,
			Capabilities: dto.ChannelCapabilities{
				HasVoiceMessage: true, HasCallStream: false, HasTextStream: true, HasMediaSupport: true,
			},
		})
	}

	agentName := cfg.Agent.Name
	if agentName == "" {
		agentName = "OpenLobster"
	}
	provider := aifactory.ProviderName(cfg)
	agentRegistry.UpdateAgent(&dto.AgentSnapshot{
		ID: "openlobster", Name: agentName, Version: version, Status: "running",
		Provider: provider, Channels: channels, AIProvider: provider,
		MemoryBackend: string(cfg.Memory.Backend),
	})
	agentRegistry.UpdateAgentChannels(channels)
	appmcp.SyncToolsToRegistry(toolRegistry, agentRegistry)

	queryService := domainservices.NewDashboardQueryService(
		taskRepo, memoryAdapter, memoryAdapter, nil, nil,
	)
	commandService := domainservices.NewDashboardCommandService(
		taskRepo, memoryAdapter, memoryAdapter,
	)
	commandService.SetTaskNotifier(func() {
		if schedulerNotify != nil {
			schedulerNotify()
		}
	})

	configSnapshot := dto.BuildConfigSnapshot(cfg, aifactory.ProviderName)

	deps := &resolvers.Deps{
		AgentRegistry:   agentRegistry,
		QuerySvc:        queryService,
		CommandSvc:      commandService,
		TaskRepo:        taskRepo,
		MemoryRepo:      memoryAdapter,
		MsgRepo:         &dto.MsgRepoAdapter{Repo: dashMsgRepo},
		ConvPort:        &dto.ConversationPortAdapter{Repo: convRepo},
		SkillsPort:      skillsAdapter,
		SysFilesPort:    filesystem.NewSystemFilesAdapter(cfg.Workspace.Path),
		ToolPermRepo:    &dto.ToolPermAdapter{Repo: toolPermRepo},
		ToolNamesSource: &appmcp.ToolNamesAdapter{Reg: toolRegistry},
		MCPServerRepo:   &dto.MCPServerAdapter{Repo: mcpServerRepo},
		SubAgentSvc:     subAgentAdapter,
		PairingPort: &dto.PairingPortAdapter{
			Svc:             pairingService,
			UserRepo:        userRepo,
			UserChannelRepo: userChannelRepo,
			ChannelRepo:     repositories.NewChannelRepository(gormDB),
			MessageSender:   chanReg,
			EventBus:        eventBus,
		},
		UserRepo:          &dto.UserRepoAdapter{Repo: userRepo},
		UserChannelRepo:   userChannelRepo,
		MessageSender:     chanReg,
		MessageDispatcher: msgHandler,
		EventBus:          &dto.EventBusAdapter{Eb: eventBus},
		AIProvider:        aiProvider,
		ConfigSnapshot:    configSnapshot,
		ConfigPath:        cfgPath,
	}

	msgHandler.SetCapabilitiesChecker(func(cap string) bool {
		if deps.ConfigSnapshot == nil || deps.ConfigSnapshot.Capabilities == nil {
			return true
		}
		switch cap {
		case "browser":
			return deps.ConfigSnapshot.Capabilities.Browser
		case "terminal":
			return deps.ConfigSnapshot.Capabilities.Terminal
		case "subagents":
			return deps.ConfigSnapshot.Capabilities.Subagents
		case "memory":
			return deps.ConfigSnapshot.Capabilities.Memory
		case "mcp":
			return deps.ConfigSnapshot.Capabilities.MCP
		case "filesystem":
			return deps.ConfigSnapshot.Capabilities.Filesystem
		case "sessions":
			return deps.ConfigSnapshot.Capabilities.Sessions
		default:
			return true
		}
	})

	// -----------------------------------------------------------------------
	// Channel hot-reload
	// -----------------------------------------------------------------------
	channelCaps := map[string]dto.ChannelCapabilities{
		"telegram": {HasVoiceMessage: true, HasCallStream: false, HasTextStream: true, HasMediaSupport: true},
		"discord":  {HasVoiceMessage: true, HasCallStream: false, HasTextStream: true, HasMediaSupport: true},
		"slack":    {HasVoiceMessage: true, HasCallStream: false, HasTextStream: true, HasMediaSupport: true},
		"whatsapp": {HasVoiceMessage: true, HasCallStream: true, HasTextStream: true, HasMediaSupport: true},
		"twilio":   {HasVoiceMessage: true, HasCallStream: true, HasTextStream: true, HasMediaSupport: true},
	}

	httpHandler := graphql.NewHandler(deps)
	healthHandler := health.NewHandler()
	metricsHandler := metrics.NewHandler(deps)

	rebuildActiveChannels := func() []dto.ChannelStatus {
		var list []dto.ChannelStatus
		for _, t := range []string{"telegram", "discord", "slack", "whatsapp", "twilio"} {
			if chanReg.Get(t) != nil {
				list = append(list, dto.ChannelStatus{
					ID: t, Name: t, Type: t, Status: "online",
					Enabled: true, Capabilities: channelCaps[t],
				})
			}
		}
		return list
	}

	var channelStartCtx context.Context

	makeChannelMsgHandler := func(ct string) func(context.Context, *models.Message) {
		return func(ctx context.Context, msg *models.Message) {
			if msg == nil || (msg.Content == "" && len(msg.Attachments) == 0 && msg.Audio == nil) {
				return
			}
			if hErr := msgHandler.Handle(ctx, domainhandlers.HandleMessageInput{
				ChannelID:   msg.ChannelID,
				Content:     msg.Content,
				ChannelType: ct,
				SenderName:  msg.SenderName,
				SenderID:    msg.SenderID,
				IsGroup:     msg.IsGroup,
				IsMentioned: msg.IsMentioned,
				GroupName:   msg.GroupName,
				Attachments: msg.Attachments,
				Audio:       msg.Audio,
			}); hErr != nil {
				log.Printf("channel %s: message handler error: %v", ct, hErr)
			}
		}
	}

	reloadChannel := func(channelType string) {
		chanReg.Remove(channelType)
		enabled := viper.GetBool("channels." + channelType + ".enabled")
		var newAdapter ports.MessagingPort
		if enabled {
			switch channelType {
			case "telegram":
				if token := viper.GetString("channels.telegram.bot_token"); token != "" && token != "YOUR_BOT_TOKEN_HERE" {
					if a, err := telegram.NewAdapter(token); err == nil {
						newAdapter = a
					} else {
						log.Printf("channel: telegram — reload failed: %v", err)
					}
				}
			case "discord":
				if token := viper.GetString("channels.discord.bot_token"); token != "" && token != "YOUR_BOT_TOKEN_HERE" {
					if a, err := discord.NewAdapter(token); err == nil {
						newAdapter = a
					} else {
						log.Printf("channel: discord — reload failed: %v", err)
					}
				}
			case "slack":
				bt := viper.GetString("channels.slack.bot_token")
				at := viper.GetString("channels.slack.app_token")
				if bt != "" && bt != "YOUR_BOT_TOKEN_HERE" && at != "" && at != "YOUR_APP_TOKEN_HERE" {
					if a, err := slackadapter.NewAdapter(bt, at); err == nil {
						newAdapter = a
					} else {
						log.Printf("channel: slack — reload failed: %v", err)
					}
				}
			case "whatsapp":
				pid := viper.GetString("channels.whatsapp.phone_id")
				tok := viper.GetString("channels.whatsapp.api_token")
				if pid != "" && tok != "" && tok != "YOUR_API_TOKEN_HERE" {
					if a, err := whatsapp.NewAdapter(pid, tok); err == nil {
						newAdapter = a
					} else {
						log.Printf("channel: whatsapp — reload failed: %v", err)
					}
				}
			case "twilio":
				sid := viper.GetString("channels.twilio.account_sid")
				tok := viper.GetString("channels.twilio.auth_token")
				from := viper.GetString("channels.twilio.from_number")
				if sid != "" && tok != "" && from != "" {
					newAdapter = twilio.NewAdapter(sid, tok, from)
				}
			}
		}
		if newAdapter != nil {
			chanReg.Set(channelType, newAdapter)
			if channelStartCtx != nil {
				switch a := newAdapter.(type) {
				case *telegram.Adapter:
					go func() {
						if err := a.Start(channelStartCtx, makeChannelMsgHandler("telegram")); err != nil {
							log.Printf("channel: telegram — listener failed (hot): %v", err)
						}
					}()
				case *discord.Adapter:
					go func() {
						if err := a.Start(channelStartCtx, makeChannelMsgHandler("discord")); err != nil {
							log.Printf("channel: discord — listener failed (hot): %v", err)
						}
					}()
				case *slackadapter.Adapter:
					go func() {
						if err := a.Start(channelStartCtx, makeChannelMsgHandler("slack")); err != nil {
							log.Printf("channel: slack — listener failed (hot): %v", err)
						}
					}()
				}
			}
			log.Printf("channel: %s — reloaded OK (hot)", channelType)
		} else if enabled {
			log.Printf("channel: %s — deactivated (no valid credentials)", channelType)
		} else {
			log.Printf("channel: %s — deactivated (disabled)", channelType)
		}
		httpHandler.UpdateAgentChannels(rebuildActiveChannels())
	}

	configWriter := &dto.ConfigUpdateAdapter{
		ConfigPath:    cfgPathAbs,
		ReloadChannel: reloadChannel,
		ViperKeys:     dto.InputToViperKeyMap(),
		OnApplied: func(providerTouched bool) {
			reloaded, err := config.Load(cfgPathAbs)
			if err != nil {
				log.Printf("config: failed to reload after save: %v", err)
				return
			}
			deps.ConfigSnapshot = dto.BuildConfigSnapshot(reloaded, aifactory.ProviderName)
			if cur := agentRegistry.GetAgent(); cur != nil {
				name := reloaded.Agent.Name
				if name == "" {
					name = "OpenLobster"
				}
				updated := *cur
				updated.Name = name
				updated.Provider = aifactory.ProviderName(reloaded)
				updated.AIProvider = aifactory.ProviderName(reloaded)
				agentRegistry.UpdateAgent(&updated)
			}
			if providerTouched {
				newProvider := aifactory.BuildFromConfig(reloaded)
				msgHandler.SetAIProvider(newProvider)
				compactionSvc.SetAIProvider(newProvider)
				deps.AIProvider = newProvider
				log.Printf("config: soft reboot — AI provider reloaded")
			}
		},
	}
	deps.ConfigWriter = configWriter
	deps.SkillsPort = skillsAdapter
	log.Printf("skills: reading from %s/skills", cfg.Workspace.Path)

	// -----------------------------------------------------------------------
	// Secrets provider + MCP client + OAuth 2.1 manager
	// -----------------------------------------------------------------------
	secretsBackend := strings.ToLower(strings.TrimSpace(cfg.Secrets.Backend))
	var secretsProvider secrets.SecretsProvider

	switch secretsBackend {
	case "openbao":
		if cfg.Secrets.Openbao == nil || cfg.Secrets.Openbao.URL == "" {
			log.Fatalf("secrets backend is openbao but secrets.openbao.url is not set")
		}
		if cfg.Secrets.Openbao.Token == "" {
			log.Fatalf("secrets backend is openbao but secrets.openbao.token is not set")
		}
		secretsProvider, err = secrets.NewOpenBAOProvider(cfg.Secrets.Openbao.URL, cfg.Secrets.Openbao.Token, "secret")
		if err != nil {
			log.Fatalf("failed to initialize OpenBao secrets provider: %v", err)
		}
		log.Printf("secrets: openbao backend at %s", cfg.Secrets.Openbao.URL)
	default:
		secretsPath := cfg.Secrets.File.Path
		if secretsPath == "" {
			secretsPath = "data/secrets.json"
		}
		if secretsBackend != "" && secretsBackend != "file" {
			log.Printf("secrets: unknown backend %q, using file backend", cfg.Secrets.Backend)
		}
		secretsProvider, err = secrets.NewFileSecretsProvider(secretsPath, config.SecretKey())
		if err != nil {
			log.Fatalf("failed to initialize secrets provider: %v", err)
		}
		log.Printf("secrets: file backend at %s", secretsPath)
	}

	mcpClientSDK := mcp.NewMCPClientSDK(secretsProvider)

	oauthCallbackURL := cfg.GraphQL.BaseURL
	if oauthCallbackURL != "" && (strings.HasPrefix(oauthCallbackURL, "http://") || strings.HasPrefix(oauthCallbackURL, "https://")) {
		oauthCallbackURL = strings.TrimSuffix(oauthCallbackURL, "/") + "/oauth/callback"
	} else {
		oauthCallbackURL = fmt.Sprintf("http://%s:%d/oauth/callback", cfg.GraphQL.Host, cfg.GraphQL.Port)
		if cfg.GraphQL.BaseURL != "" {
			log.Printf("oauth: graphql.base_url %q is not a full URL; using %q for redirect_uri.", cfg.GraphQL.BaseURL, oauthCallbackURL)
		} else if cfg.GraphQL.Host == "0.0.0.0" || cfg.GraphQL.Host == "" {
			log.Printf("oauth: redirect_uri is %q. For OAuth behind a reverse proxy, set OPENLOBSTER_GRAPHQL_BASE_URL.", oauthCallbackURL)
		}
	}
	oauthMgr := mcp.NewOAuthManager(secretsProvider, oauthCallbackURL)

	if savedServers, err := mcpServerRepo.ListAll(context.Background()); err == nil {
		for _, s := range savedServers {
			go func(name, url string) {
				ctx := context.Background()
				if err := mcpClientSDK.Connect(ctx, mcp.ServerConfig{Name: name, Type: "http", URL: url}); err != nil {
					log.Printf("mcp: startup reconnect %q failed: %v — marking as pending-auth", name, err)
					oauthMgr.RegisterPendingServer(name, url)
				} else {
					log.Printf("mcp: startup reconnected %q", name)
					if tools := mcpClientSDK.GetServerTools(name); len(tools) > 0 {
						_ = toolRegistry.RegisterMCP(name, mcpClientSDK, tools)
						log.Printf("mcp: registered %d tools from %q", len(tools), name)
						appmcp.SyncToolsToRegistry(toolRegistry, agentRegistry)
					}
				}
			}(s.Name, s.URL)
		}
	} else {
		log.Printf("mcp: failed to load saved servers: %v", err)
	}

	deps.McpConnectPort = &appmcp.ConnectAdapter{
		Client:   mcpClientSDK,
		Registry: toolRegistry,
		AgentReg: agentRegistry,
		Repo:     mcpServerRepo,
		OAuth:    oauthMgr,
		EventBus: &dto.EventBusAdapter{Eb: eventBus},
	}
	deps.McpOAuthPort = &appmcp.OAuthAdapter{OAuth: oauthMgr}

	// -----------------------------------------------------------------------
	// HTTP mux
	// -----------------------------------------------------------------------
	mux := http.NewServeMux()

	gqlgenResolver := resolvers.NewResolver(deps)
	gqlgenResolver.SetEventSubscription(&dto.EventSubscriptionAdapter{Eb: eventBus})
	gqlgenSrv := generated.NewExecutableSchema(generated.Config{Resolvers: gqlgenResolver})

	mux.HandleFunc("/ws", subManager.HandleWebSocket)
	log.Println("graphql: subscriptions WebSocket at /ws")

	gqlHandler := handler.NewDefaultServer(gqlgenSrv)
	mux.Handle("/graphql", gqlHandler)
	log.Println("graphql: gqlgen handler registered at /graphql")

	mux.Handle("/health", healthHandler)
	mux.Handle("/metrics", metricsHandler)

	mux.HandleFunc("/logs", func(w http.ResponseWriter, r *http.Request) {
		logger := logging.GetDefaultLogger()
		if logger == nil {
			http.Error(w, "logger not initialized", http.StatusInternalServerError)
			return
		}
		logs, err := logger.GetTailLines(100)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Write([]byte(logs))
	})

	webhooks.NewHandler(chanReg, msgHandler).Register(mux)

	mux.HandleFunc("/oauth/callback", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		serverName, herr := oauthMgr.HandleCallback(r.Context(), q.Get("code"), q.Get("state"), q.Get("error"))
		if herr != nil {
			log.Printf("oauth callback error: %v", herr)
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			fmt.Fprintf(w, `<!doctype html><html><head><title>OAuth Error</title></head>`+
				`<body><h2>Authorization failed</h2><p>%s</p><p>You may close this window.</p>`+
				`<script>window.opener&&window.opener.postMessage({type:"oauth_error",error:%q},"*");window.close();</script>`+
				`</body></html>`, herr.Error(), herr.Error())
			return
		}
		if serverName != "" {
			if pendingURL, ok := oauthMgr.GetPendingServers()[serverName]; ok {
				go func() {
					ctx := context.Background()
					err := mcpClientSDK.Connect(ctx, mcp.ServerConfig{Name: serverName, Type: "http", URL: pendingURL})
					if err != nil {
						log.Printf("oauth: auto-reconnect for %q failed: %v", serverName, err)
					} else {
						oauthMgr.RemovePendingServer(serverName)
						_ = mcpServerRepo.Save(ctx, serverName, pendingURL)
						if tools := mcpClientSDK.GetServerTools(serverName); len(tools) > 0 {
							_ = toolRegistry.RegisterMCP(serverName, mcpClientSDK, tools)
							log.Printf("mcp: registered %d tools from %q (oauth callback)", len(tools), serverName)
							appmcp.SyncToolsToRegistry(toolRegistry, agentRegistry)
						}
					}
				}()
			}
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprint(w, `<!doctype html><html><head><title>Authorized</title></head>`+
			`<body><h2>Authorization successful</h2><p>You may close this window.</p>`+
			`<script>window.opener&&window.opener.postMessage({type:"oauth_success"},"*");window.close();</script>`+
			`</body></html>`)
	})
	log.Println("oauth: /oauth/callback registered")

	staticResourceFS, err := fs.Sub(public, "public/static")
	if err != nil {
		log.Fatalf("failed to create sub-fs for public/static: %v", err)
	}
	mux.Handle("/static/", http.StripPrefix("/static/", http.FileServer(http.FS(staticResourceFS))))
	log.Println("static: resources served at /static/")

	staticFS, err := fs.Sub(public, "public/assets")
	if err != nil {
		log.Fatalf("failed to create sub-fs for assets: %v", err)
	}
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if strings.Contains(r.URL.Path, ".") {
			http.FileServer(http.FS(staticFS)).ServeHTTP(w, r)
			return
		}
		index, err := fs.ReadFile(public, "public/assets/index.html")
		if err != nil {
			log.Printf("failed to read index.html: %v", err)
			http.Error(w, "Internal server error", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write(index)
	})

	effectiveToken := cfg.GraphQL.AuthToken
	authMiddleware := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodOptions {
				next.ServeHTTP(w, r)
				return
			}
			protected := strings.HasPrefix(r.URL.Path, "/graphql") || strings.HasPrefix(r.URL.Path, "/logs")
			if !protected || !cfg.GraphQL.AuthEnabled || effectiveToken == "" {
				next.ServeHTTP(w, r)
				return
			}
			token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
			if token == "" {
				token = r.Header.Get("X-Access-Token")
			}
			if token != effectiveToken {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusUnauthorized)
				w.Write([]byte(`{"error":"unauthorized","message":"valid access token required"}`))
				return
			}
			next.ServeHTTP(w, r)
		})
	}

	corsHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, PATCH, DELETE, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization, X-Requested-With, X-Access-Token")
		w.Header().Set("Access-Control-Allow-Credentials", "true")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		authMiddleware(mux).ServeHTTP(w, r)
	})

	addr := fmt.Sprintf("%s:%d", cfg.GraphQL.Host, cfg.GraphQL.Port)
	httpServer := &http.Server{
		Addr:         addr,
		Handler:      corsHandler,
		ReadTimeout:  30 * time.Second,
		WriteTimeout: 30 * time.Second,
		IdleTimeout:  120 * time.Second,
	}

	// -----------------------------------------------------------------------
	// Start background goroutines
	// -----------------------------------------------------------------------
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	channelStartCtx = ctx

	if cfg.Scheduler.Enabled {
		dispatcher := domainhandlers.NewLoopbackDispatcher(msgHandler)
		sched := domainservices.NewScheduler(
			cfg.Scheduler.MemoryInterval,
			cfg.Scheduler.MemoryEnabled,
			dispatcher,
			taskRepo,
		)
		schedulerNotify = sched.Notify
		go sched.Run(ctx)
	}

	for _, a := range messagingAdapters {
		var channelType string
		switch a.(type) {
		case *telegram.Adapter:
			channelType = "telegram"
		case *discord.Adapter:
			channelType = "discord"
		case *slackadapter.Adapter:
			channelType = "slack"
		}
		ct := channelType
		adapter := a
		if err := adapter.Start(ctx, makeChannelMsgHandler(ct)); err != nil {
			log.Printf("channel %s: failed to start listener: %v", ct, err)
		} else {
			log.Printf("channel: %s — listener started", ct)
		}
	}

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)

	log.Printf("openlobster listening on http://%s", addr)

	go func() {
		if err := httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("http server error: %v", err)
		}
	}()

	<-sig
	log.Println("shutting down…")

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer shutdownCancel()

	if err := httpServer.Shutdown(shutdownCtx); err != nil {
		log.Printf("http shutdown error: %v", err)
	}

	if gml, ok := memoryAdapter.(interface{ Close() error }); ok {
		if err := gml.Close(); err != nil {
			log.Printf("memory backend flush error: %v", err)
		} else {
			log.Println("memory backend: flushed to disk")
		}
	}
}
