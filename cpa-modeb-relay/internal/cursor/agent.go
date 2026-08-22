package cursor

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	cursorauth "github.com/router-for-me/CLIProxyAPI/v7/internal/auth/cursor"
	agentv1 "github.com/router-for-me/CLIProxyAPI/v7/internal/cursor/proto/agent/v1"
	log "github.com/sirupsen/logrus"
	"google.golang.org/protobuf/proto"
)

// ChatMessage is a minimal OpenAI-style chat message.
type ChatMessage struct {
	Role       string
	Content    string
	Name       string
	ToolCallID string
	ToolCalls  []ToolCall
}

// ToolChoice captures the OpenAI tool_choice constraint for one request.
// Mode is one of "", "auto", "none", "required", "function".
// FunctionName is set when Mode is "function".
type ToolChoice struct {
	Mode         string
	FunctionName string
}

// ForcesToolCall reports whether the constraint demands at least one tool call.
func (c ToolChoice) ForcesToolCall() bool {
	return c.Mode == "required" || c.Mode == "function"
}

// ApplyToolChoice narrows the advertised tool set according to the constraint.
// "none" removes all tools; a forced function keeps only the named tool when present.
func ApplyToolChoice(tools []ToolDefinition, choice ToolChoice) []ToolDefinition {
	switch choice.Mode {
	case "none":
		return nil
	case "function":
		for i := range tools {
			if tools[i].Name == choice.FunctionName {
				return tools[i : i+1]
			}
		}
	}
	return tools
}

// toolChoiceDirective renders the constraint as an explicit system instruction.
// Cursor's Agent Connect protocol has no native tool_choice field, so the
// requirement is enforced through the conversation state instead.
func toolChoiceDirective(choice ToolChoice) string {
	switch choice.Mode {
	case "required":
		return "Tool call required: you MUST respond by invoking one of the provided tools. " +
			"Do not reply with plain text. If information seems missing, pick the most relevant tool " +
			"and supply reasonable arguments."
	case "function":
		return fmt.Sprintf("Tool call required: you MUST respond by invoking the tool named %q. "+
			"Do not reply with plain text and do not invoke any other tool.", choice.FunctionName)
	}
	return ""
}

// nativeAgentToolNames are the lowercase names of Cursor's built-in agent
// tools. A client MCP tool whose name matches one of these (case-insensitive)
// is advertised under a prefixed wire name so the model cannot bind the call
// to the unusable built-in of the same name.
var nativeAgentToolNames = map[string]bool{
	"task": true, "shell": true, "bash": true, "terminal": true, "run_terminal_cmd": true,
	"read_file": true, "read": true, "list_dir": true, "ls": true,
	"glob": true, "glob_file_search": true, "file_search": true, "codebase_search": true,
	"grep": true, "grep_search": true, "search": true, "search_replace": true,
	"edit": true, "edit_file": true, "write": true, "create_file": true, "delete_file": true,
	"todo_write": true, "todowrite": true, "todo": true,
	"web_search": true, "websearch": true, "web_fetch": true, "webfetch": true, "fetch": true,
	"read_lints": true, "create_diagram": true, "update_memory": true, "fetch_rules": true,
	"apply_patch": true,
}

// mcpWireNamePrefix namespaces colliding MCP tool names on the Cursor wire.
const mcpWireNamePrefix = "mcp_"

// wireToolName returns the name a client tool is advertised under to the
// upstream agent harness.
func wireToolName(name string) string {
	trimmed := strings.TrimSpace(name)
	if nativeAgentToolNames[strings.ToLower(trimmed)] {
		return mcpWireNamePrefix + trimmed
	}
	return trimmed
}

// mcpOnlyToolDirective forbids Cursor's built-in agent tools when the client
// declared its own toolset. The built-ins execute through exec variants this
// headless gateway does not implement, so any call to them fails with
// "No exec result" on the client side.
func mcpOnlyToolDirective(tools []ToolDefinition) string {
	names := make([]string, 0, len(tools))
	for i := range tools {
		name := strings.TrimSpace(tools[i].Name)
		if name == "" {
			continue
		}
		wire := wireToolName(name)
		if wire != name {
			names = append(names, fmt.Sprintf("%s (use this whenever %s is needed)", wire, name))
		} else {
			names = append(names, name)
		}
	}
	if len(names) == 0 {
		return ""
	}
	return "The ONLY tools available in this session are these MCP tools: " +
		strings.Join(names, ", ") + ". " +
		"Cursor built-in tools (task, shell, terminal commands, read_file, list_dir, glob, grep, " +
		"search, edit_file, write, todo, web fetch/search, subagents) are NOT available here and " +
		"every call to them fails. Never call built-in tools; always use the MCP tools listed above."
}

