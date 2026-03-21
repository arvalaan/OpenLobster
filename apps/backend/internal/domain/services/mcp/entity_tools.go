package mcp

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// entityNeoUnavailable is returned when the graph backend does not support
// Cypher (e.g. the GML file backend). The caller can fall back to add_memory.
const entityNeoUnavailable = `{"error":"entity nodes require a Neo4j backend; use add_memory as a fallback"}`

// validRelationTypes is the Go-enforced allowlist of relationship types.
// Any relation not in this set is rejected before touching the graph.
// Use the "role" rel_prop for specificity (e.g. KNOWS + role=spouse).
var validRelationTypes = map[string]bool{
	// User-to-entity
	"HAS_ENTITY": true, "KNOWS": true, "HAS_PET": true,
	"LOCATED_AT": true, "AFFILIATED_WITH": true,
	"SCHEDULED_FOR": true, "WORKING_ON": true, "COMPLETED": true,
	"HAS": true, "INTERESTED_IN": true, "HAS_NOTE": true,
	// Assertion/Episode
	"ASSERTED": true, "ABOUT": true, "DERIVED_FROM": true,
	"IN_EPISODE": true, "INVOLVES": true,
	// Entity-to-entity
	"PART_OF": true,
}

// validNodePropertyKeys is the allowlist of property keys on entity nodes.
// Anything too specific for a key goes into "description" or "notes" as a value.
var validNodePropertyKeys = map[string]bool{
	"description": true, "category": true, "notes": true, "url": true,
	"species": true, "breed": true, "industry": true,
	"city": true, "country": true, "address": true,
	"date": true, "deadline": true, "status": true,
	"make": true, "model": true, "year": true,
	"email": true, "phone": true,
}

// validRelPropertyKeys is the allowlist of property keys on relationships.
var validRelPropertyKeys = map[string]bool{
	"role": true, "valid_from": true, "valid_to": true,
	"notes": true, "source": true, "txn_created_at": true,
	"expiry_reason": true,
}

// validatePropertyKeys checks that all keys in props are in the allowed set.
func validatePropertyKeys(props map[string]interface{}, allowed map[string]bool) error {
	for k := range props {
		if !allowed[k] {
			return fmt.Errorf("unknown property key %q; see allowed list", k)
		}
	}
	return nil
}

// resolveEntityUserID returns the user ID to use for entity operations.
// It mirrors the for_user logic from AddMemoryTool.
func resolveEntityUserID(ctx context.Context, params map[string]interface{}) string {
	if forUser, ok := params["for_user"].(string); ok && forUser != "" {
		return forUser
	}
	if dn, ok := ctx.Value(ContextKeyUserDisplayName).(string); ok && dn != "" {
		return dn
	}
	if u, ok := ctx.Value(contextKeyUserID).(string); ok {
		return u
	}
	return ""
}

// cyquerySafe returns the entityNeoUnavailable sentinel when the graph query
// returns an error indicating the backend does not support Cypher.
func isCypherUnsupported(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "not supported") ||
		strings.Contains(msg, "gml") ||
		strings.Contains(msg, "cypher")
}

// ─────────────────────────────────────────────────────────────
// UpsertEntityTool
// ─────────────────────────────────────────────────────────────

type UpsertEntityTool struct {
	Tools InternalTools
}

func (t *UpsertEntityTool) Definition() ToolDefinition {
	return ToolDefinition{
		Name: "upsert_entity",
		Description: "Create or update a typed entity node (Person, Pet, Place, Organization, Event, Goal, Asset, Topic) " +
			"and create/update the relationship from the user node to it. " +
			"Use this instead of add_memory whenever the information clearly maps to one of the typed entity categories. " +
			"HAS / AFFILIATED_WITH / LOCATED_AT relationships must always include valid_from in rel_props. " +
			"Use the role rel_prop to capture specificity (e.g. relation=KNOWS, rel_props={\"role\":\"spouse\"}).",
		InputSchema: json.RawMessage(`{
			"type": "object",
			"properties": {
				"type":       {"type": "string", "description": "Entity label: Person | Pet | Place | Organization | Event | Goal | Asset | Topic"},
				"name":       {"type": "string", "description": "Canonical name — used as uniqueness key within the type"},
				"properties": {"type": "object", "description": "Allowed keys: description, category, notes, url, species, breed, industry, city, country, address, date, deadline, status, make, model, year, email, phone"},
				"relation":   {"type": "string", "description": "Relationship type from user to entity: HAS_ENTITY, KNOWS, HAS_PET, LOCATED_AT, AFFILIATED_WITH, SCHEDULED_FOR, WORKING_ON, COMPLETED, HAS, INTERESTED_IN, HAS_NOTE. Defaults to HAS_ENTITY."},
				"rel_props":  {"type": "object", "description": "Allowed keys: role, valid_from, valid_to, notes (e.g. {\"role\": \"spouse\", \"valid_from\": \"2024-01-01T00:00:00Z\"})"},
				"for_user":   {"type": "string", "description": "User display name. Only for consolidation agents — omit for normal interactions."}
			},
			"required": ["type", "name"]
		}`),
	}
}

