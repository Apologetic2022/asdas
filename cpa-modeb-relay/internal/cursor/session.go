package cursor

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	agentv1 "github.com/router-for-me/CLIProxyAPI/v7/internal/cursor/proto/agent/v1"
	log "github.com/sirupsen/logrus"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
)

// MCPProviderIdentifier is the provider_identifier advertised for OpenAI tools.
const MCPProviderIdentifier = "cliproxyapi"

// ToolDefinition is an OpenAI-style function tool projected into Cursor MCP tools.
type ToolDefinition struct {
	Name        string
	Description string
	Parameters  map[string]any
}

// ToolCall is a model-requested function invocation.
type ToolCall struct {
	ID        string
	Name      string
	Arguments map[string]any
}

// ToolResult is a client-provided tool response.
type ToolResult struct {
	ToolCallID string
	Name       string
	Content    string
	IsError    bool
}

// StreamEvent is one Cursor→OpenAI segment event.
type StreamEvent struct {
	Type             string
	Text             string
	ToolCall         *ToolCall
	Reason           string
	Message          string
	InputTokens      int64
	OutputTokens     int64
	CacheReadTokens  int64
	CacheWriteTokens int64
	ReasoningTokens  int64
}

type pendingExec struct {
	request *agentv1.ExecServerMessage
	call    ToolCall
}

// SessionOptions carries per-request modifiers for a Cursor Agent run.
type SessionOptions struct {
	// AuthID identifies the gateway credential that owns this run so that
	// follow-up tool-result requests can be pinned to the same account.
	AuthID string
	// ToolChoice is the OpenAI tool_choice constraint for the run.
	ToolChoice ToolChoice
}

// Session is a live Agent Connect run that can pause for client tools.
type Session struct {
	ID             string
	ConversationID string
	Model          string
	AuthID         string

	mu           sync.Mutex
	stream       *BidiStream
	blobStore    map[string][]byte
	tools        []ToolDefinition
	toolIndex    map[string]*ToolDefinition
	pending      map[string]*pendingExec
	events       chan StreamEvent
	errCh        chan error
	closed       bool
	waitingTools bool
	pauseCh      chan struct{}
	cancel       context.CancelFunc
	lastActivity time.Time
	manager      *SessionManager

	// turnEndedSeen records that the pre-tool segment already delivered its
	// TurnEnded (and usage). When it did not, the TurnEnded that surfaces
	// right after tool results are submitted belongs to the previous segment
	// and must not terminate the new one (see handleServerMessage).
	turnEndedSeen      bool
	swallowTurnEnd     bool
	contentSinceResume bool
}

// ChatResult is the collected text response from one Agent segment / run.
type ChatResult struct {
	Text             string
	Thinking         string
	ToolCalls        []ToolCall
	FinishReason     string
	ConversationID   string
	SessionID        string
	InputTokens      int64
	OutputTokens     int64
	CacheReadTokens  int64
	CacheWriteTokens int64
	ReasoningTokens  int64
}

