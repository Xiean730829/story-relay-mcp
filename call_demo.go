package main

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"log/slog"
	mathrand "math/rand/v2"
	"slices"
	"strings"
	"sync/atomic"
	"time"
)

const (
	toolCallLobby      = "self.call.lobby"
	toolCallOpen       = "self.call.open"
	toolCallTurn       = "self.call.turn"
	toolCallReply      = "self.call.reply"
	toolCallEnd        = "self.call.end"
	fieldCallLobby     = "lobby_ticket"
	fieldCallTurn      = "turn_key"
	fieldCallPicks     = "eligible_ids"
	fieldCallReuse     = "use_history"
	playCallAudio      = "linx_play"
	callReadWait       = 8 * time.Second
	callArchivePoll    = 5 * time.Second
	callArchiveRetry   = 30 * time.Second
	callArchiveTimeout = 15 * time.Second

	callLobbyOpened = "lobby"
	callGameActive  = "game_active"
	callBuddyTurn   = "buddy_turn"
	callChoosePeer  = "choose_peer"
	callPeerTurn    = "peer_turn"
	callEnded       = "ended"
	roleCallPeer    = "recorded_peer"
)

type callMatchPhase string

const (
	callNoCandidates  callMatchPhase = "no_candidates"
	callCandidates    callMatchPhase = "candidates_found"
	callInvalidPicks  callMatchPhase = "invalid_eligible_ids"
	callNoEligible    callMatchPhase = "no_eligible_reply"
	callHistoryAbsent callMatchPhase = "original_unavailable"
	callHistoryFailed callMatchPhase = "history_read_failed"
	callPrepareFailed callMatchPhase = "audio_prepare_failed"
	callSignFailed    callMatchPhase = "audio_sign_failed"
	callReadFailed    callMatchPhase = "signed_audio_read_failed"
	callPeerTimedOut  callMatchPhase = "audio_resolution_timed_out"
	callPeerRequested callMatchPhase = "peer_audio_requested"
)

type callMatchStatus struct {
	Phase      callMatchPhase `json:"phase"`
	Candidates int            `json:"candidate_count"`
	Eligible   int            `json:"eligible_count"`
}

type callDemoInputs struct {
	Lobby      string   `json:"lobby_ticket"`
	Title      string   `json:"title"`
	Turn       string   `json:"turn_key"`
	Child      string   `json:"child_sentence"`
	AI         string   `json:"ai_sentence"`
	Ending     string   `json:"ending"`
	Eligible   []string `json:"eligible_ids"`
	UseHistory bool     `json:"use_history"`
	marker     string
}

type callPeerOption struct {
	ID     string `json:"id"`
	Before string `json:"previous_line,omitempty"`
	Text   string `json:"reply_line"`
}

type callDemoView struct {
	Status     string           `json:"status"`
	Capture    string           `json:"capture_key,omitempty"`
	Lobby      string           `json:"lobby_ticket,omitempty"`
	Turn       string           `json:"turn_key,omitempty"`
	Title      string           `json:"title,omitempty"`
	Sentence   string           `json:"ai_sentence,omitempty"`
	Previous   string           `json:"previous_peer_line,omitempty"`
	NextAction string           `json:"next_action,omitempty"`
	Candidates []callPeerOption `json:"candidates,omitempty"`
}

// The private MCP connection loop owns this single-caller demo.
type callDemo struct {
	history    callHistory
	prepare    func(context.Context, callVoice) (string, error)
	pool       *callPool
	lobby      string
	turn       string
	run        string
	game       story
	pending    []callVoice
	used       map[string]bool
	useHistory bool
	match      atomic.Pointer[callMatchStatus]
}

func newCallDemo(
	h callHistory,
	root string,
	prepare func(context.Context, callVoice) (string, error),
) (*callDemo, error) {
	pool, err := openCallPool(root, h.agent)
	if err != nil {
		return nil, err
	}
	return &callDemo{history: h, prepare: prepare, pool: pool}, nil
}