func (t *UpsertEntityTool) Execute(ctx context.Context, params map[string]interface{}) (json.RawMessage, error) {
	entityType, _ := params["type"].(string)
	name, _ := params["name"].(string)
	if entityType == "" || name == "" {
		return nil, fmt.Errorf("type and name are required")
	}

	validTypes := map[string]bool{
		"Person": true, "Pet": true, "Place": true, "Organization": true,
		"Event": true, "Goal": true, "Asset": true, "Topic": true,
	}
	if !validTypes[entityType] {
		return nil, fmt.Errorf("type must be one of: Person, Pet, Place, Organization, Event, Goal, Asset, Topic")
	}

	relation, _ := params["relation"].(string)
	if relation == "" {
		relation = "HAS_ENTITY"
	}
	if !validRelationTypes[relation] {
		return nil, fmt.Errorf("unknown relation %q; see allowed list", relation)
	}

	userID := resolveEntityUserID(ctx, params)
	if userID == "" {
		return nil, fmt.Errorf("could not determine user identity")
	}

	now := time.Now().UTC().Format(time.RFC3339)

	// Build property map for the entity node
	properties := map[string]interface{}{}
	if p, ok := params["properties"].(map[string]interface{}); ok {
		for k, v := range p {
			properties[k] = v
		}
	}
	if err := validatePropertyKeys(properties, validNodePropertyKeys); err != nil {
		return nil, fmt.Errorf("upsert_entity properties: %w", err)
	}

	// Build relationship property map
	relProps := map[string]interface{}{}
	if rp, ok := params["rel_props"].(map[string]interface{}); ok {
		for k, v := range rp {
			relProps[k] = v
		}
	}
	if err := validatePropertyKeys(relProps, validRelPropertyKeys); err != nil {
		return nil, fmt.Errorf("upsert_entity rel_props: %w", err)
	}
	if _, hasValidFrom := relProps["valid_from"]; !hasValidFrom {
		// Temporal relationships always need valid_from; set it to now if not provided
		transientRels := map[string]bool{
			"HAS": true, "AFFILIATED_WITH": true, "LOCATED_AT": true,
		}
		if transientRels[relation] {
			relProps["valid_from"] = now
		}
	}

	nodePropCypher := buildCypherPropsLiteral("e", properties)
	relPropCypher := buildCypherPropsLiteral("r", relProps)

	cypher := fmt.Sprintf(`
MERGE (e:%s {name: %s})
ON CREATE SET e.id = randomUUID(), e.extractedAt = %s, e.source = "conversation", e.txn_created_at = %s%s
ON MATCH  SET e.extractedAt = %s, e.txn_updated_at = %s%s
WITH e
MATCH (u:User) WHERE u.id = %s OR u.name = %s
MERGE (u)-[r:%s]->(e)
ON CREATE SET r.valid_from = %s, r.source = "conversation", r.txn_created_at = %s%s
RETURN e.id AS id, e.name AS name`,
		entityType,
		jsonStr(name),
		jsonStr(now),
		jsonStr(now),
		nodePropCypher,
		jsonStr(now),
		jsonStr(now),
		nodePropCypher,
		jsonStr(userID),
		jsonStr(userID),
		relation,
		jsonStr(now),
		jsonStr(now),
		relPropCypher,
	)

	result, err := t.Tools.Memory.QueryGraph(ctx, cypher)
	if isCypherUnsupported(err) {
		return json.RawMessage(entityNeoUnavailable), nil
	}
	if err != nil {
		return nil, fmt.Errorf("upsert_entity: %w", err)
	}

	id := ""
	if len(result.Data) > 0 {
		if v, ok := result.Data[0]["id"]; ok {
			id = fmt.Sprintf("%v", v)
		}
	}
	return json.RawMessage(fmt.Sprintf(`{"status":"ok","id":%s,"name":%s,"type":%s}`,
		jsonStr(id), jsonStr(name), jsonStr(entityType))), nil
}

// ─────────────────────────────────────────────────────────────
// LinkEntitiesTool
// ─────────────────────────────────────────────────────────────

type LinkEntitiesTool struct {
	Tools InternalTools
}

func (t *LinkEntitiesTool) Definition() ToolDefinition {
	return ToolDefinition{
		Name: "link_entities",
		Description: "Create a typed relationship between two existing entity nodes identified by their IDs. " +
			"Use this to connect entities to each other (e.g. Person LOCATED_AT Place, Pet HAS_PET Person).",
		InputSchema: json.RawMessage(`{
			"type": "object",
			"properties": {
				"from_id":    {"type": "string", "description": "ID of the source entity node"},
				"relation":   {"type": "string", "description": "Relationship type (e.g. LOCATED_AT, KNOWS, AFFILIATED_WITH, HAS, PART_OF)"},
				"to_id":      {"type": "string", "description": "ID of the target entity node"},
				"properties": {"type": "object", "description": "Allowed keys: role, valid_from, valid_to, notes"}
			},
			"required": ["from_id", "relation", "to_id"]
		}`),
	}
}

