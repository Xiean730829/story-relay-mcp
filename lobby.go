package main

import (
	"crypto/rand"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

const (
	defaultLobbyTopic = "森林冒险"
	lobbyIdle         = "idle"
	lobbyWaiting      = "waiting"
	lobbyPlaying      = "playing"
	lobbyEnded        = "ended"
	maxLobbyTopic     = 40
	maxLobbyName      = 24
	maxLobbyPlayers   = 3
	matchDuration     = 3 * time.Minute
	minLobbyPlayers   = 2
	maxVoiceSeconds   = 30
	maxStoryTurns     = 9
	pcmFrameBytes     = uplinkRate * frameDuration / 1000 * bitsPerSample / 8
	maxVoiceBytes     = uplinkRate * bitsPerSample / 8 * maxVoiceSeconds
	playbackGrace     = 5 * time.Second
	roomEndGrace      = 30 * time.Second
)

type lobbyMessage struct {
	Type  string      `json:"type"`
	Text  string      `json:"text,omitempty"`
	State *lobbyState `json:"state,omitempty"`
	Play  *lobbyPlay  `json:"play,omitempty"`
}

type lobbyPlaybackState string

const (
	lobbyPlaybackFinished lobbyPlaybackState = "finished"
	lobbyPlaybackStopped  lobbyPlaybackState = "stopped"
	lobbyPlaybackFailed   lobbyPlaybackState = "failed"
)

type lobbyPlay struct {
	ID         string `json:"id"`
	Author     string `json:"author,omitempty"`
	Historical bool   `json:"historical,omitempty"`
}

type lobbyState struct {
	Room          string        `json:"room_id,omitempty"`
	Topic         string        `json:"topic,omitempty"`
	Phase         string        `json:"phase"`
	Self          string        `json:"self"`
	Members       []lobbyMember `json:"members"`
	Story         []lobbyLine   `json:"story"`
	Speaker       string        `json:"speaker,omitempty"`
	CanSpeak      bool          `json:"can_speak"`
	Recording     bool          `json:"recording"`
	WaitingOn     string        `json:"waiting_on,omitempty"`
	MatchDeadline time.Time     `json:"match_deadline,omitempty"`
	CanEnd        bool          `json:"can_end"`
}

type lobbyMember struct {
	Name  string `json:"name"`
	Self  bool   `json:"self"`
	Audio bool   `json:"audio"`
}

type lobbyLine struct {
	Author     string `json:"author"`
	Text       string `json:"text,omitempty"`
	Audio      bool   `json:"audio"`
	Historical bool   `json:"historical,omitempty"`
}

type lobbyPeer struct {
	id             string
	name           string
	room           *lobbyRoom
	reuseAfterWait bool
	// Browser playback and MCP-only seats share the same membership and turns.
	send func(lobbyOutbound)
}

type lobbyOutbound struct {
	message lobbyMessage
	frame   []byte
}

type lobbyRoom struct {
	id       string
	topic    string
	phase    string
	peers    []*lobbyPeer
	story    []lobbyLine
	speaker  *lobbyPeer
	turn     *lobbyTurn
	await    *lobbyWait
	playback map[*lobbyPeer]string
	// pendingEnd 表示已满 maxStoryTurns 句，等最后一棒的播放全部收尾后结束房间。
	pendingEnd bool
	// partner 表示这局由历史原声陪玩：真人说完后轮到历史席位，历史席位逐棒从原声池取音。
	partner bool
	// partners 记录本局已经听过的历史原声，避免同一句反复出现。
	partners map[string]bool
	// historical 是当前发言的历史伙伴名字，非空时表示轮到历史席位而不是真人。
	historical string
	// historicalPlay 是本局历史席位的播放 ID，播完或超时后把发言权交回真人。
	historicalPlay string
	// endTimer 是收尾阶段的兜底计时器，正常情况下播放回报会先于它触发结束。
	endTimer      *time.Timer
	matchTimer    *time.Timer
	matchDeadline time.Time
	matchExpired  bool
}

type lobbyTurn struct {
	data      []byte
	listeners map[*lobbyPeer]string
}

type lobbyWait struct {
	peer  *lobbyPeer
	id    string
	timer *time.Timer
}

type lobbyJoin struct {
	Name  string
	Topic string
	Reuse bool
}

// lobby owns in-memory room state; saving recordings and sending over sockets happen outside its lock.
type lobby struct {
	mu      sync.Mutex
	rooms   map[string]*lobbyRoom
	tickets map[string]*lobbyPeer
	pool    *lobbyPool
}

func newLobby(pool *lobbyPool) *lobby {
	return &lobby{
		rooms:   make(map[string]*lobbyRoom),
		tickets: make(map[string]*lobbyPeer),
		pool:    pool,
	}
}

func (l *lobby) join(p *lobbyPeer, options lobbyJoin) (lobbyState, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if p.room != nil {
		// 上一局已结束：保留终局局面给玩家回顾，重新加入时脱离旧房间。
		if p.room.phase != lobbyEnded {
			return l.snapshot(p), nil
		}
		previous := p.room
		previous.peers = slices.DeleteFunc(previous.peers, func(peer *lobbyPeer) bool { return peer == p })
		p.room = nil
	}
	name := strings.TrimSpace(options.Name)
	topic := strings.TrimSpace(options.Topic)
	if topic == "" {
		topic = defaultLobbyTopic
	}
	if utf8.RuneCountInString(name) > maxLobbyName || utf8.RuneCountInString(topic) > maxLobbyTopic {
		return lobbyState{}, errors.New("盒子名称或故事语境太长")
	}
	key := strings.ToLower(topic)
	r := l.rooms[key]
	if r != nil && len(r.peers) >= maxLobbyPlayers {
		return lobbyState{}, errors.New("这个故事的房间已经坐满了")
	}
	if r != nil && r.pendingEnd {
		return lobbyState{}, errors.New("这一局正在收尾，请稍后重新加入")
	}
	if r != nil && r.partner {
		return lobbyState{}, errors.New("这一局正在和历史伙伴玩，请换个语境或稍后再来")
	}
	if r == nil {
		r = &lobbyRoom{
			id: rand.Text(), topic: topic, phase: lobbyWaiting, peers: []*lobbyPeer{},
			story: []lobbyLine{}, playback: make(map[*lobbyPeer]string),
		}
		l.rooms[key] = r
		r.matchDeadline = time.Now().Add(matchDuration)
		r.matchTimer = time.AfterFunc(matchDuration, func() { l.finishMatching(r) })
	}
	if name == "" {
		name = fmt.Sprintf("盒子 %d", len(r.peers)+1)
	}
	p.name, p.room, p.reuseAfterWait = name, r, options.Reuse
	r.peers = append(r.peers, p)
	if r.phase == lobbyWaiting && (len(r.peers) == maxLobbyPlayers || (r.matchExpired && len(r.peers) >= minLobbyPlayers)) {
		r.phase, r.speaker = lobbyPlaying, r.peers[0]
		r.matchTimer.Stop()
	}
	l.broadcast(r)
	return l.snapshot(p), nil
}

func (l *lobby) finishMatching(r *lobbyRoom) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if r.phase != lobbyWaiting {
		return
	}
	r.matchExpired = true
	if len(r.peers) >= minLobbyPlayers {
		r.phase, r.speaker = lobbyPlaying, r.peers[0]
	} else if len(r.peers) == 1 && r.peers[0].reuseAfterWait && r.peers[0].send != nil {
		l.reuseLocked(r.peers[0])
	}
	l.broadcast(r)
}

