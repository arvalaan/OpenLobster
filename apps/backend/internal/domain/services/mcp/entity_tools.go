package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// entityNeoUnavailable is returned when the graph backend does not support
// Cypher (e.g. the GML file backend). The caller can fall back to add_memory.
const entityNeoUnavailable = `{"error":"entity nodes require a Neo4j backend; use add_memory as a fallback"}`

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
			"OWNS / LEASES / SUBSCRIBES_TO / WORKS_AT / LIVES_AT relationships must always include valid_from in rel_props.",
		InputSchema: json.RawMessage(`{
			"type": "object",
			"properties": {
				"type":       {"type": "string", "description": "Entity label: Person | Pet | Place | Organization | Event | Goal | Asset | Topic"},
				"name":       {"type": "string", "description": "Canonical name — used as uniqueness key within the type"},
				"properties": {"type": "object", "description": "Extra key/value pairs merged onto the node (e.g. species, city, category)"},
				"relation":   {"type": "string", "description": "Relationship type from user to entity. Defaults to HAS_ENTITY."},
				"rel_props":  {"type": "object", "description": "Properties on the relationship (e.g. {\"valid_from\": \"2024-01-01T00:00:00Z\"})"},
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

	// Build relationship property map
	relProps := map[string]interface{}{}
	if rp, ok := params["rel_props"].(map[string]interface{}); ok {
		for k, v := range rp {
			relProps[k] = v
		}
	}
	if _, hasValidFrom := relProps["valid_from"]; !hasValidFrom {
		// Temporal relationships always need valid_from; set it to now if not provided
		transientRels := map[string]bool{
			"OWNS": true, "LEASES": true, "SUBSCRIBES_TO": true,
			"WORKS_AT": true, "LIVES_AT": true,
		}
		if transientRels[relation] {
			relProps["valid_from"] = now
		}
	}

	// Encode extra node props as a JSON object so we can embed them in Cypher params.
	// We pass everything via literal embedding (safe: all values are user-supplied
	// strings that pass through json.Marshal, never raw SQL).
	nodePropCypher := buildCypherPropsLiteral("e", properties)
	relPropCypher := buildCypherPropsLiteral("r", relProps)

	cypher := fmt.Sprintf(`
MERGE (e:%s {name: %s})
ON CREATE SET e.id = randomUUID(), e.extractedAt = %s, e.source = "conversation"%s
ON MATCH  SET e.extractedAt = %s%s
WITH e
MATCH (u:User) WHERE u.id = %s OR u.name = %s
MERGE (u)-[r:%s]->(e)
ON CREATE SET r.valid_from = %s, r.source = "conversation"%s
RETURN e.id AS id, e.name AS name`,
		entityType,
		jsonStr(name),
		jsonStr(now),
		nodePropCypher,
		jsonStr(now),
		nodePropCypher,
		jsonStr(userID),
		jsonStr(userID),
		relation,
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
			"Use this to connect entities to each other (e.g. Person LIVES_AT Place, Pet HAS_PET Person).",
		InputSchema: json.RawMessage(`{
			"type": "object",
			"properties": {
				"from_id":    {"type": "string", "description": "ID of the source entity node"},
				"relation":   {"type": "string", "description": "Relationship type (e.g. LIVES_AT, SPOUSE_OF, WORKS_AT)"},
				"to_id":      {"type": "string", "description": "ID of the target entity node"},
				"properties": {"type": "object", "description": "Optional properties merged onto the relationship (e.g. valid_from, confidence)"}
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

	now := time.Now().UTC().Format(time.RFC3339)
	relProps := map[string]interface{}{}
	if p, ok := params["properties"].(map[string]interface{}); ok {
		for k, v := range p {
			relProps[k] = v
		}
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
// Registration
// ─────────────────────────────────────────────────────────────

// RegisterEntityTools registers all six entity management tools into the registry.
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
}

// ─────────────────────────────────────────────────────────────
// Cypher helpers
// ─────────────────────────────────────────────────────────────

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
