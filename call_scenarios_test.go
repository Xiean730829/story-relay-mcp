package main

import (
	"encoding/json"
	"testing"
)

func TestPhoneOpensPresetWithoutGeneratedOpening(t *testing.T) {
	t.Parallel()

	if got := len(callStoryTitles()); got != 10 {
		t.Fatalf("got %d preset stories, want 10", got)
	}
	for _, tc := range []struct {
		name  string
		input string
		want  string
	}{
		{name: "default forest", want: defaultCallStoryTitle},
		{name: "forest recording title", input: "兔子的胡萝卜", want: defaultCallStoryTitle},
		{name: "bear story", input: "熊出没", want: "双熊护林队"},
		{name: "light guardian story", input: "奥特曼", want: "光之巨人守护城"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			demo, err := newCallDemo(callHistory{agent: "demo-agent"}, t.TempDir(), nil)
			if err != nil {
				t.Fatal(err)
			}
			demo.callWithMarker(t.Context(), toolCallLobby, callDemoInputs{UseHistory: true})
			if err := demo.pool.save(callVoice{Turn: "old", Title: tc.want, Before: "另一局已经接到结尾了。"}); err != nil {
				t.Fatal(err)
			}
			result := demo.callWithMarker(t.Context(), toolCallOpen, callDemoInputs{Lobby: demo.lobby, Title: tc.input})
			if result.IsError {
				t.Fatalf("preset story did not open: %+v", result)
			}
			var view callDemoView
			if err := json.Unmarshal([]byte(result.Content[0].Text), &view); err != nil {
				t.Fatal(err)
			}
			preset := callStoryByTitle[normalizedCallText(tc.want)]
			if view.Title != tc.want || view.Sentence != preset.Opening || len(view.PlotBeats) == 0 {
				t.Fatalf("wrong preset or opening: %+v", view)
			}
			if view.Status != callBuddyTurn || view.Turn == "" || result.StructuredContent != nil {
				t.Fatalf("opening must wait for the player: %+v", view)
			}
			repeated := demo.callWithMarker(t.Context(), toolCallOpen, callDemoInputs{Lobby: demo.lobby, Title: tc.want})
			if repeated.IsError {
				t.Fatalf("reopening an active preset returned an error: %+v", repeated)
			}
			var resumed callDemoView
			if err := json.Unmarshal([]byte(repeated.Content[0].Text), &resumed); err != nil {
				t.Fatal(err)
			}
			if resumed.Status != callGameActive || resumed.Turn != view.Turn || resumed.Sentence != "" {
				t.Fatalf("a repeated open should keep the current turn without repeating the opening: %+v", resumed)
			}
		})
	}
}
