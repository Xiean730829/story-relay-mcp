package main

import (
	"encoding/json"
	"testing"
)

func TestPrivateMCPMatchesThreeLobbyPlayers(t *testing.T) {
	t.Parallel()
	pool, err := openLobbyPool(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	proxy := &privateProxy{lobby: newLobby(pool)}
	call := func(name string, args lobbyToolRequest) lobbyToolResponse {
		t.Helper()
		params, err := json.Marshal(struct {
			Name string           `json:"name"`
			Args lobbyToolRequest `json:"arguments"`
		}{Name: name, Args: args})
		if err != nil {
			t.Fatal(err)
		}
		reply := proxy.call(t.Context(), rpcMessage{Params: params})
		var result lobbyToolResponse
		if reply.IsError || len(reply.Content) != 1 {
			t.Fatalf("MCP %s failed: %+v", name, reply)
		}
		if err := json.Unmarshal([]byte(reply.Content[0].Text), &result); err != nil {
			t.Fatal(err)
		}
		return result
	}
	first := call(toolLobbyJoin, lobbyToolRequest{Name: "A"})
	if first.Ticket == "" || first.View.Phase != lobbyWaiting || first.Media != "transcript_only" {
		t.Fatalf("invalid first MCP join: %+v", first)
	}
	second := call(toolLobbyJoin, lobbyToolRequest{Name: "B"})
	proxy.lobby.mu.Lock()
	room := proxy.lobby.tickets[first.Ticket].room
	proxy.lobby.mu.Unlock()
	proxy.lobby.finishMatching(room)
	call(toolLobbySpeak, lobbyToolRequest{Ticket: first.Ticket, Text: "一只小熊走进了森林。"})
	third := call(toolLobbyJoin, lobbyToolRequest{Name: "C"})
	sameRoom := first.View.Room == second.View.Room && second.View.Room == third.View.Room
	if !sameRoom || len(third.View.Members) != 3 || len(third.View.Story) != 1 {
		t.Fatalf("MCP late join did not share the room: %+v", third)
	}
	call(toolLobbySpeak, lobbyToolRequest{Ticket: second.Ticket, Text: "然后它看到了一条小路。"})
	if view := call(toolLobbyState, lobbyToolRequest{Ticket: third.Ticket}).View; !view.CanSpeak {
		t.Fatalf("C did not get the floor: %+v", view)
	}
	if view := call(toolLobbyLeave, lobbyToolRequest{Ticket: third.Ticket}).View; view.Phase != lobbyIdle {
		t.Fatalf("C did not leave: %+v", view)
	}
}
