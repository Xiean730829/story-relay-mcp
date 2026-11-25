package main

import (
	"bytes"
	"fmt"
	"strings"
	"testing"
)

type lobbyTestBox struct {
	peer   *lobbyPeer
	events []lobbyOutbound
}

func testLobbyBox(id string) *lobbyTestBox {
	box := &lobbyTestBox{}
	box.peer = &lobbyPeer{id: id, send: func(event lobbyOutbound) {
		box.events = append(box.events, event)
	}}
	return box
}

func (b *lobbyTestBox) latestPlay(t *testing.T) lobbyPlay {
	t.Helper()
	for i := len(b.events) - 1; i >= 0; i-- {
		if event := b.events[i].message; event.Type == lobbyEventPlayStart {
			return *event.Play
		}
	}
	t.Fatal("box received no playback")
	return lobbyPlay{}
}

func (b *lobbyTestBox) media() []byte {
	media := []byte{}
	for _, event := range b.events {
		media = append(media, event.frame...)
	}
	return media
}

func joinTestBox(t *testing.T, l *lobby, b *lobbyTestBox, topic string, reuse bool) lobbyState {
	t.Helper()
	state, err := l.join(b.peer, lobbyJoin{Name: b.peer.id, Topic: topic, Reuse: reuse})
	if err != nil {
		t.Fatalf("join %s: %v", b.peer.id, err)
	}
	return state
}

func TestLobbyMatchesAndReusesVoice(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	pool, err := openLobbyPool(dir)
	if err != nil {
		t.Fatal(err)
	}
	l := newLobby(pool)
	a, b, c := testLobbyBox("A"), testLobbyBox("B"), testLobbyBox("C")
	waiting := joinTestBox(t, l, a, defaultLobbyTopic, false)
	retry := joinTestBox(t, l, a, defaultLobbyTopic, false)
	if waiting.Phase != lobbyWaiting || waiting.Room != retry.Room || len(retry.Members) != 1 {
		t.Fatalf("matching state changed on repeat join: %+v", retry)
	}
	if joining := joinTestBox(t, l, b, defaultLobbyTopic, false); joining.Room != waiting.Room {
		t.Fatalf("B did not match A: %+v", joining)
	}
	if l.state(a.peer).Phase != lobbyWaiting {
		t.Fatal("two players started before the deadline")
	}
	l.finishMatching(a.peer.room)
	if err := l.startVoice(a.peer); err != nil {
		t.Fatal(err)
	}
	joined := joinTestBox(t, l, c, defaultLobbyTopic, false)
	if joined.Phase != lobbyPlaying || len(joined.Members) != 3 || joined.Room != waiting.Room {
		t.Fatalf("late C did not enter the same game: %+v", joined)
	}
	frame := bytes.Repeat([]byte{42, 0}, pcmFrameBytes/2)
	if err := l.audio(c.peer, frame); err == nil {
		t.Fatal("late listener sent audio without the floor")
	}
	if err := l.audio(a.peer, frame); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(b.media(), frame) || len(c.media()) != 0 {
		t.Fatal("original frame did not go to the current listeners")
	}
	if err := l.finishVoice(a.peer, "森林里亮起一盏灯。", true); err != nil {
		t.Fatal(err)
	}
	play := b.latestPlay(t)
	l.playbackResult(c.peer, play.ID, lobbyPlaybackFinished)
	if l.state(b.peer).CanSpeak {
		t.Fatal("some other device unlocked B's turn")
	}
	l.playbackResult(b.peer, play.ID, lobbyPlaybackFinished)
	if !l.state(b.peer).CanSpeak {
		t.Fatal("B did not get the turn after playback")
	}
	if _, err := l.leave(a.peer, true); err != nil {
		t.Fatal(err)
	}

	persisted, err := openLobbyPool(dir)
	if err != nil {
		t.Fatal(err)
	}
	l = newLobby(persisted)
	d := testLobbyBox("D")
	unrelated := joinTestBox(t, l, testLobbyBox("sea"), "海底探险", true)
	if unrelated.Phase != lobbyWaiting || len(unrelated.Story) != 0 {
		t.Fatalf("unrelated context reused a recording: %+v", unrelated)
	}
	joinTestBox(t, l, d, defaultLobbyTopic, true)
	l.finishMatching(d.peer.room)
	reused := l.state(d.peer)
	old := d.latestPlay(t)
	replayed := len(reused.Story) == 1 && reused.Story[0].Historical && old.Historical
	if reused.Room == waiting.Room || !replayed || !bytes.Equal(d.media(), frame) {
		t.Fatalf("saved voice did not reach a new same-context game: %+v", reused)
	}
	t.Cleanup(func() { l.playbackResult(d.peer, old.ID, lobbyPlaybackFinished) })
	if reused.CanSpeak {
		t.Fatal("D got the floor before its historical audio finished")
	}
	l.playbackResult(d.peer, old.ID, lobbyPlaybackFinished)
	continued, err := l.sayText(d.peer, "我打开门，看见那盏灯在飞。")
	if err != nil || len(continued.Story) != 2 {
		t.Fatalf("the new game did not continue after replay: %+v, %v", continued, err)
	}
}

