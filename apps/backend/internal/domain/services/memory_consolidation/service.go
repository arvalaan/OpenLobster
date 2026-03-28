// Copyright (c) OpenLobster contributors. See LICENSE for details.

package memory_consolidation

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/google/uuid"
	"github.com/neirth/openlobster/internal/domain/models"
	"github.com/neirth/openlobster/internal/domain/ports"
	"github.com/neirth/openlobster/internal/domain/services/mcp"
	"github.com/neirth/openlobster/internal/infrastructure/logging"
)

type service struct {
	msgRepo      ports.MessageRepositoryPort
	memoryRepo   ports.MemoryPort
	aiProvider   ports.AIProviderPort
	userRepo     ports.UserRepositoryPort
	convoRepo    ports.SessionRepositoryPort
	toolRegistry *mcp.ToolRegistry
	dispatcher   ports.TaskDispatcherPort
}

const (
	// noReply is the sentinel value the extraction agent emits when it finds
	// no facts that are new or differ from what is already in memory.
	noReply = "NO_REPLY"

	extractionSystemPrompt = `You are a memory extraction and deduplication agent. The user you are processing is "%s".

Steps you MUST follow for the conversation below:
1. Identify candidate persistent facts about "%s": preferences, habits, personal details, significant events.
   Skip greetings, questions, and transient content.
2. Pay close attention to NEGATIONS and EXCLUSIONS. If the user says "NOT interested in X",
   "don't like Y", or "stopped doing Z", extract the negation faithfully — do NOT flip it
   into a positive fact. Example: "NOT interested in BMW" must NOT become "interested in BMW".
3. Skip TRIVIAL or UNIVERSAL facts that apply to all humans (e.g. "has parents", "has a wife"
   when the spouse is already known). Only extract facts that add unique, specific information.
4. Skip EPHEMERAL intent or actions (e.g. "is eating dinner", "wants to book a table right now").
   Only extract facts that will still be relevant in a week.
5. For each candidate fact, call 'search_memory' to check whether it already exists in long-term memory.
6. Output ONLY the facts that are genuinely new or meaningfully different from what is already stored.
   Format: one fact per line, starting with "- ", max 15 words each.
   For negations, prefix with "NOT: " (e.g. "- NOT: interested in BMW for EV purchase").
7. If every candidate fact already exists in memory, or if there are no candidate facts at all,
   respond with exactly: NO_REPLY`

	reductionPrompt = `
You are a memory filtering engine. You will receive several fact summaries and the current state of the User's Knowledge Graph.
Your task is to produce a **final condensed text** containing ONLY facts that are NEW or that UPDATE existing information.

## Discard Rules
- Discard anything already present and unchanged in the Knowledge Graph.
- Discard universal/biological facts obvious for any human (e.g. "has parents", "has a family").
- Discard facts that merely restate an existing entity relationship without adding detail
  (e.g. "has a wife named Nina" when a Person node for Nina with role=spouse already exists).
- Discard ephemeral actions or transient states (e.g. "is currently eating", "wants to book a table").
- Preserve negations faithfully — "NOT interested in X" is a meaningful update if the graph
  currently shows an INTERESTED_IN relationship to X, or if no record of X exists yet.
- When two candidate facts say the same thing in different words, keep only the more specific one.

Current Knowledge Graph:
%s

New Fact Summaries:
%s

Provide the final findings in a clear, bulleted text format.
`

	syncPrompt = `
You are a memory synchronization specialist. You will receive a list of new/updated findings about a user.
Your task is to update the long-term memory graph using the provided tools.

## CRITICAL: Entity vs Assertion Decision

Only create entities (upsert_entity) for CONCRETE, NAMEABLE things: specific people, physical objects,
named places, named organizations, specific media titles, specific software tools, named projects.
If you can point at it, visit it, or Google it, it is an entity.

Personality traits, preferences, habits, coping mechanisms, communication styles, values, fears,
routines, financial habits, emotional patterns, and relationship dynamics are NOT entities.
These are ABOUT the user — store them as assertions only (upsert_assertion) or as user properties
(set_user_property). Do NOT create Thing/Story entities for behavioral observations.

Examples:
- "Millie the dog" → entity (Person/Thing) + assertion ✓
- "likes to procrastinate" → assertion ONLY, NOT a Thing entity ✗
- "values honesty" → assertion ONLY ✗
- "Game of Thrones" → entity (Thing) + assertion ✓
- "evening person" → assertion ONLY or set_user_property ✗
- "TBAuctions" → entity (Organization) + assertion ✓
- "food preferences: loves pasta" → set_user_property or assertion ONLY ✗

## Tool Selection

| Information type                    | Tool to use                                       |
|------------------------------------|---------------------------------------------------|
| Named people / pets                | upsert_entity type=Person/Thing + upsert_assertion|
| Named places                       | upsert_entity type=Place + upsert_assertion       |
| Named organizations                | upsert_entity type=Organization + upsert_assertion|
| Named events / goals / projects    | upsert_entity type=Event/Story + upsert_assertion |
| Named objects / media / tools      | upsert_entity type=Thing + upsert_assertion       |
| Personality / preferences / habits | upsert_assertion ONLY (no entity)                 |
| Core user attributes               | set_user_property (name, occupation, city, etc.)  |
| Free-text with no structure        | add_memory                                        |

## Rules
- For EVERY fact, create an upsert_assertion with a short, distinctive snake_case label.
- ONLY create an entity if the fact refers to a concrete, named real-world thing.
- After creating entities, call link_entities to connect related nodes.
- Use add_memory ONLY for facts that genuinely have no entity or assertion home.
- Be precise and avoid duplicating information.
- Process findings in batches of 3-5 tool calls at a time.

New Findings:
%s
`
)

