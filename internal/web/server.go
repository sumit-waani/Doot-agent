// Package web serves the UI and its HTTP endpoints.
//
// Server-rendered Go templates plus htmx, with SSE for live updates. There is
// no SPA, no build step, and no client-side rendering layer to keep in sync.
package web

import (
	"context"
	"errors"
	"io/fs"
	"log/slog"
	"net/http"
	"time"

	"github.com/sumit-waani/doot/internal/agent"
	"github.com/sumit-waani/doot/internal/auth"
	"github.com/sumit-waani/doot/internal/bootstrap"
	"github.com/sumit-waani/doot/internal/config"
	"github.com/sumit-waani/doot/internal/db"
	"github.com/sumit-waani/doot/internal/events"
	"github.com/sumit-waani/doot/internal/project"
)

// Options configures a Server.
type Options struct {
	DB               *db.DB
	Config           *config.Store
	Auth             *auth.Service
	Events           *events.Log
	Project          *project.Service
	Agent            *agent.Service
	Dev              bool
	UsingDefaultPass bool
}

// Server holds everything the handlers need.
type Server struct {
	db      *db.DB
	cfg     *config.Store
	auth    *auth.Service
	events  *events.Log
	project *project.Service
	agent   *agent.Service
	render  *renderer
	limit   *auth.LoginLimiter
	dev     bool

	usingDefaultPass bool
}

// NewServer builds a Server with its templates parsed.
func NewServer(opts Options) (*Server, error) {
	r, err := newRenderer(opts.Dev)
	if err != nil {
		return nil, err
	}

	return &Server{
		db:               opts.DB,
		cfg:              opts.Config,
		auth:             opts.Auth,
		events:           opts.Events,
		project:          opts.Project,
		agent:            opts.Agent,
		render:           r,
		limit:            auth.NewLoginLimiter(10, 15*time.Minute),
		dev:              opts.Dev,
		usingDefaultPass: opts.UsingDefaultPass,
	}, nil
}

// Handler returns the root HTTP handler.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	// Static assets and PWA files. The service worker and manifest must be
	// served from the root scope, so they are routed explicitly rather than
	// living under /static/.
	mux.Handle("GET /static/", s.staticHandler())
	mux.HandleFunc("GET /sw.js", s.serveRootAsset("static/sw.js", "text/javascript"))
	mux.HandleFunc("GET /manifest.webmanifest", s.serveRootAsset("static/manifest.webmanifest", "application/manifest+json"))
	mux.HandleFunc("GET /offline", s.handleOffline)
	mux.HandleFunc("GET /healthz", s.handleHealthz)

	// Auth.
	mux.HandleFunc("GET /login", s.handleLoginPage)
	mux.HandleFunc("POST /login", s.handleLoginSubmit)
	mux.HandleFunc("POST /logout", s.handleLogout)

	// Screens.
	mux.Handle("GET /{$}", s.requireAuth(s.handleChat))
	mux.Handle("GET /desktop", s.requireAuth(s.handleDesktop))
	mux.Handle("GET /project", s.requireAuth(s.handleProject))
	mux.Handle("GET /settings", s.requireAuth(s.handleSettings))

	// Project and sandbox actions. These return immediately; the sandbox work
	// happens in the background and reports over SSE.
	mux.Handle("POST /project/create", s.requireAuth(s.handleProjectCreate))
	mux.Handle("POST /project/update", s.requireAuth(s.handleProjectUpdate))
	mux.Handle("POST /project/reset", s.requireAuth(s.handleProjectReset))
	mux.Handle("POST /project/delete", s.requireAuth(s.handleProjectDelete))
	mux.Handle("POST /project/setup", s.requireAuth(s.handleSetupRun))
	mux.Handle("POST /sandbox/start", s.requireAuth(s.handleSandboxStart))
	mux.Handle("POST /sandbox/stop", s.requireAuth(s.handleSandboxStop))
	mux.Handle("POST /desktop/start", s.requireAuth(s.handleDesktopStart))
	mux.Handle("POST /desktop/stop", s.requireAuth(s.handleDesktopStop))

	// Chat and the agent loop. These return immediately; the run continues in the
	// background and reports over SSE.
	mux.Handle("POST /chat/send", s.requireAuth(s.handleChatSend))
	mux.Handle("POST /chat/plan", s.requireAuth(s.handleChatPlan))
	mux.Handle("POST /chat/approve", s.requireAuth(s.handleChatApprove))
	mux.Handle("POST /chat/reject", s.requireAuth(s.handleChatReject))
	mux.Handle("POST /chat/pause", s.requireAuth(s.handleChatPause))
	mux.Handle("POST /chat/resume", s.requireAuth(s.handleChatResume))
	mux.Handle("POST /chat/cancel", s.requireAuth(s.handleChatCancel))
	mux.Handle("POST /chat/clear", s.requireAuth(s.handleChatClear))
	mux.Handle("GET /artifacts/{id}", s.requireAuth(s.handleArtifact))

	// Settings. Saved per section so a partially-applied section is impossible.
	mux.Handle("POST /settings/account", s.requireAuth(s.handleSettingsAccount))
	mux.Handle("POST /settings/secrets", s.requireAuth(s.handleSettingsSecrets))
	mux.Handle("POST /settings/{section}", s.requireAuth(s.handleSettingsSave))

	// Live updates.
	mux.Handle("GET /events", s.requireAuth(s.handleSSE))

	return s.withRecovery(s.withLogging(s.withSecurityHeaders(mux)))
}