func (t *LinkEntitiesTool) Execute(ctx context.Context, params map[string]interface{}) (json.RawMessage, error) {
	fromID, _ := params["from_id"].(string)
	relation, _ := params["relation"].(string)
	toID, _ := params["to_id"].(string)

	if fromID == "" || relation == "" || toID == "" {
		return nil, fmt.Errorf("from_id, relation and to_id are required")
	}
	if !validRelationTypes[relation] {
		return nil, fmt.Errorf("unknown relation %q; see allowed list", relation)
	}

	// Verify both nodes exist before creating the relationship.
	checkCypher := fmt.Sprintf(`MATCH (a {id: %s}), (b {id: %s}) RETURN count(*) AS cnt`,
		jsonStr(fromID), jsonStr(toID))
	checkResult, checkErr := t.Tools.Memory.QueryGraph(ctx, checkCypher)
	if isCypherUnsupported(checkErr) {
		return json.RawMessage(entityNeoUnavailable), nil
	}
	if checkErr != nil {
		return nil, fmt.Errorf("link_entities: %w", checkErr)
	}
	if len(checkResult.Data) == 0 {
		return nil, fmt.Errorf("link_entities: one or both nodes not found (from_id=%s, to_id=%s)", fromID, toID)
	}

	now := time.Now().UTC().Format(time.RFC3339)
	relProps := map[string]interface{}{}
	if p, ok := params["properties"].(map[string]interface{}); ok {
		for k, v := range p {
			relProps[k] = v
		}
	}
	if err := validatePropertyKeys(relProps, validRelPropertyKeys); err != nil {
		return nil, fmt.Errorf("link_entities properties: %w", err)
	}
	relPropCypher := buildCypherPropsLiteral("r", relProps)

	cypher := fmt.Sprintf(`
MATCH (a {id: %s}), (b {id: %s})
MERGE (a)-[r:%s]->(b)
ON CREATE SET r.valid_from = %s, r.source = "conversation"%s
RETURN type(r) AS relation`,
		jsonStr(fromID),
		jsonStr(toID),
		relation,
		jsonStr(now),
		relPropCypher,
	)

	_, err := t.Tools.Memory.QueryGraph(ctx, cypher)
	if isCypherUnsupported(err) {
		return json.RawMessage(entityNeoUnavailable), nil
	}
	if err != nil {
		return nil, fmt.Errorf("link_entities: %w", err)
	}
	return json.RawMessage(`{"status":"ok"}`), nil
}

// ─────────────────────────────────────────────────────────────
// FindEntityTool
// ─────────────────────────────────────────────────────────────

type FindEntityTool struct {
	Tools InternalTools
}

func (t *FindEntityTool) Definition() ToolDefinition {
	return ToolDefinition{
		Name: "find_entity",
		Description: "Look up an entity node by type and name. " +
			"Returns its ID, all properties, and a summary of immediate relationships.",
		InputSchema: json.RawMessage(`{
			"type": "object",
			"properties": {
				"type": {"type": "string", "description": "Entity label: Person | Pet | Place | Organization | Event | Goal | Asset | Topic | Memory"},
				"name": {"type": "string", "description": "Canonical name of the entity"}
			},
			"required": ["type", "name"]
		}`),
	}
}

func (t *FindEntityTool) Execute(ctx context.Context, params map[string]interface{}) (json.RawMessage, error) {
	entityType, _ := params["type"].(string)
	name, _ := params["name"].(string)
	if entityType == "" || name == "" {
		return nil, fmt.Errorf("type and name are required")
	}

	cypher := fmt.Sprintf(`
MATCH (e:%s {name: %s})
OPTIONAL MATCH (e)-[r]-(other)
RETURN e.id AS id, e AS props,
       collect({rel: type(r), target_name: other.name, target_type: labels(other)[0]}) AS relationships
LIMIT 1`,
		entityType, jsonStr(name),
	)

	result, err := t.Tools.Memory.QueryGraph(ctx, cypher)
	if isCypherUnsupported(err) {
		return json.RawMessage(entityNeoUnavailable), nil
	}
	if err != nil {
		return nil, fmt.Errorf("find_entity: %w", err)
	}
	if len(result.Data) == 0 {
		return json.RawMessage(fmt.Sprintf(`{"found":false,"type":%s,"name":%s}`,
			jsonStr(entityType), jsonStr(name))), nil
	}

	row := result.Data[0]
	out, _ := json.Marshal(map[string]interface{}{
		"found":         true,
		"id":            row["id"],
		"properties":    row["props"],
		"relationships": row["relationships"],
	})
	return json.RawMessage(out), nil
}

// ─────────────────────────────────────────────────────────────
// GetEntityContextTool
// ─────────────────────────────────────────────────────────────

type GetEntityContextTool struct {
	Tools InternalTools
}

func (t *GetEntityContextTool) Definition() ToolDefinition {
	return ToolDefinition{
		Name: "get_entity_context",
		Description: "Traverse from an entity up to 1 hop and return a natural-language summary of what is known about it. " +
			"Use this to enrich conversation context when the user mentions a person, pet, place, or other entity.",
		InputSchema: json.RawMessage(`{
			"type": "object",
			"properties": {
				"entity_id": {"type": "string", "description": "ID of the entity (use this OR name+type)"},
				"name":      {"type": "string", "description": "Entity name (use with type when ID is unknown)"},
				"type":      {"type": "string", "description": "Entity label when looking up by name"}
			}
		}`),
	}
}

func (t *GetEntityContextTool) Execute(ctx context.Context, params map[string]interface{}) (json.RawMessage, error) {
	entityID, _ := params["entity_id"].(string)
	name, _ := params["name"].(string)
	entityType, _ := params["type"].(string)

	var matchClause string
	if entityID != "" {
		matchClause = fmt.Sprintf("MATCH (e {id: %s})", jsonStr(entityID))
	} else if name != "" {
		if entityType != "" {
			matchClause = fmt.Sprintf("MATCH (e:%s {name: %s})", entityType, jsonStr(name))
		} else {
			matchClause = fmt.Sprintf("MATCH (e {name: %s})", jsonStr(name))
		}
	} else {
		return nil, fmt.Errorf("entity_id or name is required")
	}

	cypher := fmt.Sprintf(`
%s
OPTIONAL MATCH (e)-[r]-(neighbor)
RETURN labels(e)[0] AS nodeType, e.name AS nodeName, e AS nodeProps,
       collect({
           rel:         type(r),
           direction:   CASE WHEN startNode(r) = e THEN "outgoing" ELSE "incoming" END,
           target_name: neighbor.name,
           target_type: labels(neighbor)[0],
           rel_props:   properties(r)
       }) AS edges
LIMIT 1`, matchClause)

	result, err := t.Tools.Memory.QueryGraph(ctx, cypher)
	if isCypherUnsupported(err) {
		return json.RawMessage(entityNeoUnavailable), nil
	}
	if err != nil {
		return nil, fmt.Errorf("get_entity_context: %w", err)
	}
	if len(result.Data) == 0 {
		return json.RawMessage(`{"found":false}`), nil
	}

	row := result.Data[0]
	summary := buildContextSummary(row)
	out, _ := json.Marshal(map[string]interface{}{
		"found":   true,
		"summary": summary,
		"raw":     row,
	})
	return json.RawMessage(out), nil
}

