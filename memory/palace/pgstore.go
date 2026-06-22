package palace

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pgvector/pgvector-go"
)

// ErrNotFound is returned by Delete*/Update* operations when no row
// matches the (userID, id) pair. Callers should map this to HTTP 404.
var ErrNotFound = errors.New("palace: row not found")

// ErrLayerMismatch is returned when a caller passes an ID that
// resolves to a row in a different layer table than the operation
// expects (e.g. deleting an identity with the identityID pointing at
// an activity row). This guards against the case where the same
// nanoid-style ID happens to be reused across layer tables.
var ErrLayerMismatch = errors.New("palace: row belongs to a different layer")

// PgStore is the Postgres + pgvector implementation of [Store]. It
// writes the LobeHub `user_memories` parent table plus one of the five
// `user_memories_*` child tables in a single transaction, so partial
// failures cannot leave an orphan parent row.
type PgStore struct {
	pool     *pgxpool.Pool
	embedder Embedder
}

// NewPgStore creates a palace store from the shared pgx pool. The
// embedder may be nil — when nil, the *_vector columns are written
// as NULL and search relies on FTS or hybrid queries that pass the
// query embedding in (see userMemorySearchHybrid.read.sql).
func NewPgStore(pool *pgxpool.Pool, embedder Embedder) *PgStore {
	return &PgStore{pool: pool, embedder: embedder}
}

// HealthCheck satisfies the Store interface and is used by main.go to
// gate the /v1/memory/* routes.
func (s *PgStore) HealthCheck(ctx context.Context) error {
	if s == nil || s.pool == nil {
		return errors.New("palace: pool not configured")
	}
	return s.pool.Ping(ctx)
}

// generateParentID returns a random nanoid-style string. The LobeHub
// schema accepts up to 255 chars; we use 21 (matching the default
// nanoid length) for global uniqueness without a separate sequence.
func generateParentID() string {
	// 21 chars from a 62-char alphabet → ~125 bits of entropy.
	const alphabet = "1234567890abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ"
	const size = 21
	b := make([]byte, size)
	for i := range b {
		n, err := rand.Int(rand.Reader, big.NewInt(int64(len(alphabet))))
		if err != nil {
			panic(fmt.Errorf("palace: generate id: %w", err))
		}
		b[i] = alphabet[n.Int64()]
	}
	return string(b)
}

// insertParent inserts a row into user_memories and returns the new id.
// When a userMemoryID is supplied, it is used as-is (matches the LobeHub
// behavior of letting the caller provide a nanoid); when empty, a fresh
// id is generated.
func (s *PgStore) insertParent(ctx context.Context, tx pgx.Tx, userID, userMemoryID, layer, summary, summaryVec string) (string, error) {
	if userMemoryID == "" {
		userMemoryID = generateParentID()
	}
	const q = `
		INSERT INTO user_memories
			(id, user_id, memory_layer, status, summary, summary_vector_1024)
		VALUES ($1, $2, $3, 'active', $4, $5::vector)`
	var vec any
	if summaryVec != "" {
		vec = summaryVec
	}
	if _, err := tx.Exec(ctx, q, userMemoryID, userID, layer, summary, vec); err != nil {
		return "", fmt.Errorf("palace: insert user_memories: %w", err)
	}
	return userMemoryID, nil
}

// vecToString formats a []float32 for use in a pgvector literal. We
// format as the `[1.0,2.0,...]` syntax so it can be cast to ::vector.
func vecToString(v []float32) string {
	if len(v) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteByte('[')
	for i, f := range v {
		if i > 0 {
			b.WriteByte(',')
		}
		fmt.Fprintf(&b, "%g", f)
	}
	b.WriteByte(']')
	return b.String()
}

