package main

import (
	"fmt"
	"sync"
	"testing"
)

func TestCallPoolMatchesWithinTitleAndScene(t *testing.T) {
	t.Parallel()

	pool, err := openCallPool(t.TempDir(), "agent")
	if err != nil {
		t.Fatal(err)
	}
	const ready = "https://audio.example/recording.mp3"
	clips := []callVoice{
		{Turn: "boot", Story: "old", Title: "森林探险", Text: "地图背面画了一只红靴子。", URL: ready},
		{Turn: "footprint", Story: "old", Title: defaultCallStoryTitle, Before: "地图反面画了什么？", Text: "上面有一串泥脚印。", URL: ready},
		{Turn: "dark", Story: "old", Title: defaultCallStoryTitle, Before: "林子里为什么这么暗？", Text: "一片乌云挡住了月亮。", URL: ready},
		{Turn: "unrelated", Story: "old", Title: defaultCallStoryTitle, Text: "小松鼠把星星放进口袋。", URL: ready},
		{Turn: "wrong-scene", Story: "old", Title: defaultCallStoryTitle, Before: "地图背面写了什么？", Text: "因为天黑，树林里一点光也没有。", URL: ready},
		{Turn: "echo-question", Story: "old", Title: defaultCallStoryTitle, Text: "地图的反面画着什么呢", URL: ready},
		{Turn: "mode-switch", Story: "old", Title: defaultCallStoryTitle, Before: "地图反面画了什么？", Text: "我要跟以前的朋友一起玩。", URL: ready},
		{Turn: "pending", Story: "old", Title: defaultCallStoryTitle, Text: "地图背面画着木桥。"},
		{Turn: "other-preset", Story: "old", Title: "双熊护林队", Text: "地图背面画了一只红靴子。", URL: ready},
		{Turn: "other-title", Story: "old", Title: "自定义故事", Text: "地图背面画了一只红靴子。", URL: ready},
	}
	for _, clip := range clips {
		if err := pool.save(clip); err != nil {
			t.Fatal(err)
		}
	}
	current := callVoice{Story: "current", Title: defaultCallStoryTitle, Text: "地图背后有什么？"}
	got := pool.candidates(current, map[string]bool{"boot": true}, nil)
	seen := map[string]bool{}
	for _, clip := range got {
		seen[clip.Turn] = true
	}
	if !seen["footprint"] || !seen["dark"] {
		t.Fatalf("the Buddy needs differently worded same-story candidates: %+v", got)
	}
	for _, forbidden := range []string{"boot", "mode-switch", "pending", "other-preset", "other-title"} {
		if seen[forbidden] {
			t.Fatalf("candidate %s should not be exposed: %+v", forbidden, got)
		}
	}
}

func TestCallPoolIndexesNewTitlesAndRebuildsOnRestart(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	pool, err := openCallPool(root, "agent")
	if err != nil {
		t.Fatal(err)
	}
	const title = "山谷救援"
	voice := callVoice{
		Turn: "sled", Story: "old", Title: title, Before: "雪橇卡住了。",
		Text: "雪橇卡住了，小松鼠把它推了出来。",
	}
	if err := pool.save(voice); err != nil {
		t.Fatal(err)
	}
	current := callVoice{Story: "new", Title: title, Text: "雪橇卡住了怎么办？"}
	if got := pool.candidates(current, nil, nil); len(got) != 0 {
		t.Fatalf("pending clip was indexed: %+v", got)
	}
	voice.URL = "https://audio.example/sled.mp3"
	if err := pool.save(voice); err != nil {
		t.Fatal(err)
	}
	if got := pool.candidates(current, nil, nil); len(got) != 1 || got[0].Turn != voice.Turn {
		t.Fatalf("archive completion did not update the index: %+v", got)
	}
	pool, err = openCallPool(root, "agent")
	if err != nil {
		t.Fatal(err)
	}
	if got := pool.candidates(current, nil, nil); len(got) != 1 || got[0].Turn != voice.Turn {
		t.Fatalf("reopen did not rebuild the title index: %+v", got)
	}
	current.Title = "另一个山谷故事"
	if got := pool.candidates(current, nil, nil); len(got) != 0 {
		t.Fatalf("another custom title saw the archive: %+v", got)
	}
}

func TestCallPoolReadsDuringArchive(t *testing.T) {
	t.Parallel()
	pool, err := openCallPool(t.TempDir(), "agent")
	if err != nil {
		t.Fatal(err)
	}
	voice := callVoice{
		Turn: "archive", Story: "old", Title: "森林冒险",
		Text: "兔子喜欢胡萝卜蛋糕", Before: "一只兔子走进森林",
	}
	if err := pool.save(voice); err != nil {
		t.Fatal(err)
	}
	errors := make(chan error, 2)
	var workers sync.WaitGroup
	workers.Go(func() {
		for i := range 20 {
			voice.URL = fmt.Sprintf("https://audio.example/%d.mp3", i)
			if err := pool.save(voice); err != nil {
				errors <- err
				return
			}
		}
	})
	workers.Go(func() {
		for range 20 {
			pool.candidates(
				callVoice{Story: "new", Title: "森林冒险", Text: "兔子喜欢什么？"},
				map[string]bool{},
				nil,
			)
			pool.opening("森林冒险")
			pool.unresolved()
		}
	})
	workers.Wait()
	close(errors)
	for err := range errors {
		t.Fatal(err)
	}
}