// buildContextSummary converts a raw graph row into readable text.
func buildContextSummary(row map[string]interface{}) string {
	nodeType, _ := row["nodeType"].(string)
	nodeName, _ := row["nodeName"].(string)

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("%s: %s\n", nodeType, nodeName))

	edges, ok := row["edges"].([]interface{})
	if !ok {
		return sb.String()
	}
	for _, e := range edges {
		em, ok := e.(map[string]interface{})
		if !ok {
			continue
		}
		rel, _ := em["rel"].(string)
		dir, _ := em["direction"].(string)
		targetName, _ := em["target_name"].(string)
		targetType, _ := em["target_type"].(string)
		if rel == "" {
			continue
		}
		arrow := "→"
		if dir == "incoming" {
			arrow = "←"
		}
		// Include valid_from date if present on the relationship
		suffix := ""
		if rp, ok := em["rel_props"].(map[string]interface{}); ok {
			if vf, ok := rp["valid_from"].(string); ok && vf != "" {
				if len(vf) >= 4 {
					suffix = fmt.Sprintf(" (since %s)", vf[:4])
				}
			}
		}
		sb.WriteString(fmt.Sprintf("  %s %s — %s (%s)%s\n", arrow, rel, targetName, targetType, suffix))
	}
	return sb.String()
}

// ─────────────────────────────────────────────────────────────
// ListEntitiesTool
// ─────────────────────────────────────────────────────────────

type ListEntitiesTool struct {
	Tools InternalTools
}

func (t *ListEntitiesTool) Definition() ToolDefinition {
	return ToolDefinition{
		Name: "list_entities",
		Description: "List entity nodes linked to the user. " +
			"Optionally filter by entity type (Person, Pet, Place, …) or relationship type (SPOUSE_OF, LIKES, …).",
		InputSchema: json.RawMessage(`{
			"type": "object",
			"properties": {
				"type":     {"type": "string", "description": "Filter by entity label (omit for all types)"},
				"relation": {"type": "string", "description": "Filter by relationship type from user to entity"},
				"for_user": {"type": "string", "description": "User display name. Only for consolidation agents."}
			}
		}`),
	}
}

func (t *ListEntitiesTool) Execute(ctx context.Context, params map[string]interface{}) (json.RawMessage, error) {
	entityType, _ := params["type"].(string)
	relation, _ := params["relation"].(string)
	userID := resolveEntityUserID(ctx, params)

	relFilter := ""
	if relation != "" {
		relFilter = ":" + relation
	}
	typeFilter := ""
	if entityType != "" {
		typeFilter = ":" + entityType
	}

	var cypher string
	if userID != "" {
		cypher = fmt.Sprintf(`
MATCH (u:User) WHERE u.id = %s OR u.name = %s
WITH u
MATCH (u)-[r%s]->(e%s)
WHERE NOT e:User
RETURN labels(e)[0] AS type, e.id AS id, e.name AS name,
       type(r) AS relation, r.valid_from AS valid_from, r.valid_to AS valid_to
ORDER BY type, name`,
			jsonStr(userID), jsonStr(userID), relFilter, typeFilter,
		)
	} else {
		// No user context — list all non-User entities (useful for archivist runs)
		cypher = fmt.Sprintf(`
MATCH (e%s)
WHERE NOT e:User
OPTIONAL MATCH (:User)-[r%s]->(e)
RETURN labels(e)[0] AS type, e.id AS id, e.name AS name,
       type(r) AS relation, r.valid_from AS valid_from, r.valid_to AS valid_to
ORDER BY type, name`,
			typeFilter, relFilter,
		)
	}

	result, err := t.Tools.Memory.QueryGraph(ctx, cypher)
	if isCypherUnsupported(err) {
		return json.RawMessage(entityNeoUnavailable), nil
	}
	if err != nil {
		return nil, fmt.Errorf("list_entities: %w", err)
	}

	out, _ := json.Marshal(map[string]interface{}{
		"count":    len(result.Data),
		"entities": result.Data,
	})
	return json.RawMessage(out), nil
}

// ─────────────────────────────────────────────────────────────
// ExpireRelationshipTool
// ─────────────────────────────────────────────────────────────

type ExpireRelationshipTool struct {
	Tools InternalTools
}

func (t *ExpireRelationshipTool) Definition() ToolDefinition {
	return ToolDefinition{
		Name: "expire_relationship",
		Description: "Set valid_to = now on a specific relationship to mark it as no longer current " +
			"(e.g. old job ended, asset sold, address changed). Preserves history rather than deleting.",
		InputSchema: json.RawMessage(`{
			"type": "object",
			"properties": {
				"from_id":  {"type": "string", "description": "ID of the source entity"},
				"relation": {"type": "string", "description": "Relationship type to expire"},
				"to_id":    {"type": "string", "description": "ID of the target entity"},
				"reason":   {"type": "string", "description": "Optional human-readable reason stored on the relationship"}
			},
			"required": ["from_id", "relation", "to_id"]
		}`),
	}
}