func (d *callDemo) runArchive(ctx context.Context) {
	ticker := time.NewTicker(callArchivePoll)
	defer ticker.Stop()
	retryAfter := make(map[string]time.Time)
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-ticker.C:
			for _, voice := range d.pool.unresolved() {
				if now.Before(retryAfter[voice.Turn]) {
					continue
				}
				archiveCtx, cancel := context.WithTimeout(ctx, callArchiveTimeout)
				err := d.archiveVoice(archiveCtx, voice)
				cancel()
				if err != nil {
					retryAfter[voice.Turn] = now.Add(callArchiveRetry)
					slog.InfoContext(ctx, "original audio archive pending", "reason", err.Error())
				} else {
					delete(retryAfter, voice.Turn)
					slog.InfoContext(ctx, "original audio archived")
				}
				break
			}
		}
	}
}

func (d *callDemo) archiveVoice(ctx context.Context, voice callVoice) error {
	source, err := d.history.find(ctx, voice)
	if err != nil {
		return fmt.Errorf("read history")
	}
	if source == "" {
		return fmt.Errorf("audio not reported yet")
	}
	voice.URL = source
	prepared, err := d.prepare(ctx, voice)
	if err != nil {
		return fmt.Errorf("prepare audio")
	}
	voice.URL = prepared
	if err := d.pool.save(voice); err != nil {
		return fmt.Errorf("save audio index: %w", err)
	}
	return nil
}

func callDemoTools() []toolDescription {
	id := schemaProperty{Type: schemaString, MaxLength: 64}
	sentence := schemaProperty{Type: schemaString, MinLength: 1, MaxLength: maxSentence}
	title := schemaProperty{Type: schemaString, MinLength: 1, MaxLength: maxTitle}
	tools := []toolDescription{
		{Name: toolCallLobby, Description: "进入游戏大厅。仅孩子要求与以前伙伴原声玩时 use_history 设为 true。"},
		{Name: toolCallOpen, Description: "开始故事接龙，返回开场和首回合凭据。",
			InputSchema: inputSchema{
				Properties: map[string]schemaProperty{fieldCallLobby: id, fieldTitle: title, fieldAI: sentence},
				Required:   []string{fieldCallLobby, fieldTitle, fieldAI},
			}},
		{Name: toolCallTurn, Description: "先开始故事，再在孩子接话或询问故事情节时调用。使用当前 turn_key 和 lobby_ticket，child_sentence 原样传孩子的话，ai_sentence 写备用接话。",
			InputSchema: inputSchema{
				Properties: map[string]schemaProperty{
					fieldCallTurn: id, fieldCallLobby: id, fieldChild: sentence, fieldAI: sentence,
				},
				Required: []string{fieldCallTurn, fieldChild, fieldAI},
			}},
		{Name: toolCallReply, Description: "自动完成当前队友回合。传入所有能自然接话的候选编号，池会随机选一个。空数组由 Buddy 接话。",
			InputSchema: inputSchema{
				Properties: map[string]schemaProperty{
					fieldCallTurn:  id,
					fieldCallPicks: {Type: schemaArray, Items: &id, MaxItems: maxCallCandidates},
				},
				Required: []string{fieldCallTurn, fieldCallPicks},
			}},
		{Name: toolCallEnd, Description: "结束本局，保留已提交的原声记录。",
			InputSchema: inputSchema{
				Properties: map[string]schemaProperty{fieldCallTurn: id, fieldEnding: sentence},
				Required:   []string{fieldCallTurn, fieldEnding},
			}},
	}
	tools[0].InputSchema.Properties = map[string]schemaProperty{
		fieldCallReuse: {Type: schemaBoolean},
		fieldCallLobby: id,
	}
	tools[0].InputSchema.Required = []string{}
	for i := range tools {
		tools[i].InputSchema.Type = schemaObject
	}
	return tools
}

func (d *callDemo) call(ctx context.Context, name string, raw json.RawMessage) mcpToolResult {
	var input callDemoInputs
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &input); err != nil {
			return toolText("故事工具参数格式不正确", true)
		}
	}
	return d.callWithMarker(ctx, name, input)
}

