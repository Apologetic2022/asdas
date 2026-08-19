package cursor

import "encoding/json"

// TokenUsage is an Anthropic-style disjoint token breakdown, which is what
// Cursor reports: InputTokens counts only the prompt the provider had to read
// fresh, with the cached and newly cached prefixes counted separately.
type TokenUsage struct {
	InputTokens      int64
	OutputTokens     int64
	CacheReadTokens  int64
	CacheWriteTokens int64
	ReasoningTokens  int64
}

// PromptTokens is the whole prompt: the fresh read plus both cached parts.
func (u TokenUsage) PromptTokens() int64 {
	return u.InputTokens + u.CacheReadTokens + u.CacheWriteTokens
}

// Empty reports whether the breakdown carries no tokens at all.
func (u TokenUsage) Empty() bool {
	return u.PromptTokens() == 0 && u.OutputTokens == 0 && u.ReasoningTokens == 0
}

func (u *TokenUsage) add(other TokenUsage) {
	u.InputTokens += other.InputTokens
	u.OutputTokens += other.OutputTokens
	u.CacheReadTokens += other.CacheReadTokens
	u.CacheWriteTokens += other.CacheWriteTokens
	u.ReasoningTokens += other.ReasoningTokens
}

// runUsage derives per-segment token usage for one Agent run.
//
// Cursor reports usage exactly once per run, in the TurnEnded update that
// closes it, and those counters are cumulative over every segment the run
// produced. One gateway request drives one segment, so a run that pauses for
// client tools finishes most of its requests with no usage at all while the
// last one carries the whole run's totals — a 17-segment run reported 390,000
// input and 2,248,576 cache-read tokens against a ~135k prompt.
//
// Segments that end without a report are therefore billed from an estimate,
// and the cumulative report is reduced by whatever those estimates already
// accounted for, so the run still adds up to Cursor's own totals.
type runUsage struct {
	billed TokenUsage
	// sentPrompt is the largest prompt the run has already pushed upstream.
	// A resumed segment resends that prefix, which the provider serves from
	// its cache, so only the growth beyond it is a fresh read.
	sentPrompt int64
}

// upstream reduces a run-cumulative report to the part not yet billed.
func (r *runUsage) upstream(total TokenUsage, prompt int64) TokenUsage {
	segment := TokenUsage{
		InputTokens:      remaining(total.InputTokens, r.billed.InputTokens),
		OutputTokens:     remaining(total.OutputTokens, r.billed.OutputTokens),
		CacheReadTokens:  remaining(total.CacheReadTokens, r.billed.CacheReadTokens),
		CacheWriteTokens: remaining(total.CacheWriteTokens, r.billed.CacheWriteTokens),
		ReasoningTokens:  remaining(total.ReasoningTokens, r.billed.ReasoningTokens),
	}
	r.record(segment, prompt)
	return segment
}

// estimate bills a segment that ended before any report arrived.
func (r *runUsage) estimate(prompt, output int64) TokenUsage {
	cached := r.sentPrompt
	if cached > prompt {
		cached = prompt
	}
	segment := TokenUsage{
		InputTokens:     prompt - cached,
		OutputTokens:    output,
		CacheReadTokens: cached,
	}
	r.record(segment, prompt)
	return segment
}

func (r *runUsage) record(segment TokenUsage, prompt int64) {
	r.billed.add(segment)
	if prompt > r.sentPrompt {
		r.sentPrompt = prompt
	}
}

func remaining(total, billed int64) int64 {
	if total <= billed {
		return 0
	}
	return total - billed
}

// estimateTokensFromChars approximates a token count from a character count
// (~4 characters per token, the usual heuristic for latin-heavy prompts).
func estimateTokensFromChars(chars int) int64 {
	if chars <= 0 {
		return 0
	}
	if chars < 4 {
		return 1
	}
	return int64(chars / 4)
}

// EstimatePromptTokens approximates the prompt size of a request. It is the
// only number available for a segment that pauses for client tools, since
// Cursor withholds usage until the whole run ends.
func EstimatePromptTokens(messages []ChatMessage, tools []ToolDefinition) int64 {
	chars := 0
	for i := range messages {
		chars += len(messages[i].Role) + len(messages[i].Content) + 8
		for j := range messages[i].ToolCalls {
			chars += len(messages[i].ToolCalls[j].Name) + 16
			if b, err := json.Marshal(messages[i].ToolCalls[j].Arguments); err == nil {
				chars += len(b)
			}
		}
	}
	for i := range tools {
		chars += len(tools[i].Name) + len(tools[i].Description) + 16
		if b, err := json.Marshal(tools[i].Parameters); err == nil {
			chars += len(b)
		}
	}
	tokens := estimateTokensFromChars(chars)
	if tokens < 1 {
		tokens = 1
	}
	return tokens
}

// toolCallChars is the output size a tool call contributes.
func toolCallChars(call ToolCall) int {
	chars := len(call.Name) + 16
	if b, err := json.Marshal(call.Arguments); err == nil {
		chars += len(b)
	}
	return chars
}