// NewService creates a new memory consolidation service. The optional dispatcher
// is used to trigger an immediate confidence check when the post-consolidation
// health check detects graph quality issues (contradictions, inverted dates, etc.).
func NewService(
	msgRepo ports.MessageRepositoryPort,
	memoryRepo ports.MemoryPort,
	aiProvider ports.AIProviderPort,
	userRepo ports.UserRepositoryPort,
	convoRepo ports.SessionRepositoryPort,
	toolRegistry *mcp.ToolRegistry,
	dispatcher ports.TaskDispatcherPort,
) ports.MemoryConsolidationPort {
	return &service{
		msgRepo:      msgRepo,
		memoryRepo:   memoryRepo,
		aiProvider:   aiProvider,
		userRepo:     userRepo,
		convoRepo:    convoRepo,
		toolRegistry: toolRegistry,
		dispatcher:   dispatcher,
	}
}

func (s *service) Consolidate(ctx context.Context) error {
	messages, err := s.msgRepo.GetUnvalidated(ctx, 500) // Batch of 500
	if err != nil {
		return fmt.Errorf("discovery: %w", err)
	}

	if len(messages) == 0 {
		return nil
	}

	// Group by user
	userMsgs := make(map[string][]models.Message)
	convoMap := make(map[string]string) // conversation_id -> user_id

	for _, msg := range messages {
		userID, ok := convoMap[msg.ConversationID]
		if !ok {
			id, err := s.convoRepo.GetByID(ctx, msg.ConversationID)
			if err != nil {
				anonMsg := anonymizeToken(fmt.Sprintf("%v", msg.ID))
				logging.Printf("memory_consolidation: failed to resolve user for message %s: %v", anonMsg, err)
				continue
			}
			userID = id.UserID
			convoMap[msg.ConversationID] = userID
		}
		userMsgs[userID] = append(userMsgs[userID], msg)
	}

	for userID, msgs := range userMsgs {
		if err := s.processUserBatch(ctx, userID, msgs); err != nil {
			logging.Printf("memory_consolidation: failed to process user %s: %v", anonymizeToken(userID), err)
			continue
		}
	}

	return nil
}

// chunkMessages splits msgs into variable-size chunks that fit within the model's
// token budget. Token count is estimated from character length (1 token ≈ 4 chars).
// Half the context window is reserved for the extraction response and prompt overhead.
func chunkMessages(msgs []models.Message, maxTokens int) [][]models.Message {
	const (
		charsPerToken  = 4
		budgetRatio    = 0.5 // reserve half the context for prompt overhead and response
		promptOverhead = 400 // estimated tokens for the extraction prompt template
		msgFormatChars = 60  // estimated chars for [id] label: prefix per message
	)

	tokenBudget := int(float64(maxTokens)*budgetRatio) - promptOverhead
	if tokenBudget <= 0 {
		tokenBudget = 512
	}

	var chunks [][]models.Message
	var current []models.Message
	currentTokens := 0

	for _, msg := range msgs {
		msgTokens := (len(msg.Content) + msgFormatChars) / charsPerToken
		if len(current) > 0 && currentTokens+msgTokens > tokenBudget {
			chunks = append(chunks, current)
			current = nil
			currentTokens = 0
		}
		current = append(current, msg)
		currentTokens += msgTokens
	}
	if len(current) > 0 {
		chunks = append(chunks, current)
	}
	return chunks
}