// StartSession opens a new Agent run for the given messages/tools.
func StartSession(ctx context.Context, creds AccountCredentials, model string, messages []ChatMessage, tools []ToolDefinition, opts SessionOptions) (*Session, error) {
	if strings.TrimSpace(creds.AccessToken) == "" {
		return nil, fmt.Errorf("cursor: access_token is required")
	}
	tools = ApplyToolChoice(tools, opts.ToolChoice)
	selection := ResolveRequestedModel(model)
	clientMsg, blobStore, conversationID, err := buildRunRequest(model, messages, tools, opts)
	if err != nil {
		return nil, err
	}
	first, err := proto.Marshal(clientMsg)
	if err != nil {
		return nil, err
	}

	profile := ProfileFromCredentials(creds)

	// Detach the Agent H2 stream from the inbound HTTP request context.
	// Tool round-trips span multiple client requests; cancelling with the
	// first response would close the pipe before mcp_result can be written.
	if err = ctx.Err(); err != nil {
		return nil, err
	}
	runCtx, cancel := context.WithCancel(context.Background())
	openRun := func() (*BidiStream, error) {
		requestID := uuid.NewString()
		headers, errHdr := profile.Headers(creds.AccessToken, requestID, "")
		if errHdr != nil {
			return nil, errHdr
		}
		headers["x-original-request-id"] = requestID
		return OpenAgentRun(runCtx, creds.BaseURL, headers, first)
	}
	stream, err := openRun()
	if err != nil && isTransientRunOpenError(err) && ctx.Err() == nil {
		// Cursor's edge occasionally answers a run open with a bare 502/503;
		// a single quick retry rides it out instead of failing the request.
		log.Warnf("cursor: agent run open failed transiently (%v); retrying once", err)
		time.Sleep(500 * time.Millisecond)
		stream, err = openRun()
	}
	if err != nil {
		cancel()
		return nil, err
	}
	if profile.CookieJar != nil {
		profile.CookieJar.RememberResponse(creds.BaseURL, stream.ResponseHeader())
	}

	session := &Session{
		ID:             uuid.NewString(),
		ConversationID: conversationID,
		Model:          selection.ModelID,
		AuthID:         opts.AuthID,
		stream:         stream,
		blobStore:      blobStore,
		tools:          append([]ToolDefinition(nil), tools...),
		toolIndex:      indexTools(tools),
		pending:        map[string]*pendingExec{},
		events:         make(chan StreamEvent, 64),
		errCh:          make(chan error, 1),
		pauseCh:        make(chan struct{}),
		cancel:         cancel,
		lastActivity:   time.Now(),
		manager:        DefaultSessionManager(),
	}
	session.manager.Register(session)
	go session.heartbeatLoop(runCtx)
	go session.readLoop(runCtx)
	return session, nil
}

// isTransientRunOpenError reports whether an agent run open failure is worth
// one immediate retry (upstream edge 5xx or a broken connection attempt).
func isTransientRunOpenError(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	for _, marker := range []string{
		"cursor agent run HTTP 502",
		"cursor agent run HTTP 503",
		"cursor agent run HTTP 504",
		"cursor h2 open:",
	} {
		if strings.Contains(msg, marker) {
			return true
		}
	}
	return false
}

func indexTools(tools []ToolDefinition) map[string]*ToolDefinition {
	out := make(map[string]*ToolDefinition, len(tools))
	for i := range tools {
		name := strings.TrimSpace(tools[i].Name)
		if name == "" {
			continue
		}
		// Index by both the client name and the wire name the tool was
		// advertised under; emitted tool_call events always carry the
		// client name (tools[i].Name).
		out[name] = &tools[i]
		if wire := wireToolName(name); wire != name {
			out[wire] = &tools[i]
		}
	}
	return out
}

func (s *Session) touch() {
	s.mu.Lock()
	s.lastActivity = time.Now()
	s.mu.Unlock()
}

// markContent records that the current segment produced model output; used to
// distinguish a real TurnEnded from a stale pre-tool one after a resume.
func (s *Session) markContent() {
	s.mu.Lock()
	s.contentSinceResume = true
	s.mu.Unlock()
}

func (s *Session) heartbeatLoop(ctx context.Context) {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.mu.Lock()
			closed := s.closed
			stream := s.stream
			s.mu.Unlock()
			if closed || stream == nil {
				return
			}
			hb := &agentv1.AgentClientMessage{
				Message: &agentv1.AgentClientMessage_ClientHeartbeat{ClientHeartbeat: &agentv1.ClientHeartbeat{}},
			}
			payload, err := proto.Marshal(hb)
			if err != nil {
				continue
			}
			_ = stream.WriteEnvelope(payload, false)
		}
	}
}

// readChunk is one raw read from the Agent H2 response body.
type readChunk struct {
	data []byte
	err  error
}

// parallelToolGrace is how long the read loop keeps draining after the first
// client tool call before pausing the segment. Parallel tool calls are sent
// back-to-back by the upstream agent; without the grace window only the first
// one would surface in the current segment.
const parallelToolGrace = 120 * time.Millisecond