// AccountCredentials are the fields required to open an Agent run.
type AccountCredentials struct {
	AccessToken   string
	RefreshToken  string
	AuthClientID  string
	BaseURL       string
	ClientVersion string
	MachineID     string
	MacMachineID  string
	SessionID     string
	ClientOS      string
	ClientArch    string
	Timezone      string
	Email         string
	GhostMode     string
	CookieJar     *CookieJar
}

// CredentialsFromMetadata extracts Cursor account fields from auth metadata.
func CredentialsFromMetadata(meta map[string]any) AccountCredentials {
	get := func(keys ...string) string {
		for _, key := range keys {
			if meta == nil {
				continue
			}
			if v, ok := meta[key]; ok {
				if s, ok := v.(string); ok && strings.TrimSpace(s) != "" {
					return strings.TrimSpace(s)
				}
			}
		}
		return ""
	}
	creds := AccountCredentials{
		AccessToken:   get("access_token"),
		RefreshToken:  get("refresh_token"),
		AuthClientID:  get("auth_client_id"),
		BaseURL:       get("base_url"),
		ClientVersion: get("client_version"),
		MachineID:     get("machine_id"),
		MacMachineID:  get("mac_machine_id"),
		SessionID:     get("session_id"),
		ClientOS:      get("client_os"),
		ClientArch:    get("client_arch"),
		Timezone:      get("timezone"),
		Email:         get("email"),
		GhostMode:     get("ghost_mode"),
	}
	if creds.BaseURL == "" {
		creds.BaseURL = cursorauth.DefaultBaseURL
	}
	if creds.ClientVersion == "" {
		creds.ClientVersion = cursorauth.DefaultClientVersion
	}
	if creds.AuthClientID == "" {
		creds.AuthClientID = cursorauth.DefaultAuthClientID
	}
	if creds.MachineID == "" {
		creds.MachineID = DesktopMachineID()
	}
	if creds.MacMachineID == "" {
		creds.MacMachineID = DesktopMacMachineID()
	}
	if creds.ClientOS == "" {
		creds.ClientOS = DesktopClientOS()
	}
	if creds.ClientArch == "" {
		creds.ClientArch = DesktopClientArch()
	}
	if creds.SessionID == "" {
		creds.SessionID = uuid.NewString()
	}
	if creds.GhostMode == "" {
		creds.GhostMode = "implicit-false"
	}
	jarKey := creds.Email
	if jarKey == "" {
		jarKey = creds.MachineID
	}
	creds.CookieJar = CookieJarForAccount(jarKey)
	return creds
}

func storeBlob(store map[string][]byte, data []byte) []byte {
	sum := sha256.Sum256(data)
	id := sum[:]
	store[hex.EncodeToString(id)] = data
	return id
}

// resumeContinuationPrompt drives a rebuilt run whose trailing history already
// contains the client-supplied tool results (used when the original bidi
// stream died while waiting for those results).
const resumeContinuationPrompt = "Continue the conversation. The results of your earlier tool calls are " +
	"already provided in the conversation history above. Use them to fulfill the user's most recent " +
	"request. Call more tools if needed; otherwise answer the user directly."

// splitTrailingUserRun keeps reminder/todo rows emitted as adjacent user
// messages in the incoming turn. The prefix is the transcript a checkpoint
// stored at the end of the previous assistant turn.
func splitTrailingUserRun(messages []ChatMessage) (prefix, turn []ChatMessage) {
	i := len(messages)
	for i > 0 && messages[i-1].Role == "user" {
		i--
	}
	return messages[:i], messages[i:]
}

func joinedUserText(turn []ChatMessage) string {
	parts := make([]string, 0, len(turn))
	for i := range turn {
		if content := strings.TrimSpace(turn[i].Content); content != "" {
			parts = append(parts, turn[i].Content)
		}
	}
	return strings.Join(parts, "\n\n")
}

