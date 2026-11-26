package main

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"slices"
	"strings"
	"sync/atomic"
	"time"
	"unicode/utf8"
)

const (
	toolCallEntry      = "self.call.entry"
	toolCallLobby      = "self.call.lobby"
	toolCallOpen       = "self.call.open"
	toolCallPlay       = "self.call.play"
	toolCallTurn       = "self.call.turn"
	toolCallReply      = "self.call.reply"
	toolCallEnd        = "self.call.end"
	fieldCallLobby     = "lobby_ticket"
	fieldCallTurn      = "turn_key"
	fieldCallPicks     = "eligible_ids"
	fieldCallReason    = "selection_reason"
	fieldCallReuse     = "use_history"
	fieldCallNewStory  = "new_story"
	playCallAudio      = "linx_play"
	callReadWait       = 20 * time.Second
	callArchivePoll    = 5 * time.Second
	callArchiveRetry   = 30 * time.Second
	callArchiveTimeout = 15 * time.Second

	callLobbyOpened = "lobby"
	callChooseMode  = "choose_mode"
	callGameActive  = "game_active"
	callBuddyTurn   = "buddy_turn"
	callStorySaved  = "story_layer_pending"
	callChoosePeer  = "choose_peer"
	callPeerTurn    = "peer_turn"
	callEnded       = "ended"
	roleCallPeer    = "recorded_peer"
	callModeBuddy   = "buddy"
	callModeHistory = "history"
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
	Current    string         `json:"current_line,omitempty"`
	Selected   string         `json:"selected_line,omitempty"`
	Reason     string         `json:"selection_reason,omitempty"`
}

type callDemoInputs struct {
	Lobby      string   `json:"lobby_ticket"`
	Title      string   `json:"title"`
	Turn       string   `json:"turn_key"`
	Child      string   `json:"child_sentence"`
	AI         string   `json:"ai_sentence"`
	Ending     string   `json:"ending"`
	Eligible   []string `json:"eligible_ids"`
	Reason     string   `json:"selection_reason"`
	UseHistory bool     `json:"use_history"`
	NewStory   bool     `json:"new_story"`
	marker     string
}

type callPeerOption struct {
	ID     string `json:"id"`
	Before string `json:"previous_line,omitempty"`
	Text   string `json:"reply_line"`
}

type callDemoView struct {
	Status     string           `json:"status"`
	Mode       string           `json:"mode,omitempty"`
	Capture    string           `json:"capture_key,omitempty"`
	Lobby      string           `json:"lobby_ticket,omitempty"`
	Turn       string           `json:"turn_key,omitempty"`
	Title      string           `json:"title,omitempty"`
	Sentence   string           `json:"ai_sentence,omitempty"`
	PlotBeats  []string         `json:"plot_beats,omitempty"`
	Previous   string           `json:"previous_peer_line,omitempty"`
	Scene      []storyLine      `json:"scene,omitempty"`
	NextAction string           `json:"next_action,omitempty"`
	Candidates []callPeerOption `json:"candidates,omitempty"`
}

// The private MCP connection loop owns this single-caller demo.
type callDemo struct {
	history         callHistory
	prepare         func(context.Context, callVoice) (string, error)
	preparePlayback func(context.Context, callVoice) (string, error)
	prepareChain    func(context.Context, []callVoice) (string, error)
	chain           *callChain
	chainStory      *callChainDetail
	chainTheme      string
	chainCaptured   bool
	chainPlayed     bool
	pool            *callPool
	lobby           string
	turn            string
	run             string
	game            story
	pending         []callVoice
	used            map[string]bool
	useHistory      bool
	match           atomic.Pointer[callMatchStatus]
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
	if voice.URL == "" {
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
	}
	if voice.ChainPending && d.chain != nil {
		if err := d.chain.commit(ctx, voice); err != nil {
			return fmt.Errorf("save story segment: %w", err)
		}
		voice.ChainPending = false
		if err := d.pool.save(voice); err != nil {
			return fmt.Errorf("save story sync status: %w", err)
		}
	}
	return nil
}

