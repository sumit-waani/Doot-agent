package web

import (
	"errors"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/sumit-waani/doot/internal/auth"
)

type loginPage struct {
	basePage
	Error          string
	Username       string
	DefaultCreds   bool
	RetryAfterMins int
}

func (s *Server) handleLoginPage(w http.ResponseWriter, r *http.Request) {
	// Already signed in: skip the form.
	if _, err := s.auth.Authenticate(r.Context(), auth.TokenFromRequest(r)); err == nil {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}

	s.render.render(w, http.StatusOK, "login", loginPage{
		basePage:     basePage{Title: "Sign in", Chrome: false},
		DefaultCreds: s.usingDefaultPass,
	})
}

func (s *Server) handleLoginSubmit(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		s.renderLoginError(w, http.StatusBadRequest, "", "Could not read the form.")
		return
	}

	username := strings.TrimSpace(r.PostFormValue("username"))
	password := r.PostFormValue("password")
	key := clientKey(r)

	if ok, wait := s.limit.Allow(key); !ok {
		mins := int(wait.Round(time.Minute) / time.Minute)
		if mins < 1 {
			mins = 1
		}
		s.render.render(w, http.StatusTooManyRequests, "login", loginPage{
			basePage:       basePage{Title: "Sign in", Chrome: false},
			Username:       username,
			Error:          "Too many failed attempts.",
			RetryAfterMins: mins,
		})
		return
	}

	user, err := s.auth.Login(r.Context(), username, password)
	if err != nil {
		if !errors.Is(err, auth.ErrInvalidCredentials) {
			slog.Error("login failed", "err", err)
		}
		s.limit.RecordFailure(key)
		// Deliberately vague: do not reveal whether the username exists.
		s.renderLoginError(w, http.StatusUnauthorized, username, "Incorrect username or password.")
		return
	}

	token, expires, err := s.auth.CreateSession(r.Context(), user.ID, r.UserAgent())
	if err != nil {
		slog.Error("could not create session", "err", err)
		s.renderLoginError(w, http.StatusInternalServerError, username, "Could not start a session.")
		return
	}

	s.limit.Reset(key)
	s.auth.SetCookie(w, token, expires)
	slog.Info("signed in", "username", user.Username)

	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	if err := s.auth.DeleteSession(r.Context(), auth.TokenFromRequest(r)); err != nil {
		slog.Error("could not delete session", "err", err)
	}
	s.auth.ClearCookie(w)

	if r.Header.Get("HX-Request") == "true" {
		w.Header().Set("HX-Redirect", "/login")
		w.WriteHeader(http.StatusOK)
		return
	}
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

func (s *Server) renderLoginError(w http.ResponseWriter, status int, username, msg string) {
	s.render.render(w, status, "login", loginPage{
		basePage: basePage{Title: "Sign in", Chrome: false},
		Username: username,
		Error:    msg,
	})
}

// clientKey identifies the caller for rate limiting.
//
// Fly terminates TLS and forwards the real address in Fly-Client-IP, so that is
// preferred; RemoteAddr would otherwise be the proxy for every request and the
// limiter would throttle globally.
func clientKey(r *http.Request) string {
	if ip := r.Header.Get("Fly-Client-IP"); ip != "" {
		return ip
	}
	if fwd := r.Header.Get("X-Forwarded-For"); fwd != "" {
		if first, _, ok := strings.Cut(fwd, ","); ok {
			return strings.TrimSpace(first)
		}
		return strings.TrimSpace(fwd)
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}
