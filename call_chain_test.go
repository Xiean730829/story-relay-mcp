package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestSoloStoryChainArchivesBeforeMatchingAndReplaysInOrder(t *testing.T) {
	var detail callChainDetail
	reserved := false
	matches, creates := 0, 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request map[string]any
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
			return
		}
		var data any
		switch r.URL.Path {
		case "/v1/story/theme":
			data = map[string]bool{"saved": true}
		case "/v1/story/match":
			matches++
			data = map[string]any{"matched": detail.Story.ID != "", "story": detail}
		case "/v1/story/claim":
			reserved = true
			data = map[string]bool{"claimed": true}
		case "/v1/story/create", "/v1/story/append":
			if r.URL.Path == "/v1/story/create" {
				creates++
			}
			if r.URL.Path == "/v1/story/append" && !reserved {
				t.Error("appended without reservation")
			}
			detail.Story.ID = "story-one"
			detail.Story.Version++
			detail.Segments = append(detail.Segments, struct {
				ClipID string `json:"clip_id"`
				Text   string `json:"text"`
			}{request["clip_id"].(string), request["text"].(string)})
			reserved = false
			data = detail
		default:
			t.Errorf("unexpected route: %s", r.URL.Path)
		}
		if err := json.NewEncoder(w).Encode(map[string]any{"code": 0, "data": data}); err != nil {
			t.Error(err)
		}
	}))
	t.Cleanup(server.Close)
	demo, err := newCallDemo(callHistory{agent: "test"}, t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	demo.chain = newCallChain(server.URL)
	var played []string
	demo.prepareChain = func(_ context.Context, voices []callVoice) (string, error) {
		for _, voice := range voices {
			played = append(played, voice.Text)
		}
		return "https://example.com/story.mp3", nil
	}
	call := func(name string, in callDemoInputs) (callDemoView, mcpToolResult) {
		t.Helper()
		result := demo.callWithMarker(t.Context(), name, in)
		if result.IsError {
			t.Fatalf("%s: %+v", name, result.Content)
		}
		var view callDemoView
		if err := json.Unmarshal([]byte(result.Content[0].Text), &view); err != nil {
			t.Fatal(err)
		}
		return view, result
	}
	archive := func(turn string) {
		t.Helper()
		voice, ok := demo.pool.voice(turn)
		if !ok || !voice.ChainPending {
			t.Fatalf("missing pending recording: %+v", voice)
		}
		voice.URL = "https://example.com/saved.mp3"
		if err := demo.pool.save(voice); err != nil {
			t.Fatal(err)
		}
		if err := demo.archiveVoice(t.Context(), voice); err != nil {
			t.Fatal(err)
		}
	}
	for _, sentence := range []string{"first", "second", "third"} {
		entry, _ := call(toolCallEntry, callDemoInputs{})
		if entry.Status != callChooseMode || entry.Lobby == "" || entry.Sentence != "你想和我一起玩，还是和以前的小伙伴玩？" {
			t.Fatalf("game must ask the child to choose: %+v", entry)
		}
		lobby, _ := call(toolCallLobby, callDemoInputs{Lobby: entry.Lobby, UseHistory: true})
		opened, _ := call(toolCallOpen, callDemoInputs{Lobby: lobby.Lobby, Title: defaultCallStoryTitle})
		if len(detail.Segments) > 0 {
			_, playback := call(toolCallPlay, callDemoInputs{Turn: opened.Turn})
			if playback.StructuredContent == nil {
				t.Fatal("missing playback action")
			}
		}
		captured, _ := call(toolCallTurn, callDemoInputs{Turn: opened.Turn, Child: sentence, AI: "这句 AI 续写不应被采用"})
		if captured.Status != callStorySaved || captured.Sentence != "" {
			t.Fatalf("history mode must not return an AI continuation: %+v", captured)
		}
		if line := demo.game.Lines[len(demo.game.Lines)-1]; line.Role != roleChild || line.Text != sentence {
			t.Fatalf("history mode must end with the child's line: %+v", line)
		}
		if len(detail.Segments) == 0 && sentence == "first" && captured.Capture == "" {
			t.Fatal("missing audio capture")
		}
		archive(captured.Capture)
	}
	if len(detail.Segments) != 3 || len(played) != 3 || played[0] != "first" || played[1] != "first" || played[2] != "second" {
		t.Fatalf("story/playback order wrong: %+v / %+v", detail.Segments, played)
	}
	previousMatches := matches
	entry, _ := call(toolCallEntry, callDemoInputs{})
	lobby, _ := call(toolCallLobby, callDemoInputs{Lobby: entry.Lobby, UseHistory: true})
	opened, _ := call(toolCallOpen, callDemoInputs{Lobby: lobby.Lobby, Title: defaultCallStoryTitle, NewStory: true})
	if matches != previousMatches {
		t.Fatal("new story should not match an existing story")
	}
	captured, _ := call(toolCallTurn, callDemoInputs{Turn: opened.Turn, Child: "new beginning"})
	archive(captured.Capture)
	if creates != 2 {
		t.Fatalf("expected a second independently created story, got %d creations", creates)
	}
}

func TestChainCannotSkipModeChoice(t *testing.T) {
	demo, err := newCallDemo(callHistory{agent: "test"}, t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	demo.chain = newCallChain("http://127.0.0.1")
	result := demo.callWithMarker(t.Context(), toolCallLobby, callDemoInputs{UseHistory: true})
	if !result.IsError {
		t.Fatal("starting history mode without the choice step must fail")
	}
}

func TestStoryThemeIDsSeparateTitles(t *testing.T) {
	if callChainThemeID("森林冒险") != callChainTheme || callChainThemeID("海底探险") == callChainTheme {
		t.Fatal("story titles must not share a theme")
	}
}

func TestReadyAudioStillRetriesPendingStoryCommit(t *testing.T) {
	pool, err := openCallPool(t.TempDir(), "test")
	if err != nil {
		t.Fatal(err)
	}
	voice := callVoice{Turn: "turn", Text: "story", ChainPending: true}
	if err := pool.save(voice); err != nil {
		t.Fatal(err)
	}
	voice.URL = "https://example.com/archived.mp3"
	if err := pool.save(voice); err != nil {
		t.Fatal(err)
	}
	pending := pool.unresolved()
	if len(pending) != 1 || pending[0].URL != voice.URL {
		t.Fatalf("archived voice must remain retryable: %+v", pending)
	}
}
