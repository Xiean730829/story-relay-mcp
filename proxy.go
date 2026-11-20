package main

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"log/slog"
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

type privateProxy struct {
	endpoint  string
	online    atomic.Bool
	calls     atomic.Uint64
	ready     chan struct{}
	readyOnce sync.Once
	mu        sync.Mutex
	games     map[string]*proxyStory
	watchers  map[string]proxyWatcher
	info      privateDeployment
}

func proxyTools() []toolDescription {
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
			}{Tools: proxyTools()}
		case methodPing:
			reply.Result = struct{}{}
		case methodCall:
			reply.Result = p.call(request)
		default:
			reply.Error = &rpcError{Code: rpcMissing, Message: "method not found"}
		}
		if err := writeJSON(conn, reply); err != nil {
			return err
		}
		if request.Method == methodList {
			p.online.Store(true)
			p.readyOnce.Do(func() { close(p.ready) })
			slog.Info("private MCP proxy tools ready", "tools", len(proxyTools()))
		}
	}
}

type textContent struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type mcpToolResult struct {
	Content []textContent `json:"content"`
	IsError bool          `json:"isError"`
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

func (p *privateProxy) call(request rpcMessage) mcpToolResult {
	p.calls.Add(1)
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
