package main

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

const (
	messageHello     = "hello"
	messageMCP       = "mcp"
	messageSTT       = "stt"
	messageTTS       = "tts"
	messageListen    = "listen"
	messageAbort     = "abort"
	stateStart       = "start"
	stateStop        = "stop"
	stateDetect      = "detect"
	listenMode       = "realtime"
	methodInitialize = "initialize"
	methodList       = "tools/list"
	methodCall       = "tools/call"
	commandConnect   = "connect"
	commandText      = "text"
	commandReplace   = "replace"
	commandEnd       = "end"
	commandPlayback  = "playback.done"
	commandVoice     = "voice.activity"
	commandInterrupt = "interrupt"
	eventPhase       = "phase"
	eventReady       = "ready"
	eventError       = "error"
	eventStory       = "story"
	eventTrace       = "trace"
	eventPlatform    = "platform"
	sourceBrowser    = "browser"
	sourcePlatform   = "platform"
	rpcInvalid       = -32602
	rpcMissing       = -32601
	rpcParse         = -32700
	jsonRPCVersion   = "2.0"
	eventCapacity    = 8
	endingSilence    = "我们先把故事收藏在心里，下次再一起接着编吧。"
)

type browserCommand struct {
	Type    string         `json:"type"`
	Text    string         `json:"text"`
	Options connectOptions `json:"options"`
}

type browserEvent struct {
	Type string `json:"type"`
	Text string `json:"text,omitempty"`
	Data any    `json:"data,omitempty"`
}

type socketEvent struct {
	source string
	kind   int
	data   []byte
	err    error
}

type rpcMessage struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type traceEvent struct {
	Method string `json:"method"`
	Tool   string `json:"tool,omitempty"`
	ID     string `json:"id"`
	OK     bool   `json:"ok"`
	Detail string `json:"detail,omitempty"`
}

type gameSession struct {
	browser    *websocket.Conn
	platform   *websocket.Conn
	options    connectOptions
	sessionID  string
	game       story
	ready      bool
	timer      *time.Timer
	waiting    bool
	voiceUntil time.Time
	proxy      *privateProxy
}

func serveSession(parent context.Context, browser *websocket.Conn, cfg configuration) error {
	ctx, cancel := context.WithCancel(parent)
	var workers sync.WaitGroup
	s := &gameSession{browser: browser, game: story{Status: storyReady, Lines: []storyLine{}}, proxy: cfg.proxy}
	var proxyEvents <-chan proxyUpdate
	defer func() {
		cancel()
		closeWS(browser)
		if s.platform != nil {
			closeWS(s.platform)
		}
		if s.timer != nil {
			s.timer.Stop()
		}
		if s.proxy != nil {
			s.proxy.forget(s.options.storySession)
		}
		workers.Wait()
	}()
	browser.SetReadLimit(readLimit)
	if err := browser.SetReadDeadline(time.Now().Add(requestTimeout)); err != nil {
		return fmt.Errorf("set browser setup deadline: %w", err)
	}
	var first browserCommand
	if err := browser.ReadJSON(&first); err != nil {
		return fmt.Errorf("read browser setup: %w", err)
	}
	if err := browser.SetReadDeadline(time.Time{}); err != nil {
		return fmt.Errorf("clear browser setup deadline: %w", err)
	}
	if first.Type != commandConnect {
		return s.notifyError("请先填写连接设置")
	}
	s.options = first.Options
	if err := validateOptions(&s.options, cfg); err != nil {
		return s.notifyError(err.Error())
	}
	if s.proxy != nil {
		if !s.proxy.online.Load() {
			return s.notifyError("私有MCP暂时离线，正在自动重连，请稍后重试")
		}
		s.options.AgentID, s.options.CreateAgent, s.options.Key = cfg.agent, false, cfg.key
		s.options.proxyMode, s.options.storySession = true, rand.Text()
		proxyEvents = s.proxy.watch(ctx, s.options.storySession)
	}
	events := make(chan socketEvent, eventCapacity)
	workers.Go(func() {
		readSocket(
			ctx,
			browser,
			sourceBrowser,
			events,
			cancel,
		)
	})
	if err := s.emit(eventPhase, "正在准备智能体和设备身份…", nil); err != nil {
		return err
	}
	api := platformAPI{cfg: cfg, key: s.options.Key, http: &http.Client{
		Timeout:       requestTimeout,
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse },
	}}
	endpoint, token, err := api.prepare(ctx, &s.options)
	// Preserve a newly created identity even if activation fails, to avoid duplicate agents on retry.
	identity := struct {
		Agent  string `json:"agent_id"`
		Device string `json:"device_id"`
	}{Agent: s.options.AgentID, Device: s.options.DeviceID}
	if emitErr := s.emit(eventPhase, "正在准备游戏连接…", identity); emitErr != nil {
		return emitErr
	}
	if err != nil {
		return s.notifyError(err.Error())
	}
	platform, welcome, err := dialPlatform(
		ctx,
		endpoint,
		token,
		s.options,
	)
	s.options.Key = ""
	if err != nil {
		return s.notifyError(err.Error())
	}
	s.platform, s.sessionID = platform, welcome.SessionID
	if err := s.emit(eventPlatform, "", welcome); err != nil {
		return err
	}
	workers.Go(func() {
		readSocket(
			ctx,
			platform,
			sourcePlatform,
			events,
			nil,
		)
	})
	s.timer = time.NewTimer(requestTimeout)
	if s.proxy != nil {
		if err := s.beginInteraction(); err != nil {
			return err
		}
	}
	for {
		select {
		case <-ctx.Done():
			return nil
		case update := <-proxyEvents:
			s.game = update.Story
			if err := s.emit(eventTrace, "", update.Trace); err != nil {
				return err
			}
			if err := s.emit(eventStory, "", s.game); err != nil {
				return err
			}
		case <-s.timer.C:
			if !s.ready {
				return s.notifyError("灵矽尚未查询MCP工具，请确认智能体启用了函数调用")
			}
			if !s.waiting {
				continue
			}
			if time.Now().Before(s.voiceUntil) {
				s.timer.Reset(time.Duration(s.options.Silence) * time.Second)
				continue
			}
			s.waiting = false
			s.game.Status, s.game.Reason = storyEnded, reasonSilent
			if s.proxy != nil {
				s.proxy.expire(s.options.storySession)
			}
			if err := s.emit(eventStory, "等待超时，故事已结束", s.game); err != nil {
				return err
			}
			if err := s.sendListen(stateDetect, "", endingSilence); err != nil {
				return err
			}
		case event := <-events:
			if event.err != nil {
				if event.source == sourcePlatform {
					return s.notifyError("灵矽连接已断开，请点击重新连接；已讲的故事仍显示在页面上")
				}
				return nil
			}
			if event.source == sourceBrowser {
				err = s.onBrowser(event)
			} else {
				err = s.onPlatform(event)
			}
			if err != nil {
				return err
			}
		}
	}
}