func (s *Session) readLoop(ctx context.Context) {
	defer close(s.events)
	decoder := NewDecoder()
	if s.stream != nil {
		if enc := strings.TrimSpace(s.stream.ResponseHeader().Get("Connect-Content-Encoding")); enc != "" {
			decoder.SetCompression(enc)
		} else if enc := strings.TrimSpace(s.stream.ResponseHeader().Get("connect-content-encoding")); enc != "" {
			decoder.SetCompression(enc)
		}
	}

	// A dedicated goroutine performs the blocking body reads so the consumer
	// loop can bound waits (parallel tool-call drain) without losing data.
	reads := make(chan readChunk, 4)
	go func() {
		buf := make([]byte, 32*1024)
		for {
			n, errRead := s.stream.Read(buf)
			chunk := readChunk{err: errRead}
			if n > 0 {
				chunk.data = append([]byte(nil), buf[:n]...)
			}
			select {
			case reads <- chunk:
			case <-ctx.Done():
				return
			}
			if errRead != nil {
				return
			}
		}
	}()

	for {
		s.mu.Lock()
		waiting := s.waitingTools
		closed := s.closed
		pauseCh := s.pauseCh
		s.mu.Unlock()
		if closed {
			return
		}
		select {
		case <-ctx.Done():
			s.mu.Lock()
			alreadyClosed := s.closed
			s.mu.Unlock()
			if !alreadyClosed {
				s.emit(StreamEvent{Type: "error", Message: ctx.Err().Error()})
				s.emit(StreamEvent{Type: "segment_end", Reason: "error"})
			}
			return
		default:
		}
		if waiting {
			select {
			case <-ctx.Done():
				return
			case <-pauseCh:
				continue
			}
		}

		var chunk readChunk
		select {
		case <-ctx.Done():
			continue
		case chunk = <-reads:
		}
		endForTools, done := s.processChunk(decoder, chunk)
		if done {
			return
		}
		if !endForTools {
			continue
		}
		// First client tool call of the segment: keep draining briefly so
		// parallel tool calls issued together surface in the same segment.
		timer := time.NewTimer(parallelToolGrace)
	drain:
		for {
			select {
			case <-ctx.Done():
				timer.Stop()
				break drain
			case <-timer.C:
				break drain
			case chunk = <-reads:
				_, done = s.processChunk(decoder, chunk)
				if done {
					timer.Stop()
					return
				}
			}
		}
		s.beginWaitingTools()
		s.emit(StreamEvent{Type: "segment_end", Reason: "tool_calls"})
	}
}

// processChunk feeds one raw read into the Connect decoder and dispatches the
// complete envelopes. It reports whether the segment paused for client tools
// and whether the read loop is finished.
func (s *Session) processChunk(decoder *Decoder, chunk readChunk) (endForTools bool, done bool) {
	if len(chunk.data) > 0 {
		envelopes, errFeed := decoder.Feed(chunk.data)
		if errFeed != nil {
			s.fail(errFeed)
			return false, true
		}
		for _, env := range envelopes {
			if env.EndStream() {
				if len(env.Payload) > 0 {
					var trailer struct {
						Error json.RawMessage `json:"error"`
					}
					_ = json.Unmarshal(env.Payload, &trailer)
					if len(trailer.Error) > 0 && string(trailer.Error) != "null" {
						s.fail(fmt.Errorf("cursor connect end-stream error: %s", string(trailer.Error)))
						return false, true
					}
				}
				s.emit(StreamEvent{Type: "segment_end", Reason: "stop"})
				_ = s.Close()
				return false, true
			}
			serverMsg := &agentv1.AgentServerMessage{}
			if err := proto.Unmarshal(env.Payload, serverMsg); err != nil {
				s.fail(fmt.Errorf("cursor decode server message: %w", err))
				return false, true
			}
			pause, err := s.handleServerMessage(serverMsg)
			if err != nil {
				s.fail(err)
				return false, true
			}
			if pause {
				endForTools = true
			}
		}
	}
	if chunk.err == io.EOF {
		s.emit(StreamEvent{Type: "segment_end", Reason: "stop"})
		_ = s.Close()
		return false, true
	}
	if chunk.err != nil {
		s.fail(chunk.err)
		return false, true
	}
	return endForTools, false
}