func (l *lobby) reuse(p *lobbyPeer) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if p.room == nil || p.room.phase != lobbyWaiting || len(p.room.peers) != 1 {
		return errors.New("请在单人匹配等待时选择历史伙伴")
	}
	if !l.reuseLocked(p) {
		return errors.New("暂时没有这个语境的历史原声，请继续等待伙伴")
	}
	l.broadcast(p.room)
	return nil
}

func (l *lobby) reuseLocked(p *lobbyPeer) bool {
	r := p.room
	if r.partners == nil {
		r.partners = make(map[string]bool)
	}
	r.partner, r.phase = true, lobbyPlaying
	if !l.playHistoricalLocked(r) {
		r.partner, r.phase = false, lobbyWaiting
		return false
	}
	r.matchTimer.Stop()
	return true
}

// playHistoricalLocked 让历史席位接一棒：从原声池取一条，播给真人听，听完再把发言权交回。
func (l *lobby) playHistoricalLocked(r *lobbyRoom) bool {
	if len(r.peers) != 1 {
		return false
	}
	owner := r.peers[0]
	clip, found := l.pool.take(strings.ToLower(r.topic), owner.name, r.partners)
	if !found {
		return false
	}
	r.partners[clip.ID] = true
	r.appendLine(lobbyLine{Author: clip.Author, Audio: true, Historical: true, Text: clip.Text})
	r.speaker, r.historical = nil, clip.Author
	id := r.beginPlayback(owner, lobbyPlay{Author: clip.Author, Historical: true})
	r.historicalPlay = id
	owner.send(lobbyOutbound{frame: clip.PCM})
	owner.send(lobbyOutbound{message: lobbyMessage{Type: lobbyEventPlayEnd, Play: &lobbyPlay{ID: id}}})
	duration := time.Duration(len(clip.PCM)) * time.Second / (uplinkRate * bitsPerSample / 8)
	l.waitForPlayback(r, owner, id, duration+playbackGrace)
	return true
}

