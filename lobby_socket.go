package main

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

const (
	lobbyEventState     = "lobby.state"
	lobbyEventNotice    = "lobby.notice"
	lobbyEventAccepted  = "turn.accepted"
	lobbyEventPlayStart = "playback.start"
	lobbyEventPlayEnd   = "playback.end"
	lobbyEventPlayStop  = "playback.stop"
	lobbyCommandJoin    = "lobby.join"
	lobbyCommandReuse   = "lobby.reuse"
	lobbyCommandText    = "turn.text"
	lobbyCommandStart   = "turn.start"
	lobbyCommandFinish  = "turn.finish"
	lobbyCommandResult  = "playback.result"
	lobbyCommandLeave   = "lobby.leave"
	lobbyCommandEnd     = "lobby.end"
	lobbySocketQueue    = 128
)

type lobbyCommand struct {
	Type       string             `json:"type"`
	Name       string             `json:"name"`
	Topic      string             `json:"topic"`
	Text       string             `json:"text"`
	Reuse      bool               `json:"reuse"`
	Share      bool               `json:"share"`
	PlaybackID string             `json:"playback_id"`
	Result     lobbyPlaybackState `json:"result"`
}

func (l *lobby) socket(upgrader websocket.Upgrader, w http.ResponseWriter, r *http.Request) {
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		slog.DebugContext(r.Context(), "lobby browser upgrade failed", "error", err)
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
					slog.DebugContext(ctx, "write lobby browser event", "error", err)
					return
				}
			}
		}
	})
	p := &lobbyPeer{id: rand.Text(), send: func(out lobbyOutbound) {
		select {
		case <-ctx.Done():
		case queue <- out:
		default:
			// Slow receivers are disconnected independently of the speaker and other listeners.
			cancel()
		}
	}}
	defer func() {
		cancel()
		closeWS(conn)
		stopClose()
		writer.Wait()
		if _, err := l.leave(p, false); err != nil {
			slog.Warn("remove disconnected lobby player", "error", err)
		}
	}()
	conn.SetReadLimit(readLimit)
	for {
		kind, data, err := conn.ReadMessage()
		if err != nil {
			return
		}
		if kind == websocket.BinaryMessage {
			err = l.audio(p, data)
		} else {
			var command lobbyCommand
			if err = json.Unmarshal(data, &command); err != nil {
				err = errors.New("大厅指令的格式不正确")
			} else {
				err = l.onCommand(p, command)
			}
		}
		if err != nil {
			slog.DebugContext(ctx, "lobby command rejected", "error", err)
			p.send(lobbyOutbound{message: lobbyMessage{Type: eventError, Text: "操作没有完成：" + err.Error()}})
		}
	}
}

func (l *lobby) onCommand(p *lobbyPeer, command lobbyCommand) error {
	switch command.Type {
	case lobbyCommandJoin:
		_, err := l.join(p, lobbyJoin{Name: command.Name, Topic: command.Topic, Reuse: command.Reuse})
		return err
	case lobbyCommandReuse:
		return l.reuse(p)
	case lobbyCommandStart:
		return l.startVoice(p)
	case lobbyCommandFinish:
		if err := l.finishVoice(p, command.Text, command.Share); err != nil {
			return err
		}
		message := "这句已接进故事。"
		if command.Share {
			message = "这句已接进故事，原声已存入复用池，以后可以陪别的伙伴接龙。"
		}
		p.send(lobbyOutbound{message: lobbyMessage{Type: lobbyEventAccepted, Text: message}})
		return nil
	case lobbyCommandText:
		_, err := l.sayText(p, command.Text)
		return err
	case lobbyCommandResult:
		switch command.Result {
		case lobbyPlaybackFinished, lobbyPlaybackStopped, lobbyPlaybackFailed:
			l.playbackResult(p, command.PlaybackID, command.Result)
			return nil
		default:
			return errors.New("未知的播放回执")
		}
	case lobbyEventPlayStop:
		l.stopPlayback(p, command.PlaybackID)
		return nil
	case lobbyCommandLeave, lobbyCommandEnd:
		_, err := l.leave(p, command.Type == lobbyCommandEnd)
		return err
	default:
		return errors.New("不支持这条大厅指令")
	}
}

func writeLobbyEvent(conn *websocket.Conn, event lobbyOutbound) error {
	if len(event.frame) == 0 {
		return writeJSON(conn, event.message)
	}
	if err := conn.SetWriteDeadline(time.Now().Add(writeTimeout)); err != nil {
		return fmt.Errorf("set output frame deadline: %w", err)
	}
	if err := conn.WriteMessage(websocket.BinaryMessage, event.frame); err != nil {
		return fmt.Errorf("send output frame: %w", err)
	}
	return nil
}
