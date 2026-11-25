package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestCallDemoUsesHistoricalVoiceAsAReplyInAnotherGame(t *testing.T) {
	t.Parallel()
	const (
		agentID = "demo-agent"
		title   = "森林冒险"
		spoken  = "熊看见口袋里有一朵会说话的云"
		media   = "https://audio.example/private/original.wav"
	)
	var playable, signed string
	lastTurn := ""
	signCalls := 0
	mediaReads := 0
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var data any
		switch r.URL.Path {
		case "/xiaozhi/agent/" + agentID + "/sessions":
			data = map[string]any{"list": []map[string]string{{"sessionId": "original-call"}}}
		case "/xiaozhi/agent/" + agentID + "/chat-history/original-call":
			data = []callHistoryRow{
				{Type: callHistoryUser, Text: "不相关", Audio: "https://audio.example/another.wav"},
				{Type: callHistoryUser, Text: spoken + "。", Audio: media},
				{Type: callHistoryTool, Tool: fmt.Sprintf("agent_%s({\"turn_key\":%q})", toolCallTurn, lastTurn)},
			}
		case callSignPath:
			var request struct {
				URL string `json:"url"`
			}
			if r.Method != http.MethodPost || json.NewDecoder(r.Body).Decode(&request) != nil || request.URL != playable {
				t.Errorf("wrong recording signature request: %+v", request)
			}
			signCalls++
			data = map[string]string{"signed_url": signed}
		case "/private/original.mp3":
			mediaReads++
			if r.Header.Get("Range") != "bytes=0-2" || r.URL.Query().Get("signed") != "fixture" {
				t.Error("player preflight did not use the signed media range")
			}
			w.WriteHeader(http.StatusPartialContent)
			if _, err := w.Write([]byte("ID3")); err != nil {
				t.Error(err)
			}
			return
		default:
			t.Errorf("unexpected history route: %s", r.URL.Path)
			http.NotFound(w, r)
			return
		}
		if err := json.NewEncoder(w).Encode(map[string]any{"code": 0, "data": data}); err != nil {
			t.Error(err)
		}
	})
	server := httptest.NewTLSServer(handler)
	t.Cleanup(server.Close)
	playable = server.URL + "/private/original.mp3"
	signed = playable + "?signed=fixture"
	history := callHistory{api: platformAPI{cfg: configuration{api: server.URL}, http: server.Client()}, agent: agentID}
	root := t.TempDir()
	prepare := func(_ context.Context, ref callVoice) (string, error) {
		t.Helper()
		if ref.URL != media || ref.Turn != lastTurn {
			t.Fatalf("unexpected source recording: %+v", ref)
		}
		return playable, nil
	}
	demo, err := newCallDemo(history, root, prepare)
	if err != nil {
		t.Fatal(err)
	}
	call := func(name string, input callDemoInputs) (callDemoView, mcpToolResult) {
		t.Helper()
		args, err := json.Marshal(input)
		if err != nil {
			t.Fatal(err)
		}
		result := demo.call(t.Context(), name, args)
		if result.IsError {
			t.Fatalf("%s: %v", name, result.Content)
		}
		var v callDemoView
		if err := json.Unmarshal([]byte(result.Content[0].Text), &v); err != nil {
			t.Fatal(err)
		}
		return v, result
	}
	lobby, _ := call(toolCallLobby, callDemoInputs{})
	first, result := call(toolCallOpen, callDemoInputs{Lobby: lobby.Lobby, Title: title, AI: "从前有一只熊。"})
	if first.Status != callBuddyTurn || result.StructuredContent != nil {
		t.Fatalf("a fresh story should start with its own opening: %+v", first)
	}
	if !strings.Contains(first.NextAction, toolCallTurn) {
		t.Fatalf("story opening did not direct the next spoken turn to the game: %+v", first)
	}
	lastTurn = first.Turn
	next, _ := call(toolCallTurn, callDemoInputs{Turn: first.Turn, Child: spoken, AI: "云说我们先去森林。"})
	if next.Status != callBuddyTurn || next.Capture != lastTurn || next.Turn == first.Turn {
		t.Fatalf("turn did not record a pending index: %+v", next)
	}
	call(toolCallEnd, callDemoInputs{Turn: next.Turn, Ending: "下一次见。"})
	demo, err = newCallDemo(history, root, prepare)
	if err != nil {
		t.Fatal(err)
	}
	newLobby, _ := call(toolCallLobby, callDemoInputs{UseHistory: true})
	second, opened := call(toolCallOpen, callDemoInputs{Lobby: newLobby.Lobby, Title: title, AI: "小熊想了想。"})
	if second.Status != callBuddyTurn || opened.StructuredContent != nil || second.Sentence != "从前有一只熊。" {
		t.Fatalf("starting a new game should wait for the player's line: %+v", second)
	}
	selection, pending := call(toolCallTurn, callDemoInputs{
		Turn: second.Turn, Child: "小熊伸手摸了摸口袋。", AI: "它在找自己的钥匙。",
	})
	if selection.Status != callChoosePeer || pending.StructuredContent != nil || len(selection.Candidates) != 1 {
		t.Fatalf("player's line did not trigger a search of older games: %+v", selection)
	}
	if selection.NextAction == "" || !strings.HasPrefix(pending.Content[0].Text, `{"status":"choose_peer"`) {
		t.Fatalf("candidate response did not give an immediate next action: %s", pending.Content[0].Text)
	}
	if match := demo.match.Load(); match.Phase != callCandidates || match.Candidates != 1 {
		t.Fatalf("missing candidate search diagnostics: %+v", match)
	}
	voice := selection.Candidates[0]
	if voice.Text != spoken || voice.ID != lastTurn || voice.Before != "从前有一只熊。" {
		t.Fatalf("wrong reply candidate: %+v", voice)
	}
	peer, replay := call(toolCallReply, callDemoInputs{Turn: selection.Turn, Eligible: []string{voice.ID}})
	if peer.Status != callPeerTurn || peer.Turn != selection.Turn || peer.Sentence != "" {
		t.Fatalf("original audio did not take the Buddy's place for this turn: %+v", peer)
	}
	if match := demo.match.Load(); match.Phase != callPeerRequested || match.Eligible != 1 {
		t.Fatalf("missing peer audio request diagnostics: %+v", match)
	}
	raw, err := json.Marshal(replay)
	if err != nil {
		t.Fatal(err)
	}
	wire := string(raw)
	if !strings.Contains(wire, `"structuredContent"`) ||
		!strings.Contains(wire, `"name":"linx_play"`) ||
		!strings.Contains(wire, signed) {
		t.Fatalf("missing caller playback action: %s", wire)
	}
	if strings.Contains(replay.Content[0].Text, signed) || signCalls != 1 || mediaReads != 1 {
		t.Fatal("signed audio should be given to the player only")
	}
	continued, _ := call(toolCallTurn, callDemoInputs{
		Turn: peer.Turn, Child: "熊问云要去哪里。", AI: "云说去看海。",
	})
	if continued.Status != callBuddyTurn || continued.Previous != spoken {
		t.Fatalf("the active story did not retain the peer line or exclude reuse: %+v", continued)
	}
	otherLobby, _ := call(toolCallLobby, callDemoInputs{})
	other, otherResult := call(toolCallOpen, callDemoInputs{
		Lobby: otherLobby.Lobby, Title: "海底探险", AI: "有一条小鱼出来了。",
	})
	if other.Status != callBuddyTurn || otherResult.StructuredContent != nil {
		t.Fatal("unrelated story did not open normally")
	}
	unrelated, _ := call(toolCallTurn, callDemoInputs{
		Turn: other.Turn, Child: "小鱼吹出许多泡泡。", AI: "泡泡飘到了水面。",
	})
	if unrelated.Status != callBuddyTurn || len(unrelated.Candidates) != 0 {
		t.Fatalf("unrelated topic produced unrelated candidates: %+v", unrelated)
	}
	closeLobby, _ := call(toolCallLobby, callDemoInputs{UseHistory: true})
	closeStory, _ := call(toolCallOpen, callDemoInputs{
		Lobby: closeLobby.Lobby, Title: "山洞探险", AI: "那天，天特别暗。",
	})
	related, _ := call(toolCallTurn, callDemoInputs{
		Turn: closeStory.Turn, Child: "从前有一只熊。", AI: "它住在山洞。",
	})
	if related.Status != callChoosePeer || len(related.Candidates) == 0 {
		t.Fatalf("related context under a different title did not find a candidate: %+v", related)
	}
	fallback, refused := call(toolCallReply, callDemoInputs{Turn: related.Turn, Eligible: []string{}})
	if fallback.Status != callBuddyTurn || fallback.Sentence != "它住在山洞。" || refused.StructuredContent != nil {
		t.Fatalf("rejected candidates did not leave the Buddy's turn in place: %+v", fallback)
	}
	if match := demo.match.Load(); match.Phase != callNoEligible || match.Eligible != 0 {
		t.Fatalf("missing rejection diagnostics: %+v", match)
	}
}

