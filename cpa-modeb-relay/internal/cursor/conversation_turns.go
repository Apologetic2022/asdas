package cursor

import (
	"crypto/sha256"
	"strings"

	"github.com/google/uuid"
	agentv1 "github.com/router-for-me/CLIProxyAPI/v7/internal/cursor/proto/agent/v1"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/structpb"
)

// Cursor splits a conversation into a root prompt and a list of completed
// turns. Only the turn list is covered by the provider prompt cache, so history
// replayed as extra root-prompt rows is re-billed in full on every request.
// buildConversationTurns therefore projects chat history onto the same
// blob-addressed turn structure the desktop client sends.
//
// Each turn holds one user message plus the assistant steps it produced:
// assistant prose becomes an AssistantMessage step and every function call
// becomes an McpToolCall step carrying its result inline.

// Blob ids are content hashes, so a turn only lands on the cached prefix when
// it serializes to the same bytes every time. Go randomizes protobuf map order
// by default, which would give McpArgs.Args a fresh encoding per request.
var deterministicProto = proto.MarshalOptions{Deterministic: true}

// stableMessageID derives a message id from the message text so a replayed
// history row keeps the id the action carried when it was first sent.
func stableMessageID(text string) string {
	sum := sha256.Sum256([]byte("cursor-message\x00" + text))
	id, err := uuid.FromBytes(sum[:16])
	if err != nil {
		return uuid.NewString()
	}
	return id.String()
}

type turnBuilder struct {
	blobStore map[string][]byte
	userText  string
	steps     [][]byte
	open      bool
	turns     [][]byte
}

func (b *turnBuilder) start(userText string) {
	b.flush()
	b.userText = userText
	b.steps = nil
	b.open = true
}

func (b *turnBuilder) addStep(step *agentv1.ConversationStep) {
	if !b.open {
		// Assistant output with no preceding user message: hang it off an
		// empty-prompted turn rather than dropping it.
		b.start("")
	}
	payload, err := deterministicProto.Marshal(step)
	if err != nil {
		return
	}
	b.steps = append(b.steps, storeBlob(b.blobStore, payload))
}

func (b *turnBuilder) flush() {
	if !b.open {
		return
	}
	b.open = false
	if strings.TrimSpace(b.userText) == "" && len(b.steps) == 0 {
		return
	}
	userPayload, err := deterministicProto.Marshal(&agentv1.UserMessage{
		Text:      b.userText,
		MessageId: stableMessageID(b.userText),
	})
	if err != nil {
		return
	}
	turnPayload, err := deterministicProto.Marshal(&agentv1.ConversationTurnStructure{
		Turn: &agentv1.ConversationTurnStructure_AgentConversationTurn{
			AgentConversationTurn: &agentv1.AgentConversationTurnStructure{
				UserMessage: storeBlob(b.blobStore, userPayload),
				Steps:       b.steps,
			},
		},
	})
	if err != nil {
		return
	}
	b.turns = append(b.turns, storeBlob(b.blobStore, turnPayload))
}

func assistantStep(text string) *agentv1.ConversationStep {
	return &agentv1.ConversationStep{
		Message: &agentv1.ConversationStep_AssistantMessage{
			AssistantMessage: &agentv1.AssistantMessage{Text: text},
		},
	}
}

func toolCallStep(call ToolCall, result *ToolResult) *agentv1.ConversationStep {
	wire := wireToolName(call.Name)
	args := map[string]*structpb.Value{}
	for key, value := range call.Arguments {
		converted, err := toProtobufValue(value)
		if err != nil {
			continue
		}
		args[key] = converted
	}
	mcp := &agentv1.McpToolCall{
		Args: &agentv1.McpArgs{
			Name:               wire,
			Args:               args,
			ToolCallId:         call.ID,
			ProviderIdentifier: MCPProviderIdentifier,
			ToolName:           wire,
		},
	}
	if result != nil {
		mcp.Result = &agentv1.McpToolResult{
			Result: &agentv1.McpToolResult_Success{
				Success: &agentv1.McpSuccess{
					Content: []*agentv1.McpToolResultContentItem{{
						Content: &agentv1.McpToolResultContentItem_Text{
							Text: &agentv1.McpTextContent{Text: result.Content},
						},
					}},
					IsError: result.IsError,
				},
			},
		}
	}
	return &agentv1.ConversationStep{
		Message: &agentv1.ConversationStep_ToolCall{
			ToolCall: &agentv1.ToolCall{
				Tool:       &agentv1.ToolCall_McpToolCall{McpToolCall: mcp},
				ToolCallId: call.ID,
			},
		},
	}
}

// buildConversationTurns splits history into blob-addressed conversation turns
// and returns them alongside the system rows, which stay in the root prompt.
func buildConversationTurns(blobStore map[string][]byte, msgs []ChatMessage) (turns [][]byte, systemRows [][]byte) {
	results := map[string]*ToolResult{}
	for i := range msgs {
		if msgs[i].Role != "tool" {
			continue
		}
		id := strings.TrimSpace(msgs[i].ToolCallID)
		if id == "" {
			continue
		}
		results[id] = &ToolResult{
			ToolCallID: id,
			Name:       msgs[i].Name,
			Content:    msgs[i].Content,
		}
	}

	builder := &turnBuilder{blobStore: blobStore}
	for _, msg := range msgs {
		switch msg.Role {
		case "system":
			if strings.TrimSpace(msg.Content) == "" {
				continue
			}
			systemRows = append(systemRows, storeBlob(blobStore, mustJSON(map[string]any{
				"role":    "system",
				"content": msg.Content,
			})))
		case "user":
			if strings.TrimSpace(msg.Content) == "" {
				continue
			}
			builder.start(msg.Content)
		case "assistant":
			if strings.TrimSpace(msg.Content) != "" {
				builder.addStep(assistantStep(msg.Content))
			}
			for _, call := range msg.ToolCalls {
				builder.addStep(toolCallStep(call, results[strings.TrimSpace(call.ID)]))
			}
		}
	}
	builder.flush()
	return builder.turns, systemRows
}