func (s *service) processUserBatch(ctx context.Context, userID string, msgs []models.Message) error {
	// Resolve user display name for attribution in extraction prompts.
	userName := userID
	if user, err := s.userRepo.GetByID(ctx, userID); err == nil && user.Name != "" {
		userName = user.Name
	}

	// Inject user identity into the context so memory tools (add_memory,
	// set_user_property, search_memory) always write to the correct user node
	// without requiring the model to pass for_user explicitly.
	ctx = context.WithValue(ctx, mcp.ContextKeyUserID, userID)
	ctx = context.WithValue(ctx, mcp.ContextKeyUserDisplayName, userName)

	// Filter to only conversational turns that have not yet been validated.
	// The DB query already excludes validated messages, but we guard here too
	// to avoid unnecessary API calls if the flag was set concurrently.
	// Tool outputs and system messages carry no user-attributable memory.
	// Skip group messages as they should not contribute to individual user memory.
	var conversational []models.Message
	for _, m := range msgs {
		if m.IsValidated {
			continue
		}
		if m.IsGroup {
			continue
		}
		if m.Role == "user" || m.Role == "assistant" {
			conversational = append(conversational, m)
		}
	}

	// All messages already validated — nothing to do, skip API entirely.
	if len(conversational) == 0 {
		return nil
	}

	// 0.5. Detect replies to confidence-check messages. When a user replies to a
	// verification question from the confidence check agent, their reply is a
	// CORRECTION that should be prioritized and processed with additional context.
	var correctionContext string
	for _, m := range conversational {
		if m.IsReply && m.ReplyToID != nil && m.Role == "user" {
			parent, err := s.lookupParentMessage(ctx, *m.ReplyToID)
			if err == nil && parent != nil && parent.Role == "assistant" {
				// This reply is to an assistant message — likely a confidence check answer.
				// Include the assistant's question as context for better extraction.
				correctionContext += fmt.Sprintf("CORRECTION CONTEXT: The assistant asked: %q\nThe user replied: %q\n",
					truncate(parent.Content, 500), truncate(m.Content, 500))
			}
		}
	}

	// 1. Extraction (Map) — one LLM call per chunk, no tools, extracts raw facts.
	var summaries []string
	for _, chunk := range chunkMessages(conversational, s.aiProvider.GetContextWindow()) {
		summary, err := s.extractSummary(ctx, chunk, userName)
		if err != nil {
			return fmt.Errorf("extraction: %w", err)
		}
		if strings.TrimSpace(summary) != "" {
			summaries = append(summaries, summary)
		}
	}

	// If we detected correction context, prepend it so the reduction and sync phases
	// know these facts are corrections to existing knowledge, not just new facts.
	if correctionContext != "" {
		summaries = append([]string{
			"The following are CORRECTIONS from the user in response to verification questions. " +
				"These should UPDATE or REPLACE existing assertions, not create new duplicates:\n" +
				correctionContext,
		}, summaries...)
	}

	if len(summaries) == 0 {
		return s.markAsValidated(ctx, msgs)
	}

	// 2. Reduction — filter summaries against existing memory graph.
	// Responds with empty/noReply if nothing new to persist.
	findings, err := s.filterAgainstMemory(ctx, userID, summaries)
	if err != nil {
		return fmt.Errorf("reduction: %w", err)
	}

	if strings.TrimSpace(findings) == noReply || strings.TrimSpace(findings) == "" {
		return s.markAsValidated(ctx, msgs)
	}

	// 3. Synchronization — persist new facts to the graph.
	if err := s.syncFindings(ctx, userID, findings); err != nil {
		return fmt.Errorf("synchronization: %w", err)
	}

	// 4. Post-sync health check — detect contradictions, inverted dates, orphans
	// created in this run. If issues found, trigger an immediate confidence check.
	s.runPostSyncHealthCheck(ctx, userID)

	// 5. Log graph health metrics for observability.
	LogGraphHealthSummary(ctx, s.memoryRepo)

	return s.markAsValidated(ctx, msgs)
}

func (s *service) extractSummary(ctx context.Context, msgs []models.Message, userName string) (string, error) {
	var sb strings.Builder
	for _, m := range msgs {
		label := m.Role
		if m.Role == "user" {
			if m.SenderName != "" {
				label = m.SenderName
			} else {
				label = userName
			}
		}
		fmt.Fprintf(&sb, "[%s] %s: %s\n", m.ID, label, m.Content)
	}

	s.logPhase("extraction", userName, sb.String())
	resp, err := s.runAgenticLoop(ctx, []ports.ChatMessage{
		{Role: "system", Content: fmt.Sprintf(extractionSystemPrompt, userName, userName)},
		{Role: "user", Content: fmt.Sprintf("Extract persistent facts about %s from the following conversation:\n\n%s", userName, sb.String())},
	}, s.buildTools())
	if err != nil {
		logging.Printf("memory_consolidation: extraction failed for %s: %v", anonymizeToken(userName), err)
		return "", err
	}

	return resp.Content, nil
}

// filterAgainstMemory reads the graph directly and asks the LLM to filter
// summaries against existing knowledge. This is more efficient than a
// tool-based approach because it avoids multiple round-trips.
func (s *service) filterAgainstMemory(ctx context.Context, userID string, summaries []string) (string, error) {
	graph, err := s.memoryRepo.GetUserGraph(ctx, userID)
	if err != nil {
		return "", err
	}

	// Convert graph to a readable plain text representation for the LLM
	var graphSummary strings.Builder
	for _, n := range graph.Nodes {
		fmt.Fprintf(&graphSummary, "- %s (%s): %v\n", n.ID, n.Label, n.Properties)
	}

	combinedSummaries := strings.Join(summaries, "\n---\n")
	prompt := fmt.Sprintf(reductionPrompt, graphSummary.String(), combinedSummaries)

	s.logPhase("reduction", userID, combinedSummaries)
	resp, err := s.aiProvider.Chat(ctx, ports.ChatRequest{
		Messages: []ports.ChatMessage{{Role: "system", Content: prompt}},
	})
	if err != nil {
		logging.Printf("memory_consolidation: reduction failed for %s: %v", anonymizeToken(userID), err)
		return "", err
	}

	return resp.Content, nil
}