func TestCallVoiceRequiresTheCorrectUtteranceAndMarker(t *testing.T) {
	t.Parallel()
	ref := callVoice{Turn: "ABC123", Text: "我的故事"}
	rows := []callHistoryRow{
		{Type: callHistoryUser, Text: "我的故事", Audio: "https://example.com/wrong.wav"},
		{Type: callHistoryUser, Text: "最近一句", Audio: "https://example.com/unrelated.wav"},
		{Type: callHistoryTool, Tool: `self.call.turn({"turn_key":"ABC123"})`},
	}
	if result := findCallVoice(rows, ref); result != "" {
		t.Fatalf("found an older utterance with the same words: %s", result)
	}
	rows = append(rows, callHistoryRow{Type: callHistoryUser, Text: "我的故事。", Audio: "https://example.com/right.wav"})
	if result := findCallVoice(rows, ref); result != "https://example.com/right.wav" {
		t.Fatalf("did not recognize delayed input audio report: %s", result)
	}
	ref.Turn = "different"
	if result := findCallVoice(rows, ref); result != "" {
		t.Fatalf("unmatched round took a recording: %s", result)
	}
	ref.Turn = "ABC123"
	rows = []callHistoryRow{
		{Type: callHistoryUser, Text: "我的故事", Audio: "https://example.com/old.wav"},
		{Type: callHistoryTool, Tool: `self_call_turn({"turn_key":"another-turn"})`},
		{Type: callHistoryTool, Tool: `self_call_turn({"turn_key":"ABC123"})`},
	}
	if result := findCallVoice(rows, ref); result != "" {
		t.Fatalf("crossed an intervening tool turn: %s", result)
	}
}