const foldTurnTailResultLimit = 20000

// foldTurnTailText turns a structured tail that is newer than the latest
// checkpoint into the one text action accepted by a resumed Agent run.
func foldTurnTailText(tail []ChatMessage) string {
	var builder strings.Builder
	toolActivity := false
	write := func(text string) {
		if strings.TrimSpace(text) == "" {
			return
		}
		if builder.Len() > 0 {
			builder.WriteString("\n\n")
		}
		builder.WriteString(text)
	}
	for i := range tail {
		msg := &tail[i]
		switch msg.Role {
		case "user", "system":
			write(msg.Content)
		case "assistant":
			if strings.TrimSpace(msg.Content) != "" {
				write("[your reply so far]\n" + msg.Content)
			}
			for j := range msg.ToolCalls {
				toolActivity = true
				call := &msg.ToolCalls[j]
				args := "{}"
				if len(call.Arguments) > 0 {
					if payload, err := json.Marshal(call.Arguments); err == nil {
						args = string(payload)
					}
				}
				write(fmt.Sprintf("[called tool %s id=%s arguments=%s]",
					strings.TrimSpace(call.Name), normalizeFingerprintToolID(call.ID), args))
			}
		case "tool":
			toolActivity = true
			content := msg.Content
			if len(content) > foldTurnTailResultLimit {
				content = content[:foldTurnTailResultLimit] + "\n… (truncated)"
			}
			write(fmt.Sprintf("[tool result %s id=%s]\n%s",
				strings.TrimSpace(msg.Name), normalizeFingerprintToolID(msg.ToolCallID), content))
		}
	}
	if toolActivity {
		write("[The tool calls above have already run; continue the task from their results and do not repeat them.]")
	}
	return builder.String()
}

// lookupPrefixResume finds the longest stored checkpoint that leaves a real
// tail to execute. It also waits for an announced in-flight store, closing the
// race between a fast follow-up request and Cursor's trailing checkpoint.
func lookupPrefixResume(accountKey, wireModelID string, messages []ChatMessage) (*convEntry, string, string, []ChatMessage, bool) {
	scope := convScope(accountKey, wireModelID)
	echo := echoTranscript(messages)
	prefixes := conversationPrefixFingerprints(echo)
	resolved := func(i int, entry *convEntry) (*convEntry, string, string, []ChatMessage, bool) {
		if entry == nil || entry.model != wireModelID {
			return nil, "", "", nil, false
		}
		text := foldTurnTailText(echo[i:])
		if strings.TrimSpace(text) == "" {
			return nil, "", "", nil, false
		}
		return entry, prefixes[i], text, echo[:i], true
	}

	waitIndex := 0
	var wait <-chan struct{}
	for i := len(echo); i >= 1; i-- {
		if entry, ok := defaultConversationCache.LookupNoWait(scope, prefixes[i]); ok {
			if found, fingerprint, text, prefix, hit := resolved(i, entry); hit {
				return found, fingerprint, text, prefix, true
			}
			continue
		}
		if wait == nil {
			if pending := defaultConversationCache.PendingWait(scope, prefixes[i]); pending != nil {
				waitIndex, wait = i, pending
			}
		}
	}
	if wait == nil {
		return nil, "", "", nil, false
	}
	select {
	case <-wait:
	case <-time.After(convPendingWait):
	}
	entry, ok := defaultConversationCache.LookupNoWait(scope, prefixes[waitIndex])
	if !ok {
		return nil, "", "", nil, false
	}
	return resolved(waitIndex, entry)
}