// syncTools returns the tool definitions available to the sync-phase LLM.
func syncTools() []ports.Tool {
	return []ports.Tool{
		{
			Type: "function",
			Function: &ports.FunctionTool{
				Name:        "add_memory",
				Description: "Add a free-text fact about the user. Use only when the fact does not map to a typed entity.",
				Parameters: map[string]interface{}{
					"type": "object",
					"properties": map[string]interface{}{
						"content": map[string]interface{}{
							"type":        "string",
							"description": "The fact or knowledge to save.",
						},
					},
					"required": []string{"content"},
				},
			},
		},
		{
			Type: "function",
			Function: &ports.FunctionTool{
				Name:        "set_user_property",
				Description: "Set a core attribute for the user (name, age, occupation, city, country, language, timezone, birthday).",
				Parameters: map[string]interface{}{
					"type": "object",
					"properties": map[string]interface{}{
						"key": map[string]interface{}{
							"type":        "string",
							"description": "The property name (e.g., 'name', 'language').",
						},
						"value": map[string]interface{}{
							"type":        "string",
							"description": "The property value.",
						},
					},
					"required": []string{"key", "value"},
				},
			},
		},
		{
			Type: "function",
			Function: &ports.FunctionTool{
				Name:        "upsert_entity",
				Description: "Create or update a typed entity node (Person, Place, Thing, Story) and link it to the user.",
				Parameters: map[string]interface{}{
					"type": "object",
					"properties": map[string]interface{}{
						"type":       map[string]interface{}{"type": "string", "description": "Entity label: Person | Place | Thing | Story"},
						"name":       map[string]interface{}{"type": "string", "description": "Canonical name — used as uniqueness key within the type"},
						"relation":   map[string]interface{}{"type": "string", "description": "Relationship type: KNOWS, LOCATED_AT, AFFILIATED_WITH, INTERESTED_IN, SCHEDULED_FOR, WORKING_ON, COMPLETED, HAS, HAS_ENTITY, HAS_NOTE"},
						"properties": map[string]interface{}{"type": "object", "description": "Allowed keys: description, category, notes, url, species, breed, industry, city, country, address, date, deadline, status, make, model, year, email, phone"},
						"rel_props":  map[string]interface{}{"type": "object", "description": "Allowed keys: role, valid_from, valid_to, notes"},
					},
					"required": []string{"type", "name"},
				},
			},
		},
		{
			Type: "function",
			Function: &ports.FunctionTool{
				Name:        "upsert_assertion",
				Description: "Create or update an Assertion node — a confidence-scored claim. Duplicates matched by label increment mention_count and bump confidence.",
				Parameters: map[string]interface{}{
					"type": "object",
					"properties": map[string]interface{}{
						"content":    map[string]interface{}{"type": "string", "description": "The claim text (max 2000 chars)"},
						"label":      map[string]interface{}{"type": "string", "description": "Short distinctive label — used as dedup key"},
						"confidence": map[string]interface{}{"type": "number", "description": "0.0-1.0; 0.8=explicit, 0.5=implied, 0.3=uncertain"},
					},
					"required": []string{"content", "label"},
				},
			},
		},
		{
			Type: "function",
			Function: &ports.FunctionTool{
				Name:        "link_entities",
				Description: "Create a relationship between two existing nodes by name or ID.",
				Parameters: map[string]interface{}{
					"type": "object",
					"properties": map[string]interface{}{
						"from_name": map[string]interface{}{"type": "string", "description": "Name of the source node"},
						"to_name":   map[string]interface{}{"type": "string", "description": "Name of the target node"},
						"relation":  map[string]interface{}{"type": "string", "description": "Relationship type (e.g. KNOWS, LOCATED_AT, ABOUT, PART_OF)"},
						"rel_props": map[string]interface{}{"type": "object", "description": "Relationship properties (role, valid_from, etc.)"},
					},
					"required": []string{"from_name", "to_name", "relation"},
				},
			},
		},
	}
}

// validRelationTypes mirrors the allowlist from entity_tools.go for sync-phase validation.
var validRelationTypes = map[string]bool{
	"HAS_ENTITY": true, "KNOWS": true,
	"LOCATED_AT": true, "AFFILIATED_WITH": true,
	"SCHEDULED_FOR": true, "WORKING_ON": true, "COMPLETED": true,
	"HAS": true, "INTERESTED_IN": true, "HAS_NOTE": true,
	"ATTENDED": true, "PARTICIPATED_IN": true, "EXPERIENCED": true,
	"MEMBER_OF": true, "WORKS_FOR": true, "STUDIES_AT": true,
	"ASSERTED": true, "ABOUT": true, "DERIVED_FROM": true,
	"IN_EPISODE": true, "INVOLVES": true, "PART_OF": true,
}

// validEntityTypes mirrors the allowlist from entity_tools.go.
var validEntityTypes = map[string]bool{
	"Person": true, "Place": true, "Thing": true, "Story": true, "Event": true, "Organization": true,
}

func (s *service) syncFindings(ctx context.Context, userID string, findings string) error {
	tools := syncTools()
	prompt := fmt.Sprintf(syncPrompt, findings)

	s.logPhase("synchronization", userID, findings)

	messages := []ports.ChatMessage{
		{Role: "system", Content: prompt},
		{Role: "user", Content: "Process these findings and use your tools to update the memory now."},
	}

	// Multi-round tool calling: keep going until the LLM stops returning tool calls.
	const maxRounds = 5
	for round := 0; round < maxRounds; round++ {
		resp, err := s.aiProvider.Chat(ctx, ports.ChatRequest{
			Messages: messages,
			Tools:    tools,
		})
		if err != nil {
			logging.Printf("memory_consolidation: synchronization failed for %s: %v", anonymizeToken(userID), err)
			return err
		}

		if len(resp.ToolCalls) == 0 {
			break
		}

		// Execute each tool call and build tool-result messages for the next round.
		var toolResults []ports.ChatMessage
		for _, tc := range resp.ToolCalls {
			result := s.executeToolCall(ctx, userID, tc)
			toolResults = append(toolResults, ports.ChatMessage{
				Role:       "tool",
				Content:    result,
				ToolCallID: tc.ID,
			})
		}

		// Append the assistant's response and tool results for the next round.
		messages = append(messages, ports.ChatMessage{
			Role:      "assistant",
			Content:   resp.Content,
			ToolCalls: resp.ToolCalls,
		})
		messages = append(messages, toolResults...)
	}

	return nil
}