func TestCallHistoryFindsRecordingBeyondRecentSessions(t *testing.T) {
	t.Parallel()
	const (
		agentID = "demo-agent"
		audio   = "https://audio.example/older.wav"
	)
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var data any
		switch r.URL.Path {
		case "/xiaozhi/agent/" + agentID + "/sessions":
			if r.URL.Query().Get("page") == "1" {
				list := make([]map[string]string, 0, callHistoryPageSize)
				for i := range callHistoryPageSize {
					list = append(list, map[string]string{"sessionId": fmt.Sprintf("recent-%d", i)})
				}
				data = map[string]any{"total": callHistoryPageSize + 1, "list": list}
			} else {
				data = map[string]any{
					"total": callHistoryPageSize + 1,
					"list":  []map[string]string{{"sessionId": "older"}},
				}
			}
		case "/xiaozhi/agent/" + agentID + "/chat-history/older":
			data = []callHistoryRow{
				{Type: callHistoryUser, Text: "兔子喜欢胡萝卜", Audio: audio},
				{Type: callHistoryTool, Tool: `self.call.turn({"turn_key":"old-turn"})`},
			}
		default:
			data = []callHistoryRow{}
		}
		if err := json.NewEncoder(w).Encode(map[string]any{"code": 0, "data": data}); err != nil {
			t.Error(err)
		}
	})
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	history := callHistory{api: platformAPI{cfg: configuration{api: server.URL}, http: server.Client()}, agent: agentID}
	got, err := history.find(t.Context(), callVoice{Turn: "old-turn", Text: "兔子喜欢胡萝卜"})
	if err != nil || got != audio {
		t.Fatalf("older audio was not found: %q, %v", got, err)
	}
}

