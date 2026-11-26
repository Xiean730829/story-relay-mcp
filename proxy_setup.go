package main

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strings"
	"sync"
)

const (
	privateAgentName  = "故事接龙·私有演示"
	privateMCPName    = "故事接龙·私有MCP"
	mcpStorePath      = "/v1/mcp-store/servers"
	mcpInstancesPath  = "/v1/mcps"
	mcpProxyTokenPath = "/v1/mcpproxy/token"
	mcpProxyHTTPPath  = "/v1/mcpproxy/mcp"
	headerMCPToken    = "X-MCP-Token"
	mcpConfigType     = "sse"
	mcpPublished      = "published"
	mcpHTTPTransport  = 1
	mcpDocumentURL    = "https://linx.qiniu.com/docs/xrobot/mcp/custom-mcp"
	mcpLogoURL        = "https://linx.qiniu.com/favicon.ico"
	mcpDescription    = "由本机运行的私有故事接龙工具，支持单人接龙、多人大厅匹配和文字回合，经灵矽MCP Proxy接入。"
)

type privateDeployment struct {
	Account   string `json:"account"`
	AgentID   string `json:"agent_id"`
	ServerID  string `json:"server_id"`
	MCPID     string `json:"mcp_id"`
	ExpiresAt string `json:"expires_at"`
}

type privateMCPConfig struct {
	Type      string `json:"type"`
	Transport int    `json:"transport_type"`
	HTTP      struct {
		Type   int               `json:"type"`
		URL    string            `json:"url"`
		Params map[string]string `json:"params"`
		Header map[string]string `json:"header"`
	} `json:"sse"`
}

type privateServerRecord struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Private bool   `json:"is_private"`
	Status  string `json:"status"`
}

func startPrivateProxy(ctx context.Context, cfg configuration) (*privateProxy, context.CancelFunc, error) {
	if cfg.key == "" || cfg.agent == "" {
		return nil, nil, fmt.Errorf("私有MCP模式需要通过环境变量配置LINX_API_KEY和LINX_AGENT_ID")
	}
	api := platformAPI{cfg: cfg, key: cfg.key, http: &http.Client{
		Timeout:       requestTimeout,
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse },
	}}
	var account struct {
		Username string `json:"username"`
	}
	if err := api.request(
		ctx,
		http.MethodGet,
		"/v1/users/me",
		nil,
		&account,
	); err != nil {
		return nil, nil, err
	}
	var agent struct {
		Name string `json:"agentName"`
	}
	if err := api.request(
		ctx,
		http.MethodGet,
		agentPath+"/"+url.PathEscape(cfg.agent),
		nil,
		&agent,
	); err != nil {
		return nil, nil, err
	}
	isDemoAgent := agent.Name == privateAgentName || (cfg.callMode && agent.Name == agentName)
	if !isDemoAgent {
		return nil, nil, fmt.Errorf("请选择本演示的专用智能体“%s”，避免修改其他智能体", privateAgentName)
	}
	var token struct {
		Endpoint  string `json:"wss_endpoint"`
		ExpiresAt string `json:"expires_at"`
	}
	if err := api.request(
		ctx,
		http.MethodGet,
		mcpProxyTokenPath,
		nil,
		&token,
	); err != nil {
		return nil, nil, err
	}
	endpoint, err := url.Parse(token.Endpoint)
	if err != nil {
		return nil, nil, fmt.Errorf("灵矽返回的MCP Proxy地址格式不正确")
	}
	base, err := url.Parse(cfg.api)
	if err != nil {
		return nil, nil, fmt.Errorf("灵矽管理接口地址格式不正确")
	}
	// Behind an HTTPS gateway the token API can report ws; retain TLS for the same public origin.
	if endpoint.Scheme == "ws" && base.Scheme == "https" && endpoint.Host == base.Host {
		endpoint.Scheme = "wss"
	}
	if endpoint.Scheme != "wss" || endpoint.Query().Get("token") == "" {
		return nil, nil, fmt.Errorf("灵矽没有返回有效的安全MCP Proxy地址")
	}
	proxy := &privateProxy{
		endpoint: endpoint.String(), ready: make(chan struct{}), games: map[string]*proxyStory{},
		watchers: map[string]proxyWatcher{}, lobby: cfg.lobby,
		info: privateDeployment{Account: account.Username, AgentID: cfg.agent, ExpiresAt: token.ExpiresAt},
	}
	if cfg.callMode {
		history := callHistory{api: api, agent: cfg.agent}
		media, err := newCallMedia(history)
		if err != nil {
			return nil, nil, err
		}
		demo, err := newCallDemo(history, cfg.dataDir, media.prepare)
		if err != nil {
			return nil, nil, err
		}
		demo.preparePlayback = media.preparePlayback
		if os.Getenv("STORY_CHAIN_DEMO") == "1" {
			demo.chain = newCallChain(envOr("STORY_GAME_PLATFORM", callChainDefaultURL))
			demo.prepareChain = media.prepareStoryPlayback
		}
		proxy.callDemo = demo
	}
	runCtx, cancel := context.WithCancel(ctx)
	var worker sync.WaitGroup
	worker.Go(func() { proxy.run(runCtx) })
	if proxy.callDemo != nil {
		worker.Go(func() { proxy.callDemo.runArchive(runCtx) })
	}
	stop := func() { cancel(); worker.Wait() }
	readyCtx, stopWait := context.WithTimeout(ctx, requestTimeout)
	defer stopWait()
	select {
	case <-readyCtx.Done():
		stop()
		return nil, nil, fmt.Errorf("连接MCP Proxy超时，请检查网络后重试")
	case <-proxy.ready:
	}
	config := privateMCPConfig{Type: mcpConfigType, Transport: mcpHTTPTransport}
	httpEndpoint := *endpoint
	httpEndpoint.Scheme, httpEndpoint.Path, httpEndpoint.RawQuery = "https", mcpProxyHTTPPath, ""
	config.HTTP.URL = httpEndpoint.String()
	config.HTTP.Params = map[string]string{}
	config.HTTP.Header = map[string]string{headerMCPToken: endpoint.Query().Get("token")}
	if err := proxy.register(ctx, api, config); err != nil {
		stop()
		return nil, nil, err
	}
	return proxy, stop, nil
}