// buildResumeRunRequest replays Cursor's own checkpoint under the same
// conversation id and sends only the new turn. This is the operation that
// preserves the provider prompt cache across separate gateway requests.
func buildResumeRunRequest(model string, entry *convEntry, userText string, tools []ToolDefinition, choice ToolChoice) (*agentv1.AgentClientMessage, map[string][]byte, string, error) {
	if entry == nil || entry.state == nil || strings.TrimSpace(entry.conversationID) == "" {
		return nil, nil, "", fmt.Errorf("cursor: no conversation checkpoint to resume")
	}
	selection := ResolveRequestedModel(model)
	publicID := selection.PublicID
	if publicID == "" {
		publicID = selection.ModelID
	}
	wireID := selection.ModelID
	displayID := publicID
	displayName := publicID
	if catalog, ok := catalogEntry(publicID); ok {
		if catalog.DisplayModel != "" {
			displayID = catalog.DisplayModel
		}
		if catalog.DisplayName != "" {
			displayName = catalog.DisplayName
		}
		selection.MaxMode = selection.MaxMode || catalog.MaxMode
		if !selection.VariantStringRepr && len(selection.Parameters) == 0 && len(catalog.Parameters) > 0 {
			selection.Parameters = append([]ModelParameter(nil), catalog.Parameters...)
		}
		if selection.VariantStringRepr && strings.TrimSpace(catalog.WireID) != "" {
			wireID = catalog.WireID
			selection.ModelID = wireID
		}
	}
	details := &agentv1.ModelDetails{ModelId: wireID, DisplayModelId: displayID, DisplayName: displayName}
	if selection.MaxMode {
		maxMode := true
		details.MaxMode = &maxMode
	}

	if directive := toolChoiceDirective(choice); directive != "" {
		userText += "\n\n[Response constraint: " + directive + "]"
	}
	if directive := mcpOnlyToolDirective(tools); directive != "" {
		userText += "\n\n[Tool constraint: " + directive + "]"
	}
	blobStore := make(map[string][]byte, len(entry.blobs))
	for key, value := range entry.blobs {
		blobStore[key] = append([]byte(nil), value...)
	}
	conversationID := entry.conversationID
	supportsImages := true
	run := &agentv1.AgentRunRequest{
		ConversationId:             &conversationID,
		ConversationState:          proto.Clone(entry.state).(*agentv1.ConversationStateStructure),
		ModelDetails:               details,
		RequestedModel:             toRequestedModelProto(selection),
		ClientSupportsInlineImages: &supportsImages,
		Action: &agentv1.ConversationAction{
			Action: &agentv1.ConversationAction_UserMessageAction{
				UserMessageAction: &agentv1.UserMessageAction{
					UserMessage: &agentv1.UserMessage{
						Text:      userText,
						MessageId: uuid.NewString(),
					},
				},
			},
		},
	}
	if definitions := buildMcpToolDefinitions(tools); len(definitions) > 0 {
		run.McpTools = &agentv1.McpTools{McpTools: definitions}
	}
	log.Debugf("cursor: built checkpoint resume conv=%s model=%s turns=%d", conversationID, wireID, len(entry.state.GetTurns()))
	return &agentv1.AgentClientMessage{
		Message: &agentv1.AgentClientMessage_RunRequest{RunRequest: run},
	}, blobStore, conversationID, nil
}

func echoTranscript(messages []ChatMessage) []ChatMessage {
	return append([]ChatMessage(nil), messages...)
}

