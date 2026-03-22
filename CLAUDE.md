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
  Also keep `BuildBackgroundFromConfig` function — it creates a secondary AI
  provider for background tasks using a cheaper model. If upstream changes
  `BuildFromConfig` signature, apply same changes to `BuildBackgroundFromConfig`.

### 2. `apps/backend/internal/domain/services/scheduler/scheduler_service.go`
- **Risk**: upstream may change the `memTickerC` consolidation ticker or `NewScheduler` signature.
- **Resolution**: keep the 5-param `NewScheduler(memInterval, memEnabled, dispatcher,
  taskRepo, consolidation)` and the `memTickerC` ticker in `Run()` that calls
  `s.consolidateMemory()` → `s.consolidation.Consolidate()`. This drives the
  map-reduce consolidation pipeline directly (not through loopback).
  Also keep the `ConfidenceCheckPrompt` constant — it drives the daily assertion
  verification task.

### 3. `apps/backend/cmd/openlobster/serve/lifecycle.go`
- **Risk**: upstream may change `NewScheduler(...)` call signature or remove the
  `seedSystemTasks` call.
- **Resolution**: keep `NewScheduler(memInterval, memEnabled, dispatcher, taskRepo,
  consolidationSvc)` with all 5 params. Keep `a.seedSystemTasks(ctx)` before
  `go sched.Run(ctx)`. Keep `memory_consolidation.NewService(...)` wiring with
  `a.BackgroundAIProvider`. Preserve the Mattermost case in the channel-listener
  switch. Keep `a.BackgroundAIProvider` as the second arg to `NewLoopbackDispatcher`.

### 4. `apps/backend/cmd/openlobster/serve/graphql.go`
- **Risk**: upstream may reset `buildInitialChannels()` to only Discord/Telegram.
- **Resolution**: keep our version that also registers Slack, WhatsApp, Twilio, and
  Mattermost profiles so they appear in the UI.
  In the soft-reboot callback, keep the background provider rebuild block
  (`BuildBackgroundFromConfig` + `CompactionSvc.SetAIProvider(bgProvider)`).

### 5. `apps/backend/internal/domain/handlers/loopback_dispatcher.go`
- **Risk**: upstream may rewrite `buildMemoryConsolidationSystemPrompt()`.
- **Resolution**: merge carefully. Keep our `## Entity Storage` table, the
  `[ARCHIVIST]` prefix routing, and the `[CONFIDENCE_CHECK]` prefix routing in
  `Dispatch()`. Keep `buildConfidenceCheckSystemPrompt()` and
  `buildArchivistSystemPrompt()` (in `archivist_dispatcher.go`).
  Adopt upstream improvements to the consolidation instruction wording but do
  not drop the entity storage section or the assertion-aware extraction steps.
  Keep `backgroundProvider ports.AIProviderPort` field in `LoopbackDispatcher`
  struct. Keep the second parameter in `NewLoopbackDispatcher(handler, bgProvider)`.
  Keep `AIProviderOverride: d.backgroundProvider` in `Dispatch()`.

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
  - Keep `AIProviderOverride ports.AIProviderPort` in `HandleMessageInput`.
  - Keep the `activeProvider`/runner-copy blocks that use the override for
    compaction and `runAgenticLoop`.

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
  - `UpsertAssertionTool` uses MERGE by `label` (not contentHash) for dedup
  If upstream adds new entity tools, add relation/property validation to them too.

### 10. `apps/backend/internal/domain/handlers/archivist_dispatcher.go`
- **Risk**: upstream may rewrite or remove the Archivist system prompt.
- **Resolution**: keep our Node Type Reference table (includes Assertion and Episode
  rows), the assertion promotion step, and the quality rules. The Archivist no longer
  does Memory/Fact→entity promotion (consolidation handles that at extraction time).
  Its remaining duties: promote mature assertions, merge duplicates, expire stale
  relationships, create missing entity-to-entity links.
  Adopt upstream wording improvements but do not remove assertion-related sections.

### 11. `apps/backend/internal/domain/context/context.go`
- **Risk**: upstream may modify `formatGraphAsText()`.
- **Resolution**: keep our second loop that emits typed entity nodes (not just facts)
  in conversation context. Without it, entities stored in the graph are invisible to
  the agent during conversations.

### 12. `apps/backend/cmd/openlobster/serve/startup.go`
- **Risk**: upstream may modify `seedSystemTasks()` or `seedTaskIfAbsent()`.
- **Resolution**: keep `seedOrUpdateSystemTask` (replaces `seedTaskIfAbsent`). It's
  a superset — creates if absent, updates schedule if changed.
  Memory consolidation is NO LONGER a seeded cron task — it runs via the
  scheduler's `memTickerC` (map-reduce pipeline). On startup, any legacy
  consolidation task is removed by `removeObsoleteTask`. Only the confidence
  check task is seeded (`ConfidenceCheckPrompt` at `"0 10 * * *"`).
  Both cleanup and seeding are gated behind `MemoryEnabled`.

### 18. `apps/backend/internal/domain/services/memory_consolidation/service.go`
- **Risk**: upstream may change the consolidation service's sync phase or prompts.
- **Resolution**: we extended `syncFindings()` with entity/assertion tools
  (`upsert_entity`, `upsert_assertion`, `link_entities`) in addition to upstream's
  `add_memory` and `set_user_property`. The sync prompt includes an entity type
  mapping table. The sync phase now uses multi-round tool calling (up to 5 rounds).
  All entity validation (type allowlist, relation allowlist) mirrors `entity_tools.go`.
  Keep the `GetUnvalidated`/`MarkAsValidated` checkpointing — this prevents
  re-processing the same messages on every run.

### 13. `apps/backend/internal/domain/services/services.go`
- **Risk**: upstream may not have our re-exported constants.
- **Resolution**: keep `ConfidenceCheckPrompt = svcscheduler.ConfidenceCheckPrompt`.

### 14. `apps/backend/internal/infrastructure/config/config.go`
- **Risk**: upstream may change `OpenRouterConfig` struct.
- **Resolution**: keep `BackgroundModel string` field in `OpenRouterConfig`.
  If upstream adds new fields, merge them alongside ours.

### 15. `apps/backend/cmd/openlobster/serve/app.go`
- **Risk**: upstream may reorganize `App` struct fields.
- **Resolution**: keep `BackgroundAIProvider ports.AIProviderPort` field.

### 16. `apps/backend/cmd/openlobster/serve/services.go`
- **Risk**: upstream may change how `CompactionSvc` or `AIProvider` are wired.
- **Resolution**: keep the `BuildBackgroundFromConfig` call, the fallback logic,
  and `BackgroundAIProvider` being passed to `CompactionSvc`.

### 17. `apps/backend/internal/domain/services/message_compaction/message_compaction_service.go`
- **Risk**: upstream may change `ThresholdRatio` default.
- **Resolution**: keep `ThresholdRatio: 0.70` (was `0.85` upstream). Lower threshold
  triggers compaction earlier, reducing per-request context size.

### Rebase procedure
```bash
git fetch upstream
git checkout mattermost
git rebase upstream/master   # or merge, depending on preference
# Resolve conflicts per the notes above
go build ./apps/backend/cmd/openlobster/   # verify compile
go test ./apps/backend/...                 # verify tests
```