// buildTools creates tool definitions from the ToolRegistry for the reduction
// phase (search_memory). Used by runAgenticLoop.
func (s *service) buildTools() []ports.Tool {
	if s.toolRegistry == nil {
		return nil
	}
	// Fetch all tools with "memory" capability
	defs := s.toolRegistry.GetToolsByCapability("memory")
	tools := make([]ports.Tool, 0, len(defs))
	for _, def := range defs {
		var params map[string]interface{}
		if len(def.InputSchema) > 0 {
			_ = json.Unmarshal(def.InputSchema, &params)
		}
		tools = append(tools, ports.Tool{
			Type: "function",
			Function: &ports.FunctionTool{
				Name:        def.Name,
				Description: def.Description,
				Parameters:  params,
			},
		})
	}
	return tools
}

// runAgenticLoop runs a multi-round tool-calling loop using the ToolRegistry
// for tool dispatch. Used by the reduction phase (search_memory via ToolRegistry).
func (s *service) runAgenticLoop(ctx context.Context, messages []ports.ChatMessage, tools []ports.Tool) (*ports.ChatResponse, error) {
	const maxRounds = 5
	req := ports.ChatRequest{Messages: messages, Tools: tools}

	var lastResp ports.ChatResponse
	for round := 0; round < maxRounds; round++ {
		resp, err := s.aiProvider.Chat(ctx, req)
		if err != nil {
			return nil, err
		}
		lastResp = resp

		if resp.StopReason != "tool_use" || len(resp.ToolCalls) == 0 {
			// If we have content, we are done. If not, and we executed tools,
			// we might need one more pass to synthesize.
			if strings.TrimSpace(resp.Content) != "" || round == 0 {
				return &resp, nil
			}
			break
		}

		req.Messages = append(req.Messages, ports.ChatMessage{
			Role:      "assistant",
			Content:   resp.Content,
			ToolCalls: resp.ToolCalls,
		})

		for _, tc := range resp.ToolCalls {
			result, err := s.executeTool(ctx, tc)
			content := ""
			if err != nil {
				content = fmt.Sprintf(`{"error":"%v"}`, err)
			} else {
				content = string(result)
			}
			req.Messages = append(req.Messages, ports.ChatMessage{
				Role:       "tool",
				ToolCallID: tc.ID,
				ToolName:   tc.Function.Name,
				Content:    content,
			})
		}
	}

	// Synthesis pass if the last response was a tool call with no content.
	if len(lastResp.ToolCalls) > 0 && strings.TrimSpace(lastResp.Content) == "" {
		synthResp, err := s.aiProvider.Chat(ctx, ports.ChatRequest{Messages: req.Messages})
		if err != nil {
			return nil, err
		}
		return &synthResp, nil
	}

	return &lastResp, nil
}

// executeTool dispatches a tool call via the ToolRegistry (used by runAgenticLoop
// in the reduction phase for search_memory).
func (s *service) executeTool(ctx context.Context, tc ports.ToolCall) (json.RawMessage, error) {
	if s.toolRegistry == nil {
		return nil, fmt.Errorf("no tool registry")
	}
	var params map[string]interface{}
	if err := json.Unmarshal([]byte(tc.Function.Arguments), &params); err != nil {
		return nil, err
	}
	return s.toolRegistry.Dispatch(ctx, tc.Function.Name, params)
}

func (s *service) logPhase(phase, ident, prompt string) {
	// Use an anonymized, stable token for logging to avoid PII leakage
	anon := anonymizeToken(ident)
	logging.Printf("memory_consolidation: [%s] user=%s estimated_prompt=%d tokens", phase, anon, len(prompt)/4)

	// By default only log a truncated prompt snippet to reduce PII exposure.
	// Full prompt logging can be enabled via env var OPENLOBSTER_MEMORY_VERBOSE=1
	verbose := os.Getenv("OPENLOBSTER_MEMORY_VERBOSE") == "1"
	if verbose {
		logging.Debugf("memory_consolidation: [%s] full prompt:\n%s", phase, prompt)
		return
	}

	// Truncate the prompt snippet for safe debugging
	maxSnippet := 400
	snippet := prompt
	if len(snippet) > maxSnippet {
		snippet = snippet[:maxSnippet] + "..."
	}
	logging.Debugf("memory_consolidation: [%s] prompt_snippet:\n%s", phase, snippet)
}

func anonymizeToken(s string) string {
	if s == "" {
		return ""
	}
	h := sha256.Sum256([]byte(s))
	hexs := hex.EncodeToString(h[:])
	if len(hexs) > 12 {
		return hexs[:12]
	}
	return hexs
}

// executeToolCall dispatches a single sync-phase tool call and returns a result string.
// Used by syncFindings for our custom entity/assertion tools with inline Cypher.
func (s *service) executeToolCall(ctx context.Context, userID string, tc ports.ToolCall) string {
	switch tc.Function.Name {
	case "add_memory":
		var args struct {
			Content string `json:"content"`
		}
		if err := json.Unmarshal([]byte(tc.Function.Arguments), &args); err != nil {
			return fmt.Sprintf(`{"error":"invalid args: %s"}`, err)
		}
		if err := s.memoryRepo.AddKnowledge(ctx, userID, args.Content, "", "", "", nil); err != nil {
			logging.Printf("sync: add_memory failed for user %s: %v", anonymizeToken(userID), err)
			return fmt.Sprintf(`{"error":"%s"}`, err)
		}
		return `{"ok":true}`

	case "set_user_property":
		var args struct {
			Key   string `json:"key"`
			Value string `json:"value"`
		}
		if err := json.Unmarshal([]byte(tc.Function.Arguments), &args); err != nil {
			return fmt.Sprintf(`{"error":"invalid args: %s"}`, err)
		}
		if err := s.memoryRepo.SetUserProperty(ctx, userID, args.Key, args.Value); err != nil {
			logging.Printf("sync: set_user_property failed for user %s: %v", anonymizeToken(userID), err)
			return fmt.Sprintf(`{"error":"%s"}`, err)
		}
		return `{"ok":true}`

	case "upsert_entity":
		return s.execUpsertEntity(ctx, userID, tc.Function.Arguments)

	case "upsert_assertion":
		return s.execUpsertAssertion(ctx, userID, tc.Function.Arguments)

	case "link_entities":
		return s.execLinkEntities(ctx, userID, tc.Function.Arguments)

	default:
		return fmt.Sprintf(`{"error":"unknown tool %q"}`, tc.Function.Name)
	}
}

