package cursor

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"

	"github.com/google/uuid"
	agentv1 "github.com/router-for-me/CLIProxyAPI/v7/internal/cursor/proto/agent/v1"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/structpb"
)

// Native checkpoint replay.
//
// Cursor checkpoints store completed turns as content-addressed protobuf
// blobs. Replaying a tool call as prose (the former "[called tool ...]"
// fallback) teaches fable to print that notation to the user. These helpers
// instead append/patch Cursor's native ConversationStep.ToolCall structures.

func stableConversationMessageID(text string) string {
	sum := sha256.Sum256([]byte("cursor-message\x00" + text))
	id, err := uuid.FromBytes(sum[:16])
	if err != nil {
		return uuid.NewString()
	}
	return id.String()
}

type nativeTurnBuilder struct {
	blobStore map[string][]byte
	userText  string
	steps     [][]byte
	open      bool
	turns     [][]byte
	err       error
}

func (b *nativeTurnBuilder) start(userText string) {
	b.flush()
	b.userText = userText
	b.steps = nil
	b.open = true
}

func (b *nativeTurnBuilder) addStep(step *agentv1.ConversationStep) {
	if b.err != nil {
		return
	}
	if !b.open {
		// A checkpoint boundary can fall immediately before an assistant/tool
		// step. Cursor accepts an empty user message for that resumed turn.
		b.start("")
	}
	payload, err := proto.Marshal(step)
	if err != nil {
		b.err = err
		return
	}
	b.steps = append(b.steps, storeBlob(b.blobStore, payload))
}

func (b *nativeTurnBuilder) flush() {
	if b.err != nil || !b.open {
		return
	}
	b.open = false
	if strings.TrimSpace(b.userText) == "" && len(b.steps) == 0 {
		return
	}
	userPayload, err := proto.Marshal(&agentv1.UserMessage{
		Text:      b.userText,
		MessageId: stableConversationMessageID(b.userText),
	})
	if err != nil {
		b.err = err
		return
	}
	turnPayload, err := proto.Marshal(&agentv1.ConversationTurnStructure{
		Turn: &agentv1.ConversationTurnStructure_AgentConversationTurn{
			AgentConversationTurn: &agentv1.AgentConversationTurnStructure{
				UserMessage: storeBlob(b.blobStore, userPayload),
				Steps:       append([][]byte(nil), b.steps...),
			},
		},
	})
	if err != nil {
		b.err = err
		return
	}
	b.turns = append(b.turns, storeBlob(b.blobStore, turnPayload))
}

func nativeAssistantStep(text string) *agentv1.ConversationStep {
	return &agentv1.ConversationStep{
		Message: &agentv1.ConversationStep_AssistantMessage{
			AssistantMessage: &agentv1.AssistantMessage{Text: text},
		},
	}
}

