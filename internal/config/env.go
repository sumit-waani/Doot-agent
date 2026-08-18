// Package config owns process configuration.
//
// The split is deliberate and matches docs/01-decisions.md:
//
//   - Env holds only what is needed to reach the database and decrypt secrets.
//   - Everything else lives in the settings table, editable from the UI.
//   - Credentials live in the secrets table, encrypted, also editable from the
//     UI. Environment variables act as first-boot seed values only.
package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
)

// Env is the complete set of environment variables Doot reads.
type Env struct {
	// Required.
	TursoDatabaseURL string
	TursoAuthToken   string
	MasterKey        string

	// Optional.
	Port       int
	LogLevel   string
	ResetAdmin bool
	Dev        bool

	// First-boot seed values for the secrets table. Once a secret exists in
	// the database these are ignored, so rotating from the UI is not undone by
	// a stale environment variable.
	SeedSecrets map[string]string
}

// Secret names as stored in the secrets table.
const (
	SecretLLMAPIKey         = "llm.api_key"
	SecretDaytonaAPIKey     = "daytona.api_key"
	SecretGitHubPAT         = "github.pat"
	SecretR2AccessKeyID     = "r2.access_key_id"
	SecretR2SecretAccessKey = "r2.secret_access_key"
)

// AllSecretNames is every secret Doot knows about, used by the Settings screen
// and by first-boot seeding.
var AllSecretNames = []string{
	SecretLLMAPIKey,
	SecretDaytonaAPIKey,
	SecretGitHubPAT,
	SecretR2AccessKeyID,
	SecretR2SecretAccessKey,
}

// seedEnvVar maps a secret name to the environment variable that can seed it.
var seedEnvVar = map[string]string{
	SecretLLMAPIKey:         "DOOT_LLM_API_KEY",
	SecretDaytonaAPIKey:     "DOOT_DAYTONA_API_KEY",
	SecretGitHubPAT:         "DOOT_GITHUB_PAT",
	SecretR2AccessKeyID:     "DOOT_R2_ACCESS_KEY_ID",
	SecretR2SecretAccessKey: "DOOT_R2_SECRET_ACCESS_KEY",
}

// LoadEnv reads configuration from the environment.
//
// It does not fail on missing required values; Validate reports those, so the
// caller can decide (a local run without Turso is useful, a deploy without it
// is not).
func LoadEnv() Env {
	e := Env{
		TursoDatabaseURL: os.Getenv("TURSO_DATABASE_URL"),
		TursoAuthToken:   os.Getenv("TURSO_AUTH_TOKEN"),
		MasterKey:        os.Getenv("DOOT_MASTER_KEY"),
		Port:             envInt("PORT", 8080),
		LogLevel:         envString("DOOT_LOG_LEVEL", "info"),
		ResetAdmin:       envBool("DOOT_RESET_ADMIN", false),
		Dev:              envBool("DOOT_DEV", false),
		SeedSecrets:      map[string]string{},
	}

	for name, varName := range seedEnvVar {
		if v := strings.TrimSpace(os.Getenv(varName)); v != "" {
			e.SeedSecrets[name] = v
		}
	}

	return e
}

// Validate reports configuration that would make the process unable to run
// correctly. All problems are returned together rather than one at a time.
func (e Env) Validate() error {
	var problems []string

	if strings.TrimSpace(e.TursoDatabaseURL) == "" {
		problems = append(problems, "TURSO_DATABASE_URL is not set")
	}
	if strings.TrimSpace(e.MasterKey) == "" {
		problems = append(problems,
			"DOOT_MASTER_KEY is not set (generate one with: doot genkey)")
	}
	if e.Port < 1 || e.Port > 65535 {
		problems = append(problems, fmt.Sprintf("PORT %d is out of range", e.Port))
	}

	if len(problems) == 0 {
		return nil
	}
	return fmt.Errorf("config: %s", strings.Join(problems, "; "))
}

func envString(key, def string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return def
}

func envInt(key string, def int) int {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return def
	}
	return n
}

func envBool(key string, def bool) bool {
	v := strings.ToLower(strings.TrimSpace(os.Getenv(key)))
	switch v {
	case "":
		return def
	case "1", "true", "yes", "on":
		return true
	case "0", "false", "no", "off":
		return false
	default:
		return def
	}
}
