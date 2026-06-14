package runtime

import (
	"fmt"
	"strings"
)

// ContextBuilder constructs the system prompt with LobeHub-style context injection.
// Mirrors the LobeHub agent's prompt engineering pattern:
//   - Core system prompt
//   - User memory block (recall)
//   - Persona block (user profile)
//   - Document context (attached files)
//   - Skill hints (available tools/skills)
type ContextBuilder struct {
	corePrompt    string
	memoryBlock   string
	personaBlock  string
	documentBlock string
	skillHints    []string
	extraBlocks   []string
}

// NewContextBuilder starts with a core system prompt.
func NewContextBuilder(corePrompt string) *ContextBuilder {
	if corePrompt == "" {
		corePrompt = "You are a helpful AI assistant."
	}
	return &ContextBuilder{corePrompt: corePrompt}
}

// WithMemory injects the user memory block.
func (b *ContextBuilder) WithMemory(memoryBlock string) *ContextBuilder {
	b.memoryBlock = memoryBlock
	return b
}

// WithPersona injects a user persona block (from UserPersonaModel).
func (b *ContextBuilder) WithPersona(persona string) *ContextBuilder {
	b.personaBlock = persona
	return b
}

// WithDocuments injects attached document context.
func (b *ContextBuilder) WithDocuments(docText string) *ContextBuilder {
	b.documentBlock = docText
	return b
}

// WithSkillHints adds skill name hints (e.g. "image-generation: available").
func (b *ContextBuilder) WithSkillHints(hints []string) *ContextBuilder {
	b.skillHints = hints
	return b
}

// WithExtraBlock adds an arbitrary context block with a label.
func (b *ContextBuilder) WithExtraBlock(label, content string) *ContextBuilder {
	if content == "" {
		return b
	}
	b.extraBlocks = append(b.extraBlocks, fmt.Sprintf("[%s]\n%s", label, content))
	return b
}

// Build produces the final system prompt with all context blocks assembled.
func (b *ContextBuilder) Build() string {
	var sb strings.Builder
	sb.WriteString(b.corePrompt)
	sb.WriteString("\n\n")

	if b.personaBlock != "" {
		sb.WriteString("[User Persona]\n")
		sb.WriteString(b.personaBlock)
		sb.WriteString("\n\n")
	}
	if b.memoryBlock != "" {
		// memory block already includes "[User Memory]" header
		sb.WriteString(b.memoryBlock)
		if !strings.HasSuffix(b.memoryBlock, "\n") {
			sb.WriteString("\n")
		}
		sb.WriteString("\n")
	}
	if b.documentBlock != "" {
		sb.WriteString("[Attached Documents]\n")
		sb.WriteString(b.documentBlock)
		sb.WriteString("\n\n")
	}
	if len(b.skillHints) > 0 {
		sb.WriteString("[Available Skills]\n")
		for _, h := range b.skillHints {
			sb.WriteString("- ")
			sb.WriteString(h)
			sb.WriteString("\n")
		}
		sb.WriteString("\n")
	}
	for _, block := range b.extraBlocks {
		sb.WriteString(block)
		sb.WriteString("\n\n")
	}
	return sb.String()
}

// PromptSection represents a labeled section of a prompt.
type PromptSection struct {
	Label   string
	Content string
}

// BuildPrompt assembles a prompt from sections with consistent formatting.
// Empty sections are skipped.
func BuildPrompt(sections []PromptSection) string {
	var sb strings.Builder
	for _, s := range sections {
		if s.Content == "" {
			continue
		}
		if s.Label != "" {
			sb.WriteString("[")
			sb.WriteString(s.Label)
			sb.WriteString("]\n")
		}
		sb.WriteString(s.Content)
		sb.WriteString("\n\n")
	}
	return sb.String()
}

// TruncatePrompt caps the system prompt at maxBytes to prevent token overflow.
// Mirrors the truncation behavior in LobeHub's agent runtime.
func TruncatePrompt(prompt string, maxBytes int) string {
	if maxBytes <= 0 {
		maxBytes = 50000
	}
	if len(prompt) <= maxBytes {
		return prompt
	}
	return prompt[:maxBytes] + "\n\n[Prompt truncated to fit context window]"
}
