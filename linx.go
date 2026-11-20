package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/gorilla/websocket"
)

const (
	agentName       = "故事接龙·本地演示"
	intentFunction  = "Intent_function_call"
	memoryNone      = "Memory_nomem"
	agentPath       = "/xiaozhi/agent"
	preregisterPath = "/xiaozhi/device/preregister"
	protocolVersion = "2024-11-05"
	appVersion      = "0.1.0"
	formatPCM       = "pcm"
	uplinkRate      = 16000
	frameDuration   = 20
	bitsPerSample   = 16
	headerAuth      = "Authorization"
	headerDevice    = "Device-Id"
	headerClient    = "Client-Id"
	headerProtocol  = "Protocol-Version"
	headerAgent     = "X-Agent-ID"
	bearerPrefix    = "Bearer "
	contentType     = "Content-Type"
	contentJSON     = "application/json"
)

type connectOptions struct {
	Key          string `json:"api_key"`
	AgentID      string `json:"agent_id"`
	DeviceID     string `json:"device_id"`
	CreateAgent  bool   `json:"create_agent"`
	Silence      int    `json:"silence_seconds"`
	FirstSpeaker string `json:"first_speaker"`
	Opening      string `json:"opening"`
	proxyMode    bool
	storySession string
}

type audioParams struct {
	Format        string `json:"format"`
	SampleRate    int    `json:"sample_rate"`
	Channels      int    `json:"channels"`
	FrameDuration int    `json:"frame_duration"`
	BitsPerSample int    `json:"bits_per_sample,omitempty"`
}

type platformMessage struct {
	Type        string          `json:"type"`
	State       string          `json:"state,omitempty"`
	SessionID   string          `json:"session_id,omitempty"`
	Text        string          `json:"text,omitempty"`
	ErrorCode   int             `json:"error_code,omitempty"`
	IsAborted   bool            `json:"is_aborted,omitempty"`
	Payload     json.RawMessage `json:"payload,omitempty"`
	AudioParams audioParams     `json:"audio_params,omitempty"`
}

type platformAPI struct {
	cfg  configuration
	key  string
	http *http.Client
}

