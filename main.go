package main

import (
	"context"
	"crypto/rand"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/gorilla/websocket"
)

//go:embed web/* prompt.txt proxy-prompt.txt call-prompt.txt
var assets embed.FS

const (
	defaultAddr    = "127.0.0.1:8787"
	defaultAPI     = "https://xrobo.qiniu.com"
	defaultWS      = "wss://xrobo-io.qiniuapi.com/v1/ws/"
	defaultOTAPath = "/xiaozhi/ota/"
	requestTimeout = 20 * time.Second
	writeTimeout   = 5 * time.Second
	readLimit      = 64 << 10
	defaultSilence = 60
	minSilence     = 15
	maxSilence     = 300
)

type configuration struct {
	addr, api, ws, ota, key, agent string
	mode                           string
	proxy                          *privateProxy
	lobby                          *lobby
	callMode                       bool
	dataDir                        string
}

type apiResponse struct {
	Code  int    `json:"code"`
	Msg   string `json:"msg"`
	ReqID string `json:"reqid"`
	Data  any    `json:"data"`
}

func main() {
	if err := run(); err != nil {
		slog.Error("story relay stopped", "error", err)
		os.Exit(1)
	}
}

func run() error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	cfg := configuration{
		addr: envOr("STORY_ADDR", defaultAddr), api: envOr("LINX_API_BASE", defaultAPI),
		ws: envOr("LINX_WS_URL", defaultWS), key: os.Getenv("LINX_API_KEY"), agent: os.Getenv("LINX_AGENT_ID"),
		mode: envOr("STORY_MCP_MODE", modeDevice), callMode: os.Getenv("STORY_CALL_DEMO") == "1",
	}
	cfg.ota = envOr("LINX_OTA_URL", strings.TrimRight(cfg.api, "/")+defaultOTAPath)
	host, _, err := net.SplitHostPort(cfg.addr)
	if err != nil || !net.ParseIP(host).IsLoopback() {
		return fmt.Errorf("STORY_ADDR must use a loopback IP and port, for example %s", defaultAddr)
	}
	if cfg.mode != modeDevice && cfg.mode != modeProxy {
		return fmt.Errorf("STORY_MCP_MODE must be device or proxy")
	}
	listener, err := net.Listen("tcp", cfg.addr)
	if err != nil {
		return fmt.Errorf("本地端口已占用，请先关闭已有的故事接龙程序: %w", err)
	}
	defer func() {
		if err := listener.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
			slog.Debug("close local listener", "error", err)
		}
	}()
	dataDir := os.Getenv("STORY_LOBBY_DATA")
	if dataDir == "" {
		cacheDir, err := os.UserCacheDir()
		if err != nil {
			return fmt.Errorf("resolve lobby voice storage: %w", err)
		}
		dataDir = filepath.Join(cacheDir, "story-relay-mcp")
	}
	cfg.dataDir = dataDir
	pool, err := openLobbyPool(dataDir)
	if err != nil {
		return err
	}
	cfg.lobby = newLobby(pool)
	if cfg.mode == modeProxy {
		proxy, stopProxy, err := startPrivateProxy(ctx, cfg)
		if err != nil {
			return fmt.Errorf("prepare private MCP: %w", err)
		}
		defer stopProxy()
		cfg.proxy = proxy
		slog.Info("private MCP registered", "account", proxy.info.Account,
			"agent_id", cfg.agent, "mcp_id", proxy.info.MCPID)
	}
	web, err := fs.Sub(assets, "web")
	if err != nil {
		return fmt.Errorf("open embedded web assets: %w", err)
	}
	mux := http.NewServeMux()
	mux.Handle("/", http.FileServer(http.FS(web)))
	mux.HandleFunc("GET /prompt.txt", func(w http.ResponseWriter, r *http.Request) {
		data, err := assets.ReadFile("prompt.txt")
		if err != nil {
			slog.ErrorContext(r.Context(), "read story prompt", "error", err)
			http.Error(w, "prompt unavailable", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		if _, err := w.Write(data); err != nil {
			slog.DebugContext(r.Context(), "write story prompt", "error", err)
		}
	})
	mux.HandleFunc("GET /api/status", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		data := struct {
			HasKey           bool               `json:"has_key"`
			AgentID          string             `json:"agent_id"`
			Mode             string             `json:"mcp_mode"`
			Online           bool               `json:"proxy_online"`
			Deployment       *privateDeployment `json:"deployment,omitempty"`
			Calls            uint64             `json:"proxy_calls"`
			Linked           int                `json:"linked_stories"`
			CallDemo         bool               `json:"single_call_demo"`
			Replays          uint64             `json:"original_playback_requests"`
			CallMatch        *callMatchStatus   `json:"call_match,omitempty"`
			CallTrace        []callTrace        `json:"call_trace,omitempty"`
			CallAudioReady   int                `json:"call_audio_ready"`
			CallAudioPending int                `json:"call_audio_pending"`
		}{HasKey: cfg.key != "", AgentID: cfg.agent, Mode: cfg.mode}
		if cfg.proxy != nil {
			data.Deployment, data.Online = &cfg.proxy.info, cfg.proxy.online.Load()
			data.Calls, data.Linked = cfg.proxy.calls.Load(), cfg.proxy.linkedStories()
			data.CallDemo, data.Replays = cfg.proxy.callDemo != nil, cfg.proxy.replays.Load()
			if cfg.proxy.callDemo != nil {
				data.CallMatch = cfg.proxy.callDemo.match.Load()
				data.CallAudioReady, data.CallAudioPending = cfg.proxy.callDemo.pool.counts()
				cfg.proxy.traceMu.Lock()
				data.CallTrace = append([]callTrace{}, cfg.proxy.callTrace...)
				cfg.proxy.traceMu.Unlock()
			}
		}
		if err := json.NewEncoder(w).Encode(apiResponse{Msg: "success", ReqID: rand.Text(), Data: data}); err != nil {
			slog.DebugContext(r.Context(), "write status response", "error", err)
		}
	})
	upgrader := websocket.Upgrader{CheckOrigin: func(r *http.Request) bool {
		u, err := url.Parse(r.Header.Get("Origin"))
		return err == nil && u.Scheme == "http" && u.Host == r.Host
	}}
	mux.HandleFunc("GET /lobby/ws", func(w http.ResponseWriter, r *http.Request) {
		cfg.lobby.socket(upgrader, w, r)
	})
	mux.HandleFunc("/ws", func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			slog.DebugContext(r.Context(), "browser upgrade failed", "error", err)
			return
		}
		if err := serveSession(r.Context(), conn, cfg); err != nil {
			slog.Info("story session closed", "error", err)
		}
	})
	srv := &http.Server{Addr: cfg.addr, Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestHost, _, err := net.SplitHostPort(r.Host)
		validHost := requestHost == "localhost" || net.ParseIP(requestHost).IsLoopback()
		if err != nil || !validHost {
			http.Error(w, "invalid host", http.StatusForbidden)
			return
		}
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")
		mux.ServeHTTP(w, r)
	}), ReadHeaderTimeout: writeTimeout, BaseContext: func(net.Listener) context.Context { return ctx }}
	done := make(chan struct{})
	go func() {
		defer close(done)
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), writeTimeout)
		defer cancel()
		if err := srv.Shutdown(shutdownCtx); err != nil {
			slog.Warn("shutdown local server", "error", err)
		}
	}()
	slog.Info("story relay ready", "url", "http://"+cfg.addr)
	err = srv.Serve(listener)
	stop()
	<-done
	if err != nil && !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("serve local game: %w", err)
	}
	return nil
}

func envOr(name, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	return fallback
}

func closeWS(conn *websocket.Conn) {
	if err := conn.Close(); err != nil {
		slog.Debug("close websocket", "error", err)
	}
}

func writeJSON(conn *websocket.Conn, value any) error {
	if err := conn.SetWriteDeadline(time.Now().Add(writeTimeout)); err != nil {
		return fmt.Errorf("set websocket write deadline: %w", err)
	}
	if err := conn.WriteJSON(value); err != nil {
		return fmt.Errorf("write websocket message: %w", err)
	}
	return nil
}
