package main

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"
	"sync"

	"github.com/gorilla/websocket"
)

// This is a local test adapter. Text supplied by the simulator stands in for
// ASR and intent recognition; it is not the real Linx device protocol.
const (
	mockHello   = "mock.hello"
	mockListen  = "mock.listen"
	mockStart   = "mock.record.start"
	mockFinish  = "mock.record.finish"
	mockReuse   = "mock.history"
	mockLeave   = "mock.leave"
	mockEnd     = "mock.end"
	mockResult  = "mock.playback.result"
	mockReady   = "mock.ready"
	mockReply   = "mock.buddy"
	mockDefault = "森林冒险"
)

type mockCommand struct {
	Type       string `json:"type"`
	Name       string `json:"name"`
	Text       string `json:"text"`
	Topic      string `json:"topic"`
	Share      bool   `json:"share"`
	PlaybackID string `json:"playback_id"`
	Result     string `json:"result"`
}

func (l *lobby) mockLinxSocket(upgrader websocket.Upgrader, w http.ResponseWriter, r *http.Request) {
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		slog.DebugContext(r.Context(), "mock Linx upgrade failed", "error", err)
		return
	}
	ctx, cancel := context.WithCancel(r.Context())
	stopClose := context.AfterFunc(ctx, func() { closeWS(conn) })
	queue := make(chan lobbyOutbound, lobbySocketQueue)
	var writer sync.WaitGroup
	writer.Go(func() {
		defer cancel()
		for {
			select {
			case <-ctx.Done():
				return
			case event := <-queue:
				if err := writeLobbyEvent(conn, event); err != nil {
					return
				}
			}
		}
	})
	peer := &lobbyPeer{id: rand.Text(), send: func(event lobbyOutbound) {
		select {
		case <-ctx.Done():
		case queue <- event:
		default:
			cancel()
		}
	}}
	defer func() {
		cancel()
		closeWS(conn)
		stopClose()
		writer.Wait()
		if _, err := l.leave(peer, false); err != nil {
			slog.WarnContext(ctx, "remove mock Linx player", "error", err)
		}
	}()
	conn.SetReadLimit(readLimit)
	for {
		kind, data, err := conn.ReadMessage()
		if err != nil {
			return
		}
		if kind == websocket.BinaryMessage {
			err = l.audio(peer, data)
		} else {
			var command mockCommand
			if err = json.Unmarshal(data, &command); err == nil {
				err = l.onMockCommand(peer, command)
			}
		}
		if err != nil {
			peer.send(lobbyOutbound{message: lobbyMessage{Type: eventError, Text: err.Error()}})
		}
	}
}

func (l *lobby) onMockCommand(peer *lobbyPeer, command mockCommand) error {
	switch command.Type {
	case mockHello:
		l.mu.Lock()
		peer.name = strings.TrimSpace(command.Name)
		if peer.name == "" {
			peer.name = "模拟盒子"
		}
		l.mu.Unlock()
		peer.send(lobbyOutbound{message: lobbyMessage{Type: mockReady, Text: "模拟伙伴已连接"}})
	case mockListen:
		text := strings.TrimSpace(command.Text)
		switch {
		case strings.Contains(text, "退出") || strings.Contains(text, "不玩"):
			_, err := l.leave(peer, false)
			return err
		case strings.Contains(text, "以前") || strings.Contains(text, "历史"):
			return l.reuse(peer)
		case strings.Contains(text, "故事接龙"):
			topic := strings.TrimSpace(command.Topic)
			if topic == "" {
				topic = mockDefault
			}
			_, err := l.join(peer, lobbyJoin{Name: peer.name, Topic: topic})
			return err
		case strings.Contains(text, "游戏"):
			peer.send(lobbyOutbound{message: lobbyMessage{Type: mockReply, Text: "欢迎来到游戏大厅，你想玩什么？"}})
		default:
			peer.send(lobbyOutbound{message: lobbyMessage{Type: mockReply, Text: "可以说‘我要玩故事接龙’。"}})
		}
	case mockStart:
		return l.startVoice(peer)
	case mockFinish:
		if err := l.finishVoice(peer, command.Text, command.Share); err != nil {
			return err
		}
		peer.send(lobbyOutbound{message: lobbyMessage{Type: lobbyEventAccepted, Text: "这一句已接入游戏。"}})
	case mockReuse:
		return l.reuse(peer)
	case mockLeave:
		_, err := l.leave(peer, false)
		return err
	case mockEnd:
		_, err := l.leave(peer, true)
		return err
	case mockResult:
		state := lobbyPlaybackState(command.Result)
		switch state {
		case lobbyPlaybackFinished, lobbyPlaybackStopped, lobbyPlaybackFailed:
			l.playbackResult(peer, command.PlaybackID, state)
		}
	}
	return nil
}