func callDemoTools() []toolDescription {
	id := schemaProperty{Type: schemaString, MaxLength: 64}
	sentence := schemaProperty{Type: schemaString, MinLength: 1, MaxLength: maxSentence}
	titles := callStoryTitles()
	title := schemaProperty{Type: schemaString, MaxLength: maxTitle, Enum: titles}
	tools := []toolDescription{
		{Name: toolCallEntry, Description: "孩子选择故事接龙时首先调用。返回固定的两个玩法选项，必须先向孩子询问并等待回答。", InputSchema: inputSchema{Properties: map[string]schemaProperty{}}},
		{Name: toolCallLobby, Description: "在孩子明确选择玩法之后进入：接以前的故事用 use_history=true，和 Buddy 玩用 false。"},
		{Name: toolCallOpen, Description: "开始故事接龙。省略标题时选择森林冒险。预设主题的开场由工具提供。",
			InputSchema: inputSchema{
				Properties: map[string]schemaProperty{fieldCallLobby: id, fieldTitle: title, fieldAI: sentence, fieldCallNewStory: {Type: schemaBoolean}},
				Required:   []string{fieldCallLobby},
			}},
		{Name: toolCallPlay, Description: "接别人的故事时，简短说完固定背景后立即调用，按顺序播放已有的全部真人录音。使用当前 turn_key。",
			InputSchema: inputSchema{Properties: map[string]schemaProperty{fieldCallTurn: id}, Required: []string{fieldCallTurn}}},
		{Name: toolCallTurn, Description: "记录孩子的故事接话。接以前小伙伴故事时只传 child_sentence，不写 ai_sentence，不续写；和 AI 玩时还需传 ai_sentence 作为 AI 的下一句。",
			InputSchema: inputSchema{
				Properties: map[string]schemaProperty{
					fieldCallTurn: id, fieldCallLobby: id, fieldChild: sentence, fieldAI: sentence,
				},
				Required: []string{fieldCallTurn, fieldChild},
			}},
		{Name: toolCallReply, Description: "判断候选是否真正衔接当前故事。最多选择一条，并说明它如何接上当前人物和事件；没有合适录音时传空数组。理由只用于后台诊断。",
			InputSchema: inputSchema{
				Properties: map[string]schemaProperty{
					fieldCallTurn:   id,
					fieldCallPicks:  {Type: schemaArray, Items: &id, MaxItems: 1},
					fieldCallReason: {Type: schemaString, MaxLength: maxSentence},
				},
				Required: []string{fieldCallTurn, fieldCallPicks, fieldCallReason},
			}},
		{Name: toolCallEnd, Description: "结束本局，保留已提交的原声记录。",
			InputSchema: inputSchema{
				Properties: map[string]schemaProperty{fieldCallTurn: id, fieldEnding: sentence},
				Required:   []string{fieldCallTurn, fieldEnding},
			}},
	}
	tools[1].InputSchema.Properties = map[string]schemaProperty{
		fieldCallReuse: {Type: schemaBoolean},
		fieldCallLobby: id,
	}
	tools[1].InputSchema.Required = []string{fieldCallReuse}
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
	case toolCallEntry:
		d.lobby, d.turn = rand.Text(), ""
		d.chainStory, d.chainCaptured, d.chainPlayed = nil, false, false
		d.chainTheme = ""
		return callDemoResult(callDemoView{
			Lobby: d.lobby, Status: callChooseMode,
			Sentence:   "你想和我一起玩，还是和以前的小伙伴玩？",
			NextAction: "原样说出 ai_sentence，等待孩子选择；选择后用这个 lobby_ticket 调用 self.call.lobby。不要自行选择。",
		}, "")
	case toolCallLobby:
		if d.chain != nil && (input.Lobby == "" || input.Lobby != d.lobby) {
			return toolText("请先调用 self.call.entry 询问两种玩法，再使用返回的 lobby_ticket。", true)
		}
		mode := callModeBuddy
		if input.UseHistory {
			mode = callModeHistory
		}
		if input.Lobby != "" && input.Lobby == d.lobby {
			if d.useHistory != input.UseHistory && d.chain != nil {
				if d.useHistory && d.chainStory != nil && !d.chainCaptured {
					if err := d.chain.release(ctx, d.chainStory.Story.ID); err != nil {
						return toolText("暂时无法切换故事，请稍后再试。", true)
					}
				}
				d.turn, d.game.Status = "", storyEnded
				d.chainStory, d.chainCaptured, d.chainPlayed = nil, false, false
				d.chainTheme = ""
			}
			d.useHistory = input.UseHistory
			view := callDemoView{Lobby: d.lobby, Status: callLobbyOpened, Mode: mode}
			if d.turn != "" && d.game.Status == storyPlaying {
				view.Status, view.Turn, view.Title = callGameActive, d.turn, d.game.Title
			}
			return callDemoResult(view, "")
		}
		d.lobby, d.turn = rand.Text(), ""
		d.chainStory, d.chainCaptured, d.chainPlayed = nil, false, false
		d.chainTheme = ""
		d.useHistory = input.UseHistory
		d.pending = nil
		return callDemoResult(callDemoView{Lobby: d.lobby, Status: callLobbyOpened, Mode: mode}, "")
	case toolCallOpen:
		if input.Lobby == "" || input.Lobby != d.lobby {
			return toolText("请先进入游戏大厅", true)
		}
		var plotBeats []string
		active := d.turn != "" && d.game.Status == storyPlaying
		if strings.TrimSpace(input.Title) == "" {
			input.Title = defaultCallStoryTitle
			if active {
				input.Title = d.game.Title
			}
		}
		if scenario := callStoryByTitle[normalizedCallText(input.Title)]; scenario != nil {
			input.Title, input.AI, plotBeats = scenario.Title, scenario.Opening, scenario.Beats
			if active && d.game.Title == scenario.Title && !input.NewStory {
				return callDemoResult(callDemoView{
					Status: callGameActive, Turn: d.turn, Title: d.game.Title,
					NextAction: "故事已经开始，等待孩子接话。",
				}, "")
			}
		} else if strings.TrimSpace(input.AI) == "" {
			return toolText("请从这些故事里选一个："+strings.Join(callStoryTitles(), "、"), true)
		} else if d.useHistory {
			if opening, found := d.pool.opening(input.Title); found {
				input.AI = opening.Before
			}
		}
		if active && d.chain != nil && d.chainStory != nil && !d.chainCaptured {
			if err := d.chain.release(ctx, d.chainStory.Story.ID); err != nil {
				return toolText("暂时无法切换故事，请稍后再试。", true)
			}
		}
		d.chainStory, d.chainCaptured, d.chainPlayed = nil, false, false
		d.chainTheme = ""
		if _, err := d.game.call(toolStart, toolArguments{Title: input.Title, AI: input.AI}); err != nil {
			return toolText(err.Error(), true)
		}
		d.turn, d.run = rand.Text(), rand.Text()
		d.used, d.pending = make(map[string]bool), nil
		if d.useHistory && d.chain != nil {
			theme, err := d.chain.ensureTheme(ctx, input.Title, input.AI)
			if err != nil {
				d.turn, d.game.Status = "", storyEnded
				return toolText("故事大厅暂时连接不上，请稍后再试。", true)
			}
			d.chainTheme = theme
			var matched *callChainDetail
			if !input.NewStory {
				matched, err = d.chain.match(ctx, theme)
			}
			if err != nil {
				d.turn, d.game.Status = "", storyEnded
				return toolText("故事大厅暂时连接不上，请稍后再试。", true)
			}
			if matched != nil {
				if err := d.chain.claim(ctx, matched.Story.ID, matched.Story.Version); err != nil {
					d.turn, d.game.Status = "", storyEnded
					return toolText("这个故事刚被占用，请重新选择故事。", true)
				}
				d.chainStory = matched
				for _, segment := range matched.Segments {
					d.game.Lines = append(d.game.Lines, storyLine{Role: roleCallPeer, Text: segment.Text})
				}
			}
		}
		view := callDemoView{
			Turn: d.turn, Title: d.game.Title, Sentence: strings.TrimSpace(input.AI), Status: callBuddyTurn,
			PlotBeats:  plotBeats,
			NextAction: "等待孩子接话。孩子接话或提问时先调用 self.call.turn。",
		}
		if d.useHistory && d.chain != nil {
			if d.chainStory != nil {
				view.NextAction = "先只朗读 ai_sentence 作为故事背景，紧接着调用 self.call.play 播放完整故事，不要重复或编造故事内容。"
			} else {
				view.NextAction = "这是一个新故事，只朗读 ai_sentence，然后请孩子接一句。这句原声会留给以后的人。"
			}
		}
		return callDemoResult(view, "")
	}
	if input.Turn == "" || input.Turn != d.turn || d.game.Status != storyPlaying {
		return toolText("当前回合已失效，请根据最新工具状态继续", true)
	}
	switch name {
	case toolCallPlay:
		if d.chainStory == nil || d.chain == nil {
			return toolText("这条故事还没有前人的录音，请直接接一句。", false)
		}
		voices := make([]callVoice, 0, len(d.chainStory.Segments))
		for _, segment := range d.chainStory.Segments {
			voice, ok := d.pool.voice(segment.ClipID)
			if !ok || voice.URL == "" {
				return toolText("故事录音还没有准备好，请稍后再试。", true)
			}
			voices = append(voices, voice)
		}
		audio, err := d.prepareChain(ctx, voices)
		if err != nil {
			slog.WarnContext(ctx, "prepare story playback failed", "error", err)
			return toolText("故事录音暂时无法播放，请稍后再试。", true)
		}
		d.chainPlayed = true
		return callDemoResult(callDemoView{Status: callPeerTurn, Turn: d.turn, Title: d.game.Title,
			NextAction: "安静等待整段故事播放，播放后请孩子接一句，不要复述录音。"}, audio)
	case toolCallTurn:
		return d.appendTurn(ctx, input)
	case toolCallReply:
		return d.reply(ctx, input.Eligible, input.Reason)
	case toolCallEnd:
		if d.chain != nil && d.chainStory != nil && !d.chainCaptured {
			if err := d.chain.release(ctx, d.chainStory.Story.ID); err != nil {
				slog.WarnContext(ctx, "release story reservation failed", "error", err)
			}
		}
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
	if isCallGameControl(input.Child) {
		return toolText("这是游戏操作。请先按孩子的意愿处理切换、匹配或退出，然后再继续故事。", true)
	}
	if d.useHistory && d.chain != nil {
		if d.chainStory != nil && !d.chainPlayed {
			return toolText("请先播放前面的故事，再请孩子接话。", true)
		}
		if d.chainCaptured {
			return toolText("这一层已经留下了，可以结束或者重新开始另一个故事。", true)
		}
	}
	next := d.game
	next.Lines = append([]storyLine{}, d.game.Lines...)
	var buddyLine string
	if d.useHistory && d.chain != nil {
		child := strings.TrimSpace(input.Child)
		if child == "" || utf8.RuneCountInString(child) > maxSentence {
			return toolText("请用一句话接下故事。", true)
		}
		next.Lines = append(next.Lines, storyLine{Role: roleChild, Text: child})
	} else {
		var err error
		buddyLine, err = next.call(toolContinue, toolArguments{Child: input.Child, AI: input.AI})
		if err != nil {
			return toolText(err.Error(), true)
		}
	}
	previousIndex := len(d.game.Lines) - 1
	previous := d.game.Lines[previousIndex]
	earlier := d.game.Lines[max(0, previousIndex-callRecentSceneLines):previousIndex]
	voice := callVoice{
		Turn: d.turn, Story: d.run, Title: d.game.Title,
		Text: strings.TrimSpace(input.Child), Before: previous.Text, Marker: input.marker,
	}
	if d.useHistory && d.chain != nil {
		voice.ChainPending = true
		voice.ChainTheme = d.chainTheme
		if d.chainStory != nil {
			voice.ChainID, voice.ChainVersion = d.chainStory.Story.ID, d.chainStory.Story.Version
		}
	}
	view := callDemoView{Turn: rand.Text(), Title: d.game.Title, Sentence: buddyLine, Status: callBuddyTurn}
	if previous.Role == roleCallPeer {
		view.Previous = previous.Text
	}
	if err := d.pool.save(voice); err == nil {
		view.Capture = voice.Turn
	} else {
		slog.WarnContext(ctx, "call voice pool capture index unavailable")
		if voice.ChainPending {
			return toolText("这句故事暂时没能保存，请再说一次。", true)
		}
	}

	d.game, d.turn = next, view.Turn
	if voice.ChainPending {
		d.chainCaptured = true
		view.Status = callStorySaved
		view.Sentence = ""
		view.NextAction = "这一层已收到，录音正在后台保存。不要接故事、不要补充剧情；只可简短说一句‘我们先把故事留在这里’，然后等待孩子决定是否结束或换一个故事。"
		return callDemoResult(view, "")
	}
	if !d.useHistory {
		d.pending = nil
		return callDemoResult(view, "")
	}
	pending := d.pool.candidates(voice, d.used, earlier)
	d.pending = make([]callVoice, 0, min(len(pending), maxCallCandidates))
	seen := make(map[string]bool, maxCallCandidates)
	for _, candidate := range pending {
		transcript := normalizedCallText(candidate.Text)
		if seen[transcript] {
			continue
		}
		seen[transcript] = true
		d.pending = append(d.pending, candidate)
		if len(d.pending) == maxCallCandidates {
			break
		}
	}
	if len(d.pending) == 0 {
		d.match.Store(&callMatchStatus{Phase: callNoCandidates})
		return callDemoResult(view, "")
	}
	d.match.Store(&callMatchStatus{Phase: callCandidates, Candidates: len(d.pending)})
	view.Status, view.Sentence = callChoosePeer, ""
	view.Scene = append([]storyLine{}, d.game.Lines[max(0, len(d.game.Lines)-2-callRecentSceneLines):len(d.game.Lines)-1]...)
	view.NextAction = "根据 scene 的最近故事和每条录音的上一句及原话判断。核对人物、事物和事件关系，只有能自然承接当前情节才选。调用 self.call.reply 最多选一条，在 selection_reason 说明它如何接上当前人物和事件；不确定、矛盾或不相干传空数组。不要朗读候选和理由。"
	view.Candidates = make([]callPeerOption, 0, len(d.pending))
	for _, candidate := range d.pending {
		view.Candidates = append(view.Candidates, callPeerOption{
			ID: candidate.Turn, Before: candidate.Before, Text: candidate.Text,
		})
	}
	return callDemoResult(view, "")
}

