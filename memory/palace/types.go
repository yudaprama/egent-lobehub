// Package palace implements the structured user-memory persistence layer
// in egent-lobehub. It is the Go replacement for the LobeHub TypeScript
// `userMemory` tRPC router and the parallel `user_memories_*` Postgres
// tables.
//
// The palace model has five layers — identity, activity, context,
// experience, preference — plus a versioned persona document. Each
// row in `user_memories` is the parent of one row in one of the layer
// tables. Vectors (1024-d) live on the parent (`summary_vector_1024`,
// `details_vector_1024`) and on selected layer-specific columns
// (e.g. `user_memories_identities.description_vector`).
//
// Phase 2 of USER_MEMORY_MIGRATION_PLAN.md ships this package as the
// write path; Phase 1 SQL templates remain the read path.
package palace

import (
	"encoding/json"
	"time"
)

// IdentityType is the typed identity dimension used by
// user_memories_identities.type. Mirrors LobeHub IdentityTypeSchema.
type IdentityType string

const (
	IdentityPersonal     IdentityType = "personal"
	IdentityProfessional IdentityType = "professional"
	IdentityDemographic  IdentityType = "demographic"
)

// Valid reports whether t is one of the allowed identity types.
// Empty string is treated as valid (callers may omit the field).
func (t IdentityType) Valid() bool {
	switch t {
	case "", IdentityPersonal, IdentityProfessional, IdentityDemographic:
		return true
	}
	return false
}

// ActivityStatus is the typed activity dimension used by
// user_memories_activities.status.
type ActivityStatus string

const (
	ActivityPending   ActivityStatus = "pending"
	ActivityActive    ActivityStatus = "active"
	ActivityCompleted ActivityStatus = "completed"
	ActivityCancelled ActivityStatus = "cancelled"
)

// MemoryLayer is the typed layer dimension. Mirrors LobeHub memory_layer.
type MemoryLayer string

const (
	LayerIdentity   MemoryLayer = "identity"
	LayerActivity   MemoryLayer = "activity"
	LayerContext    MemoryLayer = "context"
	LayerExperience MemoryLayer = "experience"
	LayerPreference MemoryLayer = "preference"
)

// Valid reports whether l is one of the five allowed layers.
// Empty string is treated as valid (callers may omit the field).
func (l MemoryLayer) Valid() bool {
	switch l {
	case "", LayerIdentity, LayerActivity, LayerContext, LayerExperience, LayerPreference:
		return true
	}
	return false
}

// JSONMap is a free-form JSONB column on every palace row. It round-trips
// the LobeHub labels/metadata semantics without forcing a struct shape.
type JSONMap map[string]any

// MarshalJSON implements json.Marshaler so nil maps render as `null`
// (matches Postgres jsonb behavior). When a caller wants an empty
// object they should initialize the map.
func (m JSONMap) MarshalJSON() ([]byte, error) {
	if m == nil {
		return []byte("null"), nil
	}
	return json.Marshal(map[string]any(m))
}

// BaseMemory is the shared parent row (user_memories). Every layer row
// links back via the foreign-key-equivalent `user_memory_id` and inherits
// the user_id scope from the parent.
type BaseMemory struct {
	ID            string     `json:"id"`
	UserID        string     `json:"userId"`
	MemoryLayer   MemoryLayer `json:"memoryLayer"`
	MemoryType    string     `json:"memoryType,omitempty"`
	MemoryCategory string    `json:"memoryCategory,omitempty"`
	Title         string     `json:"title,omitempty"`
	Summary       string     `json:"summary,omitempty"`
	Details       string     `json:"details,omitempty"`
	Status        string     `json:"status,omitempty"`
	Tags          []string   `json:"tags,omitempty"`
	Metadata      JSONMap    `json:"metadata,omitempty"`

	AccessedCount  int64     `json:"accessedCount"`
	LastAccessedAt time.Time `json:"lastAccessedAt"`
	AccessedAt     time.Time `json:"accessedAt"`
	CreatedAt      time.Time `json:"createdAt"`
	UpdatedAt      time.Time `json:"updatedAt"`
	CapturedAt     time.Time `json:"capturedAt"`
}

// IdentityInput is the create payload for an identity layer row.
// Mirrors CreateUserMemoryIdentitySchema in
// lobehub/packages/types/src/userMemory/identity.ts.
type IdentityInput struct {
	UserMemoryID    string       `json:"userMemoryId,omitempty"`
	Description     string       `json:"description,omitempty"`
	Role            string       `json:"role,omitempty"`
	Relationship    string       `json:"relationship,omitempty"`
	Type            IdentityType `json:"type,omitempty"`
	EpisodicDate    *time.Time   `json:"episodicDate,omitempty"`
	ExtractedLabels []string     `json:"extractedLabels,omitempty"`
	Labels          JSONMap      `json:"labels,omitempty"`
}

// IdentityUpdate is the partial update payload. Mirrors
// UpdateUserMemoryIdentitySchema.
type IdentityUpdate struct {
	Description     *string       `json:"description,omitempty"`
	Role            *string       `json:"role,omitempty"`
	Relationship    *string       `json:"relationship,omitempty"`
	Type            *IdentityType `json:"type,omitempty"`
	EpisodicDate    *time.Time    `json:"episodicDate,omitempty"`
	ExtractedLabels *[]string     `json:"extractedLabels,omitempty"`
	Labels          JSONMap       `json:"labels,omitempty"`
}