func TestCallArchiveStoresAudioBeforeReuse(t *testing.T) {
	t.Parallel()
	const (
		agentID  = "demo-agent"
		original = "https://audio.example/original.wav"
		prepared = "https://audio.example/private.mp3"
	)
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var data any
		switch r.URL.Path {
		case "/xiaozhi/agent/" + agentID + "/sessions":
			data = map[string]any{"total": 1, "list": []map[string]string{{"sessionId": "old-session"}}}
		case "/xiaozhi/agent/" + agentID + "/chat-history/old-session":
			data = []callHistoryRow{
				{Type: callHistoryUser, Text: "兔子喜欢蛋糕", Audio: original},
				{Type: callHistoryTool, Tool: `self.call.turn({"turn_key":"turn-one"})`},
			}
		default:
			t.Errorf("unexpected route: %s", r.URL.Path)
			http.NotFound(w, r)
			return
		}
		if err := json.NewEncoder(w).Encode(map[string]any{"code": 0, "data": data}); err != nil {
			t.Error(err)
		}
	})
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	history := callHistory{api: platformAPI{cfg: configuration{api: server.URL}, http: server.Client()}, agent: agentID}
	root := t.TempDir()
	demo, err := newCallDemo(history, root, func(_ context.Context, voice callVoice) (string, error) {
		if voice.URL != original {
			t.Fatalf("wrong original recording: %q", voice.URL)
		}
		return prepared, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	voice := callVoice{Turn: "turn-one", Text: "兔子喜欢蛋糕", Story: "one", Title: "森林冒险"}
	if err := demo.pool.save(voice); err != nil {
		t.Fatal(err)
	}
	if err := demo.archiveVoice(t.Context(), voice); err != nil {
		t.Fatal(err)
	}
	reloaded, err := openCallPool(root, agentID)
	if err != nil {
		t.Fatal(err)
	}
	if got := reloaded.voices[voice.Turn].URL; got != prepared {
		t.Fatalf("prepared original was not saved for future games: %q", got)
	}
	if len(reloaded.unresolved()) != 0 {
		t.Fatal("an archived recording was still marked pending")
	}
}

func TestPhoneAutomaticallyPlaysAndAcceptsPreviousTurnAfterAction(t *testing.T) {
	t.Parallel()
	var signed string
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == callSignPath {
			if err := json.NewEncoder(w).Encode(map[string]any{"code": 0, "data": map[string]string{"signed_url": signed}}); err != nil {
				t.Error(err)
			}
			return
		}
		if _, err := w.Write([]byte("ID3")); err != nil {
			t.Error(err)
		}
	}))
	t.Cleanup(server.Close)
	signed = server.URL + "/original.mp3"
	history := callHistory{api: platformAPI{cfg: configuration{api: server.URL}, http: server.Client()}, agent: "demo-agent"}
	demo, err := newCallDemo(history, t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := demo.pool.save(callVoice{
		Turn: "old-voice", Story: "old-game", Title: "兔子",
		Text: "小兔子最爱吃胡萝卜蛋糕", URL: signed,
	}); err != nil {
		t.Fatal(err)
	}
	proxy := &privateProxy{callDemo: demo}
	call := func(name string, input callDemoInputs) (callDemoView, mcpToolResult) {
		t.Helper()
		raw, err := json.Marshal(input)
		if err != nil {
			t.Fatal(err)
		}
		result := proxy.callPhone(t.Context(), name, raw)
		if result.IsError {
			t.Fatalf("%s: %v", name, result.Content)
		}
		var view callDemoView
		if err := json.Unmarshal([]byte(result.Content[0].Text), &view); err != nil {
			t.Fatal(err)
		}
		return view, result
	}
	lobby, _ := call(toolCallLobby, callDemoInputs{UseHistory: true})
	opened, _ := call(toolCallOpen, callDemoInputs{Lobby: lobby.Lobby, Title: "兔子", AI: "森林里有只兔子。"})
	peer, result := call(toolCallTurn, callDemoInputs{
		Lobby: lobby.Lobby, Turn: opened.Turn, Child: "小兔子最喜欢什么？", AI: "小兔子最爱吃胡萝卜蛋糕。",
	})
	if peer.Status != callPeerTurn || result.StructuredContent == nil {
		t.Fatalf("matching original was not sent in the same call: %+v", peer)
	}
	// Linx may consume the action without forwarding the new turn key to the model.
	continued, _ := call(toolCallTurn, callDemoInputs{
		Lobby: lobby.Lobby, Turn: opened.Turn, Child: "它把蛋糕带回家。", AI: "大家一起分享。",
	})
	if continued.Previous != "小兔子最爱吃胡萝卜蛋糕" || continued.Capture != peer.Turn {
		t.Fatalf("story did not continue after playback: %+v", continued)
	}
	voice := demo.pool.voices[continued.Capture]
	if voice.Marker != opened.Turn || voice.Text != "它把蛋糕带回家。" {
		t.Fatalf("continued recording lost its history marker: %+v", voice)
	}
}