func readSocket(
	ctx context.Context,
	conn *websocket.Conn,
	source string,
	events chan<- socketEvent,
	onClose context.CancelFunc,
) {
	for {
		kind, data, err := conn.ReadMessage()
		event := socketEvent{source: source, kind: kind, data: data, err: err}
		select {
		case events <- event:
		case <-ctx.Done():
			return
		}
		if err != nil {
			if onClose != nil {
				onClose()
			}
			return
		}
	}
}

func (s *gameSession) emit(kind, text string, data any) error {
	return writeJSON(s.browser, browserEvent{Type: kind, Text: text, Data: data})
}

func (s *gameSession) notifyError(message string) error {
	return s.emit(eventError, message, nil)
}

func (s *gameSession) pauseWait() {
	s.waiting = false
	s.timer.Stop()
}

func (s *gameSession) armWait() {
	if s.game.Status == storyEnded {
		return
	}
	s.waiting = true
	s.timer.Reset(time.Duration(s.options.Silence) * time.Second)
}

func (s *gameSession) sendListen(state, text, response string) error {
	message := struct {
		Type     string `json:"type"`
		Session  string `json:"session_id"`
		State    string `json:"state"`
		Mode     string `json:"mode"`
		Text     string `json:"text,omitempty"`
		Response string `json:"text_response,omitempty"`
	}{Type: messageListen, Session: s.sessionID, State: state, Mode: listenMode, Text: text, Response: response}
	return writeJSON(s.platform, message)
}

func (s *gameSession) sendText(text string) error {
	s.pauseWait()
	return s.sendListen(stateDetect, text, "")
}

func (s *gameSession) onBrowser(event socketEvent) error {
	if event.kind == websocket.BinaryMessage {
		if !s.ready || s.game.Status == storyEnded {
			return nil
		}
		if len(event.data)%2 != 0 || len(event.data) > uplinkRate*2 {
			return s.notifyError("麦克风音频格式不正确，请重新开启麦克风")
		}
		if err := s.platform.SetWriteDeadline(time.Now().Add(writeTimeout)); err != nil {
			return fmt.Errorf("set audio write deadline: %w", err)
		}
		return s.platform.WriteMessage(websocket.BinaryMessage, event.data)
	}
	var cmd browserCommand
	if err := json.Unmarshal(event.data, &cmd); err != nil {
		return s.notifyError("页面消息格式不正确，请刷新重试")
	}
	if !s.ready {
		return nil
	}
	switch cmd.Type {
	case commandText:
		text := strings.TrimSpace(cmd.Text)
		if text == "" || len([]rune(text)) > maxSentence {
			return s.notifyError("请输入一句不超过400字的故事")
		}
		return s.sendText(text)
	case commandReplace:
		// Resume audio admission while the model replaces the previous story through MCP.
		s.game.Status = storyReady
		return s.sendText("我们换一个全新的故事，请你先开头，调用开始故事工具。")
	case commandEnd:
		return s.sendText("我想结束这个故事了，请调用结束工具，温柔地说一句再见。")
	case commandPlayback:
		s.armWait()
	case commandVoice:
		s.voiceUntil = time.Now().Add(time.Second)
		if s.waiting {
			s.timer.Reset(time.Duration(s.options.Silence) * time.Second)
		}
	case commandInterrupt:
		message := struct {
			Type    string `json:"type"`
			Session string `json:"session_id"`
			Reason  string `json:"reason"`
		}{Type: messageAbort, Session: s.sessionID, Reason: "wake_word_detected"}
		return writeJSON(s.platform, message)
	default:
		return s.notifyError("页面指令不受支持，请刷新重试")
	}
	return nil
}