func (s *Session) beginWaitingTools() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.waitingTools {
		return
	}
	s.waitingTools = true
	s.pauseCh = make(chan struct{})
}

func (s *Session) resumeReading() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.waitingTools {
		return
	}
	s.waitingTools = false
	close(s.pauseCh)
	s.pauseCh = make(chan struct{})
}

func (s *Session) emit(ev StreamEvent) {
	s.touch()
	select {
	case s.events <- ev:
	default:
		// Drop if consumer is gone; Close will unwind.
		select {
		case s.events <- ev:
		case <-time.After(2 * time.Second):
		}
	}
}

func (s *Session) fail(err error) {
	if err == nil {
		return
	}
	select {
	case s.errCh <- err:
	default:
	}
	s.emit(StreamEvent{Type: "error", Message: err.Error()})
	s.emit(StreamEvent{Type: "segment_end", Reason: "error"})
	_ = s.Close()
}

// Events returns the live event stream for the current segment consumer.
func (s *Session) Events() <-chan StreamEvent { return s.events }

// CollectSegment drains events until segment_end.
func (s *Session) CollectSegment(ctx context.Context) (*ChatResult, error) {
	result := &ChatResult{
		ConversationID: s.ConversationID,
		SessionID:      s.ID,
		FinishReason:   "stop",
	}
	var text strings.Builder
	var thinking strings.Builder
	for {
		select {
		case <-ctx.Done():
			_ = s.Close()
			return nil, ctx.Err()
		case err := <-s.errCh:
			if err != nil {
				return nil, err
			}
		case ev, ok := <-s.events:
			if !ok {
				result.Text = text.String()
				result.Thinking = thinking.String()
				return result, nil
			}
			switch ev.Type {
			case "text_delta":
				text.WriteString(ev.Text)
			case "thinking_delta":
				thinking.WriteString(ev.Text)
			case "tool_call":
				if ev.ToolCall != nil {
					result.ToolCalls = append(result.ToolCalls, *ev.ToolCall)
				}
			case "usage_final":
				result.InputTokens = ev.InputTokens
				result.OutputTokens = ev.OutputTokens
				result.CacheReadTokens = ev.CacheReadTokens
				result.CacheWriteTokens = ev.CacheWriteTokens
				result.ReasoningTokens = ev.ReasoningTokens
			case "error":
				return nil, fmt.Errorf("cursor: %s", ev.Message)
			case "segment_end":
				result.Text = text.String()
				result.Thinking = thinking.String()
				if ev.Reason != "" {
					result.FinishReason = ev.Reason
				}
				if result.FinishReason == "tool_calls" && len(result.ToolCalls) == 0 {
					result.FinishReason = "stop"
				}
				return result, nil
			}
		}
	}
}

// IterSegment invokes fn for each event until segment_end (inclusive).
func (s *Session) IterSegment(ctx context.Context, fn func(StreamEvent) error) error {
	for {
		select {
		case <-ctx.Done():
			_ = s.Close()
			return ctx.Err()
		case err := <-s.errCh:
			if err != nil {
				return err
			}
		case ev, ok := <-s.events:
			if !ok {
				return nil
			}
			if err := fn(ev); err != nil {
				return err
			}
			if ev.Type == "segment_end" {
				return nil
			}
			if ev.Type == "error" {
				return fmt.Errorf("cursor: %s", ev.Message)
			}
		}
	}
}

// SubmitToolResults replies on the live exec stream and resumes reading.
func (s *Session) SubmitToolResults(results []ToolResult) error {
	if s == nil {
		return fmt.Errorf("cursor: nil session")
	}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return fmt.Errorf("cursor: session closed")
	}
	pending := make([]*pendingExec, 0, len(results))
	for _, result := range results {
		item, ok := s.pending[result.ToolCallID]
		if !ok {
			s.mu.Unlock()
			return fmt.Errorf("cursor: no pending tool call %s", result.ToolCallID)
		}
		pending = append(pending, item)
	}
	s.mu.Unlock()

	for i, result := range results {
		item := pending[i]
		if err := s.sendMcpResult(item.request, result); err != nil {
			return err
		}
		s.mu.Lock()
		delete(s.pending, result.ToolCallID)
		s.mu.Unlock()
		if s.manager != nil {
			s.manager.UnbindPending(result.ToolCallID)
		}
	}
	s.mu.Lock()
	// If the pre-tool segment never delivered its TurnEnded, the next one that
	// surfaces after the resume belongs to that finished segment.
	s.swallowTurnEnd = !s.turnEndedSeen
	s.turnEndedSeen = false
	s.contentSinceResume = false
	s.mu.Unlock()
	s.resumeReading()
	return nil
}

