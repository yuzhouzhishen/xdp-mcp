package main

import (
	"context"
	"flag"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"

	"github.com/golang/glog"
	"github.com/mark3labs/mcp-go/server"
)

type contextKey string

const ctxKeyPSN contextKey = "psn"

func main() {
	cfg := &Config{}

	configPath := flag.String("config", "xdp-mcp.yaml", "path to config file")
	flag.Parse()
	defer glog.Flush()

	glog.Infof("loading config from %s", *configPath)
	if err := cfg.LoadFromFile(*configPath); err != nil && !os.IsNotExist(err) {
		glog.Exitf("load config: %v", err)
	}
	glog.Infof("config loaded: listen=%s, mqtt=%s", cfg.ListenAddr, cfg.MQTTBrokerURL)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Auth
	auth, err := NewAuthenticator(ctx, cfg.DatabaseURL)
	if err != nil {
		glog.Exitf("init authenticator: %v", err)
	}
	defer auth.Close()
	glog.Info("authenticator initialized")

	// MQTT
	mqttClient, err := NewMQTTClient(ctx, cfg)
	if err != nil {
		glog.Exitf("init mqtt client: %v", err)
	}

	// Track PSN per session for tool registration in hooks
	sessionPSN := &sync.Map{} // sessionID -> uint64

	// Telemetry manager: maintains live telemetry streams per PSN
	telemetry := NewTelemetryManager(mqttClient)

	// Hooks: register per-session tools when a session connects
	hooks := &server.Hooks{}
	hooks.AddOnRegisterSession(func(ctx context.Context, session server.ClientSession) {
		psn, ok := ctx.Value(ctxKeyPSN).(uint64)
		if !ok {
			glog.Errorf("no PSN in session context, session=%s", session.SessionID())
			return
		}
		sessionPSN.Store(session.SessionID(), psn)
		glog.Infof("session registered: session=%s psn=%d", session.SessionID(), psn)

		// Start telemetry stream eagerly
		telemetry.Start(context.Background(), psn)
	})

	hooks.AddOnUnregisterSession(func(ctx context.Context, session server.ClientSession) {
		if psn, ok := sessionPSN.Load(session.SessionID()); ok {
			telemetry.Stop(context.Background(), psn.(uint64))
		}
		sessionPSN.Delete(session.SessionID())
		glog.Infof("session unregistered: session=%s", session.SessionID())
	})

	// Create MCP server with hooks
	mcpServer := server.NewMCPServer(
		"ionbridge-mcp",
		"1.0.0",
		server.WithToolCapabilities(false),
		server.WithPromptCapabilities(false),
		server.WithResourceCapabilities(false, false),
		server.WithHooks(hooks),
	)

	// Register tools globally — handlers resolve PSN from context
	registerAllToolsWithContext(mcpServer, mqttClient, auth.Pool(), sessionPSN, telemetry)
	glog.Info("registered MCP tools")

	// Create SSE server with dynamic base path and context injection
	sseServer := server.NewSSEServer(mcpServer,
		server.WithDynamicBasePath(func(r *http.Request, sessionID string) string {
			psn, token, _, ok := extractFromPath(r.URL.Path)
			if !ok {
				return "/"
			}
			return "/" + psn + "/" + token
		}),
		server.WithSSEContextFunc(func(ctx context.Context, r *http.Request) context.Context {
			psn, token, _, ok := extractFromPath(r.URL.Path)
			if !ok {
				return ctx
			}

			if err := auth.Validate(r.Context(), psn, token); err != nil {
				glog.Warningf("SSE auth failed: psn=%s err=%v", psn, err)
				return ctx
			}

			psnUint, err := strconv.ParseUint(psn, 10, 64)
			if err != nil {
				return ctx
			}

			return context.WithValue(ctx, ctxKeyPSN, psnUint)
		}),
	)

	// Create Streamable HTTP server (for clients like Codex that use POST-only protocol)
	// NOTE: HTTPContextFunc receives the request AFTER path rewrite to /mcp,
	// so PSN is injected into r.Context() by the router and transferred here.
	streamServer := server.NewStreamableHTTPServer(mcpServer,
		server.WithEndpointPath("/mcp"),
		server.WithHTTPContextFunc(func(ctx context.Context, r *http.Request) context.Context {
			if psn, ok := r.Context().Value(ctxKeyPSN).(uint64); ok {
				return context.WithValue(ctx, ctxKeyPSN, psn)
			}
			return ctx
		}),
	)

	// Use SSEHandler/MessageHandler directly — ServeHTTP panics with WithDynamicBasePath.
	sseHandler := authMiddleware(auth, sseServer.SSEHandler())
	messageHandler := authMiddleware(auth, sseServer.MessageHandler())
	skillHandler := SkillHandler(auth.Pool())

	rootHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.Path

		// Route /{psn}/SKILL.md to the skill handler (no auth required)
		if strings.HasSuffix(p, "/SKILL.md") {
			skillHandler.ServeHTTP(w, r)
			return
		}

		// Route /{psn}/{token}/sse and /{psn}/{token}/message
		_, _, subpath, ok := extractFromPath(p)
		if !ok {
			glog.V(1).Infof("bad path: %s", p)
			http.Error(w, "error", http.StatusBadRequest)
			return
		}

		switch subpath {
		case "/sse":
			sseHandler.ServeHTTP(w, r)
		case "/message":
			messageHandler.ServeHTTP(w, r)
		case "/mcp":
			psn, token, _, _ := extractFromPath(p)
			if err := auth.Validate(r.Context(), psn, token); err != nil {
				glog.Warningf("StreamHTTP auth failed: psn=%s err=%v", psn, err)
				http.Error(w, "error", http.StatusUnauthorized)
				return
			}
			psnUint, _ := strconv.ParseUint(psn, 10, 64)
			ctx := context.WithValue(r.Context(), ctxKeyPSN, psnUint)
			r = r.WithContext(ctx)
			r.URL.Path = "/mcp"
			streamServer.ServeHTTP(w, r)
		default:
			http.NotFound(w, r)
		}
	})

	srv := &http.Server{
		Addr:    cfg.ListenAddr,
		Handler: rootHandler,
	}

	// Graceful shutdown
	go func() {
		sigCh := make(chan os.Signal, 1)
		signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
		sig := <-sigCh
		glog.Infof("received signal %s, shutting down", sig)
		cancel()
		sseServer.Shutdown(context.Background())
		streamServer.Shutdown(context.Background())
		srv.Close()
	}()

	glog.Infof("starting xdp-mcp server on %s", cfg.ListenAddr)
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		glog.Exitf("server error: %v", err)
	}
	glog.Info("server stopped")
}

// authMiddleware validates the PSN/token from URL path before passing to SSE server.
func authMiddleware(auth *Authenticator, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		psn, token, _, ok := extractFromPath(r.URL.Path)
		if !ok {
			glog.V(1).Infof("bad path: %s", r.URL.Path)
			http.Error(w, "error", http.StatusBadRequest)
			return
		}

		if err := auth.Validate(r.Context(), psn, token); err != nil {
			glog.Warningf("auth failed: psn=%s err=%v", psn, err)
			http.Error(w, "error", http.StatusUnauthorized)
			return
		}

		glog.V(1).Infof("auth ok: psn=%s", psn)
		next.ServeHTTP(w, r)
	})
}

// extractFromPath parses /<psn>/<token>/... and returns psn, token, remaining subpath, ok.
func extractFromPath(path string) (string, string, string, bool) {
	path = strings.TrimPrefix(path, "/")
	parts := strings.SplitN(path, "/", 3)
	if len(parts) < 2 {
		return "", "", "", false
	}
	psn := parts[0]
	token := parts[1]
	subpath := ""
	if len(parts) == 3 {
		subpath = "/" + parts[2]
	}
	if psn == "" || token == "" {
		return "", "", "", false
	}
	return psn, token, subpath, true
}
