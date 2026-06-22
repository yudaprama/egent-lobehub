package palace

import (
	"context"
	"fmt"
	"strings"

	fp "github.com/kawai-network/fileprocessor"
)

// Embedder is the subset of fileprocessor.Embedder the palace store
// needs. Defined as an interface so tests can inject a fake without
// touching the real OpenAI endpoint.
type Embedder interface {
	Embed(ctx context.Context, texts []string) ([][]float32, error)
	Dimension() int
}

// requiredDim is the vector dimension pinned on every palace row.
// Matches public.user_memories.summary_vector_1024 and the
// user_memories_*_vector columns. Changing this requires a migration.
const requiredDim = 1024

// NewEmbedder wraps a fileprocessor Embedder, asserting the dimension.
// Returns nil when the embedder is nil — callers treat nil as "do not
// compute vectors" (the schema accepts NULL on the *_vector columns).
func NewEmbedder(e fp.Embedder) (Embedder, error) {
	if e == nil {
		return nil, nil
	}
	if dim := e.Dimension(); dim != 0 && dim != requiredDim {
		return nil, fmt.Errorf("palace: embedder dimension must be %d to match user_memories.*_vector_1024, got %d", requiredDim, dim)
	}
	return embedderAdapter{e: e}, nil
}

type embedderAdapter struct{ e fp.Embedder }

func (a embedderAdapter) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	return a.e.Embed(ctx, texts)
}

func (a embedderAdapter) Dimension() int { return a.e.Dimension() }

// embedLayerText builds the concatenation passed to the embedder for a
// given layer row. Mirrors the LobeHub userMemoryModel embedding flow:
// priority order is the layer's "primary" text field, falling back to
// notes / description / suggestions for sparse rows.
func embedLayerText(layer MemoryLayer, in IdentityInput, act ActivityInput, ctx ContextInput, exp ExperienceInput, pref PreferenceInput) string {
	switch layer {
	case LayerIdentity:
		parts := []string{}
		if in.Description != "" {
			parts = append(parts, in.Description)
		}
		if in.Role != "" {
			parts = append(parts, "role: "+in.Role)
		}
		if in.Relationship != "" {
			parts = append(parts, "relationship: "+in.Relationship)
		}
		if string(in.Type) != "" {
			parts = append(parts, "type: "+string(in.Type))
		}
		return strings.Join(parts, ". ")
	case LayerActivity:
		parts := []string{}
		if act.Narrative != "" {
			parts = append(parts, act.Narrative)
		}
		if act.Notes != "" {
			parts = append(parts, act.Notes)
		}
		return strings.Join(parts, ". ")
	case LayerContext:
		parts := []string{}
		if ctx.Title != "" {
			parts = append(parts, ctx.Title)
		}
		if ctx.Description != "" {
			parts = append(parts, ctx.Description)
		}
		if ctx.CurrentStatus != "" {
			parts = append(parts, "status: "+ctx.CurrentStatus)
		}
		return strings.Join(parts, ". ")
	case LayerExperience:
		parts := []string{}
		if exp.Situation != "" {
			parts = append(parts, exp.Situation)
		}
		if exp.Action != "" {
			parts = append(parts, "action: "+exp.Action)
		}
		if exp.KeyLearning != "" {
			parts = append(parts, "learning: "+exp.KeyLearning)
		}
		return strings.Join(parts, ". ")
	case LayerPreference:
		parts := []string{}
		if pref.ConclusionDirectives != "" {
			parts = append(parts, pref.ConclusionDirectives)
		}
		if pref.Suggestions != "" {
			parts = append(parts, pref.Suggestions)
		}
		return strings.Join(parts, ". ")
	}
	return ""
}