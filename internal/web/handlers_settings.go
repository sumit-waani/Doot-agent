package web

import (
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"

	"github.com/sumit-waani/doot/internal/auth"
	"github.com/sumit-waani/doot/internal/config"
	"github.com/sumit-waani/doot/internal/daytona"
)

// Settings are saved one section at a time. Each section declares its fields and
// how to validate them, so the handlers stay declarative and a bad value is
// rejected before it reaches the database — a malformed VNC resolution or a
// negative TTL is much cheaper to catch here than to debug in a sandbox later.
type settingsSection struct {
	Label  string
	Texts  []string
	Ints   []intField
	Floats []floatField
	Bools  []string
	// Validate runs after per-field checks, for rules that span fields.
	Validate func(values map[string]string) error
}

type intField struct {
	Key      string
	Min, Max int
}

type floatField struct {
	Key      string
	Min, Max float64
}

var settingsSections = map[string]settingsSection{
	"model": {
		Label: "model settings",
		Texts: []string{config.KeyLLMModel, config.KeyLLMBaseURL},
		Ints: []intField{
			{Key: config.KeyLLMContextWindow, Min: 1024, Max: 10_000_000},
			{Key: config.KeyLLMMaxOutputTokens, Min: 256, Max: 1_000_000},
		},
	},
	"agent": {
		Label: "agent settings",
		Texts: []string{config.KeySystemPrompt},
		Ints:  []intField{{Key: config.KeyCompactThresholdPct, Min: 10, Max: 99}},
		Bools: []string{config.KeyReviewerEnabled, config.KeyE2EEnabled},
	},
	"daytona": {
		Label: "Daytona settings",
		Texts: []string{"daytona.api_url", "daytona.target"},
	},
	"sandbox": {
		Label: "sandbox settings",
		Texts: []string{config.KeySandboxSnapshot, config.KeySandboxVNCRes},
		Ints: []intField{
			{Key: config.KeySandboxAutoStopMin, Min: 0, Max: 100_000},
			{Key: "sandbox.heartbeat_seconds", Min: 30, Max: 86_400},
			// -1 is the only negative value with meaning: never auto-delete.
			{Key: "sandbox.auto_delete_minutes", Min: -1, Max: 100_000},
			{Key: "sandbox.ttl_minutes", Min: 0, Max: 100_000},
			{Key: "sandbox.preview_ttl_seconds", Min: 60, Max: 604_800},
			{Key: "sandbox.vnc_port", Min: 1, Max: 65535},
		},
		Bools:    []string{"sandbox.public"},
		Validate: validateSandboxSection,
	},
	"computeruse": {
		Label: "computer use settings",
		Texts: []string{"computeruse.screenshot_format"},
		Ints:  []intField{{Key: "computeruse.screenshot_quality", Min: 1, Max: 100}},
		Floats: []floatField{
			{Key: "computeruse.screenshot_scale", Min: 0.1, Max: 1},
		},
	},
	"git": {
		Label: "git settings",
		Texts: []string{
			config.KeyGitHubUsername, config.KeyGitAuthorName,
			config.KeyGitAuthorEmail, config.KeyGitWorkBranch,
		},
		Bools:    []string{config.KeyGitHubCreatePR},
		Validate: validateGitSection,
	},
	"pricing": {
		Label: "pricing",
		Floats: []floatField{
			{Key: config.KeyPricingInputPerMtok, Min: 0, Max: 10_000},
			{Key: config.KeyPricingCachedInputPerMtok, Min: 0, Max: 10_000},
			{Key: config.KeyPricingOutputPerMtok, Min: 0, Max: 10_000},
		},
	},
}