func (p platformAPI) request(
	ctx context.Context,
	method, path string,
	body, target any,
) error {
	raw, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("encode platform request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(p.cfg.api, "/")+path, bytes.NewReader(raw))
	if err != nil {
		return fmt.Errorf("create platform request: %w", err)
	}
	req.Header.Set(headerAuth, bearerPrefix+p.key)
	req.Header.Set(contentType, contentJSON)
	resp, err := p.http.Do(req)
	if err != nil {
		return fmt.Errorf("灵矽管理接口暂时无法连接，请检查网络和服务地址")
	}
	defer closeBody(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("灵矽管理接口返回HTTP %d，请检查API Key权限", resp.StatusCode)
	}
	var envelope struct {
		Code int             `json:"code"`
		Data json.RawMessage `json:"data"`
	}
	responseBody, err := io.ReadAll(io.LimitReader(resp.Body, readLimit))
	if err != nil {
		return fmt.Errorf("读取灵矽管理接口响应失败，请重试")
	}
	if err := json.Unmarshal(responseBody, &envelope); err != nil {
		return fmt.Errorf("灵矽管理接口响应格式不正确")
	}
	if envelope.Code != 0 {
		slog.Warn("platform API rejected request", "path", path, "code", envelope.Code)
		return fmt.Errorf("灵矽管理接口业务码%d，请检查API Key、智能体ID和设备绑定", envelope.Code)
	}
	if target != nil {
		data := envelope.Data
		// The preregistration API returns its result directly, unlike the other management APIs.
		if path == preregisterPath && len(data) == 0 {
			data = responseBody
		}
		if err := json.Unmarshal(data, target); err != nil {
			slog.Warn("decode platform API data", "path", path, "error", err)
			return fmt.Errorf("灵矽管理接口返回的数据不完整")
		}
	}
	return nil
}

func (p platformAPI) prepare(ctx context.Context, options *connectOptions) (string, string, error) {
	if options.CreateAgent {
		prompt, err := assets.ReadFile("prompt.txt")
		if err != nil {
			return "", "", fmt.Errorf("read embedded story prompt: %w", err)
		}
		body := struct {
			Name      string `json:"agentName"`
			Assistant string `json:"assistantName"`
			Prompt    string `json:"systemPrompt"`
			Intent    string `json:"intentModelId"`
			Memory    string `json:"memModelId"`
		}{Name: agentName, Assistant: "小星", Prompt: string(prompt), Intent: intentFunction, Memory: memoryNone}
		if err := p.request(
			ctx,
			http.MethodPost,
			agentPath,
			body,
			&options.AgentID,
		); err != nil {
			return "", "", err
		}
	}
	if options.AgentID == "" {
		return "", "", fmt.Errorf("请填写智能体ID，或选择新建专用游戏智能体")
	}
	body := struct {
		Agent string   `json:"agent_id"`
		MACs  []string `json:"mac_addresses"`
	}{Agent: options.AgentID, MACs: []string{options.DeviceID}}
	var registration struct {
		Failed []json.RawMessage `json:"failed"`
	}
	if err := p.request(
		ctx,
		http.MethodPost,
		preregisterPath,
		body,
		&registration,
	); err != nil {
		return "", "", err
	}
	if len(registration.Failed) > 0 {
		path := "/v1/devices/" + url.PathEscape(options.DeviceID) + "/agent/" + url.PathEscape(options.AgentID)
		body := struct {
			DisableMigration bool `json:"disable_chat_history_migration"`
		}{DisableMigration: true}
		if err := p.request(
			ctx,
			http.MethodPut,
			path,
			body,
			nil,
		); err != nil {
			return "", "", err
		}
	}
	return p.activate(ctx, options.DeviceID)
}

func (p platformAPI) activate(ctx context.Context, mac string) (string, string, error) {
	clientID := clientUUID(mac)
	body := struct {
		Version     int    `json:"version"`
		UUID        string `json:"uuid"`
		MAC         string `json:"mac_address"`
		Application struct {
			Name    string `json:"name"`
			Version string `json:"version"`
		} `json:"application"`
		Board struct {
			Type string `json:"type"`
		} `json:"board"`
	}{UUID: clientID, MAC: mac}
	body.Application.Name, body.Application.Version = "story-relay", appVersion
	body.Board.Type = "story-relay-web"
	raw, err := json.Marshal(body)
	if err != nil {
		return "", "", fmt.Errorf("encode activation request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.cfg.ota, bytes.NewReader(raw))
	if err != nil {
		return "", "", fmt.Errorf("create activation request: %w", err)
	}
	req.Header.Set(headerDevice, mac)
	req.Header.Set(headerClient, clientID)
	req.Header.Set("Activation-Version", "1")
	req.Header.Set(contentType, contentJSON)
	resp, err := p.http.Do(req)
	if err != nil {
		return "", "", fmt.Errorf("设备激活接口暂时无法连接，请检查OTA地址")
	}
	defer closeBody(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return "", "", fmt.Errorf("设备激活返回HTTP %d，请检查OTA地址", resp.StatusCode)
	}
	var result struct {
		Activation *struct {
			Code string `json:"code"`
		} `json:"activation"`
		Websocket struct {
			URL   string `json:"url"`
			Token string `json:"token"`
		} `json:"websocket"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, readLimit)).Decode(&result); err != nil {
		return "", "", fmt.Errorf("设备激活响应格式不正确")
	}
	if result.Activation != nil {
		return "", "", fmt.Errorf("设备尚未绑定，请在灵矽控制台绑定本页设备MAC后重试（激活码：%s）", result.Activation.Code)
	}
	endpoint := p.cfg.ws
	if result.Websocket.URL != "" {
		endpoint = result.Websocket.URL
	}
	if result.Websocket.Token == "" {
		return "", "", fmt.Errorf("激活接口没有返回设备令牌，请确认设备已绑定智能体")
	}
	return endpoint, result.Websocket.Token, nil
}

func dialPlatform(
	ctx context.Context,
	endpoint, token string,
	options connectOptions,
) (*websocket.Conn, platformMessage, error) {
	u, err := url.Parse(endpoint)
	if err != nil || (u.Scheme != "wss" && u.Scheme != "ws") {
		return nil, platformMessage{}, fmt.Errorf("灵矽WebSocket地址不正确")
	}
	hdr := http.Header{}
	hdr.Set(headerAuth, bearerPrefix+token)
	hdr.Set(headerDevice, options.DeviceID)
	hdr.Set(headerClient, clientUUID(options.DeviceID))
	hdr.Set(headerProtocol, "1")
	hdr.Set(headerAgent, options.AgentID)
	dialer := websocket.Dialer{HandshakeTimeout: requestTimeout}
	conn, resp, err := dialer.DialContext(ctx, endpoint, hdr)
	if err != nil {
		if resp != nil {
			closeBody(resp.Body)
		}
		return nil, platformMessage{}, fmt.Errorf("灵矽语音连接失败，请检查设备绑定和网络")
	}
	conn.SetReadLimit(readLimit)
	hello := struct {
		Type      string `json:"type"`
		Version   int    `json:"version"`
		Transport string `json:"transport"`
		Features  struct {
			MCP bool `json:"mcp"`
		} `json:"features"`
		Audio       audioParams `json:"audio_params"`
		AgentParams struct {
			Replace map[string]string `json:"custom_replace_prompt"`
		} `json:"agent_params"`
	}{Type: messageHello, Version: 1, Transport: "websocket", Audio: audioParams{
		Format: formatPCM, SampleRate: uplinkRate, Channels: 1, FrameDuration: frameDuration, BitsPerSample: bitsPerSample,
	}}
	hello.Features.MCP = !options.proxyMode
	hello.AgentParams.Replace = map[string]string{"story_session": options.storySession}
	if err := writeJSON(conn, hello); err != nil {
		closeWS(conn)
		return nil, platformMessage{}, err
	}
	if err := conn.SetReadDeadline(time.Now().Add(requestTimeout)); err != nil {
		closeWS(conn)
		return nil, platformMessage{}, fmt.Errorf("set hello timeout: %w", err)
	}
	var welcome platformMessage
	if err := conn.ReadJSON(&welcome); err != nil {
		closeWS(conn)
		return nil, platformMessage{}, fmt.Errorf("未收到灵矽握手确认，请检查设备是否已激活")
	}
	if err := conn.SetReadDeadline(time.Time{}); err != nil {
		closeWS(conn)
		return nil, platformMessage{}, fmt.Errorf("clear hello timeout: %w", err)
	}
	if welcome.Type != messageHello {
		closeWS(conn)
		return nil, platformMessage{}, fmt.Errorf("灵矽没有返回预期的hello握手")
	}
	if welcome.AudioParams.SampleRate == 0 {
		welcome.AudioParams.SampleRate = uplinkRate
	}
	if welcome.AudioParams.Format != formatPCM {
		closeWS(conn)
		return nil, platformMessage{}, fmt.Errorf("当前语音服务没有协商为PCM，请使用支持PCM的灵矽服务地址")
	}
	return conn, welcome, nil
}

func validateOptions(options *connectOptions, cfg configuration) error {
	options.Key = strings.TrimSpace(options.Key)
	if options.Key == "" {
		options.Key = cfg.key
	}
	if options.Key == "" {
		return fmt.Errorf("请在连接设置里填写灵矽API Key")
	}
	if options.AgentID == "" {
		options.AgentID = cfg.agent
	}
	mac, err := net.ParseMAC(options.DeviceID)
	if err != nil || len(mac) != 6 {
		return fmt.Errorf("设备MAC格式应为02:xx:xx:xx:xx:xx")
	}
	options.DeviceID = mac.String()
	if options.Silence == 0 {
		options.Silence = defaultSilence
	}
	if options.Silence < minSilence || options.Silence > maxSilence {
		return fmt.Errorf("等待时间需要在%d到%d秒之间", minSilence, maxSilence)
	}
	if options.FirstSpeaker != roleAI && options.FirstSpeaker != roleChild {
		return fmt.Errorf("请选择AI开头或孩子开头")
	}
	return nil
}

func clientUUID(mac string) string {
	clean := strings.ReplaceAll(mac, ":", "")
	return "00000000-0000-4000-8000-" + clean
}

func closeBody(body io.ReadCloser) {
	if err := body.Close(); err != nil {
		// Response payloads and credentials are intentionally never logged.
		slog.Debug("close platform response", "error", err)
	}
}