// handBackLocked 在历史原声播完（或超时）后把发言权交回真人；接满棒数则直接收尾。
func (l *lobby) handBackLocked(r *lobbyRoom) {
	if r.historical == "" || len(r.peers) == 0 {
		return
	}
	r.historical, r.historicalPlay = "", ""
	if len(r.story) >= maxStoryTurns {
		l.beginEnding(r)
		return
	}
	r.speaker = r.peers[0]
}

func (l *lobby) startVoice(p *lobbyPeer) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	r, err := l.canSubmit(p)
	if err != nil {
		return err
	}
	r.turn = &lobbyTurn{
		data: make([]byte, 0, pcmFrameBytes*20), listeners: make(map[*lobbyPeer]string),
	}
	for _, listener := range r.peers {
		if listener != p && listener.send != nil {
			r.turn.listeners[listener] = r.beginPlayback(listener, lobbyPlay{Author: p.name})
		}
	}
	l.broadcast(r)
	return nil
}

func (l *lobby) audio(p *lobbyPeer, frame []byte) error {
	if len(frame) != pcmFrameBytes {
		return errors.New("录音需要 16 kHz 单声道 PCM，每帧 20 毫秒")
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	r := p.room
	if r == nil || r.speaker != p || r.turn == nil {
		return errors.New("这台盒子当前没有发言权")
	}
	if len(r.turn.data)+len(frame) > maxVoiceBytes {
		return errors.New("这句已达到录音时长上限，请结束发言")
	}
	r.turn.data = append(r.turn.data, frame...)
	for listener := range r.turn.listeners {
		listener.send(lobbyOutbound{frame: frame})
	}
	return nil
}

func (l *lobby) finishVoice(p *lobbyPeer, text string, share bool) error {
	text, err := lobbySentence(text)
	if err != nil {
		return err
	}
	l.mu.Lock()
	r := p.room
	if r == nil || r.speaker != p || r.turn == nil {
		l.mu.Unlock()
		return errors.New("这台盒子没有正在进行的录音")
	}
	turn := r.turn
	if len(turn.data) == 0 {
		l.abortVoice(r)
		l.broadcast(r)
		l.mu.Unlock()
		return errors.New("还没收到声音，请重新说一句")
	}
	r.turn = nil
	r.appendLine(lobbyLine{Author: p.name, Text: text, Audio: true})
	for listener, id := range turn.listeners {
		listener.send(lobbyOutbound{message: lobbyMessage{Type: lobbyEventPlayEnd, Play: &lobbyPlay{ID: id}}})
	}
	switch {
	case len(r.story) >= maxStoryTurns:
		l.beginEnding(r)
	case r.partner:
		// 单人跟历史伙伴玩：真人说完轮到历史席位取一条原声，取不到就由真人继续。
		if !l.playHistoricalLocked(r) {
			r.speaker = p
		}
	default:
		r.speaker = r.next(p)
		if id, receiving := turn.listeners[r.speaker]; receiving {
			l.waitForPlayback(r, r.speaker, id, playbackGrace)
		}
	}
	if r.phase != lobbyEnded {
		l.broadcast(r)
	}
	clip := lobbyClip{
		ID: rand.Text(), SourceRoom: r.id, Topic: strings.ToLower(r.topic),
		Owner: p.name, Author: p.name, Text: text, PCM: turn.data,
	}
	l.mu.Unlock()
	if share {
		if err := l.pool.save(clip); err != nil {
			return fmt.Errorf("这句已经接上，原声留存失败: %w", err)
		}
	}
	return nil
}

func (l *lobby) sayText(p *lobbyPeer, text string) (lobbyState, error) {
	text, err := lobbySentence(text)
	if err != nil {
		return lobbyState{}, err
	}
	if text == "" {
		return lobbyState{}, errors.New("请说一句故事")
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	r, err := l.canSubmit(p)
	if err != nil {
		return lobbyState{}, err
	}
	r.appendLine(lobbyLine{Author: p.name, Text: text})
	switch {
	case len(r.story) >= maxStoryTurns:
		l.beginEnding(r)
	case r.partner:
		if !l.playHistoricalLocked(r) {
			r.speaker = p
		}
	default:
		r.speaker = r.next(p)
	}
	if r.phase != lobbyEnded {
		l.broadcast(r)
	}
	return l.snapshot(p), nil
}

func (l *lobby) playbackResult(p *lobbyPeer, id string, state lobbyPlaybackState) {
	l.mu.Lock()
	defer l.mu.Unlock()
	r := p.room
	if r == nil || r.playback[p] != id || id == "" {
		return
	}
	delete(r.playback, p)
	if r.turn != nil {
		delete(r.turn.listeners, p)
	}
	if r.await == nil || r.await.peer != p || r.await.id != id {
		return
	}
	l.clearWait(r)
	if r.historicalPlay == id {
		l.handBackLocked(r)
	}
	if state != lobbyPlaybackFinished {
		p.send(lobbyOutbound{message: lobbyMessage{
			Type: lobbyEventNotice, Text: "上一段没有完整播放，可以继续接话。",
		}})
	}
	l.broadcast(r)
}

func (l *lobby) stopPlayback(p *lobbyPeer, id string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	r := p.room
	if r == nil {
		return
	}
	if id == "" || r.playback[p] != id {
		return
	}
	r.stopPlayback(p, id)
	if r.turn != nil {
		delete(r.turn.listeners, p)
	}
	if r.await != nil && r.await.peer == p {
		l.clearWait(r)
	}
	l.broadcast(r)
}

func (l *lobby) leave(p *lobbyPeer, end bool) (lobbyState, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	r := p.room
	if r == nil {
		return l.snapshot(p), nil
	}
	if end {
		if r.peers[0] != p {
			return lobbyState{}, errors.New("只有当前房主可以结束整局，你可以随时退出")
		}
		l.endRoom(r)
		return l.snapshot(p), nil
	}
	if r.speaker == p {
		l.abortVoice(r)
		r.speaker = r.next(p)
	}
	if r.await != nil && r.await.peer == p {
		l.clearWait(r)
	}
	if id, playing := r.playback[p]; playing {
		r.stopPlayback(p, id)
	}
	if r.turn != nil {
		delete(r.turn.listeners, p)
	}
	r.peers = slices.DeleteFunc(r.peers, func(peer *lobbyPeer) bool { return peer == p })
	p.room = nil
	if len(r.peers) == 0 {
		l.endRoom(r)
	}
	l.broadcast(r)
	l.sendState(p)
	return l.snapshot(p), nil
}

func (l *lobby) endRoom(r *lobbyRoom) {
	l.clearWait(r)
	if r.matchTimer != nil {
		r.matchTimer.Stop()
	}
	if r.endTimer != nil {
		r.endTimer.Stop()
		r.endTimer = nil
	}
	r.turn, r.speaker = nil, nil
	r.phase = lobbyEnded
	for peer, id := range r.playback {
		r.stopPlayback(peer, id)
	}
	l.broadcast(r)
	delete(l.rooms, strings.ToLower(r.topic))
}

// beginEnding 在接满 maxStoryTurns 句后收尾：不再交棒，等仍在播放的听众听完后结束房间；
// 若没有人在听则立即结束，播放超时由 endTimer 兜底。
func (l *lobby) beginEnding(r *lobbyRoom) {
	r.pendingEnd = true
	r.speaker = nil
	for _, peer := range r.peers {
		if peer.send != nil {
			peer.send(lobbyOutbound{message: lobbyMessage{
				Type: lobbyEventNotice,
				Text: fmt.Sprintf("故事接满 %d 句了，听完最后一段就圆满收尾。", maxStoryTurns),
			}})
		}
	}
	if len(r.playback) == 0 {
		l.endRoom(r)
		return
	}
	r.endTimer = time.AfterFunc(roomEndGrace, func() {
		l.mu.Lock()
		defer l.mu.Unlock()
		if r.pendingEnd && r.phase != lobbyEnded {
			l.endRoom(r)
		}
	})
}

func (l *lobby) abortVoice(r *lobbyRoom) {
	if r.turn == nil {
		return
	}
	for listener, id := range r.turn.listeners {
		r.stopPlayback(listener, id)
	}
	r.turn = nil
}

func (l *lobby) canSubmit(p *lobbyPeer) (*lobbyRoom, error) {
	r := p.room
	if r == nil || r.phase != lobbyPlaying {
		return nil, errors.New("正在等待搭档加入故事")
	}
	if r.speaker != p || r.await != nil {
		return nil, errors.New("还没轮到这台盒子开口")
	}
	if r.turn != nil {
		return nil, errors.New("请先结束这次发言")
	}
	return r, nil
}

func (l *lobby) waitForPlayback(r *lobbyRoom, p *lobbyPeer, id string, limit time.Duration) {
	r.await = &lobbyWait{peer: p, id: id, timer: time.AfterFunc(limit, func() {
		l.mu.Lock()
		defer l.mu.Unlock()
		if r.await == nil || r.await.id != id {
			return
		}
		r.await = nil
		r.stopPlayback(p, id)
		p.send(lobbyOutbound{message: lobbyMessage{
			Type: lobbyEventNotice, Text: "等待播放超时，这台盒子可以接话了。",
		}})
		if r.historicalPlay == id {
			l.handBackLocked(r)
		}
		l.broadcast(r)
	})}
}

func (l *lobby) clearWait(r *lobbyRoom) {
	if r.await != nil {
		r.await.timer.Stop()
		r.await = nil
	}
}

func (r *lobbyRoom) next(p *lobbyPeer) *lobbyPeer {
	at := slices.Index(r.peers, p)
	return r.peers[(at+1)%len(r.peers)]
}

func (r *lobbyRoom) beginPlayback(p *lobbyPeer, play lobbyPlay) string {
	if previous, playing := r.playback[p]; playing {
		r.stopPlayback(p, previous)
	}
	play.ID = rand.Text()
	r.playback[p] = play.ID
	p.send(lobbyOutbound{message: lobbyMessage{Type: lobbyEventPlayStart, Play: &play}})
	return play.ID
}

func (r *lobbyRoom) stopPlayback(p *lobbyPeer, id string) {
	delete(r.playback, p)
	p.send(lobbyOutbound{message: lobbyMessage{Type: lobbyEventPlayStop, Play: &lobbyPlay{ID: id}}})
}

func (r *lobbyRoom) appendLine(line lobbyLine) {
	r.story = append(r.story, line)
	if len(r.story) > maxLines {
		r.story = slices.Delete(r.story, 0, 1)
	}
}

func (l *lobby) state(p *lobbyPeer) lobbyState {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.snapshot(p)
}

func (l *lobby) snapshot(p *lobbyPeer) lobbyState {
	view := lobbyState{Self: p.name, Phase: lobbyIdle, Members: []lobbyMember{}, Story: []lobbyLine{}}
	if r := p.room; r != nil {
		view.Room, view.Topic, view.Phase = r.id, r.topic, r.phase
		if r.phase == lobbyWaiting && !r.matchExpired {
			view.MatchDeadline = r.matchDeadline
		}
		view.CanEnd = r.peers[0] == p && r.phase != lobbyEnded
		for _, peer := range r.peers {
			view.Members = append(view.Members, lobbyMember{Name: peer.name, Self: peer == p, Audio: peer.send != nil})
		}
		view.Story = append(view.Story, r.story...)
		if r.historical != "" {
			view.Speaker = r.historical
			view.WaitingOn = r.historical
		} else if r.speaker != nil {
			view.Speaker = r.speaker.name
			view.CanSpeak = r.speaker == p && r.await == nil && r.turn == nil
			view.Recording = r.speaker == p && r.turn != nil
		}
		if r.await != nil {
			view.WaitingOn = r.await.peer.name
		}
	}
	return view
}

func (l *lobby) broadcast(r *lobbyRoom) {
	for _, p := range r.peers {
		l.sendState(p)
	}
}

func (l *lobby) sendState(p *lobbyPeer) {
	if p.send != nil {
		view := l.snapshot(p)
		p.send(lobbyOutbound{message: lobbyMessage{Type: lobbyEventState, State: &view}})
	}
}

func lobbySentence(text string) (string, error) {
	text = strings.TrimSpace(text)
	if utf8.RuneCountInString(text) > maxSentence {
		return "", fmt.Errorf("一句故事不能超过 %d 字", maxSentence)
	}
	return text, nil
}