func TestPrivatePhoneCallsKeepIndependentGames(t *testing.T) {
	t.Parallel()
	demo, err := newCallDemo(callHistory{agent: "demo-agent"}, t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	proxy := &privateProxy{callDemo: demo}
	call := func(name string, input callDemoInputs) (callDemoView, mcpToolResult) {
		t.Helper()
		args, err := json.Marshal(input)
		if err != nil {
			t.Fatal(err)
		}
		params, err := json.Marshal(struct {
			Name string          `json:"name"`
			Args json.RawMessage `json:"arguments"`
		}{Name: name, Args: args})
		if err != nil {
			t.Fatal(err)
		}
		result := proxy.call(t.Context(), rpcMessage{Params: params})
		var view callDemoView
		if !result.IsError {
			if err := json.Unmarshal([]byte(result.Content[0].Text), &view); err != nil {
				t.Fatal(err)
			}
		}
		return view, result
	}
	first, _ := call(toolCallLobby, callDemoInputs{})
	second, _ := call(toolCallLobby, callDemoInputs{})
	_, result := call(toolCallTurn, callDemoInputs{
		Lobby: first.Lobby, Child: "还没开始", AI: "先开故事。",
	})
	if !result.IsError || !strings.Contains(result.Content[0].Text, toolCallOpen) {
		t.Fatalf("a turn before the game did not explain how to start: %+v", result)
	}
	a, result := call(toolCallOpen, callDemoInputs{Lobby: first.Lobby, Title: "森林冒险", AI: "小熊走进森林。"})
	if result.IsError {
		t.Fatal(result.Content)
	}
	b, result := call(toolCallOpen, callDemoInputs{Lobby: second.Lobby, Title: "海底探险", AI: "小鱼游进大海。"})
	if result.IsError {
		t.Fatal(result.Content)
	}
	updated, result := call(toolCallLobby, callDemoInputs{Lobby: first.Lobby, UseHistory: true})
	if result.IsError || updated.Lobby != first.Lobby || updated.Turn != a.Turn || updated.Status != callGameActive {
		t.Fatalf("changing play mode replaced the current game: %+v %+v", updated, result)
	}
	nextA, result := call(toolCallTurn, callDemoInputs{Turn: a.Turn, Child: "小熊看到树洞。", AI: "树洞里亮着灯。"})
	if result.IsError || nextA.Title != "森林冒险" {
		t.Fatalf("first caller was affected by the second: %+v %+v", nextA, result)
	}
	nextB, result := call(toolCallTurn, callDemoInputs{Turn: b.Turn, Child: "小鱼看到珊瑚。", AI: "珊瑚在摇摆。"})
	if result.IsError || nextB.Title != "海底探险" {
		t.Fatalf("second caller was affected by the first: %+v %+v", nextB, result)
	}
	recovered, result := call(toolCallTurn, callDemoInputs{
		Lobby: second.Lobby, Child: "小鱼找到了贝壳。", AI: "贝壳轻轻打开。",
	})
	if result.IsError || recovered.Title != "海底探险" || recovered.Capture != nextB.Turn {
		t.Fatalf("the active game could not recover a missing turn key: %+v %+v", recovered, result)
	}
	_, result = call(toolCallTurn, callDemoInputs{Turn: a.Turn, Child: "旧回合", AI: "不应接受"})
	if !result.IsError {
		t.Fatal("an old turn was accepted after the caller advanced")
	}
	if len(proxy.callTrace) != 10 || proxy.callTrace[9].OK || proxy.callTrace[8].Status != callBuddyTurn {
		t.Fatalf("phone call trace did not record the results: %+v", proxy.callTrace)
	}
}