func (s *Session) sendMcpResult(req *agentv1.ExecServerMessage, result ToolResult) error {
	mcp := &agentv1.McpResult{
		Result: &agentv1.McpResult_Success{
			Success: &agentv1.McpSuccess{
				Content: []*agentv1.McpToolResultContentItem{
					{
						Content: &agentv1.McpToolResultContentItem_Text{
							Text: &agentv1.McpTextContent{Text: result.Content},
						},
					},
				},
				IsError: result.IsError,
			},
		},
	}
	execClient := &agentv1.ExecClientMessage{
		Id:     req.Id,
		ExecId: req.ExecId,
		Message: &agentv1.ExecClientMessage_McpResult{
			McpResult: mcp,
		},
	}
	client := &agentv1.AgentClientMessage{
		Message: &agentv1.AgentClientMessage_ExecClientMessage{ExecClientMessage: execClient},
	}
	payload, err := proto.Marshal(client)
	if err != nil {
		return err
	}
	if err = s.stream.WriteEnvelope(payload, false); err != nil {
		return err
	}
	return sendExecStreamClose(s.stream, req.Id)
}

// Close tears down the Agent stream.
func (s *Session) Close() error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	stream := s.stream
	cancel := s.cancel
	s.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	if s.manager != nil {
		s.manager.Remove(s)
	}
	if stream != nil {
		return stream.Close()
	}
	return nil
}

func (s *Session) handleServerMessage(msg *agentv1.AgentServerMessage) (pauseForTools bool, err error) {
	switch m := msg.Message.(type) {
	case *agentv1.AgentServerMessage_InteractionUpdate:
		switch u := m.InteractionUpdate.Message.(type) {
		case *agentv1.InteractionUpdate_TextDelta:
			s.markContent()
			s.emit(StreamEvent{Type: "text_delta", Text: u.TextDelta.GetText()})
		case *agentv1.InteractionUpdate_ThinkingDelta:
			s.markContent()
			s.emit(StreamEvent{Type: "thinking_delta", Text: u.ThinkingDelta.GetText()})
		case *agentv1.InteractionUpdate_TurnEnded:
			ev := StreamEvent{Type: "usage_final"}
			if u.TurnEnded.InputTokens != nil {
				ev.InputTokens = *u.TurnEnded.InputTokens
			}
			if u.TurnEnded.OutputTokens != nil {
				ev.OutputTokens = *u.TurnEnded.OutputTokens
			}
			if u.TurnEnded.CacheReadTokens != nil {
				ev.CacheReadTokens = *u.TurnEnded.CacheReadTokens
			}
			if u.TurnEnded.CacheWriteTokens != nil {
				ev.CacheWriteTokens = *u.TurnEnded.CacheWriteTokens
			}
			if u.TurnEnded.ReasoningTokens != nil {
				ev.ReasoningTokens = *u.TurnEnded.ReasoningTokens
			}
			s.mu.Lock()
			pendingCount := len(s.pending)
			waiting := s.waitingTools
			swallow := s.swallowTurnEnd && !s.contentSinceResume
			s.swallowTurnEnd = false
			if pendingCount > 0 || waiting {
				s.turnEndedSeen = true
			}
			s.mu.Unlock()
			if pendingCount > 0 || waiting {
				// cursor2api: some transports emit turn_ended for the pre-tool
				// segment before mcp_result is returned. Surface its usage so
				// tool_calls responses carry token counts, but keep the bidi
				// run open while client tools are still pending.
				s.emit(ev)
				return false, nil
			}
			if swallow {
				// Late TurnEnded for the pre-tool segment that was still in
				// flight when reading paused; it must not end the segment the
				// client resumed with tool results.
				s.emit(ev)
				return false, nil
			}
			s.emit(ev)
			s.emit(StreamEvent{Type: "segment_end", Reason: "stop"})
			go func() { _ = s.Close() }()
		case *agentv1.InteractionUpdate_ToolCallStarted,
			*agentv1.InteractionUpdate_PartialToolCall,
			*agentv1.InteractionUpdate_ToolCallCompleted:
			// Client-visible tool calls are driven by Exec mcp_args for declared tools.
		}
	case *agentv1.AgentServerMessage_KvServerMessage:
		return false, handleKV(s.stream, m.KvServerMessage, s.blobStore)
	case *agentv1.AgentServerMessage_ExecServerMessage:
		return s.handleExec(m.ExecServerMessage)
	case *agentv1.AgentServerMessage_ConversationCheckpointUpdate:
		// ignore for MVP
	case *agentv1.AgentServerMessage_ServerMetrics:
		// ignore
	}
	return false, nil
}