// validateSandboxSection guards the values that would quietly break a sandbox.
func validateSandboxSection(values map[string]string) error {
	// A resolution that the X server rejects falls back to the default, which
	// then silently mismatches whatever the agent assumes.
	if res, ok := values[config.KeySandboxVNCRes]; ok {
		if err := daytona.ValidateResolution(res); err != nil {
			return err
		}
	}

	// 0 means "delete as soon as it stops", which would take the working tree
	// with it the first time auto-stop fired.
	if raw, ok := values["sandbox.auto_delete_minutes"]; ok && raw == "0" {
		return errors.New(
			"auto-delete of 0 would destroy the sandbox as soon as it stops; use -1 for never")
	}

	// Computer-use only exists in Daytona's default images.
	if snapshot, ok := values[config.KeySandboxSnapshot]; ok {
		if !daytona.IsDefaultSnapshot(snapshot) {
			return fmt.Errorf(
				"%q is not a known default-image snapshot; computer-use needs one of "+
					"daytona-small, daytona-medium, daytona-large", snapshot)
		}
	}
	return nil
}

func validateGitSection(values map[string]string) error {
	branch, ok := values[config.KeyGitWorkBranch]
	if !ok {
		return nil
	}
	if strings.TrimSpace(branch) == "" {
		return errors.New("work branch is required")
	}
	switch strings.ToLower(strings.TrimSpace(branch)) {
	case "main", "master":
		return errors.New("the work branch must not be main or master; Doot never commits to the base branch")
	}
	return nil
}

// handleSettingsSave saves one section.
func (s *Server) handleSettingsSave(w http.ResponseWriter, r *http.Request, _ auth.User) {
	name := r.PathValue("section")
	section, ok := settingsSections[name]
	if !ok {
		s.redirectBack(w, r, "/settings", "", fmt.Errorf("unknown settings section %q", name))
		return
	}

	if err := r.ParseForm(); err != nil {
		s.redirectBack(w, r, "/settings", "", errors.New("Could not read the form."))
		return
	}

	values := map[string]string{}

	for _, key := range section.Texts {
		if !r.Form.Has(key) {
			continue
		}
		values[key] = strings.TrimSpace(r.PostFormValue(key))
	}

	for _, f := range section.Ints {
		if !r.Form.Has(f.Key) {
			continue
		}
		raw := strings.TrimSpace(r.PostFormValue(f.Key))
		n, err := strconv.Atoi(raw)
		if err != nil {
			s.redirectBack(w, r, "/settings", "", fmt.Errorf("%s must be a whole number", f.Key))
			return
		}
		if n < f.Min || n > f.Max {
			s.redirectBack(w, r, "/settings", "",
				fmt.Errorf("%s must be between %d and %d", f.Key, f.Min, f.Max))
			return
		}
		values[f.Key] = strconv.Itoa(n)
	}

	for _, f := range section.Floats {
		if !r.Form.Has(f.Key) {
			continue
		}
		raw := strings.TrimSpace(r.PostFormValue(f.Key))
		v, err := strconv.ParseFloat(raw, 64)
		if err != nil {
			s.redirectBack(w, r, "/settings", "", fmt.Errorf("%s must be a number", f.Key))
			return
		}
		if v < f.Min || v > f.Max {
			s.redirectBack(w, r, "/settings", "",
				fmt.Errorf("%s must be between %g and %g", f.Key, f.Min, f.Max))
			return
		}
		values[f.Key] = strconv.FormatFloat(v, 'f', -1, 64)
	}

	// Unchecked checkboxes are not submitted at all, so absence has to be
	// written as "0" rather than skipped, or a toggle could never be turned off.
	for _, key := range section.Bools {
		if r.PostFormValue(key) != "" {
			values[key] = "1"
		} else {
			values[key] = "0"
		}
	}

	if section.Validate != nil {
		if err := section.Validate(values); err != nil {
			s.redirectBack(w, r, "/settings", "", err)
			return
		}
	}

	if err := s.cfg.SetMany(r.Context(), values); err != nil {
		slog.Error("could not save settings", "section", name, "err", err)
		s.redirectBack(w, r, "/settings", "", errors.New("Could not save settings."))
		return
	}

	slog.Info("settings saved", "section", name, "keys", len(values))
	s.redirectBack(w, r, "/settings", section.Label+" saved", nil)
}

