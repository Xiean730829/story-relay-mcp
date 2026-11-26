package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

type mockTestClient struct {
	t    *testing.T
	conn *websocket.Conn
}

func newMockTestClient(t *testing.T, base, name string) *mockTestClient {
	t.Helper()
	header := http.Header{"Origin": []string{base}}
	address := "ws" + strings.TrimPrefix(base, "http") + "/mock-linx/ws"
	conn, _, err := websocket.DefaultDialer.Dial(address, header)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	client := &mockTestClient{t: t, conn: conn}
	client.send(mockCommand{Type: mockHello, Name: name})
	client.until(func(msg lobbyMessage) bool { return msg.Type == mockReady })
	return client
}

func (c *mockTestClient) send(command mockCommand) {
	c.t.Helper()
	if err := c.conn.WriteJSON(command); err != nil {
		c.t.Fatal(err)
	}
}

func (c *mockTestClient) until(match func(lobbyMessage) bool) (lobbyMessage, []byte) {
	c.t.Helper()
	if err := c.conn.SetReadDeadline(time.Now().Add(4 * time.Second)); err != nil {
		c.t.Fatal(err)
	}
	var audio []byte
	for {
		kind, data, err := c.conn.ReadMessage()
		if err != nil {
			c.t.Fatal(err)
		}
		if kind == websocket.BinaryMessage {
			audio = append(audio, data...)
			continue
		}
		var msg lobbyMessage
		if err := json.Unmarshal(data, &msg); err != nil {
			c.t.Fatal(err)
		}
		if msg.Type == eventError {
			c.t.Fatalf("mock partner error: %s", msg.Text)
		}
		if match(msg) {
			return msg, audio
		}
	}
}

func TestMockLinxThreePlayersAndHistoricalAudio(t *testing.T) {
	t.Parallel()
	pool, err := openLobbyPool(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	lobby := newLobby(pool)
	upgrader := websocket.Upgrader{CheckOrigin: func(r *http.Request) bool {
		return r.Header.Get("Origin") == "http://"+r.Host
	}}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		lobby.mockLinxSocket(upgrader, w, r)
	}))
	t.Cleanup(server.Close)
	a := newMockTestClient(t, server.URL, "A")
	b := newMockTestClient(t, server.URL, "B")
	c := newMockTestClient(t, server.URL, "C")
	for _, client := range []*mockTestClient{a, b, c} {
		client.send(mockCommand{Type: mockListen, Text: "我要玩游戏"})
		client.until(func(msg lobbyMessage) bool { return msg.Type == mockReply })
		client.send(mockCommand{Type: mockListen, Text: "我要玩故事接龙", Topic: mockDefault})
	}
	a.until(func(msg lobbyMessage) bool {
		return msg.State != nil && msg.State.Phase == lobbyPlaying && msg.State.CanSpeak
	})
	a.send(mockCommand{Type: mockStart})
	a.until(func(msg lobbyMessage) bool { return msg.State != nil && msg.State.Recording })
	first := bytes.Repeat([]byte{0x12, 0x34}, pcmFrameBytes/2)
	if err := a.conn.WriteMessage(websocket.BinaryMessage, first); err != nil {
		t.Fatal(err)
	}
	a.send(mockCommand{Type: mockFinish, Text: "兔子发现一座城堡", Share: true})
	a.until(func(msg lobbyMessage) bool { return msg.Type == lobbyEventAccepted })
	play, heard := b.until(func(msg lobbyMessage) bool { return msg.Type == lobbyEventPlayEnd })
	if !bytes.Equal(heard, first) || play.Play == nil {
		t.Fatal("B did not receive A's exact audio")
	}
	b.send(mockCommand{Type: mockResult, PlaybackID: play.Play.ID, Result: string(lobbyPlaybackFinished)})
	play, heard = c.until(func(msg lobbyMessage) bool { return msg.Type == lobbyEventPlayEnd })
	if !bytes.Equal(heard, first) || play.Play == nil {
		t.Fatal("C did not receive A's exact audio")
	}
	c.send(mockCommand{Type: mockResult, PlaybackID: play.Play.ID, Result: string(lobbyPlaybackFinished)})
	b.until(func(msg lobbyMessage) bool { return msg.State != nil && msg.State.CanSpeak })
	b.send(mockCommand{Type: mockStart})
	b.until(func(msg lobbyMessage) bool { return msg.State != nil && msg.State.Recording })
	second := bytes.Repeat([]byte{0x56, 0x78}, pcmFrameBytes/2)
	if err := b.conn.WriteMessage(websocket.BinaryMessage, second); err != nil {
		t.Fatal(err)
	}
	b.send(mockCommand{Type: mockFinish, Text: "城堡里有一盏灯", Share: true})
	b.until(func(msg lobbyMessage) bool { return msg.Type == lobbyEventAccepted })
	play, heard = a.until(func(msg lobbyMessage) bool { return msg.Type == lobbyEventPlayEnd })
	if !bytes.Equal(heard, second) || play.Play == nil {
		t.Fatal("A did not receive B's exact audio")
	}
	a.send(mockCommand{Type: mockResult, PlaybackID: play.Play.ID, Result: string(lobbyPlaybackFinished)})
	play, heard = c.until(func(msg lobbyMessage) bool { return msg.Type == lobbyEventPlayEnd })
	if !bytes.Equal(heard, second) || play.Play == nil {
		t.Fatal("C did not receive B's exact audio")
	}
	c.send(mockCommand{Type: mockResult, PlaybackID: play.Play.ID, Result: string(lobbyPlaybackFinished)})
	c.until(func(msg lobbyMessage) bool { return msg.State != nil && msg.State.CanSpeak })
	a.send(mockCommand{Type: mockEnd})
	a.until(func(msg lobbyMessage) bool { return msg.State != nil && msg.State.Phase == lobbyEnded })
	b.send(mockCommand{Type: mockListen, Text: "我要玩故事接龙", Topic: mockDefault})
	b.until(func(msg lobbyMessage) bool { return msg.State != nil && msg.State.Phase == lobbyWaiting })
	b.send(mockCommand{Type: mockReuse})
	play, heard = b.until(func(msg lobbyMessage) bool { return msg.Type == lobbyEventPlayEnd })
	if play.Play == nil || !bytes.Equal(heard, first) {
		t.Fatal("a later game did not receive A's retained audio")
	}
	b.send(mockCommand{Type: mockResult, PlaybackID: play.Play.ID, Result: string(lobbyPlaybackFinished)})
	b.until(func(msg lobbyMessage) bool { return msg.State != nil && msg.State.CanSpeak })
}
