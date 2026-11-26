package main

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
)

const (
	modeDevice       = "device"
	modeProxy        = "proxy"
	fieldStoryID     = "story_id"
	fieldSessionKey  = "session_key"
	methodPing       = "ping"
	proxyRetryDelay  = 3 * time.Second
	storyRetention   = 30 * time.Minute
	proxyEventBuffer = 8
	callTraceLimit   = 20
)

type proxyUpdate struct {
	Trace traceEvent
	Story story
}

type proxyWatcher struct {
	ctx    context.Context
	events chan proxyUpdate
}

type proxyStory struct {
	game    story
	slot    string
	updated time.Time
}

type proxyCallSession struct {
	demo    *callDemo
	updated time.Time
	resume  string
}

type callTrace struct {
	Tool       string `json:"tool"`
	OK         bool   `json:"ok"`
	Status     string `json:"status,omitempty"`
	UseHistory *bool  `json:"use_history,omitempty"`
}

type privateProxy struct {
	endpoint     string
	online       atomic.Bool
	calls        atomic.Uint64
	ready        chan struct{}
	readyOnce    sync.Once
	mu           sync.Mutex
	games        map[string]*proxyStory
	watchers     map[string]proxyWatcher
	info         privateDeployment
	lobby        *lobby
	callDemo     *callDemo
	callMu       sync.Mutex
	callSessions map[string]*proxyCallSession
	callTurns    map[string]*proxyCallSession
	traceMu      sync.Mutex
	callTrace    []callTrace
	replays      atomic.Uint64
}

func (p *privateProxy) tools() []toolDescription {
	tools := storyTools()
	handle := schemaProperty{Type: schemaString, MinLength: 1, MaxLength: 64}
	for i := range tools {
		if tools[i].Name == toolStart {
			tools[i].InputSchema.Properties[fieldSessionKey] = handle
			tools[i].Description += " 返回story_id；后续续写和结束必须原样传入。若提示词给出了session_key，开始时传入它。"
			continue
		}
		tools[i].InputSchema.Properties[fieldStoryID] = handle
		tools[i].InputSchema.Required = append(tools[i].InputSchema.Required, fieldStoryID)
		tools[i].Description += " story_id必须使用开始工具返回的当前故事标识。"
	}
	if p.callDemo != nil {
		phoneTools := callDemoTools()
		if p.callDemo.chain != nil {
			filtered := make([]toolDescription, 0, len(phoneTools))
			for _, tool := range phoneTools {
				if tool.Name != toolCallReply {
					filtered = append(filtered, tool)
				}
			}
			return filtered
		}
		phoneTools = append(phoneTools, lobbyTools()...)
		return append(phoneTools, tools...)
	}
	tools = append(tools, lobbyTools()...)
	return tools
}

func (p *privateProxy) watch(ctx context.Context, slot string) <-chan proxyUpdate {
	events := make(chan proxyUpdate, proxyEventBuffer)
	p.mu.Lock()
	p.watchers[slot] = proxyWatcher{ctx: ctx, events: events}
	p.mu.Unlock()
	return events
}

func (p *privateProxy) forget(slot string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.watchers, slot)
	for handle, entry := range p.games {
		if entry.slot == slot {
			delete(p.games, handle)
		}
	}
}

func (p *privateProxy) expire(slot string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, entry := range p.games {
		if entry.slot == slot {
			entry.game.Status, entry.game.Reason = storyEnded, reasonSilent
			entry.updated = time.Now()
		}
	}
}

