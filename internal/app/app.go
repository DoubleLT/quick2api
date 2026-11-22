// Package app wires the HTTP server: routes, auth, and the shared handler
// surface. Standard library mux only — the API is intentionally small.
package app

import (
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/kilbertert/quick2api/internal/api/openai"
	"github.com/kilbertert/quick2api/internal/bridge"
	"github.com/kilbertert/quick2api/internal/config"
	"github.com/kilbertert/quick2api/internal/debuglog"
)

// App is the running server.
type App struct {
	Cfg     *config.Config
	Bridge  *bridge.Bridge
	Debug   *debuglog.Logger
	Handler *openai.Handler
	Mux     *http.ServeMux
}

// New builds the app over an already-initialized bridge.
func New(cfg *config.Config, b *bridge.Bridge, dlog *debuglog.Logger) *App {
	max := cfg.Server.MaxConcurrency
	if max <= 0 {
		max = 16
	}
	h := &openai.Handler{
		Bridge: b,
		Cfg:    cfg,
		Debug:  dlog,
		Sem:    make(chan struct{}, max),
	}
	a := &App{Cfg: cfg, Bridge: b, Debug: dlog, Handler: h, Mux: http.NewServeMux()}
	a.routes()
	return a
}

func (a *App) routes() {
	a.Mux.HandleFunc("POST /v1/chat/completions", a.auth(a.Handler.ChatCompletions))
	a.Mux.HandleFunc("GET /v1/models", a.auth(a.Handler.Models))
	a.Mux.HandleFunc("GET /healthz", a.healthz)
	a.Mux.HandleFunc("GET /{$}", a.index)
}

// auth enforces the configured bearer on /v1/*; empty api_key is pass-through.
func (a *App) auth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if a.Cfg.API.APIKey != "" {
			got := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
			if got == "" {
				got = r.Header.Get("X-Api-Key")
			}
			if got != a.Cfg.API.APIKey {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusUnauthorized)
				_ = json.NewEncoder(w).Encode(map[string]any{
					"error": map[string]any{"message": "invalid api key", "type": "authentication_error"},
				})
				return
			}
		}
		next(w, r)
	}
}

func (a *App) healthz(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"ok":      true,
		"version": version,
		"bridge":  a.Bridge.Info(),
		"time":    time.Now().UTC(),
	})
}

func (a *App) index(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"service": "quick2api",
		"version": version,
		"endpoints": []string{
			"POST /v1/chat/completions",
			"GET  /v1/models",
			"GET  /healthz",
		},
	})
}

// version is set by main at startup (avoids a config<->main import cycle).
var version = "dev"

// SetVersion lets main inject the resolved build version.
func SetVersion(v string) {
	version = v
}