func (s *service) execUpsertEntity(ctx context.Context, userID string, rawArgs string) string {
	var args struct {
		Type       string                 `json:"type"`
		Name       string                 `json:"name"`
		Relation   string                 `json:"relation"`
		Properties map[string]interface{} `json:"properties"`
		RelProps   map[string]interface{} `json:"rel_props"`
	}
	if err := json.Unmarshal([]byte(rawArgs), &args); err != nil {
		return fmt.Sprintf(`{"error":"invalid args: %s"}`, err)
	}
	if args.Type == "" || args.Name == "" {
		return `{"error":"type and name are required"}`
	}
	if !validEntityTypes[args.Type] {
		return `{"error":"type must be one of: Person, Place, Thing, Story"}`
	}
	relation := args.Relation
	if relation == "" {
		relation = "HAS_ENTITY"
	}
	if !validRelationTypes[relation] {
		return fmt.Sprintf(`{"error":"unknown relation %q"}`, relation)
	}

	// Build node property SET clause
	propSet := ""
	if len(args.Properties) > 0 {
		for k, v := range args.Properties {
			vStr, _ := json.Marshal(v)
			propSet += fmt.Sprintf(", e.%s = %s", k, string(vStr))
		}
	}

	// Build relationship property clause — use datetime() for native Neo4j temporal
	relSet := "r.txn_created_at = datetime()"
	if len(args.RelProps) > 0 {
		for k, v := range args.RelProps {
			vStr, _ := json.Marshal(v)
			// Wrap temporal values in datetime() for native storage
			if k == "valid_from" || k == "valid_to" {
				if s, ok := v.(string); ok && s != "" {
					relSet += fmt.Sprintf(`, r.%s = datetime(%s)`, k, string(vStr))
					continue
				}
			}
			relSet += fmt.Sprintf(`, r.%s = %s`, k, string(vStr))
		}
	}

	cypher := fmt.Sprintf(`
MATCH (u:User) WHERE u.id = %s OR u.name = %s
MERGE (e:%s {name: %s})
ON CREATE SET e.id = randomUUID(), e.source = "consolidation", e.txn_created_at = datetime() %s
ON MATCH SET  e.txn_updated_at = datetime() %s
WITH u, e
MERGE (u)-[r:%s]->(e)
ON CREATE SET %s
RETURN e.id AS id, e.name AS name`,
		jsonQuote(userID), jsonQuote(userID),
		args.Type, jsonQuote(args.Name),
		propSet,
		propSet,
		relation,
		relSet,
	)

	result, err := s.memoryRepo.QueryGraph(ctx, cypher)
	if err != nil {
		logging.Printf("sync: upsert_entity failed for user %s: %v", anonymizeToken(userID), err)
		return fmt.Sprintf(`{"error":"%s"}`, err)
	}
	b, _ := json.Marshal(result)
	return string(b)
}

func (s *service) execUpsertAssertion(ctx context.Context, userID string, rawArgs string) string {
	var args struct {
		Content    string  `json:"content"`
		Label      string  `json:"label"`
		Confidence float64 `json:"confidence"`
	}
	if err := json.Unmarshal([]byte(rawArgs), &args); err != nil {
		return fmt.Sprintf(`{"error":"invalid args: %s"}`, err)
	}
	if args.Content == "" {
		return `{"error":"content is required"}`
	}
	if len(args.Content) > 2000 {
		return `{"error":"content exceeds 2000 character limit"}`
	}
	if args.Label == "" {
		args.Label = args.Content
		if len(args.Label) > 80 {
			args.Label = args.Label[:80]
		}
	}
	if args.Confidence <= 0 {
		args.Confidence = 0.5
	}
	if args.Confidence > 1.0 {
		args.Confidence = 1.0
	}

	cypher := fmt.Sprintf(`
MERGE (a:Assertion {label: %s})
ON CREATE SET a.id = randomUUID(), a.content = %s,
              a.confidence = %f, a.txn_created_at = datetime(),
              a.source = "consolidation", a.mention_count = 1, a.promoted = false
ON MATCH SET  a.content = CASE WHEN size(%s) > size(coalesce(a.content, ""))
                           THEN %s ELSE a.content END,
              a.txn_updated_at = datetime(),
              a.mention_count = a.mention_count + 1,
              a.confidence = CASE WHEN a.confidence + 0.1 > 1.0 THEN 1.0
                             ELSE a.confidence + 0.1 END
WITH a
MATCH (u:User) WHERE u.id = %s OR u.name = %s
MERGE (u)-[r:ASSERTED]->(a)
ON CREATE SET r.txn_created_at = datetime()
RETURN a.id AS id, a.confidence AS confidence, a.mention_count AS mentions`,
		jsonQuote(args.Label),
		jsonQuote(args.Content),
		args.Confidence,
		jsonQuote(args.Content),
		jsonQuote(args.Content),
		jsonQuote(userID),
		jsonQuote(userID),
	)

	result, err := s.memoryRepo.QueryGraph(ctx, cypher)
	if err != nil {
		logging.Printf("sync: upsert_assertion failed for user %s: %v", anonymizeToken(userID), err)
		return fmt.Sprintf(`{"error":"%s"}`, err)
	}
	b, _ := json.Marshal(result)
	return string(b)
}