func (p *privateProxy) run(ctx context.Context) {
	for ctx.Err() == nil {
		if err := p.connect(ctx); err != nil && ctx.Err() == nil {
			// The endpoint contains a secret query parameter, so transport errors are not logged verbatim.
			slog.Warn("private MCP proxy disconnected; retrying")
		}
		p.online.Store(false)
		timer := time.NewTimer(proxyRetryDelay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
	}
}

func (p *privateProxy) connect(ctx context.Context) error {
	dialer := websocket.Dialer{HandshakeTimeout: requestTimeout}
	conn, response, err := dialer.DialContext(ctx, p.endpoint, nil)
	if err != nil {
		if response != nil {
			closeBody(response.Body)
		}
		return fmt.Errorf("connect private MCP proxy")
	}
	defer closeWS(conn)
	stop := context.AfterFunc(ctx, func() { closeWS(conn) })
	defer stop()
	conn.SetReadLimit(readLimit)
	for {
		var request rpcMessage
		if err := conn.ReadJSON(&request); err != nil {
			return fmt.Errorf("read private MCP request")
		}
		if len(request.ID) == 0 {
			continue
		}
		reply := rpcMessage{JSONRPC: jsonRPCVersion, ID: request.ID}
		switch request.Method {
		case methodInitialize:
			reply.Result = mcpInitialization()
		case methodList:
			reply.Result = struct {
				Tools []toolDescription `json:"tools"`
			}{Tools: p.tools()}
		case methodPing:
			reply.Result = struct{}{}
		case methodCall:
			reply.Result = p.call(ctx, request)
		default:
			reply.Error = &rpcError{Code: rpcMissing, Message: "method not found"}
		}
		if err := writeJSON(conn, reply); err != nil {
			return err
		}
		if request.Method == methodList {
			p.online.Store(true)
			p.readyOnce.Do(func() { close(p.ready) })
			slog.Info("private MCP proxy tools ready", "tools", len(p.tools()))
		}
	}
}

type textContent struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type mcpToolResult struct {
	Content           []textContent `json:"content"`
	StructuredContent any           `json:"structuredContent,omitempty"`
	IsError           bool          `json:"isError"`
}

func toolText(text string, failed bool) mcpToolResult {
	return mcpToolResult{Content: []textContent{{Type: commandText, Text: text}}, IsError: failed}
}

func mcpInitialization() any {
	result := struct {
		Version      string `json:"protocolVersion"`
		Capabilities struct {
			Tools struct{} `json:"tools"`
		} `json:"capabilities"`
		Server struct {
			Name    string `json:"name"`
			Version string `json:"version"`
		} `json:"serverInfo"`
	}{Version: protocolVersion}
	result.Server.Name, result.Server.Version = "story-relay", appVersion
	return result
}

func (p *privateProxy) callPhone(ctx context.Context, name string, raw json.RawMessage) mcpToolResult {
	var input callDemoInputs
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &input); err != nil {
			return toolText("故事工具参数格式不正确", true)
		}
	}
	if name == toolCallLobby {
		var choice struct {
			UseHistory *bool `json:"use_history"`
		}
		if err := json.Unmarshal(raw, &choice); err != nil || choice.UseHistory == nil {
			return toolText("请根据孩子的选择明确传入 use_history：以前的伙伴为 true，否则为 false", true)
		}
	}
	p.callMu.Lock()
	defer p.callMu.Unlock()

	if p.callSessions == nil {
		p.callSessions = make(map[string]*proxyCallSession)
		p.callTurns = make(map[string]*proxyCallSession)
	}
	now := time.Now()
	for key, session := range p.callSessions {
		if now.Sub(session.updated) > storyRetention {
			delete(p.callSessions, key)
			delete(p.callTurns, session.demo.turn)
			delete(p.callTurns, session.resume)
		}
	}

	var session *proxyCallSession
	switch name {
	case toolCallEntry:
		session = &proxyCallSession{demo: &callDemo{
			history: p.callDemo.history, prepare: p.callDemo.prepare, preparePlayback: p.callDemo.preparePlayback, pool: p.callDemo.pool,
			chain: p.callDemo.chain, prepareChain: p.callDemo.prepareChain,
		}}
	case toolCallLobby:
		if input.Lobby != "" {
			session = p.callSessions[input.Lobby]
			break
		}
		if p.callDemo.chain != nil {
			return toolText("请先调用 self.call.entry 询问两种玩法，再使用返回的 lobby_ticket。", true)
		}
		session = &proxyCallSession{demo: &callDemo{
			history: p.callDemo.history, prepare: p.callDemo.prepare, preparePlayback: p.callDemo.preparePlayback, pool: p.callDemo.pool,
			chain: p.callDemo.chain, prepareChain: p.callDemo.prepareChain,
		}}
	case toolCallOpen:
		session = p.callSessions[input.Lobby]
	default:
		session = p.callTurns[input.Turn]
		if name == toolCallTurn && input.Turn == "" && input.Lobby != "" {
			session = p.callSessions[input.Lobby]
			if session != nil {
				if session.demo.game.Status != storyPlaying {
					return toolText("请先用本次 lobby_ticket 调用 self.call.open 开始故事，再接话", true)
				}
				input.Turn = session.demo.turn
				var err error
				raw, err = json.Marshal(input)
				if err != nil {
					return toolText("故事工具参数格式不正确", true)
				}
			}
		}
	}
	if session == nil {
		return toolText("请先用本次大厅凭据调用 self.call.open 开始故事，再使用返回的 turn_key 接话", true)
	}
	oldTurn := session.demo.turn
	if name == toolCallTurn && input.Turn != oldTurn && input.Turn == session.resume {
		resume := input.Turn
		input.marker = input.Turn
		input.Turn = oldTurn
		// The marker is internal and must survive the JSON call boundary.
		result := session.demo.callWithMarker(ctx, name, input)
		return p.finishPhoneCall(session, oldTurn, resume, name, now, result)
	}
	result := session.demo.call(ctx, name, raw)
	return p.finishPhoneCall(session, oldTurn, oldTurn, name, now, result)
}