func (t *ExpireRelationshipTool) Execute(ctx context.Context, params map[string]interface{}) (json.RawMessage, error) {
	fromID, _ := params["from_id"].(string)
	relation, _ := params["relation"].(string)
	toID, _ := params["to_id"].(string)
	reason, _ := params["reason"].(string)

	if fromID == "" || relation == "" || toID == "" {
		return nil, fmt.Errorf("from_id, relation and to_id are required")
	}

	now := time.Now().UTC().Format(time.RFC3339)
	reasonClause := ""
	if reason != "" {
		reasonClause = fmt.Sprintf(", r.expiry_reason = %s", jsonStr(reason))
	}

	cypher := fmt.Sprintf(`
MATCH (a {id: %s})-[r:%s]->(b {id: %s})
WHERE r.valid_to IS NULL
SET r.valid_to = %s%s
RETURN count(r) AS expired`,
		jsonStr(fromID), relation, jsonStr(toID),
		jsonStr(now), reasonClause,
	)

	result, err := t.Tools.Memory.QueryGraph(ctx, cypher)
	if isCypherUnsupported(err) {
		return json.RawMessage(entityNeoUnavailable), nil
	}
	if err != nil {
		return nil, fmt.Errorf("expire_relationship: %w", err)
	}

	expired := 0
	if len(result.Data) > 0 {
		if v, ok := result.Data[0]["expired"]; ok {
			switch n := v.(type) {
			case int64:
				expired = int(n)
			case float64:
				expired = int(n)
			}
		}
	}
	return json.RawMessage(fmt.Sprintf(`{"status":"ok","expired":%d}`, expired)), nil
}

// ─────────────────────────────────────────────────────────────
// UpsertAssertionTool
// ─────────────────────────────────────────────────────────────

type UpsertAssertionTool struct {
	Tools InternalTools
}

func (t *UpsertAssertionTool) Definition() ToolDefinition {
	return ToolDefinition{
		Name: "upsert_assertion",
		Description: "Create or update an Assertion node — a claim extracted from conversation. " +
			"Assertions serve as a staging area with confidence tracking before promotion to typed entities. " +
			"Duplicate content (by hash) increments mention_count and bumps confidence.",
		InputSchema: json.RawMessage(`{
			"type": "object",
			"properties": {
				"content":         {"type": "string", "description": "The claim text (max 2000 chars)"},
				"label":           {"type": "string", "description": "Short display label for the assertion"},
				"confidence":      {"type": "number", "description": "0.0-1.0; 0.8=explicit, 0.5=implied, 0.3=uncertain"},
				"valid_from":      {"type": "string", "description": "RFC3339 timestamp for when the claim became true"},
				"about_entity_id": {"type": "string", "description": "ID of an existing entity this assertion is about"},
				"conversation_id": {"type": "string", "description": "ID of the source conversation"},
				"for_user":        {"type": "string", "description": "User display name. Only for consolidation agents."}
			},
			"required": ["content"]
		}`),
	}
}

func (t *UpsertAssertionTool) Execute(ctx context.Context, params map[string]interface{}) (json.RawMessage, error) {
	content, _ := params["content"].(string)
	if content == "" {
		return nil, fmt.Errorf("content is required")
	}
	if len(content) > 2000 {
		return nil, fmt.Errorf("content exceeds 2000 character limit")
	}

	label, _ := params["label"].(string)
	if label == "" {
		label = content
		if len(label) > 80 {
			label = label[:80]
		}
	}

	conf := 0.5
	if c, ok := params["confidence"].(float64); ok {
		conf = c
	}
	if conf < 0.0 {
		conf = 0.0
	}
	if conf > 1.0 {
		conf = 1.0
	}

	validFrom, _ := params["valid_from"].(string)
	aboutEntityID, _ := params["about_entity_id"].(string)
	convID, _ := params["conversation_id"].(string)

	userID := resolveEntityUserID(ctx, params)
	if userID == "" {
		return nil, fmt.Errorf("could not determine user identity")
	}

	now := time.Now().UTC().Format(time.RFC3339)
	hash := contentHash(content)

	cypher := fmt.Sprintf(`
MERGE (a:Assertion {contentHash: %s})
ON CREATE SET a.id = randomUUID(), a.content = %s, a.label = %s,
              a.confidence = %f, a.valid_from = %s,
              a.txn_created_at = %s, a.source = "conversation",
              a.conversation_id = %s, a.mention_count = 1, a.promoted = false
ON MATCH SET  a.txn_updated_at = %s,
              a.mention_count = a.mention_count + 1,
              a.confidence = CASE WHEN a.confidence + 0.1 > 1.0 THEN 1.0
                             ELSE a.confidence + 0.1 END
WITH a
MATCH (u:User) WHERE u.id = %s OR u.name = %s
MERGE (u)-[r:ASSERTED]->(a)
ON CREATE SET r.txn_created_at = %s
RETURN a.id AS id, a.confidence AS confidence, a.mention_count AS mentions`,
		jsonStr(hash),
		jsonStr(content),
		jsonStr(label),
		conf,
		jsonStr(validFrom),
		jsonStr(now),
		jsonStr(convID),
		jsonStr(now),
		jsonStr(userID),
		jsonStr(userID),
		jsonStr(now),
	)

	result, err := t.Tools.Memory.QueryGraph(ctx, cypher)
	if isCypherUnsupported(err) {
		return json.RawMessage(entityNeoUnavailable), nil
	}
	if err != nil {
		return nil, fmt.Errorf("upsert_assertion: %w", err)
	}

	// If about_entity_id is provided, create ABOUT edge.
	if aboutEntityID != "" {
		aboutCypher := fmt.Sprintf(`
MATCH (a:Assertion {contentHash: %s}), (e {id: %s})
MERGE (a)-[r:ABOUT]->(e)
ON CREATE SET r.txn_created_at = %s
RETURN type(r) AS rel`,
			jsonStr(hash), jsonStr(aboutEntityID), jsonStr(now))
		_, aboutErr := t.Tools.Memory.QueryGraph(ctx, aboutCypher)
		if aboutErr != nil && !isCypherUnsupported(aboutErr) {
			return nil, fmt.Errorf("upsert_assertion: failed to link ABOUT: %w", aboutErr)
		}
	}

	id := ""
	confidence := conf
	mentions := 1
	if len(result.Data) > 0 {
		if v, ok := result.Data[0]["id"]; ok {
			id = fmt.Sprintf("%v", v)
		}
		if v, ok := result.Data[0]["confidence"]; ok {
			if n, ok := v.(float64); ok {
				confidence = n
			}
		}
		if v, ok := result.Data[0]["mentions"]; ok {
			switch n := v.(type) {
			case int64:
				mentions = int(n)
			case float64:
				mentions = int(n)
			}
		}
	}
	return json.RawMessage(fmt.Sprintf(`{"status":"ok","id":%s,"confidence":%.2f,"mentions":%d}`,
		jsonStr(id), confidence, mentions)), nil
}