func (s *Session) handleExec(req *agentv1.ExecServerMessage) (bool, error) {
	switch m := req.Message.(type) {
	case nil:
		// The agent harness asked the client to execute one of Cursor's
		// built-in tools (task, shell, read_file, list_dir, …) whose exec
		// variant is not part of the trimmed MVP proto, so the oneof decoded
		// to nil. Closing the exec stream silently makes the model see the
		// opaque "No exec result" failure; answer with an instructive error
		// instead so the model reroutes to the declared MCP tools.
		fields := unknownProtoFieldNumbers(req.ProtoReflect().GetUnknown())
		log.Warnf("cursor: rejecting built-in tool exec request (unknown exec variant, proto fields %v); steering model to MCP tools %v", fields, s.availableToolNames())
		return false, s.throwExec(req, s.nativeToolUnavailableMessage())
	case *agentv1.ExecServerMessage_RequestContextArgs:
		result := &agentv1.RequestContextResult{
			Result: &agentv1.RequestContextResult_Success{
				Success: &agentv1.RequestContextSuccess{
					RequestContext: s.requestContext(),
				},
			},
		}
		execClient := &agentv1.ExecClientMessage{
			Id:     req.Id,
			ExecId: req.ExecId,
			Message: &agentv1.ExecClientMessage_RequestContextResult{
				RequestContextResult: result,
			},
		}
		client := &agentv1.AgentClientMessage{
			Message: &agentv1.AgentClientMessage_ExecClientMessage{ExecClientMessage: execClient},
		}
		payload, err := proto.Marshal(client)
		if err != nil {
			return false, err
		}
		if err = s.stream.WriteEnvelope(payload, false); err != nil {
			return false, err
		}
		return false, sendExecStreamClose(s.stream, req.Id)
	case *agentv1.ExecServerMessage_McpArgs:
		return s.handleMcpArgs(req, m.McpArgs)
	}
	return false, rejectUnsupportedExec(s.stream, req)
}

func (s *Session) handleMcpArgs(req *agentv1.ExecServerMessage, args *agentv1.McpArgs) (bool, error) {
	toolName := strings.TrimSpace(args.GetToolName())
	if toolName == "" {
		toolName = strings.TrimSpace(args.GetName())
	}
	provider := strings.TrimSpace(args.GetProviderIdentifier())
	if provider == "" {
		provider = strings.TrimSpace(args.GetServerIdentifier())
	}
	def := s.lookupTool(toolName, provider)
	if def == nil {
		result := &agentv1.McpResult{
			Result: &agentv1.McpResult_ToolNotFound{
				ToolNotFound: &agentv1.McpToolNotFound{
					Name:           toolName,
					AvailableTools: s.availableToolNames(),
				},
			},
		}
		return false, s.replyMcp(req, result)
	}
	callID := strings.TrimSpace(args.GetToolCallId())
	if callID == "" {
		callID = uuid.NewString()
	}
	call := ToolCall{
		ID:        callID,
		Name:      def.Name,
		Arguments: decodeMcpArguments(args),
	}
	copyReq := proto.Clone(req).(*agentv1.ExecServerMessage)
	s.mu.Lock()
	s.pending[callID] = &pendingExec{request: copyReq, call: call}
	s.mu.Unlock()
	if s.manager != nil {
		s.manager.BindPending(callID, s)
	}
	s.markContent()
	s.emit(StreamEvent{Type: "tool_call", ToolCall: &call})
	return true, nil
}

