package web

import (
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"strings"

	"github.com/sumit-waani/doot/internal/agent"
	"github.com/sumit-waani/doot/internal/auth"
	"github.com/sumit-waani/doot/internal/llm"
)

// friendlyAgentError turns internal errors into something worth reading on a
// phone. The model and Daytona credentials are the two things most likely to be
// missing, so they get explicit instructions rather than a raw error.
func friendlyAgentError(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, llm.ErrNoAPIKey):
		return errors.New("LLM API key is not set. Add it under Settings → Credentials.")
	case errors.Is(err, llm.ErrNoModel):
		return errors.New("No model is configured. Set one under Settings → Model.")
	case errors.Is(err, agent.ErrRunActive):
		return errors.New("A run is already active. Pause it first.")
	case errors.Is(err, agent.ErrNoRun):
		return errors.New("Nothing is running.")
	case errors.Is(err, agent.ErrNoPlan):
		return errors.New("There is no plan to act on.")
	default:
		return friendlyError(err)
	}
}

func (s *Server) handleChatSend(w http.ResponseWriter, r *http.Request, _ auth.User) {
	if err := r.ParseForm(); err != nil {
		s.redirectBack(w, r, "/", "", errors.New("Could not read the form."))
		return
	}

	message := strings.TrimSpace(r.PostFormValue("message"))
	if message == "" {
		s.redirectBack(w, r, "/", "", nil)
		return
	}

	if err := s.agent.SendMessage(r.Context(), message); err != nil {
		slog.Error("could not send message", "err", err)
		s.redirectBack(w, r, "/", "", friendlyAgentError(err))
		return
	}

	// No flash: the message itself appearing in the timeline is the confirmation,
	// and a banner on top of it would be noise.
	s.redirectBack(w, r, "/", "", nil)
}

func (s *Server) handleChatPlan(w http.ResponseWriter, r *http.Request, _ auth.User) {
	if err := s.agent.CreatePlan(r.Context()); err != nil {
		slog.Error("could not request plan", "err", err)
		s.redirectBack(w, r, "/", "", friendlyAgentError(err))
		return
	}
	s.redirectBack(w, r, "/", "", nil)
}

func (s *Server) handleChatApprove(w http.ResponseWriter, r *http.Request, _ auth.User) {
	planID, err := formInt64(r, "plan_id")
	if err != nil {
		s.redirectBack(w, r, "/", "", err)
		return
	}

	if err := s.agent.ApprovePlan(r.Context(), planID); err != nil {
		slog.Error("could not approve plan", "err", err)
		s.redirectBack(w, r, "/", "", friendlyAgentError(err))
		return
	}
	slog.Info("plan approved", "plan", planID)
	s.redirectBack(w, r, "/", "Plan approved. Work has started.", nil)
}

func (s *Server) handleChatReject(w http.ResponseWriter, r *http.Request, _ auth.User) {
	planID, err := formInt64(r, "plan_id")
	if err != nil {
		s.redirectBack(w, r, "/", "", err)
		return
	}

	reason := strings.TrimSpace(r.PostFormValue("reason"))
	if err := s.agent.RejectPlan(r.Context(), planID, reason); err != nil {
		slog.Error("could not reject plan", "err", err)
		s.redirectBack(w, r, "/", "", friendlyAgentError(err))
		return
	}
	s.redirectBack(w, r, "/", "Plan rejected. Tell it what you want instead.", nil)
}

func (s *Server) handleChatPause(w http.ResponseWriter, r *http.Request, _ auth.User) {
	if err := s.agent.Pause(r.Context()); err != nil {
		s.redirectBack(w, r, "/", "", friendlyAgentError(err))
		return
	}
	// Cooperative: the loop stops at its next checkpoint rather than mid-tool.
	s.redirectBack(w, r, "/", "Pausing at the next safe point…", nil)
}

func (s *Server) handleChatResume(w http.ResponseWriter, r *http.Request, _ auth.User) {
	if err := s.agent.Resume(r.Context()); err != nil {
		s.redirectBack(w, r, "/", "", friendlyAgentError(err))
		return
	}
	s.redirectBack(w, r, "/", "Resumed.", nil)
}

func (s *Server) handleChatCancel(w http.ResponseWriter, r *http.Request, _ auth.User) {
	if err := s.agent.Cancel(r.Context()); err != nil {
		s.redirectBack(w, r, "/", "", friendlyAgentError(err))
		return
	}
	s.redirectBack(w, r, "/", "Run cancelled.", nil)
}

func (s *Server) handleChatClear(w http.ResponseWriter, r *http.Request, _ auth.User) {
	if err := s.agent.ClearConversation(r.Context()); err != nil {
		slog.Error("could not clear conversation", "err", err)
		s.redirectBack(w, r, "/", "", friendlyAgentError(err))
		return
	}
	slog.Info("conversation cleared")
	s.redirectBack(w, r, "/", "Conversation cleared. History was kept; the sandbox is untouched.", nil)
}

// handleArtifact serves a screenshot captured during a run.
func (s *Server) handleArtifact(w http.ResponseWriter, r *http.Request, _ auth.User) {
	id := r.PathValue("id")

	data, contentType, ok := s.agent.Artifact(id)
	if !ok {
		http.NotFound(w, r)
		return
	}

	w.Header().Set("Content-Type", contentType)
	// Held in memory only for the life of the process, so the browser may cache
	// it freely: the id never refers to different bytes.
	w.Header().Set("Cache-Control", "private, max-age=3600")
	w.Write(data)
}

func formInt64(r *http.Request, field string) (int64, error) {
	if err := r.ParseForm(); err != nil {
		return 0, errors.New("Could not read the form.")
	}
	raw := strings.TrimSpace(r.PostFormValue(field))
	if raw == "" {
		return 0, errors.New("Missing " + field + ".")
	}
	v, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return 0, errors.New("Invalid " + field + ".")
	}
	return v, nil
}