func (s *gameSession) onPlatform(event socketEvent) error {
	if event.kind == websocket.BinaryMessage {
		if err := s.browser.SetWriteDeadline(time.Now().Add(writeTimeout)); err != nil {
			return fmt.Errorf("set speaker write deadline: %w", err)
		}
		return s.browser.WriteMessage(websocket.BinaryMessage, event.data)
	}
	var message platformMessage
	if err := json.Unmarshal(event.data, &message); err != nil {
		return fmt.Errorf("decode platform message: %w", err)
	}
	if message.Type == messageMCP {
		return s.handleMCP(message.Payload)
	}
	if message.Type == messageSTT {
		if message.ErrorCode != 0 {
			return s.notifyError("灵矽提示设备尚未就绪，请检查设备MAC与智能体绑定")
		}
		s.pauseWait()
	}
	if message.Type == messageTTS {
		switch message.State {
		case stateStart:
			s.pauseWait()
		case stateStop:
			// Browser arms the timer after queued audio has actually finished playing.
			if message.IsAborted {
				s.armWait()
			}
		}
	}
	return s.emit(eventPlatform, "", message)
}

func (s *gameSession) handleMCP(payload json.RawMessage) error {
	var req rpcMessage
	if err := json.Unmarshal(payload, &req); err != nil {
		return s.replyRPC(rpcMessage{Error: &rpcError{Code: rpcParse, Message: "invalid JSON-RPC"}})
	}
	if len(req.ID) == 0 {
		return nil
	}
	trace := traceEvent{Method: req.Method, ID: string(req.ID), OK: true}
	reply := rpcMessage{ID: req.ID}
	switch req.Method {
	case methodInitialize:
		reply.Result = mcpInitialization()
	case methodList:
		reply.Result = struct {
			Tools  []toolDescription `json:"tools"`
			Cursor string            `json:"nextCursor"`
		}{Tools: storyTools()}
	case methodCall:
		var call toolCallParams
		if err := json.Unmarshal(req.Params, &call); err != nil {
			reply.Error = &rpcError{Code: rpcInvalid, Message: "invalid tool arguments"}
			trace.OK = false
			break
		}
		trace.Tool = call.Name
		text, err := s.game.call(call.Name, call.Arguments)
		if err != nil {
			text, trace.OK = err.Error(), false
		}
		trace.Detail = text
		reply.Result = toolText(text, !trace.OK)
	default:
		trace.OK = false
		reply.Error = &rpcError{Code: rpcMissing, Message: "method not found"}
	}
	if err := s.replyRPC(reply); err != nil {
		return err
	}
	if err := s.emit(eventTrace, "", trace); err != nil {
		return err
	}
	if req.Method == methodCall && trace.OK {
		if err := s.emit(eventStory, "", s.game); err != nil {
			return err
		}
	}
	if req.Method == methodList && !s.ready {
		return s.beginInteraction()
	}
	return nil
}

func (s *gameSession) beginInteraction() error {
	s.ready = true
	s.timer.Stop()
	if err := s.emit(eventReady, "语音和MCP已连接", nil); err != nil {
		return err
	}
	if err := s.sendListen(stateStart, "", ""); err != nil {
		return err
	}
	if s.options.FirstSpeaker == roleAI {
		return s.sendText("我们来玩故事接龙，请你先编第一句，并调用开始故事工具记录。")
	}
	if strings.TrimSpace(s.options.Opening) != "" {
		return s.sendText(s.options.Opening)
	}
	return s.sendListen(stateDetect, "", "我们一人一句编故事，你来讲第一句吧。")
}

func (s *gameSession) replyRPC(reply rpcMessage) error {
	reply.JSONRPC = jsonRPCVersion
	message := struct {
		Type    string     `json:"type"`
		Session string     `json:"session_id"`
		Payload rpcMessage `json:"payload"`
	}{Type: messageMCP, Session: s.sessionID, Payload: reply}
	return writeJSON(s.platform, message)
}