func (p *privateProxy) finishPhoneCall(session *proxyCallSession, oldTurn, resume, name string, now time.Time, result mcpToolResult) mcpToolResult {
	if result.IsError {
		return result
	}
	session.updated = now
	previousResume := session.resume
	if session.resume != "" {
		delete(p.callTurns, session.resume)
		session.resume = ""
	}
	if oldTurn != session.demo.turn {
		delete(p.callTurns, oldTurn)
	}
	if name == toolCallTurn && result.StructuredContent != nil {
		session.resume = resume
		p.callTurns[resume] = session
	}
	if name == toolCallTurn && result.StructuredContent == nil && len(result.Content) > 0 {
		var view callDemoView
		if json.Unmarshal([]byte(result.Content[0].Text), &view) == nil && view.Status == callChoosePeer {
			session.resume = oldTurn
			p.callTurns[oldTurn] = session
		}
	}
	if name == toolCallReply && result.StructuredContent != nil && previousResume != "" {
		session.resume = previousResume
		p.callTurns[previousResume] = session
	}
	if session.demo.turn != "" {
		p.callTurns[session.demo.turn] = session
	}
	if session.demo.lobby != "" {
		p.callSessions[session.demo.lobby] = session
	}
	if name == toolCallEnd {
		delete(p.callSessions, session.demo.lobby)
		delete(p.callTurns, session.resume)
	}
	if match := session.demo.match.Load(); match != nil {
		p.callDemo.match.Store(match)
	}
	return result
}

func (p *privateProxy) call(ctx context.Context, request rpcMessage) mcpToolResult {
	p.calls.Add(1)
	var callEnvelope struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}
	if err := json.Unmarshal(request.Params, &callEnvelope); err != nil {
		return toolText("工具请求格式不正确", true)
	}
	if p.callDemo != nil && strings.HasPrefix(callEnvelope.Name, "self.call.") {
		result := p.callPhone(ctx, callEnvelope.Name, callEnvelope.Arguments)
		var view callDemoView
		if !result.IsError && len(result.Content) > 0 {
			if err := json.Unmarshal([]byte(result.Content[0].Text), &view); err != nil {
				slog.Warn("phone tool result could not be inspected", "tool", callEnvelope.Name)
			}
		}
		trace := callTrace{Tool: callEnvelope.Name, OK: !result.IsError, Status: view.Status}
		if callEnvelope.Name == toolCallLobby {
			var choice struct {
				UseHistory *bool `json:"use_history"`
			}
			if err := json.Unmarshal(callEnvelope.Arguments, &choice); err == nil {
				trace.UseHistory = choice.UseHistory
			}
		}
		p.traceMu.Lock()
		p.callTrace = append(p.callTrace, trace)
		if len(p.callTrace) > callTraceLimit {
			p.callTrace = p.callTrace[len(p.callTrace)-callTraceLimit:]
		}
		p.traceMu.Unlock()
		playback := result.StructuredContent != nil
		if playback {
			p.replays.Add(1)
		}
		slog.Info("private call demo tool", "tool", callEnvelope.Name, "ok", !result.IsError, "original_requested", playback)
		return result
	}
	if strings.HasPrefix(callEnvelope.Name, "self.lobby.") {
		return p.lobby.callTool(callEnvelope.Name, callEnvelope.Arguments)
	}
	var call toolCallParams
	if err := json.Unmarshal(request.Params, &call); err != nil {
		return toolText("工具请求格式不正确", true)
	}
	p.mu.Lock()
	now := time.Now()
	for handle, entry := range p.games {
		if now.Sub(entry.updated) > storyRetention {
			delete(p.games, handle)
		}
	}
	handle := call.Arguments.StoryID
	entry, found := p.games[handle]
	if call.Name == toolStart {
		handle = rand.Text()
		entry = &proxyStory{game: story{Status: storyReady, Lines: []storyLine{}}, slot: call.Arguments.Slot}
	} else if !found {
		p.mu.Unlock()
		return toolText("找不到这个故事，请用开始工具创建新故事，并保存返回的story_id", true)
	}
	text, err := entry.game.call(call.Name, call.Arguments)
	if err != nil {
		p.mu.Unlock()
		return toolText(err.Error(), true)
	}
	if call.Name == toolStart && entry.slot != "" {
		for previous, game := range p.games {
			if game.slot == entry.slot {
				delete(p.games, previous)
			}
		}
	}
	entry.updated = now
	p.games[handle] = entry
	snapshot := entry.game
	snapshot.Lines = append([]storyLine{}, entry.game.Lines...)
	watcher, watching := p.watchers[entry.slot]
	p.mu.Unlock()
	trace := traceEvent{Method: methodCall, Tool: call.Name, ID: string(request.ID), OK: true, Detail: text}
	if watching {
		select {
		case watcher.events <- proxyUpdate{Trace: trace, Story: snapshot}:
		case <-watcher.ctx.Done():
		}
	}
	slog.Info("private MCP tool executed", "tool", call.Name)
	result := struct {
		StoryID string `json:"story_id"`
		AI      string `json:"ai_sentence,omitempty"`
		Ending  string `json:"ending,omitempty"`
		Status  string `json:"status"`
	}{StoryID: handle, Status: snapshot.Status, AI: text}
	if call.Name == toolEnd {
		result.Ending, result.AI = text, ""
	}
	raw, err := json.Marshal(result)
	if err != nil {
		return toolText("故事结果暂时无法返回，请稍后重试", true)
	}
	return toolText(string(raw), false)
}

func (p *privateProxy) linkedStories() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	count := 0
	for _, entry := range p.games {
		if _, connected := p.watchers[entry.slot]; connected {
			count++
		}
	}
	return count
}