func (s *Session) replyMcp(req *agentv1.ExecServerMessage, result *agentv1.McpResult) error {
	execClient := &agentv1.ExecClientMessage{
		Id:      req.Id,
		ExecId:  req.ExecId,
		Message: &agentv1.ExecClientMessage_McpResult{McpResult: result},
	}
	client := &agentv1.AgentClientMessage{
		Message: &agentv1.AgentClientMessage_ExecClientMessage{ExecClientMessage: execClient},
	}
	payload, err := proto.Marshal(client)
	if err != nil {
		return err
	}
	if err = s.stream.WriteEnvelope(payload, false); err != nil {
		return err
	}
	return sendExecStreamClose(s.stream, req.Id)
}

func (s *Session) lookupTool(name, provider string) *ToolDefinition {
	if name == "" {
		return nil
	}
	def := s.toolIndex[name]
	if def == nil {
		return nil
	}
	if provider == "" || provider == MCPProviderIdentifier {
		return def
	}
	return nil
}

// availableToolNames returns the wire names the model can actually call.
func (s *Session) availableToolNames() []string {
	names := make([]string, 0, len(s.tools))
	for _, t := range s.tools {
		if t.Name != "" {
			names = append(names, wireToolName(t.Name))
		}
	}
	return names
}

func (s *Session) requestContext() *agentv1.RequestContext {
	ctx := headlessRequestContext()
	if len(s.tools) > 0 {
		ctx.Tools = buildMcpToolDefinitions(s.tools)
	}
	return ctx
}

func rejectUnsupportedExec(stream *BidiStream, req *agentv1.ExecServerMessage) error {
	return throwExecOnStream(stream, req.Id, "No handler for Cursor exec message")
}

// nativeToolUnavailableMessage tells the model why a built-in tool failed and
// which tools it must use instead.
func (s *Session) nativeToolUnavailableMessage() string {
	names := s.availableToolNames()
	if len(names) == 0 {
		return "This built-in tool is not available in this environment. Answer the user directly without tools."
	}
	return "This built-in tool is not available in this environment. Do not retry it. " +
		"Use ONLY these MCP tools instead: " + strings.Join(names, ", ") + "."
}

// throwExec reports a client-side exec failure with a model-readable reason.
func (s *Session) throwExec(req *agentv1.ExecServerMessage, message string) error {
	return throwExecOnStream(s.stream, req.Id, message)
}

func throwExecOnStream(stream *BidiStream, id uint32, message string) error {
	code := "exec_variant_unsupported"
	control := &agentv1.ExecClientControlMessage{
		Message: &agentv1.ExecClientControlMessage_Throw{
			Throw: &agentv1.ExecClientThrow{
				Id:        id,
				Error:     message,
				ErrorCode: &code,
			},
		},
	}
	client := &agentv1.AgentClientMessage{
		Message: &agentv1.AgentClientMessage_ExecClientControlMessage{ExecClientControlMessage: control},
	}
	payload, err := proto.Marshal(client)
	if err != nil {
		return err
	}
	if err = stream.WriteEnvelope(payload, false); err != nil {
		return err
	}
	return sendExecStreamClose(stream, id)
}

// unknownProtoFieldNumbers lists the field numbers present in a message's
// unknown-fields blob (used to identify trimmed-away exec variants in logs).
func unknownProtoFieldNumbers(raw []byte) []int {
	var fields []int
	for len(raw) > 0 {
		num, _, n := protowire.ConsumeField(raw)
		if n < 0 {
			break
		}
		fields = append(fields, int(num))
		raw = raw[n:]
	}
	return fields
}

