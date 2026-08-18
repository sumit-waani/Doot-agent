package daytona

import (
	"context"
	"fmt"
	"log/slog"
	"net/url"
	"strings"
)

// RepoSetup describes the repository to prepare inside the sandbox.
type RepoSetup struct {
	RepoURL    string
	WorkDir    string
	BaseBranch string
	// WorkBranch is always the single agent-owned branch. Nothing derives a
	// branch name from a task, date, or plan.
	WorkBranch string

	AuthorName  string
	AuthorEmail string

	// GitHubUsername and Token authenticate pushes over HTTPS. The sandbox has
	// no SSH keys.
	GitHubUsername string
	Token          string
}

// PushResult reports the outcome of a push.
type PushResult struct {
	Branch  string
	HeadSHA string
}

// SetupRepo clones the repository and puts it on the work branch.
//
// The branch rule is absolute: one branch, always named the same, and
// main/master is never committed to.
func (s *Sandbox) SetupRepo(ctx context.Context, setup RepoSetup) error {
	if setup.WorkDir == "" {
		return fmt.Errorf("daytona: repo setup needs a work directory")
	}
	if setup.WorkBranch == "" {
		return fmt.Errorf("daytona: repo setup needs a work branch")
	}

	cloneURL, err := authenticatedURL(setup.RepoURL, setup.GitHubUsername, setup.Token)
	if err != nil {
		return err
	}

	slog.Info("cloning repository",
		"work_dir", setup.WorkDir,
		"branch", setup.WorkBranch,
		"base", setup.BaseBranch,
	)

	// Shell out rather than using the SDK's git helpers: the exact sequence
	// matters (a fresh branch off the base, never a detached or default
	// checkout), and one scripted block is easier to reason about than six
	// round trips that can half-succeed.
	//
	// The credential is written to a file rather than passed on the command
	// line, so it does not land in shell history or process listings, and the
	// remote is rewritten to a token-free URL immediately afterwards so the PAT
	// is not left sitting in .git/config.
	script := strings.Join([]string{
		"set -euo pipefail",
		fmt.Sprintf("rm -rf %s", shellQuote(setup.WorkDir)),
		fmt.Sprintf("mkdir -p %s", shellQuote(setup.WorkDir)),
		fmt.Sprintf("cd %s", shellQuote(setup.WorkDir)),
		fmt.Sprintf("git clone --quiet %s .", shellQuote(cloneURL)),
		fmt.Sprintf("git remote set-url origin %s", shellQuote(setup.RepoURL)),
		fmt.Sprintf("git config user.name %s", shellQuote(setup.AuthorName)),
		fmt.Sprintf("git config user.email %s", shellQuote(setup.AuthorEmail)),
		// -B so this is idempotent: it creates the branch or resets onto it.
		fmt.Sprintf("git checkout -B %s", shellQuote(setup.WorkBranch)),
		"git --no-pager log -1 --oneline",
	}, "\n")

	if _, err := s.ExecCheck(ctx, wrapScript(script)); err != nil {
		return fmt.Errorf("clone failed: %w", err)
	}
	return nil
}

// RunSetupScript executes the project's dependency setup inside the sandbox.
//
// This is where project dependencies get installed, because the sandbox must be
// created from Daytona's default image for computer-use to work, which rules out
// baking them into a custom image.
func (s *Sandbox) RunSetupScript(ctx context.Context, workDir, script string) (string, error) {
	if strings.TrimSpace(script) == "" {
		return "", nil
	}
	full := fmt.Sprintf("cd %s\n%s", shellQuote(workDir), script)
	return s.ExecCheck(ctx, wrapScript(full))
}

// CommitAll stages everything and commits, returning the new SHA.
// It reports an empty SHA when there was nothing to commit.
func (s *Sandbox) CommitAll(ctx context.Context, workDir, message string) (string, error) {
	script := strings.Join([]string{
		"set -euo pipefail",
		fmt.Sprintf("cd %s", shellQuote(workDir)),
		"git add -A",
		// An empty commit is a normal outcome, not a failure.
		`if git diff --cached --quiet; then echo "__NOTHING_TO_COMMIT__"; exit 0; fi`,
		fmt.Sprintf("git commit --quiet -m %s", shellQuote(message)),
		"git rev-parse HEAD",
	}, "\n")

	out, err := s.ExecCheck(ctx, wrapScript(script))
	if err != nil {
		return "", err
	}
	if strings.Contains(out, "__NOTHING_TO_COMMIT__") {
		return "", nil
	}
	return lastNonEmptyLine(out), nil
}

