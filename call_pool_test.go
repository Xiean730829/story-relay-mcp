package main

import (
	"fmt"
	"sync"
	"testing"
)

func TestCallPoolRanksRelatedReplyAcrossStoryTitles(t *testing.T) {
	t.Parallel()

	pool := &callPool{dir: t.TempDir(), voices: map[string]callVoice{
		"rabbit": {
			Turn: "rabbit", Story: "previous", Title: "兔子和胡萝卜",
			Before: "森林里住着一只小兔子，它有一片胡萝卜田。",
			Text:   "小兔子最爱吃胡萝卜蛋糕。",
		},
		"forest1": {Turn: "forest1", Story: "older1", Title: "森林冒险", Text: "星星说了什么？"},
		"forest2": {Turn: "forest2", Story: "older2", Title: "森林冒险", Text: "松鼠发现了松果。"},
		"forest3": {Turn: "forest3", Story: "older3", Title: "森林冒险", Text: "树上有一盏灯。"},
		"forest4": {Turn: "forest4", Story: "older4", Title: "森林冒险", Text: "下起了一场雨。"},
	}}
	current := callVoice{Story: "current", Title: "森林冒险", Text: "小兔子最喜欢什么？"}
	candidates, err := pool.candidates(current, map[string]bool{})
	if err != nil {
		t.Fatal(err)
	}
	if len(candidates) == 0 || candidates[0].Turn != "rabbit" {
		t.Fatalf("related answer did not rank ahead of unrelated same-title clips: %+v", candidates)
	}
}

func TestAutomaticCallReplies(t *testing.T) {
	t.Parallel()
	candidates := []callVoice{
		{Turn: "answer", Text: "小兔子最爱吃胡萝卜蛋糕", URL: "stored"},
		{Turn: "opposite", Text: "小兔子不爱吃胡萝卜蛋糕", URL: "stored"},
		{Turn: "question", Text: "小兔子最爱吃胡萝卜蛋糕吗？", URL: "stored"},
		{Turn: "missing", Text: "小兔子最爱吃胡萝卜蛋糕"},
	}
	got := automaticCallReplies(candidates, "小兔子最爱吃胡萝卜蛋糕。")
	if len(got) != 1 || got[0] != "answer" {
		t.Fatalf("unexpected automatic matches: %v", got)
	}
	if got := automaticCallReplies(candidates, "小兔子喜欢什么？"); len(got) != 0 {
		t.Fatalf("a question was treated as an answer: %v", got)
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
			_, err := pool.candidates(
				callVoice{Story: "new", Title: "森林冒险", Text: "兔子喜欢什么？"},
				map[string]bool{},
			)
			if err != nil {
				errors <- err
				return
			}
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