// handleSettingsSecrets saves credentials.
//
// A blank field leaves the stored value alone, and clearing requires the
// explicit Remove button. This matters because the main use case is pasting a
// rotated token on a phone, where an accidental empty submit wiping every
// credential would be infuriating.
func (s *Server) handleSettingsSecrets(w http.ResponseWriter, r *http.Request, _ auth.User) {
	if err := r.ParseForm(); err != nil {
		s.redirectBack(w, r, "/settings", "", errors.New("Could not read the form."))
		return
	}

	ctx := r.Context()

	if remove := r.PostFormValue("remove"); remove != "" {
		if !isKnownSecret(remove) {
			s.redirectBack(w, r, "/settings", "", fmt.Errorf("unknown credential %q", remove))
			return
		}
		if err := s.cfg.DeleteSecret(ctx, remove); err != nil {
			slog.Error("could not remove secret", "name", remove, "err", err)
			s.redirectBack(w, r, "/settings", "", errors.New("Could not remove the credential."))
			return
		}
		slog.Warn("credential removed", "name", remove)
		s.redirectBack(w, r, "/settings", "credential removed", nil)
		return
	}

	saved := 0
	for _, name := range config.AllSecretNames {
		value := strings.TrimSpace(r.PostFormValue(name))
		if value == "" {
			continue // blank means unchanged
		}
		if err := s.cfg.SetSecret(ctx, name, value); err != nil {
			slog.Error("could not save secret", "name", name, "err", err)
			s.redirectBack(w, r, "/settings", "", errors.New("Could not save the credential."))
			return
		}
		saved++
	}

	if saved == 0 {
		s.redirectBack(w, r, "/settings", "nothing changed", nil)
		return
	}
	slog.Info("credentials saved", "count", saved)
	s.redirectBack(w, r, "/settings", fmt.Sprintf("%d credential(s) saved", saved), nil)
}

// handleSettingsAccount changes the username and password.
func (s *Server) handleSettingsAccount(w http.ResponseWriter, r *http.Request, user auth.User) {
	if err := r.ParseForm(); err != nil {
		s.redirectBack(w, r, "/settings", "", errors.New("Could not read the form."))
		return
	}

	ctx := r.Context()
	username := strings.TrimSpace(r.PostFormValue("username"))
	current := r.PostFormValue("current_password")
	next := r.PostFormValue("new_password")

	if username != "" && username != user.Username {
		if err := s.auth.ChangeUsername(ctx, user.ID, username); err != nil {
			slog.Error("could not change username", "err", err)
			s.redirectBack(w, r, "/settings", "", errors.New("Could not change the username."))
			return
		}
		slog.Info("username changed", "from", user.Username, "to", username)
	}

	if next == "" {
		s.redirectBack(w, r, "/settings", "account saved", nil)
		return
	}

	if len(next) < 4 {
		s.redirectBack(w, r, "/settings", "", errors.New("New password must be at least 4 characters."))
		return
	}

	if err := s.auth.ChangePassword(ctx, user.ID, current, next); err != nil {
		if errors.Is(err, auth.ErrInvalidCredentials) {
			s.redirectBack(w, r, "/settings", "", errors.New("Current password is incorrect."))
			return
		}
		slog.Error("could not change password", "err", err)
		s.redirectBack(w, r, "/settings", "", errors.New("Could not change the password."))
		return
	}

	// ChangePassword revokes every session, including this one, so the cookie is
	// cleared and the operator lands back on the login screen rather than seeing
	// mysterious redirects.
	s.auth.ClearCookie(w)
	s.usingDefaultPass = false
	slog.Info("password changed; all sessions revoked")

	if r.Header.Get("HX-Request") == "true" {
		w.Header().Set("HX-Redirect", "/login")
		w.WriteHeader(http.StatusOK)
		return
	}
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

func isKnownSecret(name string) bool {
	for _, n := range config.AllSecretNames {
		if n == name {
			return true
		}
	}
	return false
}