func (d *callDemo) callWithMarker(ctx context.Context, name string, input callDemoInputs) mcpToolResult {
	switch name {
	case toolCallLobby:
		if input.Lobby != "" && input.Lobby == d.lobby {
			d.useHistory = input.UseHistory
			view := callDemoView{Lobby: d.lobby, Status: callLobbyOpened}
			if d.turn != "" && d.game.Status == storyPlaying {
				view.Status, view.Turn, view.Title = callGameActive, d.turn, d.game.Title
			}
			return callDemoResult(view, "")
		}
		d.lobby, d.turn = rand.Text(), ""
		d.useHistory = input.UseHistory
		d.pending = nil
		return callDemoResult(callDemoView{Lobby: d.lobby, Status: callLobbyOpened}, "")
	case toolCallOpen:
		if input.Lobby == "" || input.Lobby != d.lobby {
			return toolText("请先进入游戏大厅", true)
		}
		if d.useHistory {
			if opening, found := d.pool.opening(input.Title); found {
				input.AI = opening.Before
			}
		}
		if _, err := d.game.call(toolStart, toolArguments{Title: input.Title, AI: input.AI}); err != nil {
			return toolText(err.Error(), true)
		}
		d.turn, d.run = rand.Text(), rand.Text()
		d.used, d.pending = make(map[string]bool), nil
		view := callDemoView{
			Turn: d.turn, Title: d.game.Title, Sentence: strings.TrimSpace(input.AI), Status: callBuddyTurn,
			NextAction: "等待孩子接话。孩子接话或提问时先调用 self.call.turn。",
		}
		return callDemoResult(view, "")
	}
	if input.Turn == "" || input.Turn != d.turn || d.game.Status != storyPlaying {
		return toolText("当前回合已失效，请根据最新工具状态继续", true)
	}
	switch name {
	case toolCallTurn:
		return d.appendTurn(ctx, input)
	case toolCallReply:
		return d.reply(ctx, input.Eligible)
	case toolCallEnd:
		ending, err := d.game.call(toolEnd, toolArguments{End: input.Ending, Why: reasonChild})
		if err != nil {
			return toolText(err.Error(), true)
		}
		d.turn = ""
		d.pending = nil
		return callDemoResult(callDemoView{Title: d.game.Title, Sentence: ending, Status: callEnded}, "")
	default:
		return toolText("没有这个故事工具", true)
	}
}

func (d *callDemo) appendTurn(ctx context.Context, input callDemoInputs) mcpToolResult {
	next := d.game
	next.Lines = append([]storyLine{}, d.game.Lines...)
	buddyLine, err := next.call(toolContinue, toolArguments{Child: input.Child, AI: input.AI})
	if err != nil {
		return toolText(err.Error(), true)
	}
	previous := d.game.Lines[len(d.game.Lines)-1]
	voice := callVoice{
		Turn: d.turn, Story: d.run, Title: d.game.Title,
		Text: strings.TrimSpace(input.Child), Before: previous.Text, Marker: input.marker,
	}
	view := callDemoView{Turn: rand.Text(), Title: d.game.Title, Sentence: buddyLine, Status: callBuddyTurn}
	if previous.Role == roleCallPeer {
		view.Previous = previous.Text
	}
	if err := d.pool.save(voice); err == nil {
		view.Capture = voice.Turn
	} else {
		slog.WarnContext(ctx, "call voice pool capture index unavailable")
	}

	d.game, d.turn = next, view.Turn
	if !d.useHistory {
		d.pending = nil
		return callDemoResult(view, "")
	}
	pending, err := d.pool.candidates(voice, d.used)
	d.pending = pending
	if err != nil {
		slog.WarnContext(ctx, "legacy call voice index unavailable")
	}
	if len(d.pending) == 0 {
		d.match.Store(&callMatchStatus{Phase: callNoCandidates})
		return callDemoResult(view, "")
	}
	if ids := automaticCallReplies(d.pending, input.AI); len(ids) > 0 {
		return d.reply(ctx, ids)
	}
	d.match.Store(&callMatchStatus{Phase: callCandidates, Candidates: len(d.pending)})
	view.Status, view.Sentence = callChoosePeer, ""
	view.NextAction = "请立即调用 self.call.reply，填入能自然接话的候选编号；没有合适候选时填空数组。"
	view.Candidates = make([]callPeerOption, 0, len(d.pending))
	for _, candidate := range d.pending {
		view.Candidates = append(view.Candidates, callPeerOption{
			ID: candidate.Turn, Before: candidate.Before, Text: candidate.Text,
		})
	}
	return callDemoResult(view, "")
}

