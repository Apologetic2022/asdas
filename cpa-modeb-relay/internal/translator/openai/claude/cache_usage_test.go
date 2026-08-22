package claude

import (
	"testing"

	"github.com/tidwall/gjson"
)

func TestExtractOpenAIUsageSeparatesCacheReadAndCreation(t *testing.T) {
	usage := gjson.Parse(`{
		"prompt_tokens": 42760,
		"completion_tokens": 4,
		"prompt_tokens_details": {
			"cached_tokens": 18717,
			"cache_creation_tokens": 24041
		}
	}`)
	input, output, read, creation := extractOpenAIUsage(usage)
	if input != 2 || output != 4 || read != 18717 || creation != 24041 {
		t.Fatalf("usage = input:%d output:%d read:%d creation:%d", input, output, read, creation)
	}
}

func TestSetClaudeCacheUsageReportsWritesWhereAnthropicReadsThem(t *testing.T) {
	output := setClaudeCacheUsage([]byte(`{}`), 18717, 24041)
	if got := gjson.GetBytes(output, "usage.cache_read_input_tokens").Int(); got != 18717 {
		t.Fatalf("cache_read_input_tokens = %d", got)
	}
	if got := gjson.GetBytes(output, "usage.cache_creation_input_tokens").Int(); got != 24041 {
		t.Fatalf("cache_creation_input_tokens = %d", got)
	}
}