func (s *service) execLinkEntities(ctx context.Context, userID string, rawArgs string) string {
	var args struct {
		FromName string                 `json:"from_name"`
		ToName   string                 `json:"to_name"`
		Relation string                 `json:"relation"`
		RelProps map[string]interface{} `json:"rel_props"`
	}
	if err := json.Unmarshal([]byte(rawArgs), &args); err != nil {
		return fmt.Sprintf(`{"error":"invalid args: %s"}`, err)
	}
	if args.FromName == "" || args.ToName == "" || args.Relation == "" {
		return `{"error":"from_name, to_name, and relation are required"}`
	}
	if !validRelationTypes[args.Relation] {
		return fmt.Sprintf(`{"error":"unknown relation %q"}`, args.Relation)
	}

	relSet := "r.txn_created_at = datetime()"
	if len(args.RelProps) > 0 {
		for k, v := range args.RelProps {
			vStr, _ := json.Marshal(v)
			if k == "valid_from" || k == "valid_to" {
				if ts, ok := v.(string); ok && ts != "" {
					relSet += fmt.Sprintf(`, r.%s = datetime(%s)`, k, string(vStr))
					continue
				}
			}
			relSet += fmt.Sprintf(`, r.%s = %s`, k, string(vStr))
		}
	}

	// Use OPTIONAL MATCH + WHERE to avoid cartesian products when
	// multiple nodes share the same name. LIMIT 1 picks the first match.
	cypher := fmt.Sprintf(`
MATCH (a) WHERE a.name = %s
WITH a LIMIT 1
MATCH (b) WHERE b.name = %s
With a, b LIMIT 1
MERGE (a)-[r:%s]->(b)
ON CREATE SET %s
RETURN a.name AS from, b.name AS to, type(r) AS relation`,
		jsonQuote(args.FromName),
		jsonQuote(args.ToName),
		args.Relation,
		relSet,
	)

	result, err := s.memoryRepo.QueryGraph(ctx, cypher)
	if err != nil {
		logging.Printf("sync: link_entities failed: %v", err)
		return fmt.Sprintf(`{"error":"%s"}`, err)
	}
	b, _ := json.Marshal(result)
	return string(b)
}

// jsonQuote returns a Cypher-safe JSON string literal.
func jsonQuote(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// lookupParentMessage retrieves a message by ID to check if a reply is to a
// confidence-check message. Returns nil (no error) if the message cannot be found.
func (s *service) lookupParentMessage(ctx context.Context, parentID uuid.UUID) (*models.Message, error) {
	// Get all messages in the conversation containing this ID and find the parent.
	// This is a best-effort lookup — if the parent is in a different conversation
	// or has been compacted away, we return nil gracefully.
	msgs, err := s.msgRepo.GetByConversation(ctx, "", 0)
	if err != nil {
		// Fallback: try to find the message across all unvalidated messages
		return nil, nil
	}
	for _, m := range msgs {
		if m.ID == parentID {
			return &m, nil
		}
	}
	return nil, nil
}

// truncate returns s truncated to maxLen characters with "..." appended if needed.
func truncate(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen] + "..."
}

func (s *service) markAsValidated(ctx context.Context, msgs []models.Message) error {
	var ids []string
	for _, m := range msgs {
		ids = append(ids, m.ID.String())
	}
	return s.msgRepo.MarkAsValidated(ctx, ids)
}

// ─────────────────────────────────────────────────────────────
// Post-sync health check
// ─────────────────────────────────────────────────────────────

// runPostSyncHealthCheck runs lightweight diagnostic Cypher queries against the
// graph after a consolidation sync. If issues are found, it triggers an
// immediate [CONFIDENCE_CHECK] dispatch with the specific problems so the user
// is notified promptly rather than waiting for the daily scheduled check.
func (s *service) runPostSyncHealthCheck(ctx context.Context, userID string) {
	issues := s.detectGraphIssues(ctx, userID)
	if len(issues) == 0 {
		logging.Printf("memory_consolidation: health_check: no issues detected for %s", anonymizeToken(userID))
		return
	}

	logging.Printf("memory_consolidation: health_check: %d issue(s) detected for %s", len(issues), anonymizeToken(userID))
	for _, issue := range issues {
		logging.Printf("memory_consolidation: health_check: - %s", issue)
	}

	// Trigger an immediate confidence check if we have a dispatcher
	if s.dispatcher != nil {
		userName := userID
		if user, err := s.userRepo.GetByID(ctx, userID); err == nil && user.Name != "" {
			userName = user.Name
		}
		prompt := fmt.Sprintf("[CONFIDENCE_CHECK] Post-consolidation health check found issues for user %s. "+
			"Issues detected:\n%s\n\nPlease verify these with the user and correct the graph.",
			userName, strings.Join(issues, "\n"))
		if err := s.dispatcher.Dispatch(ctx, prompt); err != nil {
			logging.Printf("memory_consolidation: health_check: failed to dispatch confidence check: %v", err)
		}
	}
}