func (d *callDemo) reply(ctx context.Context, ids []string) mcpToolResult {
	if len(d.pending) == 0 {
		return toolText("当前没有待选择的队友回合", true)
	}
	if ids == nil || len(ids) > maxCallCandidates {
		d.match.Store(&callMatchStatus{Phase: callInvalidPicks, Candidates: len(d.pending)})
		return toolText("请传入可接话的候选编号列表，或传空数组", true)
	}
	eligible := make([]callVoice, 0, len(d.pending))
	for _, voice := range d.pending {
		if slices.Contains(ids, voice.Turn) {
			eligible = append(eligible, voice)
		}
	}
	match := callMatchStatus{Phase: callNoEligible, Candidates: len(d.pending), Eligible: len(eligible)}
	defer d.match.Store(&match)
	d.pending = nil
	line := &d.game.Lines[len(d.game.Lines)-1]
	view := callDemoView{Turn: d.turn, Title: d.game.Title, Sentence: line.Text, Status: callBuddyTurn}
	if len(eligible) == 0 {
		return callDemoResult(view, "")
	}

	ctx, cancel := context.WithTimeout(ctx, callReadWait)
	defer cancel()
	mathrand.Shuffle(len(eligible), func(i, j int) {
		eligible[i], eligible[j] = eligible[j], eligible[i]
	})
	for _, selected := range eligible {
		if ctx.Err() != nil {
			match.Phase = callPeerTimedOut
			break
		}
		signed, phase := d.signPeer(ctx, selected)
		match.Phase = phase
		if signed == "" {
			continue
		}
		d.used[selected.Turn] = true
		*line = storyLine{Role: roleCallPeer, Text: selected.Text}
		view.Status, view.Sentence = callPeerTurn, ""
		return callDemoResult(view, signed)
	}
	return callDemoResult(view, "")
}

func (d *callDemo) signPeer(ctx context.Context, selected callVoice) (string, callMatchPhase) {
	if selected.URL == "" {
		source, err := d.history.find(ctx, selected)
		if err != nil {
			slog.WarnContext(ctx, "call teammate audio unavailable while reading history")
			return "", callHistoryFailed
		}
		if source == "" {
			slog.InfoContext(ctx, "call teammate audio not available", "stage", "history")
			return "", callHistoryAbsent
		}
		selected.URL = source
		media, err := d.prepare(ctx, selected)
		if err != nil {
			slog.WarnContext(ctx, "call teammate audio not available", "stage", "prepare")
			return "", callPrepareFailed
		}
		selected.URL = media
		if err := d.pool.save(selected); err != nil {
			slog.WarnContext(ctx, "call teammate audio cache update failed")
		}
	}

	signed, err := d.history.signed(ctx, selected.URL)
	if err != nil {
		slog.WarnContext(ctx, "call teammate audio not available", "stage", "sign")
		return "", callSignFailed
	}
	if err := d.history.checkMP3(ctx, signed); err != nil {
		slog.WarnContext(ctx, "call teammate audio not available", "stage", "read", "error", err)
		return "", callReadFailed
	}
	return signed, callPeerRequested
}

func callDemoResult(view callDemoView, audio string) mcpToolResult {
	data, err := json.Marshal(view)
	if err != nil {
		return toolText("故事状态暂时无法返回", true)
	}
	result := toolText(string(data), false)
	if audio != "" {
		result.StructuredContent = map[string]any{
			"code":    0,
			"message": "",
			"funcs": []any{map[string]any{
				"name":      playCallAudio,
				"arguments": map[string]string{"audio_url": audio, "title": "队友接龙"},
			}},
		}
	}
	return result
}