// ─────────────────────────────────────────────────────────────
// CreateEpisodeTool
// ─────────────────────────────────────────────────────────────

type CreateEpisodeTool struct {
	Tools InternalTools
}

func (t *CreateEpisodeTool) Definition() ToolDefinition {
	return ToolDefinition{
		Name: "create_episode",
		Description: "Create an Episode node — a time-bounded container grouping assertions from one consolidation run. " +
			"Returns the episode ID for linking assertions via IN_EPISODE relationships.",
		InputSchema: json.RawMessage(`{
			"type": "object",
			"properties": {
				"label":           {"type": "string", "description": "Episode label, e.g. 'Consolidation 2026-03-20'"},
				"conversation_id": {"type": "string", "description": "Source conversation ID"},
				"for_user":        {"type": "string", "description": "User display name. Only for consolidation agents."}
			},
			"required": ["label"]
		}`),
	}
}

func (t *CreateEpisodeTool) Execute(ctx context.Context, params map[string]interface{}) (json.RawMessage, error) {
	label, _ := params["label"].(string)
	if label == "" {
		return nil, fmt.Errorf("label is required")
	}

	convID, _ := params["conversation_id"].(string)

	userID := resolveEntityUserID(ctx, params)
	if userID == "" {
		return nil, fmt.Errorf("could not determine user identity")
	}

	now := time.Now().UTC().Format(time.RFC3339)

	cypher := fmt.Sprintf(`
CREATE (ep:Episode {id: randomUUID(), label: %s, started_at: %s,
        conversation_id: %s, source: "consolidation", txn_created_at: %s})
WITH ep
MATCH (u:User) WHERE u.id = %s OR u.name = %s
MERGE (ep)-[r:INVOLVES]->(u)
ON CREATE SET r.txn_created_at = %s
RETURN ep.id AS id`,
		jsonStr(label),
		jsonStr(now),
		jsonStr(convID),
		jsonStr(now),
		jsonStr(userID),
		jsonStr(userID),
		jsonStr(now),
	)

	result, err := t.Tools.Memory.QueryGraph(ctx, cypher)
	if isCypherUnsupported(err) {
		return json.RawMessage(entityNeoUnavailable), nil
	}
	if err != nil {
		return nil, fmt.Errorf("create_episode: %w", err)
	}

	id := ""
	if len(result.Data) > 0 {
		if v, ok := result.Data[0]["id"]; ok {
			id = fmt.Sprintf("%v", v)
		}
	}
	return json.RawMessage(fmt.Sprintf(`{"status":"ok","id":%s}`, jsonStr(id))), nil
}

// ─────────────────────────────────────────────────────────────
// ListAssertionsTool
// ─────────────────────────────────────────────────────────────

type ListAssertionsTool struct {
	Tools InternalTools
}

func (t *ListAssertionsTool) Definition() ToolDefinition {
	return ToolDefinition{
		Name: "list_assertions",
		Description: "List Assertion nodes linked to a user. " +
			"Optionally filter by minimum confidence and promotion status. " +
			"Used by the Archivist to find assertions ready for promotion.",
		InputSchema: json.RawMessage(`{
			"type": "object",
			"properties": {
				"for_user":        {"type": "string", "description": "User display name."},
				"min_confidence":  {"type": "number", "description": "Minimum confidence threshold (0.0-1.0)"},
				"unpromoted_only": {"type": "boolean", "description": "If true, only return assertions not yet promoted"},
				"limit":           {"type": "integer", "description": "Maximum number of results (default 50)"}
			}
		}`),
	}
}

