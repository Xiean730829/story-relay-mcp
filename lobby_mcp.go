package main

import (
	"crypto/rand"
	"encoding/json"
	"errors"
	"strings"
)

const (
	toolLobbyJoin   = "self.lobby.join"
	toolLobbyState  = "self.lobby.status"
	toolLobbySpeak  = "self.lobby.speak"
	toolLobbyLeave  = "self.lobby.leave"
	fieldTicket     = "ticket"
	fieldLobbyTopic = "topic"
	fieldLobbyName  = "box_name"
	fieldLobbyText  = "text"
)

type lobbyToolRequest struct {
	Ticket string `json:"ticket"`
	Topic  string `json:"topic"`
	Name   string `json:"box_name"`
	Text   string `json:"text"`
}

type lobbyToolResponse struct {
	Ticket string     `json:"ticket,omitempty"`
	View   lobbyState `json:"game"`
	Media  string     `json:"media"`
}

func lobbyTools() []toolDescription {
	ticket := schemaProperty{Type: schemaString, MinLength: 1, MaxLength: 64}
	return []toolDescription{
		{
			Name: toolLobbyJoin, Description: "孩子想和在线玩家玩故事接龙时加入三分钟匹配。返回 ticket，后续查询和发言必须使用自己的 ticket。",
			InputSchema: inputSchema{Type: schemaObject, Properties: map[string]schemaProperty{
				fieldTicket: ticket, fieldLobbyTopic: {Type: schemaString, MaxLength: maxLobbyTopic},
				fieldLobbyName: {Type: schemaString, MaxLength: maxLobbyName},
			}, Required: []string{}},
		},
		{
			Name: toolLobbyState, Description: "查询这个盒子的匹配、人员和故事进度，不推进回合。",
			InputSchema: inputSchema{Type: schemaObject, Properties: map[string]schemaProperty{
				fieldTicket: ticket,
			}, Required: []string{fieldTicket}},
		},
		{
			Name: toolLobbySpeak, Description: "轮到本设备时提交孩子的故事转写。当前 MCP 路径仅传文字。",
			InputSchema: inputSchema{Type: schemaObject, Properties: map[string]schemaProperty{
				fieldTicket: ticket, fieldLobbyText: {Type: schemaString, MinLength: 1, MaxLength: maxSentence},
			}, Required: []string{fieldTicket, fieldLobbyText}},
		},
		{
			Name: toolLobbyLeave, Description: "孩子说不玩了或要换游戏时，退出本设备的故事接龙。",
			InputSchema: inputSchema{Type: schemaObject, Properties: map[string]schemaProperty{
				fieldTicket: ticket,
			}, Required: []string{fieldTicket}},
		},
	}
}

func (l *lobby) callTool(name string, args json.RawMessage) mcpToolResult {
	var request lobbyToolRequest
	if len(args) != 0 {
		if err := json.Unmarshal(args, &request); err != nil {
			return toolText("大厅工具参数格式不正确", true)
		}
	}
	request.Ticket = strings.TrimSpace(request.Ticket)
	view, ticket, err := l.runTool(name, request)
	if err != nil {
		return toolText(err.Error(), true)
	}
	data, err := json.Marshal(lobbyToolResponse{Ticket: ticket, View: view, Media: "transcript_only"})
	if err != nil {
		return toolText("暂时无法返回大厅状态", true)
	}
	return toolText(string(data), false)
}

func (l *lobby) runTool(name string, request lobbyToolRequest) (lobbyState, string, error) {
	l.mu.Lock()
	p := l.tickets[request.Ticket]
	l.mu.Unlock()
	if name == toolLobbyJoin && request.Ticket == "" {
		p = &lobbyPeer{id: rand.Text()}
		view, err := l.join(p, lobbyJoin{Name: request.Name, Topic: request.Topic})
		if err != nil {
			return lobbyState{}, "", err
		}
		l.mu.Lock()
		l.tickets[p.id] = p
		l.mu.Unlock()
		return view, p.id, nil
	}
	if p == nil {
		return lobbyState{}, "", errors.New("本盒子还没加入大厅，请先匹配")
	}
	switch name {
	case toolLobbyJoin:
		view, err := l.join(p, lobbyJoin{Name: request.Name, Topic: request.Topic})
		return view, p.id, err
	case toolLobbyState:
		return l.state(p), "", nil
	case toolLobbySpeak:
		view, err := l.sayText(p, request.Text)
		return view, "", err
	case toolLobbyLeave:
		view, err := l.leave(p, false)
		l.mu.Lock()
		delete(l.tickets, p.id)
		l.mu.Unlock()
		return view, "", err
	default:
		return lobbyState{}, "", errors.New("未知的大厅工具")
	}
}