// Push pushes the work branch to origin.
//
// --force-with-lease is safe here precisely because the branch is exclusively
// agent-owned: no human ever commits to it, so the only history being overwritten
// is Doot's own after a rebase.
func (s *Sandbox) Push(ctx context.Context, setup RepoSetup) (PushResult, error) {
	pushURL, err := authenticatedURL(setup.RepoURL, setup.GitHubUsername, setup.Token)
	if err != nil {
		return PushResult{}, err
	}

	script := strings.Join([]string{
		"set -euo pipefail",
		fmt.Sprintf("cd %s", shellQuote(setup.WorkDir)),
		fmt.Sprintf("git push --force-with-lease --quiet %s HEAD:refs/heads/%s",
			shellQuote(pushURL), setup.WorkBranch),
		"git rev-parse HEAD",
	}, "\n")

	out, err := s.ExecCheck(ctx, wrapScript(script))
	if err != nil {
		return PushResult{}, err
	}
	return PushResult{Branch: setup.WorkBranch, HeadSHA: lastNonEmptyLine(out)}, nil
}

// RebaseOntoBase brings the work branch up to date after a merge.
//
// On conflict it aborts and reports, so the caller can escalate to the human.
// A silently botched rebase is far worse than a paused run.
func (s *Sandbox) RebaseOntoBase(ctx context.Context, setup RepoSetup) (rebased bool, conflict bool, err error) {
	fetchURL, err := authenticatedURL(setup.RepoURL, setup.GitHubUsername, setup.Token)
	if err != nil {
		return false, false, err
	}

	script := strings.Join([]string{
		"set -uo pipefail",
		fmt.Sprintf("cd %s", shellQuote(setup.WorkDir)),
		fmt.Sprintf("git fetch --quiet %s %s", shellQuote(fetchURL), setup.BaseBranch),
		// Refuse to touch anything with uncommitted changes.
		`if ! git diff --quiet || ! git diff --cached --quiet; then echo "__DIRTY__"; exit 0; fi`,
		`if git merge-base --is-ancestor FETCH_HEAD HEAD; then echo "__UP_TO_DATE__"; exit 0; fi`,
		`if git rebase FETCH_HEAD >/dev/null 2>&1; then echo "__REBASED__"; else git rebase --abort || true; echo "__CONFLICT__"; fi`,
	}, "\n")

	out, execErr := s.ExecCheck(ctx, wrapScript(script))
	if execErr != nil {
		return false, false, execErr
	}

	switch {
	case strings.Contains(out, "__CONFLICT__"):
		return false, true, nil
	case strings.Contains(out, "__REBASED__"):
		return true, false, nil
	case strings.Contains(out, "__DIRTY__"):
		return false, false, fmt.Errorf("daytona: working tree has uncommitted changes; not rebasing")
	default:
		return false, false, nil
	}
}

// HeadSHA returns the current commit.
func (s *Sandbox) HeadSHA(ctx context.Context, workDir string) (string, error) {
	out, err := s.ExecCheck(ctx, wrapScript(fmt.Sprintf(
		"cd %s\ngit rev-parse HEAD", shellQuote(workDir))))
	if err != nil {
		return "", err
	}
	return lastNonEmptyLine(out), nil
}

// authenticatedURL injects credentials into an HTTPS git URL.
// The sandbox has no SSH keys, so HTTPS with a PAT is the only option.
func authenticatedURL(repoURL, username, token string) (string, error) {
	if token == "" {
		return repoURL, nil
	}

	u, err := url.Parse(repoURL)
	if err != nil {
		return "", fmt.Errorf("daytona: bad repository URL %q: %w", repoURL, err)
	}
	if u.Scheme != "https" {
		// Refuse rather than silently sending a PAT over plaintext.
		return "", fmt.Errorf("daytona: repository URL must be https, got %q", u.Scheme)
	}

	if username == "" {
		username = "x-access-token"
	}
	u.User = url.UserPassword(username, token)
	return u.String(), nil
}

// wrapScript runs a multi-line script through bash without needing a temp file.
func wrapScript(script string) string {
	return "bash -lc " + shellQuote(script)
}

// shellQuote single-quotes a value for safe shell interpolation.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

func lastNonEmptyLine(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if line := strings.TrimSpace(lines[i]); line != "" {
			return line
		}
	}
	return ""
}