func (t *ListAssertionsTool) Execute(ctx context.Context, params map[string]interface{}) (json.RawMessage, error) {
	userID := resolveEntityUserID(ctx, params)

	minConf := 0.0
	if c, ok := params["min_confidence"].(float64); ok {
		minConf = c
	}
	unpromotedOnly := false
	if u, ok := params["unpromoted_only"].(bool); ok {
		unpromotedOnly = u
	}
	limit := 50
	if l, ok := params["limit"].(float64); ok && l > 0 {
		limit = int(l)
	}
	if limit > 200 {
		limit = 200
	}

	var whereClauses []string
	whereClauses = append(whereClauses, fmt.Sprintf("a.confidence >= %f", minConf))
	if unpromotedOnly {
		whereClauses = append(whereClauses, "a.promoted = false")
	}
	whereStr := strings.Join(whereClauses, " AND ")

	var cypher string
	if userID != "" {
		cypher = fmt.Sprintf(`
MATCH (u:User)-[:ASSERTED]->(a:Assertion)
WHERE (u.id = %s OR u.name = %s) AND %s
RETURN a.id AS id, a.label AS label, a.content AS content,
       a.confidence AS confidence, a.mention_count AS mention_count,
       a.promoted AS promoted, a.txn_created_at AS created_at,
       a.valid_from AS valid_from, a.valid_to AS valid_to
ORDER BY a.confidence DESC, a.mention_count DESC
LIMIT %d`,
			jsonStr(userID), jsonStr(userID), whereStr, limit)
	} else {
		cypher = fmt.Sprintf(`
MATCH (a:Assertion)
WHERE %s
RETURN a.id AS id, a.label AS label, a.content AS content,
       a.confidence AS confidence, a.mention_count AS mention_count,
       a.promoted AS promoted, a.txn_created_at AS created_at,
       a.valid_from AS valid_from, a.valid_to AS valid_to
ORDER BY a.confidence DESC, a.mention_count DESC
LIMIT %d`,
			whereStr, limit)
	}

	result, err := t.Tools.Memory.QueryGraph(ctx, cypher)
	if isCypherUnsupported(err) {
		return json.RawMessage(entityNeoUnavailable), nil
	}
	if err != nil {
		return nil, fmt.Errorf("list_assertions: %w", err)
	}

	out, _ := json.Marshal(map[string]interface{}{
		"count":      len(result.Data),
		"assertions": result.Data,
	})
	return json.RawMessage(out), nil
}

// ─────────────────────────────────────────────────────────────
// PromoteAssertionTool
// ─────────────────────────────────────────────────────────────

type PromoteAssertionTool struct {
	Tools InternalTools
}

func (t *PromoteAssertionTool) Definition() ToolDefinition {
	return ToolDefinition{
		Name: "promote_assertion",
		Description: "Promote a high-confidence Assertion into a typed entity node. " +
			"Creates the entity, links it to the assertion via ABOUT, and sets promoted=true. " +
			"The assertion is preserved as provenance — never deleted.",
		InputSchema: json.RawMessage(`{
			"type": "object",
			"properties": {
				"assertion_id": {"type": "string", "description": "ID of the Assertion to promote"},
				"entity_type":  {"type": "string", "description": "Person | Pet | Place | Organization | Event | Goal | Asset | Topic"},
				"entity_name":  {"type": "string", "description": "Canonical name for the entity"},
				"relation":     {"type": "string", "description": "Relationship type from user to entity (e.g. KNOWS, LOCATED_AT, HAS). Defaults to HAS_ENTITY."},
				"properties":   {"type": "object", "description": "Allowed keys: description, category, notes, url, species, breed, industry, city, country, address, date, deadline, status, make, model, year, email, phone"},
				"rel_props":    {"type": "object", "description": "Allowed keys: role, valid_from, valid_to, notes"},
				"for_user":     {"type": "string", "description": "User display name."}
			},
			"required": ["assertion_id", "entity_type", "entity_name"]
		}`),
	}
}

