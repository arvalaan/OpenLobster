# OpenLobster — Mattermost Fork

## Purpose
This is a fork of OpenLobster adding Mattermost as a messaging channel and
multi-agent group chat support via mention-based routing.

## Architecture
Clean/Hexagonal Architecture:
- Domain layer: `apps/backend/internal/domain/` (models, ports, handlers, services)
- Infrastructure: `apps/backend/internal/infrastructure/adapters/messaging/mattermost/`
- Application: `apps/backend/application/` (GraphQL, webhooks)

## Key Interfaces
- MessagingPort: `apps/backend/internal/domain/ports/messaging.go`
  All channel adapters implement this. Do not change the interface without
  checking all existing adapters.

## Reference Implementation
OpenClaw's TypeScript Mattermost adapter lives next door at:
  `../OpenClaw/extensions/mattermost/src/mattermost/`
Use it as a behavioral reference, not a copy-paste source.

## Build
cd apps/backend && go build ./cmd/openlobster/

## Deploy (local instance)
1. Build: `pnpm --filter @openlobster/backend build:fast` (from repo root)
   - Output binary: `dist/openlobsterd`
2. Stop, copy, start (binary is at `/usr/local/bin/openlobsterd`, can't overwrite while running):
   ```
   sudo systemctl stop openlobster.service
   sudo cp dist/openlobsterd /usr/local/bin/openlobsterd
   sudo systemctl start openlobster.service
   ```

## Run
OPENLOBSTER_CHANNELS_MATTERMOST_ENABLED=true \
OPENLOBSTER_CHANNELS_MATTERMOST_SERVER_URL=https://chat.example.com \
  ./openlobster

## Key Files for Mattermost Work
- Adapter: apps/backend/internal/infrastructure/adapters/messaging/mattermost/
- Config: apps/backend/internal/infrastructure/config/config.go
- Registration: apps/backend/cmd/openlobster/main.go
- Message handler: apps/backend/internal/domain/handlers/message_handler.go

## Development Rules
- Follow existing adapter patterns (slack adapter is the closest reference)
- Do not mock HTTP in tests; use httptest.NewServer for integration-style unit tests
- Do not change MessagingPort interface without updating all adapters
- Keep Mattermost-specific types out of the domain layer

## Syncing with Upstream (Neirth/OpenLobster)

When pulling upstream commits into the `mattermost` branch, the following files
are known conflict zones. For each one the correct resolution is described.

### 1. `apps/backend/internal/infrastructure/adapters/ai/factory/factory.go`
- **Risk**: upstream may reset `MaxOutputTokens`-equivalent to 500 (Discord-sized).
- **Resolution**: keep `defaultMaxOutputTokens = 4096`. The actual value is now
  config-driven (`agent.max_output_tokens`), so even if this constant drifts the
  deployed config overrides it. But keep the constant at 4096 as the default.

### 2. `apps/backend/internal/domain/services/scheduler/scheduler_service.go`
- **Risk**: upstream may re-add the hidden `memTickerC` memory consolidation ticker.
- **Resolution**: keep our version — the ticker was removed because consolidation
  now runs as a visible cron task seeded at startup (`startup.go`). Do NOT bring
  the ticker back; it would double-run consolidation.
  Also keep the `ConfidenceCheckPrompt` constant — it drives the daily assertion
  verification task.

### 3. `apps/backend/cmd/openlobster/serve/lifecycle.go`
- **Risk**: upstream may change `NewScheduler(...)` call signature or remove the
  `seedSystemTasks` call.
- **Resolution**: keep `domainservices.NewScheduler(dispatcher, a.TaskRepo)` (no
  mem-interval params) and keep `a.seedSystemTasks(ctx)` before `go sched.Run(ctx)`.
  Also preserve the Mattermost case in the channel-listener switch.

### 4. `apps/backend/cmd/openlobster/serve/graphql.go`
- **Risk**: upstream may reset `buildInitialChannels()` to only Discord/Telegram.
- **Resolution**: keep our version that also registers Slack, WhatsApp, Twilio, and
  Mattermost profiles so they appear in the UI.

### 5. `apps/backend/internal/domain/handlers/loopback_dispatcher.go`
- **Risk**: upstream may rewrite `buildMemoryConsolidationSystemPrompt()`.
- **Resolution**: merge carefully. Keep our `## Entity Storage` table, the
  `[ARCHIVIST]` prefix routing, and the `[CONFIDENCE_CHECK]` prefix routing in
  `Dispatch()`. Keep `buildConfidenceCheckSystemPrompt()` and
  `buildArchivistSystemPrompt()` (in `archivist_dispatcher.go`).
  Adopt upstream improvements to the consolidation instruction wording but do
  not drop the entity storage section or the assertion-aware extraction steps.

### 6. `apps/backend/internal/domain/handlers/message_handler.go`
- **Risk**: upstream may reset `maxToolRounds` to 5, or remove the `!isLoopback`
  guard on `injectMemoryTurn`, or remove the `stopReason="length"` fix.
- **Resolution**:
  - `maxToolRounds` must stay at **20**.
  - `injectMemoryTurn` must be guarded with `if !isLoopback`.
  - The `stopReason == "length" && hasToolCalls` branch must remain.
  - The `role == "tool"` branch must **NOT** persist to DB and must **NOT** call
    `h.messageRepo.Save()`. `models.Message` has no `ToolCallID` field; saving tool
    results without it causes Anthropic/OpenRouter to reject the conversation history
    replay with `tool_use_id: String should match pattern '^[a-zA-Z0-9_-]+'`.
    Only publish the event bus event so the UI shows live tool activity.

### 7. `schema/config.graphql` and `apps/backend/internal/application/graphql/generated/`
- **Risk**: upstream may not have the Mattermost fields in `ChannelSecretsConfig`
  or `UpdateConfigInput`.
- **Resolution**: after merging, run `go run github.com/99designs/gqlgen generate`
  from `apps/backend/`. If that fails due to missing deps, run
  `go get github.com/99designs/gqlgen@v0.17.88` first.
  The three fields that must be present are:
  `mattermostEnabled`, `mattermostServerURL`, `mattermostBotToken`
  in both `ChannelSecretsConfig` (query) and `UpdateConfigInput` (mutation).

### 8. `apps/backend/internal/infrastructure/adapters/memory/neo4j/adapter.go`
- **Risk**: upstream may revert `QueryGraph` back to `AccessModeRead`.
- **Resolution**: keep `AccessModeWrite` + `mu.Lock()`. Using read-only sessions
  causes MERGE/CREATE to silently return 0 rows with no error.

### 9. `apps/backend/internal/domain/services/mcp/entity_tools.go`
- **Risk**: upstream may add or modify entity tools without our validation layer.
- **Resolution**: this file is heavily extended on our branch. Keep:
  - `validRelationTypes` allowlist (17 types) — reject unknown relations in Go
  - `validNodePropertyKeys` / `validRelPropertyKeys` allowlists
  - `validatePropertyKeys()` helper
  - Relation validation in `UpsertEntityTool`, `LinkEntitiesTool`
  - Property key validation in `UpsertEntityTool`, `LinkEntitiesTool`, `PromoteAssertionTool`
  - `txn_created_at` / `txn_updated_at` in `UpsertEntityTool` Cypher
  - Node existence check in `LinkEntitiesTool`
  - Four new tools: `UpsertAssertionTool`, `CreateEpisodeTool`, `ListAssertionsTool`,
    `PromoteAssertionTool` — plus their registration in `RegisterEntityTools()`
  - `contentHash()` helper
  If upstream adds new entity tools, add relation/property validation to them too.

### 10. `apps/backend/internal/domain/handlers/archivist_dispatcher.go`
- **Risk**: upstream may rewrite or remove the Archivist system prompt.
- **Resolution**: keep our Node Type Reference table (includes Assertion and Episode
  rows), Step 1.5 (assertion review/promotion), and the four assertion quality rules.
  Adopt upstream wording improvements but do not remove assertion-related sections.

### 11. `apps/backend/internal/domain/context/context.go`
- **Risk**: upstream may modify `formatGraphAsText()`.
- **Resolution**: keep our second loop that emits typed entity nodes (not just facts)
  in conversation context. Without it, entities stored in the graph are invisible to
  the agent during conversations.

### 12. `apps/backend/cmd/openlobster/serve/startup.go`
- **Risk**: upstream may modify `seedSystemTasks()`.
- **Resolution**: keep the confidence check task seed (`ConfidenceCheckPrompt` at
  `"0 10 * * *"`). It is gated behind `MemoryEnabled` alongside the consolidation
  task.

### 13. `apps/backend/internal/domain/services/services.go`
- **Risk**: upstream may not have our re-exported constants.
- **Resolution**: keep `ConfidenceCheckPrompt = svcscheduler.ConfidenceCheckPrompt`.

### Rebase procedure
```bash
git fetch upstream
git checkout mattermost
git rebase upstream/master   # or merge, depending on preference
# Resolve conflicts per the notes above
go build ./apps/backend/cmd/openlobster/   # verify compile
go test ./apps/backend/...                 # verify tests
```