func (p *privateProxy) register(ctx context.Context, api platformAPI, config privateMCPConfig) error {
	var servers struct {
		Servers []privateServerRecord `json:"servers"`
	}
	if err := api.request(
		ctx,
		http.MethodGet,
		mcpStorePath,
		nil,
		&servers,
	); err != nil {
		return err
	}
	var server privateServerRecord
	for _, item := range servers.Servers {
		if item.Name == privateMCPName {
			server = item
			break
		}
	}
	if server.ID != "" && !server.Private {
		return fmt.Errorf("存在同名公共MCP，请使用独立的私有服务名称")
	}
	if server.ID == "" {
		schema := config
		schema.HTTP.Header = map[string]string{headerMCPToken: ""}
		body := struct {
			Name        string           `json:"name"`
			Private     bool             `json:"is_private"`
			Logo        string           `json:"logo_url"`
			Document    string           `json:"document_url"`
			Tags        []string         `json:"tags"`
			Description string           `json:"desc"`
			Content     string           `json:"content"`
			Config      privateMCPConfig `json:"config_schema"`
		}{Name: privateMCPName, Private: true, Logo: mcpLogoURL, Document: mcpDocumentURL,
			Tags: []string{"语音游戏", "故事接龙"}, Description: mcpDescription, Content: mcpDescription, Config: schema}
		if err := api.request(
			ctx,
			http.MethodPost,
			mcpStorePath,
			body,
			&server,
		); err != nil {
			return err
		}
	}
	if !server.Private {
		return fmt.Errorf("MCP服务未被标记为私有，停止账号绑定")
	}
	p.info.ServerID = server.ID
	if server.Status != mcpPublished {
		if err := api.request(
			ctx,
			http.MethodPut,
			mcpStorePath+"/"+url.PathEscape(server.ID)+"/publish",
			nil,
			nil,
		); err != nil {
			return err
		}
	}
	var instances struct {
		MCPs []struct {
			ID     string `json:"id"`
			Server string `json:"mcp_id"`
		} `json:"mcps"`
	}
	if err := api.request(
		ctx,
		http.MethodGet,
		mcpInstancesPath,
		nil,
		&instances,
	); err != nil {
		return err
	}
	for _, item := range instances.MCPs {
		if item.Server == server.ID {
			p.info.MCPID = item.ID
			break
		}
	}
	body := struct {
		Server      string           `json:"mcp_id"`
		Name        string           `json:"name"`
		Description string           `json:"desc"`
		Config      privateMCPConfig `json:"config"`
	}{Server: server.ID, Name: privateMCPName, Description: mcpDescription, Config: config}
	var instance struct {
		ID string `json:"id"`
	}
	var responseTarget any = &instance
	method, path := http.MethodPost, mcpInstancesPath
	if p.info.MCPID != "" {
		method, path = http.MethodPut, mcpInstancesPath+"/"+url.PathEscape(p.info.MCPID)
		responseTarget = nil
	}
	if err := api.request(
		ctx,
		method,
		path,
		body,
		responseTarget,
	); err != nil {
		return err
	}
	if p.info.MCPID == "" {
		if instance.ID == "" {
			return fmt.Errorf("灵矽未返回新建MCP实例的标识")
		}
		p.info.MCPID = instance.ID
	}
	bindingsPath := "/v1/agents/" + url.PathEscape(p.info.AgentID) + "/mcps"
	var bindings struct {
		Selected []struct {
			ID string `json:"mcp_id"`
		} `json:"selecteds"`
	}
	if err := api.request(
		ctx,
		http.MethodGet,
		bindingsPath,
		nil,
		&bindings,
	); err != nil {
		return err
	}
	ids := []string{}
	for _, selected := range bindings.Selected {
		ids = append(ids, selected.ID)
	}
	if !slices.Contains(ids, p.info.MCPID) {
		ids = append(ids, p.info.MCPID)
		if err := api.request(
			ctx,
			http.MethodPut,
			bindingsPath,
			struct {
				IDs []string `json:"mcp_ids"`
			}{IDs: ids},
			nil,
		); err != nil {
			return err
		}
	}
	prompt, err := assets.ReadFile("prompt.txt")
	if err != nil {
		return fmt.Errorf("read story prompt: %w", err)
	}
	addition, err := assets.ReadFile("proxy-prompt.txt")
	if err != nil {
		return fmt.Errorf("read private MCP prompt: %w", err)
	}
	sections := []string{string(prompt), string(addition)}
	const saveTextAndAudio = 2
	historyMode := 0
	if p.callDemo != nil {
		callPrompt, err := assets.ReadFile("call-prompt.txt")
		if err != nil {
			return fmt.Errorf("read embedded call demo prompt: %w", err)
		}
		sections = []string{string(callPrompt)}
		historyMode = saveTextAndAudio
	}
	update := struct {
		Prompt          string `json:"systemPrompt"`
		Intent          string `json:"intentModelId"`
		ChatHistoryConf int    `json:"chatHistoryConf,omitempty"`
	}{Prompt: strings.Join(sections, "\n"), Intent: intentFunction, ChatHistoryConf: historyMode}
	return api.request(
		ctx,
		http.MethodPut,
		agentPath+"/"+url.PathEscape(p.info.AgentID),
		update,
		nil,
	)
}