func nativeToolResult(result ToolResult) *agentv1.McpToolResult {
	return &agentv1.McpToolResult{
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

func nativeToolCallStep(call ToolCall, result *ToolResult) *agentv1.ConversationStep {
	wireName := wireToolName(call.Name)
	args := make(map[string]*structpb.Value, len(call.Arguments))
	for key, value := range call.Arguments {
		converted, err := toProtobufValue(value)
		if err == nil {
			args[key] = converted
		}
	}
	mcp := &agentv1.McpToolCall{
		Args: &agentv1.McpArgs{
			Name:               wireName,
			Args:               args,
			ToolCallId:         call.ID,
			ProviderIdentifier: MCPProviderIdentifier,
			ToolName:           wireName,
		},
	}
	if result != nil {
		mcp.Result = nativeToolResult(*result)
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

func buildNativeConversationTurns(blobStore map[string][]byte, messages []ChatMessage) (turns, systemRows [][]byte, err error) {
	results := make(map[string]*ToolResult)
	for i := range messages {
		if messages[i].Role != "tool" {
			continue
		}
		id := normalizeFingerprintToolID(messages[i].ToolCallID)
		if id == "" {
			continue
		}
		result := ToolResult{
			ToolCallID: messages[i].ToolCallID,
			Name:       messages[i].Name,
			Content:    messages[i].Content,
		}
		results[id] = &result
	}

	builder := &nativeTurnBuilder{blobStore: blobStore}
	for i := range messages {
		message := &messages[i]
		switch message.Role {
		case "system":
			if strings.TrimSpace(message.Content) != "" {
				systemRows = append(systemRows, storeBlob(blobStore, mustJSON(map[string]any{
					"role":    "system",
					"content": message.Content,
				})))
			}
		case "user":
			if strings.TrimSpace(message.Content) != "" {
				builder.start(message.Content)
			}
		case "assistant":
			if strings.TrimSpace(message.Content) != "" {
				builder.addStep(nativeAssistantStep(message.Content))
			}
			for _, call := range message.ToolCalls {
				builder.addStep(nativeToolCallStep(call, results[normalizeFingerprintToolID(call.ID)]))
			}
		}
	}
	builder.flush()
	return builder.turns, systemRows, builder.err
}

// patchNativeToolResults fills results into calls already present in the
// checkpoint. It returns the call ids found and the tool-result ids consumed.
func patchNativeToolResults(state *agentv1.ConversationStateStructure, blobs map[string][]byte, results map[string]ToolResult) (map[string]bool, map[string]bool) {
	existingCalls := make(map[string]bool)
	consumedResults := make(map[string]bool)
	for turnIndex, turnID := range state.GetTurns() {
		rawTurn := blobs[hex.EncodeToString(turnID)]
		if len(rawTurn) == 0 {
			continue
		}
		turn := &agentv1.ConversationTurnStructure{}
		if proto.Unmarshal(rawTurn, turn) != nil {
			continue
		}
		agentTurn := turn.GetAgentConversationTurn()
		if agentTurn == nil {
			continue
		}
		turnChanged := false
		for stepIndex, stepID := range agentTurn.GetSteps() {
			rawStep := blobs[hex.EncodeToString(stepID)]
			if len(rawStep) == 0 {
				continue
			}
			step := &agentv1.ConversationStep{}
			if proto.Unmarshal(rawStep, step) != nil {
				continue
			}
			call := step.GetToolCall()
			if call == nil || call.GetMcpToolCall() == nil {
				continue
			}
			callID := normalizeFingerprintToolID(call.GetToolCallId())
			if callID == "" {
				callID = normalizeFingerprintToolID(call.GetMcpToolCall().GetArgs().GetToolCallId())
			}
			if callID == "" {
				continue
			}
			existingCalls[callID] = true
			result, ok := results[callID]
			if !ok {
				continue
			}
			call.GetMcpToolCall().Result = nativeToolResult(result)
			payload, err := proto.Marshal(step)
			if err != nil {
				continue
			}
			agentTurn.Steps[stepIndex] = storeBlob(blobs, payload)
			turnChanged = true
			consumedResults[callID] = true
		}
		if !turnChanged {
			continue
		}
		payload, err := proto.Marshal(turn)
		if err != nil {
			continue
		}
		state.Turns[turnIndex] = storeBlob(blobs, payload)
	}
	return existingCalls, consumedResults
}

// buildNativeTailResumeRequest applies the uncheckpointed request tail as
// native Cursor turns. A tool-result tail is followed by an explicit
// continuation action so a rebuilt stream consumes the historical result;
// new user rows become the next UserMessageAction directly.
func buildNativeTailResumeRequest(model string, entry *convEntry, tail []ChatMessage, tools []ToolDefinition, choice ToolChoice) (*agentv1.AgentClientMessage, map[string][]byte, string, error) {
	if entry == nil || entry.state == nil {
		return nil, nil, "", fmt.Errorf("cursor: native resume has no checkpoint")
	}
	state := proto.Clone(entry.state).(*agentv1.ConversationStateStructure)
	blobs := cloneBlobStore(entry.blobs)

	results := make(map[string]ToolResult)
	for i := range tail {
		if tail[i].Role == "tool" {
			results[normalizeFingerprintToolID(tail[i].ToolCallID)] = ToolResult{
				ToolCallID: tail[i].ToolCallID,
				Name:       tail[i].Name,
				Content:    tail[i].Content,
			}
		}
	}
	existingCalls, consumedResults := patchNativeToolResults(state, blobs, results)

	filtered := make([]ChatMessage, 0, len(tail))
	nativeChanged := len(consumedResults) > 0
	for i := range tail {
		message := tail[i]
		switch message.Role {
		case "assistant":
			calls := make([]ToolCall, 0, len(message.ToolCalls))
			removedExisting := false
			for _, call := range message.ToolCalls {
				if existingCalls[normalizeFingerprintToolID(call.ID)] {
					removedExisting = true
					continue
				}
				calls = append(calls, call)
			}
			message.ToolCalls = calls
			if removedExisting && len(calls) == 0 {
				// The checkpoint already contains this assistant step.
				message.Content = ""
			}
			if strings.TrimSpace(message.Content) == "" && len(message.ToolCalls) == 0 {
				continue
			}
		case "tool":
			if consumedResults[normalizeFingerprintToolID(message.ToolCallID)] {
				continue
			}
		}
		filtered = append(filtered, message)
	}

	nativeHistory, trailingUsers := splitTrailingUserRun(filtered)
	turns, systemRows, err := buildNativeConversationTurns(blobs, nativeHistory)
	if err != nil {
		return nil, nil, "", fmt.Errorf("cursor: encode native resume tail: %w", err)
	}
	if len(turns) > 0 || len(systemRows) > 0 {
		nativeChanged = true
		state.Turns = append(state.Turns, turns...)
		state.RootPromptMessagesJson = append(state.RootPromptMessagesJson, systemRows...)
	}
	userText := joinedUserText(trailingUsers)
	if userText == "" && len(results) > 0 {
		// A dead live stream cannot consume the MCP result event directly.
		// The result is embedded in the native turn above; this action tells
		// the replacement run to process it instead of treating the request
		// as an empty/no-op resume.
		userText = resumeContinuationPrompt
	}
	if !nativeChanged && userText == "" {
		return nil, nil, "", fmt.Errorf("cursor: tail cannot be represented as native conversation turns")
	}

	nativeEntry := &convEntry{
		conversationID: entry.conversationID,
		state:          state,
		blobs:          blobs,
		model:          entry.model,
	}
	message, blobStore, conversationID, err := buildResumeRunRequest(model, nativeEntry, userText, tools, choice)
	if err != nil {
		return nil, nil, "", err
	}
	if userText == "" {
		message.GetRunRequest().Action = &agentv1.ConversationAction{
			Action: &agentv1.ConversationAction_ResumeAction{
				ResumeAction: &agentv1.ResumeAction{},
			},
		}
	}
	return message, blobStore, conversationID, nil
}
