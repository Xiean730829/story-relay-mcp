package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"unicode"
)

const (
	callHistoryUser     = 1
	callHistoryTool     = 3
	callSignPath        = "/v1/objects/signed-url"
	callHistoryPageSize = 20
)

type callHistory struct {
	api   platformAPI
	agent string
}

type callHistoryRow struct {
	Type  int    `json:"chatType"`
	Text  string `json:"content"`
	Audio string `json:"audioUrl"`
	Tool  string `json:"toolInfo"`
}

func (h callHistory) find(ctx context.Context, ref callVoice) (string, error) {
	base := agentPath + "/" + url.PathEscape(h.agent)
	for page := 1; ; page++ {
		var sessions struct {
			Total int `json:"total"`
			List  []struct {
				ID string `json:"sessionId"`
			} `json:"list"`
		}
		err := h.api.request(
			ctx,
			http.MethodGet,
			fmt.Sprintf("%s/sessions?page=%d&limit=%d", base, page, callHistoryPageSize),
			nil,
			&sessions,
		)
		if err != nil {
			return "", fmt.Errorf("读取智能体通话会话: %w", err)
		}
		for _, session := range sessions.List {
			var rows []callHistoryRow
			err := h.api.request(
				ctx,
				http.MethodGet,
				base+"/chat-history/"+url.PathEscape(session.ID),
				nil,
				&rows,
			)
			if err != nil {
				return "", fmt.Errorf("读取通话回合录音: %w", err)
			}
			if audio := findCallVoice(rows, ref); audio != "" {
				return audio, nil
			}
		}
		if len(sessions.List) < callHistoryPageSize || (sessions.Total > 0 && page*callHistoryPageSize >= sessions.Total) {
			return "", nil
		}
	}
}

func (h callHistory) signed(ctx context.Context, source string) (string, error) {
	var reply struct {
		URL string `json:"signed_url"`
	}
	err := h.api.request(
		ctx,
		http.MethodPost,
		callSignPath,
		struct {
			URL string `json:"url"`
		}{URL: source},
		&reply,
	)
	if err != nil {
		return "", fmt.Errorf("获取原声播放授权: %w", err)
	}
	address, err := url.Parse(reply.URL)
	if err != nil || address.Scheme != "https" || address.Host == "" {
		return "", fmt.Errorf("灵矽没有返回有效的 HTTPS 音频地址")
	}
	return reply.URL, nil
}

func (h callHistory) checkMP3(ctx context.Context, address string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, address, nil)
	if err != nil {
		return fmt.Errorf("build audio range request")
	}
	req.Header.Set("Range", "bytes=0-2")
	resp, err := h.api.http.Do(req)
	if err != nil {
		return fmt.Errorf("request signed audio bytes")
	}
	defer closeBody(resp.Body)
	if resp.StatusCode != http.StatusPartialContent && resp.StatusCode != http.StatusOK {
		return fmt.Errorf("signed audio returned HTTP %d", resp.StatusCode)
	}
	var header [3]byte
	if _, err := io.ReadFull(resp.Body, header[:]); err != nil {
		return fmt.Errorf("read signed audio header")
	}
	isFrame := header[0] == 0xff && header[1]&0xe0 == 0xe0
	if string(header[:]) != "ID3" && !isFrame {
		return fmt.Errorf("signed object does not start with an MP3 header")
	}
	return nil
}

func findCallVoice(rows []callHistoryRow, ref callVoice) string {
	expected := normalizedCallText(ref.Text)
	if expected == "" || ref.Turn == "" {
		return ""
	}
	marker := ref.Turn
	if ref.Marker != "" {
		marker = ref.Marker
	}
	var found string
	for i, row := range rows {
		if row.Type != callHistoryTool || !hasCallVoiceMarker(row.Tool, marker) {
			continue
		}
		// History may commit the input audio on either side of its tool record.
		// Compare only the nearest user utterance on each side of the marker.
		left := nearestCallVoice(rows, i, -1, expected)
		right := nearestCallVoice(rows, i, 1, expected)
		switch {
		case left == "" && right == "":
			continue
		case left == "":
			left = right
		case right == "" || left == right:
		default:
			return ""
		}
		if found != "" && found != left {
			return ""
		}
		found = left
	}
	return found
}

func hasCallVoiceMarker(tool, turn string) bool {
	key := `"` + turn + `"`
	toolName := strings.ReplaceAll(tool, "_", ".")
	isRequest := strings.Contains(toolName, toolCallTurn) && strings.Contains(tool, key)
	isReceipt := strings.HasPrefix(tool, "result:") && strings.Contains(tool, `"capture_key":`+key)
	return isRequest || isReceipt
}

func nearestCallVoice(rows []callHistoryRow, anchor, direction int, expected string) string {
	for i := anchor + direction; i >= 0 && i < len(rows); i += direction {
		row := rows[i]
		if row.Type == callHistoryUser {
			if normalizedCallText(row.Text) == expected {
				return row.Audio
			}
			return ""
		}
		if row.Type == callHistoryTool && strings.Contains(strings.ReplaceAll(row.Tool, "_", "."), toolCallTurn) {
			break
		}
	}
	return ""
}

func normalizedCallText(text string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsSpace(r) || unicode.IsPunct(r) {
			return -1
		}
		return unicode.ToLower(r)
	}, text)
}