func (t *PromoteAssertionTool) Execute(ctx context.Context, params map[string]interface{}) (json.RawMessage, error) {
	assertionID, _ := params["assertion_id"].(string)
	entityType, _ := params["entity_type"].(string)
	entityName, _ := params["entity_name"].(string)

	if assertionID == "" || entityType == "" || entityName == "" {
		return nil, fmt.Errorf("assertion_id, entity_type, and entity_name are required")
	}

	validTypes := map[string]bool{
		"Person": true, "Pet": true, "Place": true, "Organization": true,
		"Event": true, "Goal": true, "Asset": true, "Topic": true,
	}
	if !validTypes[entityType] {
		return nil, fmt.Errorf("entity_type must be one of: Person, Pet, Place, Organization, Event, Goal, Asset, Topic")
	}

	relation, _ := params["relation"].(string)
	if relation == "" {
		relation = "HAS_ENTITY"
	}
	if !validRelationTypes[relation] {
		return nil, fmt.Errorf("unknown relation %q; see allowed list", relation)
	}

	userID := resolveEntityUserID(ctx, params)
	if userID == "" {
		return nil, fmt.Errorf("could not determine user identity")
	}

	now := time.Now().UTC().Format(time.RFC3339)

	// Build property map for the entity node.
	properties := map[string]interface{}{}
	if p, ok := params["properties"].(map[string]interface{}); ok {
		for k, v := range p {
			properties[k] = v
		}
	}
	if err := validatePropertyKeys(properties, validNodePropertyKeys); err != nil {
		return nil, fmt.Errorf("promote_assertion properties: %w", err)
	}
	nodePropCypher := buildCypherPropsLiteral("e", properties)

	// Build relationship property map.
	relProps := map[string]interface{}{}
	if rp, ok := params["rel_props"].(map[string]interface{}); ok {
		for k, v := range rp {
			relProps[k] = v
		}
	}
	if err := validatePropertyKeys(relProps, validRelPropertyKeys); err != nil {
		return nil, fmt.Errorf("promote_assertion rel_props: %w", err)
	}
	relPropCypher := buildCypherPropsLiteral("r", relProps)

	// Verify assertion exists.
	checkCypher := fmt.Sprintf(
		`MATCH (a:Assertion {id: %s}) RETURN a.promoted AS promoted`,
		jsonStr(assertionID))
	checkResult, checkErr := t.Tools.Memory.QueryGraph(ctx, checkCypher)
	if isCypherUnsupported(checkErr) {
		return json.RawMessage(entityNeoUnavailable), nil
	}
	if checkErr != nil {
		return nil, fmt.Errorf("promote_assertion: %w", checkErr)
	}
	if len(checkResult.Data) == 0 {
		return nil, fmt.Errorf("promote_assertion: assertion %s not found", assertionID)
	}

	// Create/merge entity, link user→entity, link assertion→entity, set promoted=true.
	cypher := fmt.Sprintf(`
MATCH (a:Assertion {id: %s})
MERGE (e:%s {name: %s})
ON CREATE SET e.id = randomUUID(), e.extractedAt = %s, e.source = "assertion",
              e.txn_created_at = %s%s
ON MATCH  SET e.txn_updated_at = %s%s
WITH a, e
MATCH (u:User) WHERE u.id = %s OR u.name = %s
MERGE (u)-[r:%s]->(e)
ON CREATE SET r.valid_from = %s, r.source = "assertion", r.txn_created_at = %s%s
WITH a, e
MERGE (a)-[ab:ABOUT]->(e)
ON CREATE SET ab.txn_created_at = %s
SET a.promoted = true, a.txn_updated_at = %s
RETURN e.id AS entity_id, e.name AS entity_name, a.id AS assertion_id`,
		jsonStr(assertionID),
		entityType,
		jsonStr(entityName),
		jsonStr(now),
		jsonStr(now),
		nodePropCypher,
		jsonStr(now),
		nodePropCypher,
		jsonStr(userID),
		jsonStr(userID),
		relation,
		jsonStr(now),
		jsonStr(now),
		relPropCypher,
		jsonStr(now),
		jsonStr(now),
	)

	result, err := t.Tools.Memory.QueryGraph(ctx, cypher)
	if isCypherUnsupported(err) {
		return json.RawMessage(entityNeoUnavailable), nil
	}
	if err != nil {
		return nil, fmt.Errorf("promote_assertion: %w", err)
	}

	entityID := ""
	if len(result.Data) > 0 {
		if v, ok := result.Data[0]["entity_id"]; ok {
			entityID = fmt.Sprintf("%v", v)
		}
	}
	return json.RawMessage(fmt.Sprintf(`{"status":"ok","entity_id":%s,"entity_name":%s,"entity_type":%s,"assertion_id":%s}`,
		jsonStr(entityID), jsonStr(entityName), jsonStr(entityType), jsonStr(assertionID))), nil
}

// ─────────────────────────────────────────────────────────────
// Registration
// ─────────────────────────────────────────────────────────────

// RegisterEntityTools registers all entity and assertion management tools into the registry.
// Called once at the end of RegisterAllInternalTools.
func RegisterEntityTools(reg *ToolRegistry, tools InternalTools) {
	if tools.Memory == nil {
		return
	}
	reg.RegisterInternal("upsert_entity", &UpsertEntityTool{Tools: tools})
	reg.RegisterInternal("link_entities", &LinkEntitiesTool{Tools: tools})
	reg.RegisterInternal("find_entity", &FindEntityTool{Tools: tools})
	reg.RegisterInternal("get_entity_context", &GetEntityContextTool{Tools: tools})
	reg.RegisterInternal("list_entities", &ListEntitiesTool{Tools: tools})
	reg.RegisterInternal("expire_relationship", &ExpireRelationshipTool{Tools: tools})
	reg.RegisterInternal("upsert_assertion", &UpsertAssertionTool{Tools: tools})
	reg.RegisterInternal("promote_assertion", &PromoteAssertionTool{Tools: tools})
	reg.RegisterInternal("list_assertions", &ListAssertionsTool{Tools: tools})
	reg.RegisterInternal("create_episode", &CreateEpisodeTool{Tools: tools})
}

// ─────────────────────────────────────────────────────────────
// Cypher helpers
// ─────────────────────────────────────────────────────────────

// contentHash returns a truncated SHA-256 hex digest for deduplication.
func contentHash(s string) string {
	normalized := strings.ToLower(strings.TrimSpace(s))
	h := sha256.Sum256([]byte(normalized))
	return hex.EncodeToString(h[:16])
}

// jsonStr returns a Cypher-safe JSON string literal (double-quoted, escaped).
func jsonStr(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// buildCypherPropsLiteral converts a map of extra properties into a Cypher SET
// fragment of the form `, <var>.key = "value"`. Returns empty string if map is empty.
// varName is the Cypher binding variable (e.g. "e" for nodes, "r" for relationships).
func buildCypherPropsLiteral(varName string, props map[string]interface{}) string {
	if len(props) == 0 {
		return ""
	}
	var sb strings.Builder
	for k, v := range props {
		val, _ := json.Marshal(v)
		sb.WriteString(fmt.Sprintf(", %s.%s = %s", varName, k, string(val)))
	}
	return sb.String()
}