// staticHandler serves embedded assets with long-lived caching.
func (s *Server) staticHandler() http.Handler {
	sub, err := fs.Sub(staticFS, "static")
	if err != nil {
		slog.Error("static assets unavailable", "err", err)
		return http.NotFoundHandler()
	}

	fileServer := http.FileServer(http.FS(sub))
	return http.StripPrefix("/static/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.dev {
			w.Header().Set("Cache-Control", "no-store")
		} else {
			w.Header().Set("Cache-Control", "public, max-age=3600")
		}
		fileServer.ServeHTTP(w, r)
	}))
}

// serveRootAsset serves one embedded file from the root path.
func (s *Server) serveRootAsset(path, contentType string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		body, err := staticFS.ReadFile(path)
		if err != nil {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", contentType)
		// The service worker itself must never be cached, or a stale worker
		// keeps serving a stale shell indefinitely.
		w.Header().Set("Cache-Control", "no-cache")
		w.Write(body)
	}
}

func (s *Server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()

	if err := s.db.PingContext(ctx); err != nil {
		http.Error(w, "database unreachable", http.StatusServiceUnavailable)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Write([]byte("ok"))
}

func (s *Server) handleOffline(w http.ResponseWriter, r *http.Request) {
	s.render.render(w, http.StatusOK, "offline", basePage{Title: "Offline", Nav: "", Chrome: false})
}

// ---------------------------------------------------------------- middleware

type ctxKey string

const ctxUser ctxKey = "user"

// requireAuth gates a handler behind a valid session.
func (s *Server) requireAuth(next func(http.ResponseWriter, *http.Request, auth.User)) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, err := s.auth.Authenticate(r.Context(), auth.TokenFromRequest(r))
		if err != nil {
			if !errors.Is(err, auth.ErrNoSession) {
				slog.Error("authentication failed", "err", err)
			}
			s.redirectToLogin(w, r)
			return
		}
		next(w, r, user)
	})
}

// redirectToLogin sends the client to the login screen. htmx requests get a
// header instead of a redirect, since htmx would otherwise swap the login page
// into a fragment.
func (s *Server) redirectToLogin(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("HX-Request") == "true" {
		w.Header().Set("HX-Redirect", "/login")
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

func (s *Server) withSecurityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "same-origin")
		h.Set("X-Frame-Options", "SAMEORIGIN")
		next.ServeHTTP(w, r)
	})
}

func (s *Server) withLogging(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// SSE connections are long-lived; logging them on completion would
		// report a misleading multi-hour duration.
		if r.URL.Path == "/events" {
			next.ServeHTTP(w, r)
			return
		}

		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)

		level := slog.LevelInfo
		if rec.status >= 500 {
			level = slog.LevelError
		}
		slog.Log(r.Context(), level, "request",
			"method", r.Method,
			"path", r.URL.Path,
			"status", rec.status,
			"dur", time.Since(start).Round(time.Millisecond).String(),
		)
	})
}

func (s *Server) withRecovery(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				slog.Error("panic serving request", "path", r.URL.Path, "panic", rec)
				http.Error(w, "internal error", http.StatusInternalServerError)
			}
		}()
		next.ServeHTTP(w, r)
	})
}

type statusRecorder struct {
	http.ResponseWriter
	status      int
	wroteHeader bool
}

func (r *statusRecorder) WriteHeader(status int) {
	if r.wroteHeader {
		return
	}
	r.status = status
	r.wroteHeader = true
	r.ResponseWriter.WriteHeader(status)
}

// Flush forwards to the underlying writer so SSE keeps working through the
// middleware chain.
func (r *statusRecorder) Flush() {
	if f, ok := r.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// SetDefaultPasswordBanner updates whether the default-password banner shows.
func (s *Server) SetDefaultPasswordBanner(on bool) { s.usingDefaultPass = on }

// DefaultUsername is re-exported so templates can name it on the login screen.
const DefaultUsername = bootstrap.DefaultUsername
