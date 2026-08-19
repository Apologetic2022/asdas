package cursor

import "testing"

// A run that pauses for client tools bills each paused segment from an
// estimate, and the cumulative TurnEnded that closes the run only adds what
// those estimates missed, so the run totals Cursor's own numbers exactly.
func TestRunUsageReconcilesEstimatesWithCumulativeReport(t *testing.T) {
	var run runUsage

	first := run.estimate(40000, 30)
	if first.InputTokens != 40000 || first.CacheReadTokens != 0 {
		t.Fatalf("cold segment = %+v, want the whole prompt read fresh", first)
	}

	second := run.estimate(42000, 25)
	if second.CacheReadTokens != 40000 {
		t.Fatalf("resumed segment cache read = %d, want the 40000 prefix already sent", second.CacheReadTokens)
	}
	if second.InputTokens != 2000 {
		t.Fatalf("resumed segment fresh read = %d, want only the 2000 the prompt grew by", second.InputTokens)
	}

	total := TokenUsage{InputTokens: 90000, OutputTokens: 4000, CacheReadTokens: 120000}
	final := run.upstream(total, 44000)
	if got, want := final.InputTokens, int64(90000-42000); got != want {
		t.Fatalf("final input = %d, want %d", got, want)
	}
	if got, want := final.CacheReadTokens, int64(120000-40000); got != want {
		t.Fatalf("final cache read = %d, want %d", got, want)
	}
	if got, want := final.OutputTokens, int64(4000-55); got != want {
		t.Fatalf("final output = %d, want %d", got, want)
	}

	if run.billed.InputTokens != total.InputTokens ||
		run.billed.CacheReadTokens != total.CacheReadTokens ||
		run.billed.OutputTokens != total.OutputTokens {
		t.Fatalf("run billed %+v, want it to add up to the cumulative report %+v", run.billed, total)
	}
}

// Estimates that overshoot the cumulative report must not bill a negative
// segment, and must not be topped up again by the report.
func TestRunUsageClampsOvershootingEstimates(t *testing.T) {
	var run runUsage
	run.estimate(100000, 500)

	final := run.upstream(TokenUsage{InputTokens: 10000, OutputTokens: 100}, 100000)
	if !final.Empty() {
		t.Fatalf("segment = %+v, want nothing left to bill", final)
	}
}

func TestPromptTokensSumsTheDisjointCounters(t *testing.T) {
	u := TokenUsage{InputTokens: 100, CacheReadTokens: 900, CacheWriteTokens: 50}
	if got := u.PromptTokens(); got != 1050 {
		t.Fatalf("prompt tokens = %d, want 1050", got)
	}
}