// embedIfConfigured returns the embedding for text as a pgvector
// literal string. Returns empty string when no embedder is configured
// or text is empty.
func (s *PgStore) embedIfConfigured(ctx context.Context, text string) (string, error) {
	if s.embedder == nil || text == "" {
		return "", nil
	}
	vecs, err := s.embedder.Embed(ctx, []string{text})
	if err != nil {
		return "", fmt.Errorf("palace: embed: %w", err)
	}
	if len(vecs) == 0 || len(vecs[0]) != requiredDim {
		return "", fmt.Errorf("palace: embedder returned %d-dim vector, want %d", len(vecs[0]), requiredDim)
	}
	return vecToString(vecs[0]), nil
}

// encodeJSON marshals a value for a jsonb column. Returns nil when
// the value is the zero value, so the column stores SQL NULL instead
// of an empty object — matches the LobeHub model behavior.
func encodeJSON(v any) (any, error) {
	if v == nil {
		return nil, nil
	}
	b, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	return string(b), nil
}

// createLayerRow is the shared logic for inserting a parent + child row.
// It runs inside the transaction returned by the caller.
type layerChild interface {
	insert(ctx context.Context, tx pgx.Tx, parentID, userID string, vec string) (string, error)
}

func (s *PgStore) runCreate(ctx context.Context, userID string, in layerChild) (string, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return "", fmt.Errorf("palace: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	parentID, err := in.insert(ctx, tx, "", userID, "")
	if err != nil {
		return "", err
	}
	if err := tx.Commit(ctx); err != nil {
		return "", fmt.Errorf("palace: commit: %w", err)
	}
	return parentID, nil
}

// CreateIdentity satisfies the Store interface.
func (s *PgStore) CreateIdentity(ctx context.Context, userID string, in IdentityInput) (string, error) {
	if !in.Type.Valid() {
		return "", fmt.Errorf("palace: invalid identity type %q", in.Type)
	}
	text := embedLayerText(LayerIdentity, in, ActivityInput{}, ContextInput{}, ExperienceInput{}, PreferenceInput{})
	vec, err := s.embedIfConfigured(ctx, text)
	if err != nil {
		return "", err
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return "", fmt.Errorf("palace: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	parentID, err := s.insertParent(ctx, tx, userID, in.UserMemoryID, string(LayerIdentity), in.Description, vec)
	if err != nil {
		return "", err
	}

	identityID := generateParentID()
	labelsJSON, err := encodeJSON(in.Labels)
	if err != nil {
		return "", err
	}
	const q = `
		INSERT INTO user_memories_identities
			(id, user_id, user_memory_id, description, role, relationship, type, episodic_date, metadata, tags, description_vector)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9::jsonb, $10, $11::vector)`
	if _, err := tx.Exec(ctx, q,
		identityID, userID, parentID,
		nullableString(in.Description),
		nullableString(in.Role),
		nullableString(in.Relationship),
		nullableString(string(in.Type)),
		in.EpisodicDate,
		labelsJSON,
		in.ExtractedLabels,
		nullableString(vec),
	); err != nil {
		return "", fmt.Errorf("palace: insert identity: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return "", fmt.Errorf("palace: commit: %w", err)
	}
	return identityID, nil
}

// CreateActivity satisfies the Store interface.
func (s *PgStore) CreateActivity(ctx context.Context, userID string, in ActivityInput) (string, error) {
	text := embedLayerText(LayerActivity, IdentityInput{}, in, ContextInput{}, ExperienceInput{}, PreferenceInput{})
	vec, err := s.embedIfConfigured(ctx, text)
	if err != nil {
		return "", err
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return "", fmt.Errorf("palace: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	parentID, err := s.insertParent(ctx, tx, userID, in.UserMemoryID, string(LayerActivity), in.Narrative, vec)
	if err != nil {
		return "", err
	}

	activityID := generateParentID()
	objsJSON, err := encodeJSON(in.AssociatedObjects)
	if err != nil {
		return "", err
	}
	subsJSON, err := encodeJSON(in.AssociatedSubjects)
	if err != nil {
		return "", err
	}
	locsJSON, err := encodeJSON(in.AssociatedLocations)
	if err != nil {
		return "", err
	}
	labelsJSON, err := encodeJSON(in.Labels)
	if err != nil {
		return "", err
	}
	const q = `
		INSERT INTO user_memories_activities
			(id, user_id, user_memory_id, type, status, timezone, starts_at, ends_at,
			 associated_objects, associated_subjects, associated_locations,
			 notes, narrative, feedback, metadata, tags,
			 narrative_vector, feedback_vector)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9::jsonb, $10::jsonb, $11::jsonb,
		        $12, $13, $14, $15::jsonb, $16, $17::vector, $18::vector)`
	status := in.Status
	if status == "" {
		status = ActivityPending
	}
	if _, err := tx.Exec(ctx, q,
		activityID, userID, parentID,
		in.Type, status, in.Timezone, in.StartsAt, in.EndsAt,
		objsJSON, subsJSON, locsJSON,
		in.Notes, in.Narrative, in.Feedback, labelsJSON, in.ExtractedLabels,
		nullableString(vec), nullableString(vec),
	); err != nil {
		return "", fmt.Errorf("palace: insert activity: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return "", fmt.Errorf("palace: commit: %w", err)
	}
	return activityID, nil
}

// CreateContext satisfies the Store interface.
func (s *PgStore) CreateContext(ctx context.Context, userID string, in ContextInput) (string, error) {
	text := embedLayerText(LayerContext, IdentityInput{}, ActivityInput{}, in, ExperienceInput{}, PreferenceInput{})
	vec, err := s.embedIfConfigured(ctx, text)
	if err != nil {
		return "", err
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return "", fmt.Errorf("palace: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// Contexts reference parent memories via the JSONB user_memory_ids
	// array; there is no user_memory_id column on this table. We still
	// create a parent row (memory_layer='context') and persist its ID as
	// the first entry in user_memory_ids so Phase 1 read templates can
	// join back to user_memories.
	parentID, err := s.insertParent(ctx, tx, userID, "", string(LayerContext), in.Title, vec)
	if err != nil {
		return "", err
	}

	contextID := generateParentID()
	memoryIDs := append([]string{parentID}, in.UserMemoryIDs...)
	idsJSON, err := encodeJSON(memoryIDs)
	if err != nil {
		return "", err
	}
	objsJSON, err := encodeJSON(in.AssociatedObjects)
	if err != nil {
		return "", err
	}
	subsJSON, err := encodeJSON(in.AssociatedSubjects)
	if err != nil {
		return "", err
	}
	labelsJSON, err := encodeJSON(in.Labels)
	if err != nil {
		return "", err
	}
	const q = `
		INSERT INTO user_memories_contexts
			(id, user_id, user_memory_ids, title, description, type,
			 current_status, score_impact, score_urgency,
			 associated_objects, associated_subjects, metadata, tags, description_vector)
		VALUES ($1, $2, $3::jsonb, $4, $5, $6, $7, $8, $9, $10::jsonb, $11::jsonb, $12::jsonb, $13, $14::vector)`
	if _, err := tx.Exec(ctx, q,
		contextID, userID, idsJSON,
		in.Title, in.Description, in.Type,
		in.CurrentStatus, in.ScoreImpact, in.ScoreUrgency,
		objsJSON, subsJSON, labelsJSON, in.ExtractedLabels,
		nullableString(vec),
	); err != nil {
		return "", fmt.Errorf("palace: insert context: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return "", fmt.Errorf("palace: commit: %w", err)
	}
	return contextID, nil
}

// CreateExperience satisfies the Store interface.
func (s *PgStore) CreateExperience(ctx context.Context, userID string, in ExperienceInput) (string, error) {
	text := embedLayerText(LayerExperience, IdentityInput{}, ActivityInput{}, ContextInput{}, in, PreferenceInput{})
	vec, err := s.embedIfConfigured(ctx, text)
	if err != nil {
		return "", err
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return "", fmt.Errorf("palace: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	parentID, err := s.insertParent(ctx, tx, userID, in.UserMemoryID, string(LayerExperience), in.Situation, vec)
	if err != nil {
		return "", err
	}

	expID := generateParentID()
	labelsJSON, err := encodeJSON(in.Labels)
	if err != nil {
		return "", err
	}
	const q = `
		INSERT INTO user_memories_experiences
			(id, user_id, user_memory_id, type, situation, reasoning, possible_outcome,
			 action, key_learning, score_confidence, metadata, tags,
			 situation_vector, action_vector, key_learning_vector)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11::jsonb, $12,
		        $13::vector, $14::vector, $15::vector)`
	if _, err := tx.Exec(ctx, q,
		expID, userID, parentID,
		in.Type, in.Situation, in.Reasoning, in.PossibleOutcome,
		in.Action, in.KeyLearning, in.ScoreConfidence, labelsJSON, in.ExtractedLabels,
		nullableString(vec), nullableString(vec), nullableString(vec),
	); err != nil {
		return "", fmt.Errorf("palace: insert experience: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return "", fmt.Errorf("palace: commit: %w", err)
	}
	return expID, nil
}

// CreatePreference satisfies the Store interface.
func (s *PgStore) CreatePreference(ctx context.Context, userID string, in PreferenceInput) (string, error) {
	text := embedLayerText(LayerPreference, IdentityInput{}, ActivityInput{}, ContextInput{}, ExperienceInput{}, in)
	vec, err := s.embedIfConfigured(ctx, text)
	if err != nil {
		return "", err
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return "", fmt.Errorf("palace: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	parentID, err := s.insertParent(ctx, tx, userID, in.UserMemoryID, string(LayerPreference), in.ConclusionDirectives, vec)
	if err != nil {
		return "", err
	}

	prefID := generateParentID()
	labelsJSON, err := encodeJSON(in.Labels)
	if err != nil {
		return "", err
	}
	const q = `
		INSERT INTO user_memories_preferences
			(id, user_id, user_memory_id, conclusion_directives, type, suggestions,
			 score_priority, metadata, tags, conclusion_directives_vector)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8::jsonb, $9, $10::vector)`
	if _, err := tx.Exec(ctx, q,
		prefID, userID, parentID,
		in.ConclusionDirectives, in.Type, in.Suggestions,
		in.ScorePriority, labelsJSON, in.ExtractedLabels,
		nullableString(vec),
	); err != nil {
		return "", fmt.Errorf("palace: insert preference: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return "", fmt.Errorf("palace: commit: %w", err)
	}
	return prefID, nil
}

// UpdateIdentity applies a partial update. Only the fields explicitly
// set on `in` are written.
func (s *PgStore) UpdateIdentity(ctx context.Context, userID, identityID string, in IdentityUpdate) error {
	if in.Type != nil && !in.Type.Valid() {
		return fmt.Errorf("palace: invalid identity type %q", *in.Type)
	}
	setClauses := []string{}
	args := []any{userID, identityID}
	idx := 3
	if in.Description != nil {
		setClauses = append(setClauses, fmt.Sprintf("description = $%d", idx))
		args = append(args, *in.Description)
		idx++
	}
	if in.Role != nil {
		setClauses = append(setClauses, fmt.Sprintf("role = $%d", idx))
		args = append(args, *in.Role)
		idx++
	}
	if in.Relationship != nil {
		setClauses = append(setClauses, fmt.Sprintf("relationship = $%d", idx))
		args = append(args, *in.Relationship)
		idx++
	}
	if in.Type != nil {
		setClauses = append(setClauses, fmt.Sprintf("type = $%d", idx))
		args = append(args, string(*in.Type))
		idx++
	}
	if in.EpisodicDate != nil {
		setClauses = append(setClauses, fmt.Sprintf("episodic_date = $%d", idx))
		args = append(args, *in.EpisodicDate)
		idx++
	}
	if in.ExtractedLabels != nil {
		setClauses = append(setClauses, fmt.Sprintf("tags = $%d", idx))
		args = append(args, *in.ExtractedLabels)
		idx++
	}
	if in.Labels != nil {
		labelsJSON, err := encodeJSON(in.Labels)
		if err != nil {
			return err
		}
		setClauses = append(setClauses, fmt.Sprintf("metadata = $%d::jsonb", idx))
		args = append(args, labelsJSON)
		idx++
	}
	if len(setClauses) == 0 {
		return nil
	}
	setClauses = append(setClauses, "updated_at = now()")
	q := fmt.Sprintf(`
		UPDATE user_memories_identities
		SET %s
		WHERE user_id = $1 AND id = $2`,
		strings.Join(setClauses, ", "))
	tag, err := s.pool.Exec(ctx, q, args...)
	if err != nil {
		return fmt.Errorf("palace: update identity: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// UpdateActivity applies a partial update.
func (s *PgStore) UpdateActivity(ctx context.Context, userID, activityID string, in ActivityUpdate) error {
	setClauses := []string{}
	args := []any{userID, activityID}
	idx := 3
	if in.Narrative != nil {
		setClauses = append(setClauses, fmt.Sprintf("narrative = $%d", idx))
		args = append(args, *in.Narrative)
		idx++
	}
	if in.Notes != nil {
		setClauses = append(setClauses, fmt.Sprintf("notes = $%d", idx))
		args = append(args, *in.Notes)
		idx++
	}
	if in.Status != nil {
		setClauses = append(setClauses, fmt.Sprintf("status = $%d", idx))
		args = append(args, string(*in.Status))
		idx++
	}
	if len(setClauses) == 0 {
		return nil
	}
	setClauses = append(setClauses, "updated_at = now()")
	q := fmt.Sprintf(`
		UPDATE user_memories_activities
		SET %s
		WHERE user_id = $1 AND id = $2`,
		strings.Join(setClauses, ", "))
	tag, err := s.pool.Exec(ctx, q, args...)
	if err != nil {
		return fmt.Errorf("palace: update activity: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// UpdateContext applies a partial update.
func (s *PgStore) UpdateContext(ctx context.Context, userID, contextID string, in ContextUpdate) error {
	setClauses := []string{}
	args := []any{userID, contextID}
	idx := 3
	if in.Title != nil {
		setClauses = append(setClauses, fmt.Sprintf("title = $%d", idx))
		args = append(args, *in.Title)
		idx++
	}
	if in.Description != nil {
		setClauses = append(setClauses, fmt.Sprintf("description = $%d", idx))
		args = append(args, *in.Description)
		idx++
	}
	if in.CurrentStatus != nil {
		setClauses = append(setClauses, fmt.Sprintf("current_status = $%d", idx))
		args = append(args, *in.CurrentStatus)
		idx++
	}
	if len(setClauses) == 0 {
		return nil
	}
	setClauses = append(setClauses, "updated_at = now()")
	q := fmt.Sprintf(`
		UPDATE user_memories_contexts
		SET %s
		WHERE user_id = $1 AND id = $2`,
		strings.Join(setClauses, ", "))
	tag, err := s.pool.Exec(ctx, q, args...)
	if err != nil {
		return fmt.Errorf("palace: update context: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// UpdateExperience applies a partial update.
func (s *PgStore) UpdateExperience(ctx context.Context, userID, experienceID string, in ExperienceUpdate) error {
	setClauses := []string{}
	args := []any{userID, experienceID}
	idx := 3
	if in.Situation != nil {
		setClauses = append(setClauses, fmt.Sprintf("situation = $%d", idx))
		args = append(args, *in.Situation)
		idx++
	}
	if in.Action != nil {
		setClauses = append(setClauses, fmt.Sprintf("action = $%d", idx))
		args = append(args, *in.Action)
		idx++
	}
	if in.KeyLearning != nil {
		setClauses = append(setClauses, fmt.Sprintf("key_learning = $%d", idx))
		args = append(args, *in.KeyLearning)
		idx++
	}
	if len(setClauses) == 0 {
		return nil
	}
	setClauses = append(setClauses, "updated_at = now()")
	q := fmt.Sprintf(`
		UPDATE user_memories_experiences
		SET %s
		WHERE user_id = $1 AND id = $2`,
		strings.Join(setClauses, ", "))
	tag, err := s.pool.Exec(ctx, q, args...)
	if err != nil {
		return fmt.Errorf("palace: update experience: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// UpdatePreference applies a partial update.
func (s *PgStore) UpdatePreference(ctx context.Context, userID, preferenceID string, in PreferenceUpdate) error {
	setClauses := []string{}
	args := []any{userID, preferenceID}
	idx := 3
	if in.ConclusionDirectives != nil {
		setClauses = append(setClauses, fmt.Sprintf("conclusion_directives = $%d", idx))
		args = append(args, *in.ConclusionDirectives)
		idx++
	}
	if in.Suggestions != nil {
		setClauses = append(setClauses, fmt.Sprintf("suggestions = $%d", idx))
		args = append(args, *in.Suggestions)
		idx++
	}
	if len(setClauses) == 0 {
		return nil
	}
	setClauses = append(setClauses, "updated_at = now()")
	q := fmt.Sprintf(`
		UPDATE user_memories_preferences
		SET %s
		WHERE user_id = $1 AND id = $2`,
		strings.Join(setClauses, ", "))
	tag, err := s.pool.Exec(ctx, q, args...)
	if err != nil {
		return fmt.Errorf("palace: update preference: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// deleteChild is the shared logic for DELETE on a child row plus its
// parent. We delete the child first; if that succeeds we delete the
// parent only if it has no other child rows (matches the LobeHub
// behavior of "a memory row is the parent of exactly one layer row,
// except contexts which fan out via JSONB").
func (s *PgStore) deleteChild(ctx context.Context, userID, childID, layer string) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("palace: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// Look up the child row to get the parent id and verify the user
	// scope. We do this explicitly (rather than relying on the FK +
	// user_id filter) so we can return a clean ErrNotFound.
	var parentID string
	var childTable string
	switch layer {
	case string(LayerIdentity):
		childTable = "user_memories_identities"
	case string(LayerActivity):
		childTable = "user_memories_activities"
	case string(LayerContext):
		childTable = "user_memories_contexts"
	case string(LayerExperience):
		childTable = "user_memories_experiences"
	case string(LayerPreference):
		childTable = "user_memories_preferences"
	default:
		return fmt.Errorf("palace: unknown layer %q", layer)
	}
	q := fmt.Sprintf("SELECT user_memory_id FROM %s WHERE id = $1 AND user_id = $2", childTable)
	if layer == string(LayerContext) {
		q = fmt.Sprintf("SELECT user_memory_ids->>0 FROM %s WHERE id = $1 AND user_id = $2", childTable)
	}
	if err := tx.QueryRow(ctx, q, childID, userID).Scan(&parentID); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		return fmt.Errorf("palace: lookup child: %w", err)
	}
	delQ := fmt.Sprintf("DELETE FROM %s WHERE id = $1 AND user_id = $2", childTable)
	if _, err := tx.Exec(ctx, delQ, childID, userID); err != nil {
		return fmt.Errorf("palace: delete child: %w", err)
	}
	// Delete the parent only when no other layer row references it.
	// This keeps the schema invariant that each parent has at most
	// one child (except contexts, which we never auto-cascade).
	var remaining int
	if err := tx.QueryRow(ctx, `
		SELECT (
			(SELECT COUNT(*) FROM user_memories_identities   WHERE user_memory_id = $1) +
			(SELECT COUNT(*) FROM user_memories_activities   WHERE user_memory_id = $1) +
			(SELECT COUNT(*) FROM user_memories_contexts    WHERE user_memory_ids->>0 = $1) +
			(SELECT COUNT(*) FROM user_memories_experiences  WHERE user_memory_id = $1) +
			(SELECT COUNT(*) FROM user_memories_preferences  WHERE user_memory_id = $1)
		)`, parentID).Scan(&remaining); err != nil {
		return fmt.Errorf("palace: count children: %w", err)
	}
	if remaining == 0 {
		if _, err := tx.Exec(ctx, `DELETE FROM user_memories WHERE id = $1 AND user_id = $2`, parentID, userID); err != nil {
			return fmt.Errorf("palace: delete parent: %w", err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("palace: commit: %w", err)
	}
	return nil
}

// DeleteIdentity satisfies the Store interface.
func (s *PgStore) DeleteIdentity(ctx context.Context, userID, identityID string) error {
	return s.deleteChild(ctx, userID, identityID, string(LayerIdentity))
}

// DeleteActivity satisfies the Store interface.
func (s *PgStore) DeleteActivity(ctx context.Context, userID, activityID string) error {
	return s.deleteChild(ctx, userID, activityID, string(LayerActivity))
}

// DeleteContext satisfies the Store interface.
func (s *PgStore) DeleteContext(ctx context.Context, userID, contextID string) error {
	return s.deleteChild(ctx, userID, contextID, string(LayerContext))
}

// DeleteExperience satisfies the Store interface.
func (s *PgStore) DeleteExperience(ctx context.Context, userID, experienceID string) error {
	return s.deleteChild(ctx, userID, experienceID, string(LayerExperience))
}

// DeletePreference satisfies the Store interface.
func (s *PgStore) DeletePreference(ctx context.Context, userID, preferenceID string) error {
	return s.deleteChild(ctx, userID, preferenceID, string(LayerPreference))
}

// DeleteAll wipes every palace row for the user, in a single transaction.
// The persona document is also removed (matches the LobeHub deleteAll
// behavior in userMemoryRouter.deleteAll).
func (s *PgStore) DeleteAll(ctx context.Context, userID string) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("palace: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// Delete child rows first; the FK from each child to user_memories
	// would block a top-down delete on the parent table.
	tables := []string{
		"user_memories_identities",
		"user_memories_activities",
		"user_memories_contexts",
		"user_memories_experiences",
		"user_memories_preferences",
	}
	for _, t := range tables {
		if _, err := tx.Exec(ctx, "DELETE FROM "+t+" WHERE user_id = $1", userID); err != nil {
			return fmt.Errorf("palace: delete %s: %w", t, err)
		}
	}
	if _, err := tx.Exec(ctx, "DELETE FROM user_memories WHERE user_id = $1", userID); err != nil {
		return fmt.Errorf("palace: delete user_memories: %w", err)
	}
	if _, err := tx.Exec(ctx, "DELETE FROM user_memory_persona_documents WHERE user_id = $1", userID); err != nil {
		return fmt.Errorf("palace: delete persona: %w", err)
	}
	if _, err := tx.Exec(ctx, "DELETE FROM user_memory_persona_document_histories WHERE user_id = $1", userID); err != nil {
		return fmt.Errorf("palace: delete persona history: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("palace: commit: %w", err)
	}
	return nil
}

// nullableString returns nil when s is empty so the SQL column stores
// NULL rather than an empty string — matches the LobeHub convention.
func nullableString(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// Compile-time check that PgStore satisfies the Store interface.
var _ Store = (*PgStore)(nil)

// Compile-time check that pgvector-go types are reachable. Without
// this the dependency may be tree-shaken out of go.mod if no other
// code in the binary imports pgvector directly. The import is used
// by insertParent above; this line is a guard.
var _ = pgvector.NewVector