func (d *callDemo) reply(ctx context.Context, ids []string, reason string) mcpToolResult {
	if len(d.pending) == 0 {
		return toolText("当前没有待选择的队友回合", true)
	}
	if ids == nil || len(ids) > 1 || (len(ids) > 0 && strings.TrimSpace(reason) == "") {
		d.match.Store(&callMatchStatus{Phase: callInvalidPicks, Candidates: len(d.pending)})
		return toolText("请选择至多一条候选并说明接话理由，没有合适候选时传空数组", true)
	}
	eligible := make([]callVoice, 0, len(d.pending))
	for _, voice := range d.pending {
		if slices.Contains(ids, voice.Turn) {
			eligible = append(eligible, voice)
		}
	}
	match := callMatchStatus{Phase: callNoEligible, Candidates: len(d.pending), Eligible: len(eligible)}
	match.Current = d.game.Lines[len(d.game.Lines)-2].Text
	match.Reason = strings.TrimSpace(reason)
	defer d.match.Store(&match)
	d.pending = nil
	line := &d.game.Lines[len(d.game.Lines)-1]
	view := callDemoView{Turn: d.turn, Title: d.game.Title, Sentence: line.Text, Status: callBuddyTurn}
	if len(eligible) == 0 {
		return callDemoResult(view, "")
	}

	ctx, cancel := context.WithTimeout(ctx, callReadWait)
	defer cancel()
	for _, selected := range eligible {
		match.Selected = selected.Text
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

	playable := selected.URL
	if d.preparePlayback != nil {
		if selected.PlaybackURL == "" {
			padded, err := d.preparePlayback(ctx, selected)
			if err != nil {
				slog.WarnContext(ctx, "call teammate audio playback preparation failed")
				return "", callPrepareFailed
			}
			selected.PlaybackURL = padded
			if err := d.pool.save(selected); err != nil {
				slog.WarnContext(ctx, "call teammate playback cache update failed")
			}
		}
		playable = selected.PlaybackURL
	}
	signed, err := d.history.signed(ctx, playable)
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
		actions := []any{map[string]any{
			"name":      playCallAudio,
			"arguments": map[string]string{"audio_url": audio, "title": "队友接龙"},
		}}
		if os.Getenv("STORY_CALL_SILENT_CHAIN_TEST") == "1" {
			prompt, err := assets.ReadFile("call-prompt.txt")
			if err != nil {
				return toolText("故事伙伴暂时无法准备", true)
			}
			role := string(prompt) + "\n当工具结果包含切换角色已处理时，只说一句：有小伙伴回复你了。然后安静等待，等孩子下一次发言再继续游戏。"
			actions = append(actions, map[string]any{
				"name":      "change_role",
				"arguments": map[string]string{"role_prompt": role},
			})
		}
		result.StructuredContent = map[string]any{
			"code":    0,
			"message": "",
			"funcs":   actions,
		}
	}
	return result
}
