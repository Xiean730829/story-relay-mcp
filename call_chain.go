package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

const (
	callChainGame       = "story-relay"
	callChainTheme      = "forest-adventure"
	callChainPlayer     = "phone-demo"
	callChainPlayerName = "电话体验者"
	callChainDefaultURL = "http://127.0.0.1:18789"
)

type callChain struct {
	base string
	http *http.Client
}

type callChainDetail struct {
	Story struct {
		ID      string `json:"id"`
		Version int    `json:"version"`
	} `json:"story"`
	Segments []struct {
		ClipID string `json:"clip_id"`
		Text   string `json:"text"`
	} `json:"segments"`
}

func (c *callChain) post(ctx context.Context, path string, body any, out any) error {
	data, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(c.base, "/")+path, bytes.NewReader(data))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("connect story platform: %w", err)
	}
	defer closeBody(resp.Body)
	var envelope struct {
		Code    int             `json:"code"`
		Message string          `json:"msg"`
		Data    json.RawMessage `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&envelope); err != nil {
		return fmt.Errorf("decode story platform response: %w", err)
	}
	if resp.StatusCode != http.StatusOK || envelope.Code != 0 {
		return fmt.Errorf("story platform error %d: %s", envelope.Code, envelope.Message)
	}
	if out != nil {
		return json.Unmarshal(envelope.Data, out)
	}
	return nil
}

func callChainThemeID(title string) string {
	if title == defaultCallStoryTitle {
		return callChainTheme
	}
	sum := sha256.Sum256([]byte(normalizedCallText(title)))
	return fmt.Sprintf("story-%x", sum[:12])
}

func (c *callChain) ensureTheme(ctx context.Context, title, opening string) (string, error) {
	id := callChainThemeID(title)
	err := c.post(ctx, "/v1/story/theme", map[string]string{
		"id": id, "game_id": callChainGame, "title": title, "background": opening, "opening_text": opening,
	}, nil)
	return id, err
}

func (c *callChain) match(ctx context.Context, themeID string) (*callChainDetail, error) {
	var result struct {
		Matched bool            `json:"matched"`
		Story   callChainDetail `json:"story"`
	}
	err := c.post(ctx, "/v1/story/match", map[string]any{"game_id": callChainGame, "theme_id": themeID, "player_id": callChainPlayer, "allow_self": true}, &result)
	if err != nil {
		return nil, err
	}
	if !result.Matched {
		return nil, nil
	}
	return &result.Story, nil
}

func (c *callChain) claim(ctx context.Context, storyID string, version int) error {
	return c.post(ctx, "/v1/story/claim", map[string]any{"story_id": storyID, "player_id": callChainPlayer, "expect_version": version}, nil)
}

func (c *callChain) release(ctx context.Context, storyID string) error {
	return c.post(ctx, "/v1/story/release", map[string]string{"story_id": storyID, "player_id": callChainPlayer}, nil)
}

func (c *callChain) commit(ctx context.Context, voice callVoice) error {
	input := map[string]any{
		"player_id": callChainPlayer, "player_name": callChainPlayerName,
		"clip_id": voice.Turn, "text": voice.Text, "allow_self": true,
	}
	path := "/v1/story/create"
	if voice.ChainID == "" {
		input["theme_id"] = voice.ChainTheme
	} else {
		path = "/v1/story/append"
		input["story_id"] = voice.ChainID
		input["expect_version"] = voice.ChainVersion
	}
	return c.post(ctx, path, input, nil)
}

func newCallChain(address string) *callChain {
	return &callChain{base: address, http: &http.Client{Timeout: 15 * time.Second}}
}