func buildMcpToolDefinitions(tools []ToolDefinition) []*agentv1.McpToolDefinition {
	out := make([]*agentv1.McpToolDefinition, 0, len(tools))
	for _, tool := range tools {
		name := strings.TrimSpace(tool.Name)
		if name == "" {
			continue
		}
		// Advertise under the wire name so calls cannot bind to a Cursor
		// built-in tool of the same name (which this gateway cannot execute).
		wire := wireToolName(name)
		schema := tool.Parameters
		if schema == nil {
			schema = map[string]any{"type": "object", "properties": map[string]any{}}
		}
		value, err := toProtobufValue(schema)
		if err != nil {
			value, _ = toProtobufValue(map[string]any{"type": "object"})
		}
		out = append(out, &agentv1.McpToolDefinition{
			Name:               wire,
			Description:        tool.Description,
			InputSchema:        value,
			ProviderIdentifier: MCPProviderIdentifier,
			ToolName:           wire,
		})
	}
	return out
}

// RunChat performs a text/tool Cursor Agent segment and returns the collected result.
func RunChat(ctx context.Context, creds AccountCredentials, model string, messages []ChatMessage, tools []ToolDefinition, opts SessionOptions) (*ChatResult, error) {
	if results := extractToolResults(messages); len(results) > 0 {
		result, err := resumeWithToolResults(ctx, results)
		if err == nil {
			return result, nil
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		// The live run is gone (idle stream torn down, gateway restart, …).
		// Rebuild a fresh run whose history carries the tool results.
		log.Warnf("cursor: resuming tool results failed (%v); rebuilding run from history", err)
	}
	session, err := StartSession(ctx, creds, model, messages, tools, opts)
	if err != nil {
		return nil, err
	}
	result, err := session.CollectSegment(ctx)
	if err != nil {
		_ = session.Close()
		return nil, err
	}
	if result.FinishReason != "tool_calls" {
		_ = session.Close()
	}
	return result, nil
}

// resumeWithToolResults feeds trailing tool results into the live session that
// requested them and collects the follow-up segment.
func resumeWithToolResults(ctx context.Context, results []ToolResult) (*ChatResult, error) {
	session, err := ResumeSession(results)
	if err != nil {
		return nil, err
	}
	result, err := session.CollectSegment(ctx)
	if err != nil {
		_ = session.Close()
		return nil, err
	}
	if result.FinishReason != "tool_calls" {
		_ = session.Close()
	}
	return result, nil
}

// TrailingToolResults returns the tool results trailing the latest assistant
// tool_calls message, or nil when the request is not a tool-results turn.
func TrailingToolResults(messages []ChatMessage) []ToolResult {
	return extractToolResults(messages)
}

// ResumeSession resolves the live session that owns the trailing tool results
// and submits them so the run continues. Client-rewritten tool_call_ids are
// normalized back to the upstream pending ids before submission. The session
// is closed on submit failure so callers can rebuild a fresh run from history.
func ResumeSession(results []ToolResult) (*Session, error) {
	session, normalized, err := DefaultSessionManager().ResolveForToolResults(results)
	if err != nil {
		return nil, err
	}
	if err = session.SubmitToolResults(normalized); err != nil {
		_ = session.Close()
		return nil, err
	}
	return session, nil
}

func extractToolResults(messages []ChatMessage) []ToolResult {
	// Only trailing tool messages after the latest assistant tool_calls are
	// treated as a live round-trip resume; older history tool rows are ignored.
	start := -1
	for i := len(messages) - 1; i >= 0; i-- {
		switch messages[i].Role {
		case "tool":
			start = i
			continue
		case "assistant":
			if len(messages[i].ToolCalls) > 0 && start >= 0 {
				out := make([]ToolResult, 0, len(messages)-start)
				for _, msg := range messages[start:] {
					if msg.Role != "tool" || strings.TrimSpace(msg.ToolCallID) == "" {
						continue
					}
					out = append(out, ToolResult{
						ToolCallID: msg.ToolCallID,
						Name:       msg.Name,
						Content:    msg.Content,
					})
				}
				return out
			}
			return nil
		default:
			return nil
		}
	}
	return nil
}
