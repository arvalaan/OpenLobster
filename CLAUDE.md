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
  - `maxToolRounds` must stay at **40**. Complex browser interactions (booking
    flows, multi-step forms) easily exceed 20 rounds.
  - `injectMemoryTurn` must be guarded with `if !isLoopback`.
  - The `stopReason == "length" && hasToolCalls` branch must remain.
  - Keep `AIProviderOverride ports.AIProviderPort` in `HandleMessageInput`.
  - Keep the `activeProvider`/runner-copy blocks that use the override for
    compaction and `runAgenticLoop`.
  - The compaction threshold must use `activeProvider.GetContextWindow()`, **NOT**
    `activeProvider.GetMaxTokens()`. `GetMaxTokens()` returns max *output* tokens
    (4096), causing compaction to fire after ~2,800 tokens of conversation — far
    too aggressive. `GetContextWindow()` returns the model's full context (200k for
    Claude), so compaction triggers at ~140k tokens as intended.
  - The system prompt tool-use instructions (around line 958) must tell the agent to
    work **silently** — no narrating tool calls, no sharing intermediate screenshots,
    only message the user with final results or when input is needed. Upstream's
    default instructions force acknowledgement before and after every tool call which
    makes the agent extremely verbose. Keep our concise version.
  - Keep the **Browser Playbooks** system prompt section. It instructs the agent to
    look up `Story` nodes with `category="playbook"` (keyed as
    `playbook:<domain>:<task>`) before starting multi-step browser tasks, and to
    save successful sequences as playbooks after completion. This uses existing
    entity infrastructure — no new node types or tools required.
  - In `saveIntermediate`, the `SendMessage` call must be gated with `!hasToolCalls`
    so intermediate assistant messages paired with tool calls are NOT forwarded to the
    channel. Only final answers (no tool calls) should be sent to the user. Without
    this, the agent narrates every tool call in real-time even if the system prompt
    says to be silent.
  - When the tool round limit is exhausted, inject a `[SYSTEM]` user message asking
    the model to summarise progress and tell the user to say "continue" to resume.
    The synthesis call must have a `context.WithTimeout` (3 min) and error logging.
    Without this, the agent silently stops responding mid-task.
  - `dispatchToolCall` must wrap `toolRegistry.Dispatch()` with a 2-minute
    `context.WithTimeout`. Chromedp actions (WaitVisible, Click, etc.) can hang
    indefinitely if a selector doesn't exist on the page, blocking the entire
    agentic loop with no error or log output.

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
- **Risk**: upstream may change how `CompactionSvc` or `AIProvider` are wired,
  or reset `ChromeDPConfig` to defaults.
- **Resolution**: keep the `BuildBackgroundFromConfig` call, the fallback logic,
  and `BackgroundAIProvider` being passed to `CompactionSvc`.
  Keep the `ChromeDPConfig` with `Headless: true` and a realistic desktop
  `UserAgent` string — without it the headless browser is trivially detected by
  bot-detection systems (Google, Cloudflare, etc.).

### 17. `apps/backend/internal/domain/services/message_compaction/message_compaction_service.go`
- **Risk**: upstream may change `ThresholdRatio` default.
- **Resolution**: keep `ThresholdRatio: 0.70` (was `0.85` upstream). Lower threshold
  triggers compaction earlier, reducing per-request context size.

### 19. `apps/backend/internal/domain/services/mcp/internal_tools.go`
- **Risk**: upstream may add or modify browser tools, change `browser_fetch`
  extraction JS, or reset the `browser_screenshot` result message.
- **Resolution**: this file has significant additions on our branch. Keep:
  - `browser_fetch` uses comprehensive JS extraction (text + links + scripts +
    iframes + meta), not just `document.documentElement.innerText`.
  - `browser_eval` tool — executes arbitrary JS in an existing browser session,
    IIFE-wrapped in `Execute()` to prevent `const`/`let` redeclaration errors.
  - `BrowserService` interface includes `Eval(ctx, sessionID, script)`.
  - `WebSearchTool` — uses DuckDuckGo HTML endpoint (`html.duckduckgo.com`) to
    bypass Google/Cloudflare CAPTCHAs. Registered as `web_search` under the
    `"browser"` capability in `CapabilityForTool()`, `BuiltinToolNames()`, and
    `RegisterAllInternalTools()`.
  - `browser_screenshot` result message says "Only share with the user via
    send_file if they asked for it or you need them to verify something visual."
    — upstream's version tells the agent to always share screenshots.
  - All browser tool `session_id` descriptions emphasize reusing the same
    session_id across calls to maintain page state.
  If upstream adds new browser tools, register them with the same session_id
  pattern and add to `CapabilityForTool`/`BuiltinToolNames`.

### 20. `apps/backend/internal/infrastructure/adapters/browser/chromedp/adapter.go`
- **Risk**: upstream may change `NewChromeDPAdapter` flags or `Navigate()` flow.
- **Resolution**: keep all stealth flags in `NewChromeDPAdapter`:
  - `chromedp.Flag("headless", "new")` (new headless mode, harder to detect)
  - `chromedp.Flag("disable-blink-features", "AutomationControlled")` (removes
    `navigator.webdriver = true`)
  - `chromedp.Flag("no-sandbox", true)`, `disable-dev-shm-usage`, `disable-gpu`
  - `chromedp.UserAgent(...)` support via `ChromeDPConfig.UserAgent`
  Keep `Navigate()` stealth JS injection (`Object.defineProperty(navigator,
  'webdriver', {get: () => false})`) before each navigation, and the
  `WaitReady("body")` + `Sleep(1*time.Second)` post-navigation wait.
  Without these, Google, Cloudflare, and similar bot-detection systems block
  the headless browser immediately.

### Rebase procedure
```bash
git fetch upstream
git checkout mattermost
git rebase upstream/master   # or merge, depending on preference
# Resolve conflicts per the notes above
go build ./apps/backend/cmd/openlobster/   # verify compile
go test ./apps/backend/...                 # verify tests
```