// detectGraphIssues runs diagnostic Cypher queries and returns a list of
// human-readable issue descriptions. Returns nil if the graph is clean.
func (s *service) detectGraphIssues(ctx context.Context, userID string) []string {
	var issues []string

	// 1. Contradictions: same node has both HAS and INTERESTED_IN from User
	contradictions, err := s.memoryRepo.QueryGraph(ctx, fmt.Sprintf(`
		MATCH (u:User)-[r1:HAS]->(t)<-[r2:INTERESTED_IN]-(u)
		WHERE (u.id = %s OR u.name = %s)
		  AND r1.valid_to IS NULL AND r2.valid_to IS NULL
		RETURN t.name AS name LIMIT 5`, jsonQuote(userID), jsonQuote(userID)))
	if err == nil {
		for _, row := range contradictions.Data {
			if name, ok := row["name"]; ok {
				issues = append(issues, fmt.Sprintf("CONTRADICTION: %v has both HAS and INTERESTED_IN (active)", name))
			}
		}
	}

	// 2. Inverted date ranges created recently
	inverted, err := s.memoryRepo.QueryGraph(ctx, fmt.Sprintf(`
		MATCH (u:User)-[r]->(n)
		WHERE (u.id = %s OR u.name = %s)
		  AND r.valid_from IS NOT NULL AND r.valid_to IS NOT NULL
		  AND r.valid_from > r.valid_to
		RETURN n.name AS name, type(r) AS rel, r.valid_from AS from, r.valid_to AS to
		LIMIT 5`, jsonQuote(userID), jsonQuote(userID)))
	if err == nil {
		for _, row := range inverted.Data {
			issues = append(issues, fmt.Sprintf("INVERTED_DATES: %v -%v-> valid_from=%v > valid_to=%v",
				row["name"], row["rel"], row["from"], row["to"]))
		}
	}

	// 3. Duplicate entity names (same type, same name, multiple nodes)
	dupes, err := s.memoryRepo.QueryGraph(ctx, `
		MATCH (n)
		WHERE NOT n:Assertion AND NOT n:Episode AND NOT n:User
		  AND n.name IS NOT NULL
		WITH labels(n)[0] AS type, toLower(n.name) AS lname, count(*) AS cnt
		WHERE cnt > 1
		RETURN type, lname, cnt LIMIT 5`)
	if err == nil {
		for _, row := range dupes.Data {
			issues = append(issues, fmt.Sprintf("DUPLICATE: %v nodes named %q (type=%v)",
				row["cnt"], row["lname"], row["type"]))
		}
	}

	// 4. Orphaned entities (non-Assertion, non-Episode) with zero relationships
	orphans, err := s.memoryRepo.QueryGraph(ctx, `
		MATCH (n)
		WHERE NOT n:Assertion AND NOT n:Episode AND NOT n:User
		  AND NOT (n)--()
		  AND n.txn_created_at IS NOT NULL
		RETURN labels(n)[0] AS type, n.name AS name LIMIT 5`)
	if err == nil {
		for _, row := range orphans.Data {
			issues = append(issues, fmt.Sprintf("ORPHAN: %v %q has no relationships",
				row["type"], row["name"]))
		}
	}

	// 5. WORKING_ON + COMPLETED on same story (should expire WORKING_ON)
	workCompleted, err := s.memoryRepo.QueryGraph(ctx, fmt.Sprintf(`
		MATCH (u:User)-[r1:WORKING_ON]->(s)<-[r2:COMPLETED]-(u)
		WHERE (u.id = %s OR u.name = %s)
		  AND r1.valid_to IS NULL
		RETURN s.name AS name LIMIT 5`, jsonQuote(userID), jsonQuote(userID)))
	if err == nil {
		for _, row := range workCompleted.Data {
			issues = append(issues, fmt.Sprintf("STALE_STATUS: %q has both active WORKING_ON and COMPLETED",
				row["name"]))
		}
	}

	return issues
}

// LogGraphHealthSummary runs a comprehensive set of diagnostic queries and logs
// a structured summary. Called after archivist runs for observability.
func LogGraphHealthSummary(ctx context.Context, memoryRepo ports.MemoryPort) {
	counts, err := memoryRepo.QueryGraph(ctx, `
		MATCH (n) RETURN labels(n)[0] AS type, count(n) AS count ORDER BY count DESC`)
	if err != nil {
		logging.Printf("graph_health: failed to query node counts: %v", err)
		return
	}

	var summary strings.Builder
	summary.WriteString("graph_health: node_counts=[")
	for i, row := range counts.Data {
		if i > 0 {
			summary.WriteString(", ")
		}
		fmt.Fprintf(&summary, "%v:%v", row["type"], row["count"])
	}
	summary.WriteString("]")

	relCounts, err := memoryRepo.QueryGraph(ctx, `
		MATCH ()-[r]->() RETURN type(r) AS rel, count(r) AS count ORDER BY count DESC`)
	if err == nil {
		summary.WriteString(" rel_counts=[")
		for i, row := range relCounts.Data {
			if i > 0 {
				summary.WriteString(", ")
			}
			fmt.Fprintf(&summary, "%v:%v", row["rel"], row["count"])
		}
		summary.WriteString("]")
	}

	orphanCount, err := memoryRepo.QueryGraph(ctx, `
		MATCH (n)
		WHERE NOT n:Assertion AND NOT n:Episode AND NOT n:User AND NOT (n)--()
		RETURN count(n) AS count`)
	if err == nil && len(orphanCount.Data) > 0 {
		fmt.Fprintf(&summary, " orphaned_entities=%v", orphanCount.Data[0]["count"])
	}

	logging.Printf("%s", summary.String())
}
