package main

import (
	"fmt"
	"strings"
	"unicode/utf8"
)

const (
	toolStart     = "self.story.start"
	toolContinue  = "self.story.continue"
	toolEnd       = "self.story.end"
	storyReady    = "ready"
	storyPlaying  = "playing"
	storyEnded    = "ended"
	roleChild     = "child"
	roleAI        = "ai"
	reasonChild   = "child_stop"
	reasonAI      = "ai_finish"
	reasonSilent  = "silence"
	maxSentence   = 400
	maxTitle      = 40
	maxLines      = 100
	fieldTitle    = "title"
	fieldChild    = "child_sentence"
	fieldAI       = "ai_sentence"
	fieldEnding   = "ending"
	fieldReason   = "reason"
	schemaObject  = "object"
	schemaString  = "string"
	schemaBoolean = "boolean"
	schemaArray   = "array"
)

type storyLine struct {
	Role string `json:"role"`
	Text string `json:"text"`
}

type story struct {
	ID     int         `json:"id"`
	Title  string      `json:"title"`
	Status string      `json:"status"`
	Reason string      `json:"reason,omitempty"`
	Lines  []storyLine `json:"lines"`
}

type toolArguments struct {
	Title   string `json:"title"`
	Child   string `json:"child_sentence"`
	AI      string `json:"ai_sentence"`
	End     string `json:"ending"`
	Why     string `json:"reason"`
	StoryID string `json:"story_id,omitempty"`
	Slot    string `json:"session_key,omitempty"`
}

type toolCallParams struct {
	Name      string        `json:"name"`
	Arguments toolArguments `json:"arguments"`
}

type toolDescription struct {
	Name        string      `json:"name"`
	Description string      `json:"description"`
	InputSchema inputSchema `json:"inputSchema"`
}

type inputSchema struct {
	Type       string                    `json:"type"`
	Properties map[string]schemaProperty `json:"properties"`
	Required   []string                  `json:"required"`
	Additional bool                      `json:"additionalProperties"`
}

type schemaProperty struct {
	Type      string          `json:"type"`
	MinLength int             `json:"minLength,omitempty"`
	MaxLength int             `json:"maxLength,omitempty"`
	Enum      []string        `json:"enum,omitempty"`
	Items     *schemaProperty `json:"items,omitempty"`
	MaxItems  int             `json:"maxItems,omitempty"`
}

func storyTools() []toolDescription {
	sentence := schemaProperty{Type: schemaString, MinLength: 1, MaxLength: maxSentence}
	return []toolDescription{
		{
			Name:        toolStart,
			Description: "开始或更换故事接龙。保存孩子的第一句（可为空）和AI第一句；成功后只说ai_sentence。",
			InputSchema: inputSchema{Type: schemaObject, Properties: map[string]schemaProperty{
				fieldTitle: {Type: schemaString, MinLength: 1, MaxLength: maxTitle},
				fieldChild: {Type: schemaString, MaxLength: maxSentence}, fieldAI: sentence,
			}, Required: []string{fieldTitle, fieldAI}},
		},
		{
			Name:        toolContinue,
			Description: "孩子接了一句后，记录孩子这一句和AI接的一句。必须等待孩子发言，不能自行续写多轮。成功后只说ai_sentence。",
			InputSchema: inputSchema{Type: schemaObject, Properties: map[string]schemaProperty{
				fieldChild: sentence, fieldAI: sentence,
			}, Required: []string{fieldChild, fieldAI}},
		},
		{
			Name:        toolEnd,
			Description: "孩子要求停止或故事有自然结局时结束接龙。ending是最后一句，reason为child_stop或ai_finish。成功后只说ending。",
			InputSchema: inputSchema{Type: schemaObject, Properties: map[string]schemaProperty{
				fieldEnding: sentence, fieldChild: {Type: schemaString, MaxLength: maxSentence},
				fieldReason: {Type: schemaString, Enum: []string{reasonChild, reasonAI}},
			}, Required: []string{fieldEnding, fieldReason}},
		},
	}
}

func (g *story) call(name string, a toolArguments) (string, error) {
	a.Title = strings.TrimSpace(a.Title)
	a.Child = strings.TrimSpace(a.Child)
	a.AI = strings.TrimSpace(a.AI)
	a.End = strings.TrimSpace(a.End)
	for _, text := range []string{a.Child, a.AI, a.End} {
		if utf8.RuneCountInString(text) > maxSentence {
			return "", fmt.Errorf("每次只接一句，不能超过%d字", maxSentence)
		}
	}
	switch name {
	case toolStart:
		if a.Title == "" || utf8.RuneCountInString(a.Title) > maxTitle || a.AI == "" {
			return "", fmt.Errorf("请提供简短的故事标题和AI第一句")
		}
		*g = story{ID: g.ID + 1, Title: a.Title, Status: storyPlaying, Lines: []storyLine{}}
		if a.Child != "" {
			g.Lines = append(g.Lines, storyLine{Role: roleChild, Text: a.Child})
		}
		g.Lines = append(g.Lines, storyLine{Role: roleAI, Text: a.AI})
		return a.AI, nil
	case toolContinue:
		if g.Status != storyPlaying {
			return "", fmt.Errorf("当前没有进行中的故事，请先调用开始故事工具")
		}
		if a.Child == "" || a.AI == "" {
			return "", fmt.Errorf("请同时提供孩子刚说的一句和AI接的一句")
		}
		if len(g.Lines) >= maxLines {
			return "", fmt.Errorf("故事已经很长了，请用结束工具给它一个温暖的结局")
		}
		g.Lines = append(g.Lines, storyLine{Role: roleChild, Text: a.Child}, storyLine{Role: roleAI, Text: a.AI})
		return a.AI, nil
	case toolEnd:
		if a.End == "" || (a.Why != reasonChild && a.Why != reasonAI) {
			return "", fmt.Errorf("请提供结束语和正确的结束原因")
		}
		if g.Status == storyEnded {
			return "", fmt.Errorf("这个故事已经结束，请等待孩子开始新故事")
		}
		g.Status, g.Reason = storyEnded, a.Why
		if a.Child != "" && a.Why == reasonAI {
			g.Lines = append(g.Lines, storyLine{Role: roleChild, Text: a.Child})
		}
		g.Lines = append(g.Lines, storyLine{Role: roleAI, Text: a.End})
		return a.End, nil
	default:
		return "", fmt.Errorf("没有这个故事工具")
	}
}