func buildRunRequest(model string, messages []ChatMessage, tools []ToolDefinition, choice ToolChoice) (*agentv1.AgentClientMessage, map[string][]byte, string, error) {
	selection := ResolveRequestedModel(model)
	model = selection.ModelID
	blobStore := map[string][]byte{}
	systemPrompt := "You are a helpful assistant."
	systemBlob := storeBlob(blobStore, mustJSON(map[string]any{
		"role":    "system",
		"content": systemPrompt,
	}))

	var activeUser *ChatMessage
	historyEnd := len(messages)
	for i := len(messages) - 1; i >= 0; i-- {
		if messages[i].Role == "user" && strings.TrimSpace(messages[i].Content) != "" {
			activeUser = &messages[i]
			historyEnd = i
			break
		}
	}
	if activeUser == nil {
		return nil, nil, "", fmt.Errorf("cursor: request has no user message")
	}

	// When assistant/tool rows trail the last user message the request is a
	// tool-results continuation that must be replayed in a fresh run: keep the
	// whole transcript as history and drive the run with a continuation prompt.
	resume := historyEnd < len(messages)-1
	historyMsgs := messages[:historyEnd]
	actionText := activeUser.Content
	if resume {
		historyMsgs = messages
		actionText = resumeContinuationPrompt
	}

	// Track tool names so tool-result parts can include toolName (cursor2api).
	toolNames := map[string]string{}
	rootIDs := [][]byte{systemBlob}
	for _, msg := range historyMsgs {
		switch msg.Role {
		case "user":
			if strings.TrimSpace(msg.Content) == "" {
				continue
			}
			rootIDs = append(rootIDs, storeBlob(blobStore, mustJSON(map[string]any{
				"role": "user",
				"content": []map[string]string{
					{"type": "text", "text": msg.Content},
				},
			})))
		case "assistant":
			if strings.TrimSpace(msg.Content) == "" && len(msg.ToolCalls) == 0 {
				continue
			}
			content := make([]map[string]any, 0, 1+len(msg.ToolCalls))
			if strings.TrimSpace(msg.Content) != "" {
				content = append(content, map[string]any{
					"type": "text",
					"text": msg.Content,
				})
			}
			for _, tc := range msg.ToolCalls {
				if strings.TrimSpace(tc.ID) != "" && strings.TrimSpace(tc.Name) != "" {
					toolNames[tc.ID] = tc.Name
				}
				args := any(tc.Arguments)
				if args == nil {
					args = map[string]any{}
				}
				content = append(content, map[string]any{
					"type":       "tool-call",
					"toolCallId": tc.ID,
					// History rows use the wire name the tool is advertised
					// under so the model links past calls to a callable tool.
					"toolName": wireToolName(tc.Name),
					"args":     args,
				})
			}
			if len(content) == 0 {
				continue
			}
			rootIDs = append(rootIDs, storeBlob(blobStore, mustJSON(map[string]any{
				"role":    "assistant",
				"content": content,
			})))
		case "tool":
			if strings.TrimSpace(msg.ToolCallID) == "" {
				continue
			}
			toolName := strings.TrimSpace(msg.Name)
			if toolName == "" {
				toolName = toolNames[msg.ToolCallID]
			}
			resultPart := map[string]any{
				"type":       "tool-result",
				"toolName":   wireToolName(toolName),
				"toolCallId": msg.ToolCallID,
				"result":     msg.Content,
				"toolKind":   "mcp",
			}
			rootIDs = append(rootIDs, storeBlob(blobStore, mustJSON(map[string]any{
				"role": "tool",
				"id":   msg.ToolCallID,
				"content": []map[string]any{
					resultPart,
				},
			})))
		case "system":
			if strings.TrimSpace(msg.Content) == "" {
				continue
			}
			rootIDs = append(rootIDs, storeBlob(blobStore, mustJSON(map[string]any{
				"role":    "system",
				"content": msg.Content,
			})))
		}
	}

	if directive := toolChoiceDirective(choice); directive != "" {
		rootIDs = append(rootIDs, storeBlob(blobStore, mustJSON(map[string]any{
			"role":    "system",
			"content": directive,
		})))
		// Agent scaffolding can drown out system rows, so repeat the
		// constraint on the user action itself; models weight it heavily.
		actionText = actionText + "\n\n[Response constraint: " + directive + "]"
	}

	if directive := mcpOnlyToolDirective(tools); directive != "" {
		// The Cursor agent harness also advertises its built-in tools (task,
		// shell, read_file, list_dir, glob, grep, …). This headless gateway
		// cannot execute them — the model would see "No exec result" — so
		// steer the model to the declared MCP tools on the first attempt.
		rootIDs = append(rootIDs, storeBlob(blobStore, mustJSON(map[string]any{
			"role":    "system",
			"content": directive,
		})))
		actionText = actionText + "\n\n[Tool constraint: " + directive + "]"
	}

	conversationID := uuid.NewString()
	// Desktop / cursor2api do not set exclude_workspace_context by default;
	// forcing it true is rejected for many accounts ("Workspace context
	// exclusion is not allowed…").
	supportsImages := true
	publicID := selection.PublicID
	if publicID == "" {
		publicID = model
	}
	wireID := selection.ModelID
	displayID := publicID
	displayName := publicID
	if entry, ok := catalogEntry(publicID); ok {
		if entry.DisplayModel != "" {
			displayID = entry.DisplayModel
		}
		if entry.DisplayName != "" {
			displayName = entry.DisplayName
		}
		selection.MaxMode = selection.MaxMode || entry.MaxMode
		if !selection.VariantStringRepr && len(selection.Parameters) == 0 && len(entry.Parameters) > 0 {
			selection.Parameters = append([]ModelParameter(nil), entry.Parameters...)
		}
		if selection.VariantStringRepr && strings.TrimSpace(entry.WireID) != "" {
			wireID = entry.WireID
			selection.ModelID = wireID
		}
	}
	details := &agentv1.ModelDetails{ModelId: wireID, DisplayModelId: displayID, DisplayName: displayName}
	if selection.MaxMode {
		maxMode := true
		details.MaxMode = &maxMode
	}
	run := &agentv1.AgentRunRequest{
		ConversationId:             &conversationID,
		ConversationState:          &agentv1.ConversationStateStructure{RootPromptMessagesJson: rootIDs},
		ModelDetails:               details,
		RequestedModel:             toRequestedModelProto(selection),
		ClientSupportsInlineImages: &supportsImages,
		Action: &agentv1.ConversationAction{
			Action: &agentv1.ConversationAction_UserMessageAction{
				UserMessageAction: &agentv1.UserMessageAction{
					UserMessage: &agentv1.UserMessage{
						Text:      actionText,
						MessageId: uuid.NewString(),
					},
				},
			},
		},
	}
	if defs := buildMcpToolDefinitions(tools); len(defs) > 0 {
		run.McpTools = &agentv1.McpTools{McpTools: defs}
	}
	client := &agentv1.AgentClientMessage{
		Message: &agentv1.AgentClientMessage_RunRequest{RunRequest: run},
	}
	return client, blobStore, conversationID, nil
}