// IdentityRow is the join of user_memories + user_memories_identities,
// the shape returned by GET /v1/memory/identities and the existing
// pREST template userMemoryIdentitiesByUser.read.sql.
type IdentityRow struct {
	BaseMemory
	IdentityID       string     `json:"identityId"`
	IdentityDescription string  `json:"identityDescription,omitempty"`
	IdentityRole     string     `json:"identityRole,omitempty"`
	IdentityRelationship string `json:"identityRelationship,omitempty"`
	IdentityType     IdentityType `json:"identityType,omitempty"`
	IdentityEpisodicDate *time.Time `json:"identityEpisodicDate,omitempty"`
	IdentityMetadata JSONMap    `json:"identityMetadata,omitempty"`
}

// ActivityInput is the create payload for an activity layer row.
type ActivityInput struct {
	UserMemoryID         string         `json:"userMemoryId,omitempty"`
	Type                 string         `json:"type"`
	Status               ActivityStatus `json:"status,omitempty"`
	Timezone             string         `json:"timezone,omitempty"`
	StartsAt             *time.Time     `json:"startsAt,omitempty"`
	EndsAt               *time.Time     `json:"endsAt,omitempty"`
	AssociatedObjects    JSONMap        `json:"associatedObjects,omitempty"`
	AssociatedSubjects   JSONMap        `json:"associatedSubjects,omitempty"`
	AssociatedLocations  JSONMap        `json:"associatedLocations,omitempty"`
	Notes                string         `json:"notes,omitempty"`
	Narrative            string         `json:"narrative,omitempty"`
	Feedback             string         `json:"feedback,omitempty"`
	ExtractedLabels      []string       `json:"extractedLabels,omitempty"`
	Labels               JSONMap        `json:"labels,omitempty"`
}

// ActivityUpdate is the partial update payload. Mirrors the inline
// z.object schema in userMemoryRouter.updateActivity.
type ActivityUpdate struct {
	Narrative *string         `json:"narrative,omitempty"`
	Notes     *string         `json:"notes,omitempty"`
	Status    *ActivityStatus `json:"status,omitempty"`
}

// ContextInput is the create payload for a context layer row.
// Contexts reference parent memories via the JSONB `user_memory_ids`
// array (not via FK) so they can fan out across multiple parent rows.
type ContextInput struct {
	UserMemoryIDs      []string  `json:"userMemoryIds,omitempty"`
	Title              string    `json:"title"`
	Description        string    `json:"description,omitempty"`
	Type               string    `json:"type,omitempty"`
	CurrentStatus      string    `json:"currentStatus,omitempty"`
	ScoreImpact        float64   `json:"scoreImpact,omitempty"`
	ScoreUrgency       float64   `json:"scoreUrgency,omitempty"`
	AssociatedObjects  JSONMap   `json:"associatedObjects,omitempty"`
	AssociatedSubjects JSONMap   `json:"associatedSubjects,omitempty"`
	ExtractedLabels    []string  `json:"extractedLabels,omitempty"`
	Labels             JSONMap   `json:"labels,omitempty"`
}

// ContextUpdate is the partial update payload.
type ContextUpdate struct {
	Title         *string `json:"title,omitempty"`
	Description   *string `json:"description,omitempty"`
	CurrentStatus *string `json:"currentStatus,omitempty"`
}

// ExperienceInput is the create payload for an experience layer row.
type ExperienceInput struct {
	UserMemoryID    string    `json:"userMemoryId,omitempty"`
	Type            string    `json:"type,omitempty"`
	Situation       string    `json:"situation,omitempty"`
	Reasoning       string    `json:"reasoning,omitempty"`
	PossibleOutcome string    `json:"possibleOutcome,omitempty"`
	Action          string    `json:"action,omitempty"`
	KeyLearning     string    `json:"keyLearning,omitempty"`
	ScoreConfidence float32   `json:"scoreConfidence,omitempty"`
	ExtractedLabels []string  `json:"extractedLabels,omitempty"`
	Labels          JSONMap   `json:"labels,omitempty"`
}

// ExperienceUpdate is the partial update payload.
type ExperienceUpdate struct {
	Situation   *string `json:"situation,omitempty"`
	Action      *string `json:"action,omitempty"`
	KeyLearning *string `json:"keyLearning,omitempty"`
}

// PreferenceInput is the create payload for a preference layer row.
type PreferenceInput struct {
	UserMemoryID        string   `json:"userMemoryId,omitempty"`
	ConclusionDirectives string  `json:"conclusionDirectives,omitempty"`
	Type                string   `json:"type,omitempty"`
	Suggestions         string   `json:"suggestions,omitempty"`
	ScorePriority       float64  `json:"scorePriority,omitempty"`
	ExtractedLabels     []string `json:"extractedLabels,omitempty"`
	Labels              JSONMap  `json:"labels,omitempty"`
}

// PreferenceUpdate is the partial update payload.
type PreferenceUpdate struct {
	ConclusionDirectives *string `json:"conclusionDirectives,omitempty"`
	Suggestions          *string `json:"suggestions,omitempty"`
}