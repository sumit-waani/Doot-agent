package web

import (
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/sumit-waani/doot/internal/auth"
	"github.com/sumit-waani/doot/internal/daytona"
	"github.com/sumit-waani/doot/internal/project"
)

// redirectBack sends the browser back to a screen with a flash message.
//
// Sandbox operations are asynchronous, so these handlers return immediately and
// the real progress arrives over SSE. The flash only confirms the request was
// accepted.
func (s *Server) redirectBack(w http.ResponseWriter, r *http.Request, path, okMsg string, err error) {
	q := url.Values{}
	if err != nil {
		q.Set("err", err.Error())
	} else if okMsg != "" {
		q.Set("ok", okMsg)
	}

	target := path
	if len(q) > 0 {
		target += "?" + q.Encode()
	}

	if r.Header.Get("HX-Request") == "true" {
		w.Header().Set("HX-Redirect", target)
		w.WriteHeader(http.StatusOK)
		return
	}
	http.Redirect(w, r, target, http.StatusSeeOther)
}

// friendlyError turns internal errors into something worth reading on a phone.
func friendlyError(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, daytona.ErrNoAPIKey):
		return errors.New("Daytona API key is not set. Add it under Settings → Credentials.")
	case errors.Is(err, daytona.ErrNoSandbox):
		return errors.New("This project has no sandbox yet.")
	case errors.Is(err, project.ErrNoProject):
		return errors.New("No project exists yet.")
	case errors.Is(err, project.ErrProjectExists):
		return errors.New("A project already exists. Delete it before creating another.")
	case errors.Is(err, project.ErrBusy):
		return errors.New("A sandbox operation is already running. Wait for it to finish.")
	default:
		return err
	}
}

func (s *Server) handleProjectCreate(w http.ResponseWriter, r *http.Request, _ auth.User) {
	if err := r.ParseForm(); err != nil {
		s.redirectBack(w, r, "/project", "", errors.New("Could not read the form."))
		return
	}

	name := strings.TrimSpace(r.PostFormValue("name"))
	repoURL := strings.TrimSpace(r.PostFormValue("repo_url"))

	if err := s.project.Create(r.Context(), name, repoURL); err != nil {
		slog.Error("could not create project", "err", err)
		s.redirectBack(w, r, "/project", "", friendlyError(err))
		return
	}

	slog.Info("project created", "name", name, "repo", repoURL)
	s.redirectBack(w, r, "/project", "Project created. Provisioning the sandbox…", nil)
}

func (s *Server) handleProjectDelete(w http.ResponseWriter, r *http.Request, _ auth.User) {
	if err := s.project.Delete(r.Context()); err != nil {
		slog.Error("could not delete project", "err", err)
		s.redirectBack(w, r, "/project", "", friendlyError(err))
		return
	}
	slog.Warn("project deleted")
	s.redirectBack(w, r, "/project", "Project deleted. Conversation history was kept.", nil)
}

func (s *Server) handleProjectReset(w http.ResponseWriter, r *http.Request, _ auth.User) {
	if err := s.project.Reset(r.Context()); err != nil {
		slog.Error("could not reset sandbox", "err", err)
		s.redirectBack(w, r, "/project", "", friendlyError(err))
		return
	}
	slog.Warn("sandbox reset requested")
	s.redirectBack(w, r, "/project", "Rebuilding the sandbox…", nil)
}

func (s *Server) handleProjectUpdate(w http.ResponseWriter, r *http.Request, _ auth.User) {
	if err := r.ParseForm(); err != nil {
		s.redirectBack(w, r, "/project", "", errors.New("Could not read the form."))
		return
	}

	port := 0
	if raw := strings.TrimSpace(r.PostFormValue("dev_port")); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed < 1 || parsed > 65535 {
			s.redirectBack(w, r, "/project", "", errors.New("Dev port must be between 1 and 65535."))
			return
		}
		port = parsed
	}

	err := s.project.UpdateBuild(r.Context(),
		r.PostFormValue("setup_script"),
		strings.TrimSpace(r.PostFormValue("dev_command")),
		port,
	)
	if err != nil {
		s.redirectBack(w, r, "/project", "", friendlyError(err))
		return
	}
	s.redirectBack(w, r, "/project", "Saved.", nil)
}

func (s *Server) handleSandboxStart(w http.ResponseWriter, r *http.Request, _ auth.User) {
	if err := s.project.Start(r.Context()); err != nil {
		s.redirectBack(w, r, "/project", "", friendlyError(err))
		return
	}
	s.redirectBack(w, r, "/project", "Starting the sandbox…", nil)
}

func (s *Server) handleSandboxStop(w http.ResponseWriter, r *http.Request, _ auth.User) {
	if err := s.project.Stop(r.Context()); err != nil {
		s.redirectBack(w, r, "/project", "", friendlyError(err))
		return
	}
	s.redirectBack(w, r, "/project", "Stopping the sandbox…", nil)
}

func (s *Server) handleSetupRun(w http.ResponseWriter, r *http.Request, _ auth.User) {
	out, err := s.project.RunSetupScript(r.Context())
	if err != nil {
		slog.Error("setup script failed", "err", err, "output", out)
		s.redirectBack(w, r, "/project", "", friendlyError(err))
		return
	}
	s.redirectBack(w, r, "/project", "Setup script finished.", nil)
}

func (s *Server) handleDesktopStart(w http.ResponseWriter, r *http.Request, _ auth.User) {
	if err := s.project.StartDesktop(r.Context()); err != nil {
		s.redirectBack(w, r, "/desktop", "", friendlyError(err))
		return
	}
	s.redirectBack(w, r, "/desktop", "Starting the desktop…", nil)
}

func (s *Server) handleDesktopStop(w http.ResponseWriter, r *http.Request, _ auth.User) {
	if err := s.project.StopDesktop(r.Context()); err != nil {
		s.redirectBack(w, r, "/desktop", "", friendlyError(err))
		return
	}
	s.redirectBack(w, r, "/desktop", "Desktop stopped.", nil)
}