func mustJSON(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		return []byte(`{}`)
	}
	return b
}

func handleKV(stream *BidiStream, req *agentv1.KvServerMessage, blobStore map[string][]byte) error {
	resp := &agentv1.KvClientMessage{Id: req.Id}
	switch m := req.Message.(type) {
	case *agentv1.KvServerMessage_GetBlobArgs:
		key := hex.EncodeToString(m.GetBlobArgs.GetBlobId())
		if data, ok := blobStore[key]; ok {
			resp.Message = &agentv1.KvClientMessage_GetBlobResult{
				GetBlobResult: &agentv1.GetBlobResult{BlobData: data},
			}
		} else {
			resp.Message = &agentv1.KvClientMessage_GetBlobResult{GetBlobResult: &agentv1.GetBlobResult{}}
		}
	case *agentv1.KvServerMessage_SetBlobArgs:
		key := hex.EncodeToString(m.SetBlobArgs.GetBlobId())
		blobStore[key] = append([]byte(nil), m.SetBlobArgs.GetBlobData()...)
		resp.Message = &agentv1.KvClientMessage_SetBlobResult{SetBlobResult: &agentv1.SetBlobResult{}}
	default:
		return nil
	}
	client := &agentv1.AgentClientMessage{
		Message: &agentv1.AgentClientMessage_KvClientMessage{KvClientMessage: resp},
	}
	payload, err := proto.Marshal(client)
	if err != nil {
		return err
	}
	return stream.WriteEnvelope(payload, false)
}

func sendExecStreamClose(stream *BidiStream, id uint32) error {
	control := &agentv1.ExecClientControlMessage{
		Message: &agentv1.ExecClientControlMessage_StreamClose{
			StreamClose: &agentv1.ExecClientStreamClose{Id: id},
		},
	}
	client := &agentv1.AgentClientMessage{
		Message: &agentv1.AgentClientMessage_ExecClientControlMessage{ExecClientControlMessage: control},
	}
	payload, err := proto.Marshal(client)
	if err != nil {
		return err
	}
	return stream.WriteEnvelope(payload, false)
}

func headlessRequestContext() *agentv1.RequestContext {
	trueVal := true
	falseVal := false
	ctx := &agentv1.RequestContext{
		EnvInfoComplete:             &trueVal,
		RulesInfoComplete:           &trueVal,
		RepositoryInfoComplete:      &trueVal,
		GitRepoInfoComplete:         &trueVal,
		GitStatusInfoComplete:       &trueVal,
		CustomSubagentsInfoComplete: &trueVal,
		AgentSkillsInfoComplete:     &trueVal,
		McpInfoComplete:             &trueVal,
		McpFileSystemInfoComplete:   &trueVal,
		WebFetchEnabled:             &falseVal,
		WebSearchEnabled:            &falseVal,
		SupportsMcpAuth:             &falseVal,
		ReadLintsEnabled:            &falseVal,
	}
	return ctx
}
