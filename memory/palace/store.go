package palace

import (
	"context"
	"time"
)

// Store is the structured-memory persistence interface backed by
// Postgres + pgvector. It replaces the LobeHub TypeScript
// UserMemoryModel / UserMemoryIdentityModel / UserMemoryActivityModel /
// UserMemoryContextModel / UserMemoryExperienceModel / UserMemoryPreferenceModel
// and the parallel lambda/userMemory.ts tRPC router.
//
// All operations are user-scoped — the userID parameter is the tenant
// filter, mirroring pREST [[auth.user_id_filters]] on the underlying
// tables. The caller is responsible for extracting userID from the
// authenticated request (e.g. from x-arch-actor-id header).
type Store interface {
	// CreateIdentity inserts a parent user_memories row plus a child
	// user_memories_identities row in one transaction. The returned ID
	// is the identity row's id (matches the existing pREST template).
	CreateIdentity(ctx context.Context, userID string, in IdentityInput) (string, error)

	// UpdateIdentity applies a partial update to the identity row.
	UpdateIdentity(ctx context.Context, userID, identityID string, in IdentityUpdate) error

	// DeleteIdentity removes the identity row and (when no siblings
	// remain) the parent user_memories row.
	DeleteIdentity(ctx context.Context, userID, identityID string) error

	// CreateActivity inserts a parent + activity row pair.
	CreateActivity(ctx context.Context, userID string, in ActivityInput) (string, error)

	// UpdateActivity applies a partial update.
	UpdateActivity(ctx context.Context, userID, activityID string, in ActivityUpdate) error

	// DeleteActivity removes an activity row.
	DeleteActivity(ctx context.Context, userID, activityID string) error

	// CreateContext inserts a parent + context row pair. Contexts may
	// reference multiple parent memories via user_memory_ids JSONB.
	CreateContext(ctx context.Context, userID string, in ContextInput) (string, error)

	// UpdateContext applies a partial update.
	UpdateContext(ctx context.Context, userID, contextID string, in ContextUpdate) error

	// DeleteContext removes a context row.
	DeleteContext(ctx context.Context, userID, contextID string) error

	// CreateExperience inserts a parent + experience row pair.
	CreateExperience(ctx context.Context, userID string, in ExperienceInput) (string, error)

	// UpdateExperience applies a partial update.
	UpdateExperience(ctx context.Context, userID, experienceID string, in ExperienceUpdate) error

	// DeleteExperience removes an experience row.
	DeleteExperience(ctx context.Context, userID, experienceID string) error

	// CreatePreference inserts a parent + preference row pair.
	CreatePreference(ctx context.Context, userID string, in PreferenceInput) (string, error)

	// UpdatePreference applies a partial update.
	UpdatePreference(ctx context.Context, userID, preferenceID string, in PreferenceUpdate) error

	// DeletePreference removes a preference row.
	DeletePreference(ctx context.Context, userID, preferenceID string) error

	// DeleteAll wipes every palace row for the user. Used by the
	// /v1/memory/all endpoint (mirrors lambda/userMemory.deleteAll).
	DeleteAll(ctx context.Context, userID string) error

	// HealthCheck returns nil when the pool is reachable. Used by
	// main.go to gate the /v1/memory/* route family — the routes
	// return 503 if the palace store is not configured.
	HealthCheck(ctx context.Context) error
}

// Persona is the read-only shape returned by GetPersona.
// Mirrors the response shape of userMemoryRouter.getPersona.
type Persona struct {
	ID      string    `json:"id"`
	UserID  string    `json:"userId"`
	Profile string    `json:"profile,omitempty"`
	Tagline string    `json:"summary"`
	Persona string    `json:"content"`
	Version int       `json:"version"`
	CreatedAt time.Time `json:"createdAt"`
	UpdatedAt time.Time `json:"updatedAt"`
}