func TestLobbyMatchDeadlineAndExplicitReuse(t *testing.T) {
	t.Parallel()
	pool, err := openLobbyPool(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	l := newLobby(pool)
	a, b := testLobbyBox("A"), testLobbyBox("B")
	joinTestBox(t, l, a, defaultLobbyTopic, false)
	room := a.peer.room
	l.finishMatching(room)
	if state := l.state(a.peer); state.Phase != lobbyWaiting {
		t.Fatalf("one player started without a partner: %+v", state)
	}
	if err := l.reuse(a.peer); err == nil {
		t.Fatal("empty pool should leave the player waiting")
	}
	joined := joinTestBox(t, l, b, defaultLobbyTopic, false)
	if joined.Phase != lobbyPlaying {
		t.Fatalf("second player did not start after deadline: %+v", joined)
	}
	if _, err := l.leave(a.peer, true); err != nil {
		t.Fatal(err)
	}
}

func TestLobbyStopsTargetPlaybackIndependently(t *testing.T) {
	t.Parallel()
	pool, err := openLobbyPool(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	l := newLobby(pool)
	a, b, c := testLobbyBox("A"), testLobbyBox("B"), testLobbyBox("C")
	for _, box := range []*lobbyTestBox{a, b, c} {
		joinTestBox(t, l, box, defaultLobbyTopic, false)
	}
	if err := l.startVoice(a.peer); err != nil {
		t.Fatal(err)
	}
	bPlay, cPlay := b.latestPlay(t), c.latestPlay(t)
	if bPlay.ID == cPlay.ID {
		t.Fatal("B and C received the same playback reference")
	}
	l.stopPlayback(c.peer, bPlay.ID)
	frame := bytes.Repeat([]byte{4, 0}, pcmFrameBytes/2)
	if err := l.audio(a.peer, frame); err != nil {
		t.Fatal(err)
	}
	l.stopPlayback(c.peer, cPlay.ID)
	if err := l.audio(a.peer, frame); err != nil {
		t.Fatal(err)
	}
	if len(c.media()) != len(frame) || len(b.media()) != 2*len(frame) {
		t.Fatal("stopping C changed B's playback, or did not stop C")
	}
	if err := l.finishVoice(a.peer, "", false); err != nil {
		t.Fatal(err)
	}
	l.playbackResult(b.peer, bPlay.ID, lobbyPlaybackFinished)
	if !l.state(b.peer).CanSpeak {
		t.Fatal("C's playback status blocked B's handoff")
	}
}

func TestLobbyEndsAfterMaxStoryTurns(t *testing.T) {
	t.Parallel()
	pool, err := openLobbyPool(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	l := newLobby(pool)
	boxes := []*lobbyTestBox{testLobbyBox("A"), testLobbyBox("B"), testLobbyBox("C")}
	for _, box := range boxes {
		joinTestBox(t, l, box, defaultLobbyTopic, false)
	}
	if opening := l.state(boxes[0].peer); opening.Phase != lobbyPlaying || opening.Speaker != boxes[0].peer.name {
		t.Fatalf("三人到齐应开局并轮到第一位: %+v", opening)
	}
	for i := 0; i < maxStoryTurns; i++ {
		box := boxes[i%len(boxes)]
		state, err := l.sayText(box.peer, "第"+string(rune('一'+i))+"句")
		if err != nil {
			t.Fatalf("第 %d 句被拒绝: %v", i+1, err)
		}
		if i < maxStoryTurns-1 && state.Phase != lobbyPlaying {
			t.Fatalf("第 %d 句后阶段 = %s，应该继续", i+1, state.Phase)
		}
	}
	final := l.state(boxes[0].peer)
	if final.Phase != lobbyEnded {
		t.Fatalf("接满 %d 句后阶段 = %s", maxStoryTurns, final.Phase)
	}
	if len(final.Story) != maxStoryTurns {
		t.Fatalf("终局故事 %d 句, 期望 %d", len(final.Story), maxStoryTurns)
	}
	if _, err := l.sayText(boxes[0].peer, "还想再接一句"); err == nil {
		t.Fatal("收尾后仍允许接话")
	}
	if state := joinTestBox(t, l, boxes[0], defaultLobbyTopic, false); state.Phase != lobbyWaiting || len(state.Story) != 0 {
		t.Fatalf("重新加入应开新局: %+v", state)
	}
}

// TestLobbyHistoryPartnerCarriesStory 验证单人跟历史伙伴玩能走完整局：
// 每轮到历史席位就从池里取一条没听过的原声，听完交回真人；池空了由真人继续；接满 9 棒收尾。
func TestLobbyHistoryPartnerCarriesStory(t *testing.T) {
	t.Parallel()
	pool, err := openLobbyPool(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	voice := bytes.Repeat([]byte{7, 0}, pcmFrameBytes/2)
	clipVoice := bytes.Repeat(voice, 8)
	for i, author := range []string{"伙伴甲", "伙伴乙"} {
		clip := lobbyClip{
			ID: fmt.Sprintf("clip-%d", i), SourceRoom: "history", Topic: strings.ToLower(defaultLobbyTopic),
			Owner: fmt.Sprintf("kid-%d", i), Author: author, Text: "从前有座山。", PCM: clipVoice,
		}
		if err := pool.save(clip); err != nil {
			t.Fatal(err)
		}
	}
	l := newLobby(pool)
	a := testLobbyBox("A")
	joinTestBox(t, l, a, defaultLobbyTopic, true)
	if err := l.reuse(a.peer); err != nil {
		t.Fatalf("单人选择历史伙伴: %v", err)
	}
	opening := l.state(a.peer)
	if opening.Phase != lobbyPlaying || len(opening.Story) != 1 || !opening.Story[0].Historical {
		t.Fatalf("历史伙伴没有先开口: %+v", opening)
	}
	if len(a.media()) == 0 {
		t.Fatal("历史原声没有送到盒子")
	}
	l.playbackResult(a.peer, a.latestPlay(t).ID, lobbyPlaybackFinished)
	if !l.state(a.peer).CanSpeak {
		t.Fatal("历史原声播完后没有把发言权交回真人")
	}
	for turn := 0; turn < maxStoryTurns; turn++ {
		state := l.state(a.peer)
		if state.Phase == lobbyEnded {
			break
		}
		if !state.CanSpeak {
			l.playbackResult(a.peer, a.latestPlay(t).ID, lobbyPlaybackFinished)
			if state = l.state(a.peer); !state.CanSpeak {
				t.Fatalf("第 %d 棒前没有人可以开口: %+v", turn+1, state)
			}
		}
		if err := l.startVoice(a.peer); err != nil {
			t.Fatalf("第 %d 棒开口: %v", turn+1, err)
		}
		if err := l.audio(a.peer, voice); err != nil {
			t.Fatal(err)
		}
		// 真人这句不留存，池里始终只有两条历史原声，用来验证池空后的降级。
		if err := l.finishVoice(a.peer, "山里有座庙。", false); err != nil {
			t.Fatalf("第 %d 棒接话: %v", turn+1, err)
		}
	}
	final := l.state(a.peer)
	if len(final.Story) != maxStoryTurns {
		t.Fatalf("应接满 %d 句收尾，实际 %d 句", maxStoryTurns, len(final.Story))
	}
	heard := map[string]bool{}
	for _, line := range final.Story {
		if !line.Historical {
			continue
		}
		if heard[line.Author] {
			t.Fatalf("同一位历史伙伴的原声重复出现: %s", line.Author)
		}
		heard[line.Author] = true
	}
	if len(heard) != 2 {
		t.Fatalf("池里的两条历史原声都应被复用，实际 %d 条", len(heard))
	}
	if _, err := l.leave(a.peer, true); err != nil {
		t.Fatalf("收尾后离场: %v", err)
	}
}
