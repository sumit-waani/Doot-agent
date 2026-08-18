package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/sumit-waani/doot/internal/llm"
)

// The two subagents are invoked as ordinary tools by the primary agent. They are
// not an orchestrator and they cannot act on their own: the reviewer returns an
// opinion, the verifier returns observations, and the primary agent decides what
// to do about either.

const reviewerPrompt = `You are reviewing a coding agent's work at the behavioural level, not the syntactic level.

You will be given a diff and the task it was meant to accomplish. Judge whether the change actually does what was intended and whether it breaks anything.

Look for:
- Logic that does not match the stated intent
- Cases the change does not handle: empty input, errors, concurrency, boundaries
- State that is written but never read, or read but never written
- Silent failures: swallowed errors, ignored return values
- Anything that contradicts the surrounding code's existing patterns

Do not comment on:
- Formatting, naming, or style preferences
- Missing tests, unless the change is obviously untestable as written
- Hypothetical future requirements

Respond with JSON only:
{"verdict":"clean"|"issues","findings":[{"severity":"high"|"medium"|"low","where":"file:line or function","what":"the problem","why":"why it matters"}],"summary":"one or two sentences"}

If the change is sound, return verdict "clean" with an empty findings array. Do not invent problems to appear useful.`

const verifierPrompt = `You are verifying that an application actually works, by driving its real interface.

You will be given a goal and a screenshot of the current screen. Decide the next action to take, one step at a time, and report what you observe.

You can:
- click at coordinates
- type text
- press a key
- scroll
- take another screenshot after an action settles
- finish, with a verdict

Rules:
- Coordinates are in the screenshot's own pixel space. Do not guess at a different resolution.
- After any action that changes the page, take a screenshot before deciding the next step.
- Report what you actually see, not what should be there. A blank page is a finding, not a loading state to assume away.
- Stop and report failure if the app is broken. Do not try to fix it; that is not your job.

Respond with JSON only:
{"action":"click"|"type"|"key"|"scroll"|"screenshot"|"finish","x":0,"y":0,"text":"","key":"","direction":"up"|"down","observation":"what you see now","verdict":"pass"|"fail","reason":"only when finishing"}`

// ---------------------------------------------------------------- reviewer

func toolReviewCode() toolDef {
	return toolDef{
		Name: "review_code",
		Description: "Ask a second agent to review the current uncommitted or recently committed changes. " +
			"Returns findings, not orders: fix what is genuinely wrong, and say so when a finding is a " +
			"false positive. Call after finishing a task that changed code.",
		NeedsSandbox: true,
		Parameters: object(map[string]any{
			"intent": stringProp("What the change was meant to accomplish."),
			"against": enumProp("What to review: uncommitted changes, or the last commit.",
				"uncommitted", "last_commit"),
			"task_number": intProp("Plan task this review belongs to, if any."),
		}, "intent"),
		Run: func(ctx context.Context, tc toolContext, raw json.RawMessage) (toolResult, error) {
			var args struct {
				Intent     string `json:"intent"`
				Against    string `json:"against"`
				TaskNumber int    `json:"task_number"`
			}
			if err := decodeArgs(raw, &args); err != nil {
				return toolResult{}, err
			}

			if !tc.Service.cfg.Bool("agent.reviewer_enabled", true) {
				return toolResult{
					Content: "Review is disabled in settings; continuing without it.",
					Preview: "reviewer disabled",
				}, nil
			}

			diffCmd := "git diff HEAD"
			if args.Against == "last_commit" {
				diffCmd = "git diff HEAD~1 HEAD"
			}

			out, _, err := tc.Sandbox.Exec(ctx, wrapShell(fmt.Sprintf(
				"cd %s && %s -- . ':(exclude)*.lock' ':(exclude)*-lock.json' | head -3000",
				shellQuote(tc.Project.WorkDir), diffCmd)))
			if err != nil {
				return toolResult{}, err
			}

			diff := strings.TrimSpace(out)
			if diff == "" {
				return toolResult{
					Content: "No changes to review.",
					Preview: "nothing to review",
				}, nil
			}

			review, err := tc.Service.runReviewer(ctx, tc, args.Intent, diff)
			if err != nil {
				// A failed reviewer must not fail the task. The primary agent is
				// told and continues; losing a second opinion is not losing work.
				slog.Warn("reviewer failed", "err", err)
				return toolResult{
					Content: "The reviewer could not be reached: " + err.Error() + ". Continuing without it.",
					Preview: "reviewer unavailable",
				}, nil
			}

			if args.TaskNumber > 0 {
				if task, _, err := tc.Service.findTask(ctx, tc.Epoch, args.TaskNumber); err == nil {
					verdict := VerdictClean
					if review.Verdict != "clean" {
						verdict = VerdictIssues
					}
					_ = tc.Service.store.setTaskReview(ctx, task.ID, verdict, review.Summary)
					tc.Service.emitTaskUpdate(ctx, tc.RunID, tc.Epoch)
				}
			}

			return toolResult{
				Content: review.render(),
				Preview: fmt.Sprintf("%s · %d findings", review.Verdict, len(review.Findings)),
			}, nil
		},
	}
}

// reviewFinding is one issue the reviewer raised.
type reviewFinding struct {
	Severity string `json:"severity"`
	Where    string `json:"where"`
	What     string `json:"what"`
	Why      string `json:"why"`
}

// reviewResult is the reviewer's response.
type reviewResult struct {
	Verdict  string          `json:"verdict"`
	Findings []reviewFinding `json:"findings"`
	Summary  string          `json:"summary"`
}

func (r reviewResult) render() string {
	var b strings.Builder

	fmt.Fprintf(&b, "Reviewer verdict: %s\n", r.Verdict)
	if r.Summary != "" {
		fmt.Fprintf(&b, "%s\n", r.Summary)
	}

	if len(r.Findings) == 0 {
		b.WriteString("\nNo findings.")
		return b.String()
	}

	b.WriteString("\nFindings:\n")
	for i, f := range r.Findings {
		fmt.Fprintf(&b, "%d. [%s] %s\n   %s\n   Why: %s\n", i+1, f.Severity, f.Where, f.What, f.Why)
	}

	b.WriteString("\nThese are opinions, not instructions. Fix what is genuinely wrong; " +
		"if a finding is mistaken, note why and move on.")
	return b.String()
}

// runReviewer performs the reviewer call.
func (s *Service) runReviewer(ctx context.Context, tc toolContext, intent, diff string) (reviewResult, error) {
	client, err := s.LLM(ctx)
	if err != nil {
		return reviewResult{}, err
	}

	user := fmt.Sprintf("Intent of the change:\n%s\n\nDiff:\n```diff\n%s\n```", intent, diff)

	// No tools: the reviewer reads a diff and answers. Giving it tools would
	// invite it to start changing the code it is meant to be judging.
	resp, err := client.Complete(ctx, llm.PurposeReviewer, []llm.Message{
		{Role: "system", Content: reviewerPrompt},
		{Role: "user", Content: user},
	}, nil, nil)
	if err != nil {
		return reviewResult{}, err
	}

	var out reviewResult
	if err := json.Unmarshal([]byte(extractJSON(resp.Content)), &out); err != nil {
		// Rather than discard a review we paid for, pass the prose through.
		return reviewResult{
			Verdict: "issues",
			Summary: strings.TrimSpace(resp.Content),
		}, nil
	}
	if out.Verdict == "" {
		out.Verdict = "clean"
	}
	return out, nil
}

// ---------------------------------------------------------------- e2e verifier

func toolVerifyE2E() toolDef {
	return toolDef{
		Name: "verify_e2e",
		Description: "Drive the real UI in the sandbox's desktop and report whether a flow works. " +
			"This is the most expensive tool available: call it once before declaring the goal " +
			"complete, and mid-task only when a change is both UI-facing and important.",
		NeedsSandbox: true,
		Parameters: object(map[string]any{
			"goal":      stringProp("The flow to verify, in concrete steps a person could follow."),
			"url":       stringProp("URL to open in the sandbox's browser."),
			"max_steps": intProp("Step budget. Defaults to 12."),
		}, "goal", "url"),
		Run: func(ctx context.Context, tc toolContext, raw json.RawMessage) (toolResult, error) {
			var args struct {
				Goal     string `json:"goal"`
				URL      string `json:"url"`
				MaxSteps int    `json:"max_steps"`
			}
			if err := decodeArgs(raw, &args); err != nil {
				return toolResult{}, err
			}
			if strings.TrimSpace(args.Goal) == "" || strings.TrimSpace(args.URL) == "" {
				return toolResult{}, errors.New("goal and url are both required")
			}
			if !tc.Service.cfg.Bool("agent.e2e_enabled", true) {
				return toolResult{
					Content: "End-to-end verification is disabled in settings; continuing without it.",
					Preview: "e2e disabled",
				}, nil
			}
			if args.MaxSteps <= 0 || args.MaxSteps > 30 {
				args.MaxSteps = tc.Service.cfg.Int("agent.e2e_max_steps", 12)
			}

			result, err := tc.Service.runVerifier(ctx, tc, args.Goal, args.URL, args.MaxSteps)
			if err != nil {
				slog.Warn("e2e verification failed to run", "err", err)
				return toolResult{
					Content: "End-to-end verification could not run: " + err.Error() +
						"\nTreat the flow as unverified; do not claim it works.",
					Preview: "e2e unavailable",
				}, nil
			}
			return result, nil
		},
	}
}

// verifierStep is one decision from the verifier.
type verifierStep struct {
	Action      string `json:"action"`
	X           int    `json:"x"`
	Y           int    `json:"y"`
	Text        string `json:"text"`
	Key         string `json:"key"`
	Direction   string `json:"direction"`
	Observation string `json:"observation"`
	Verdict     string `json:"verdict"`
	Reason      string `json:"reason"`
}

// runVerifier drives the desktop, one step at a time.
//
// The loop deliberately holds the screenshots outside the conversation it sends
// back to the primary agent: only the transcript of observations goes back, since
// re-sending images would multiply the cost of the most expensive tool.
func (s *Service) runVerifier(ctx context.Context, tc toolContext, goal, url string, maxSteps int) (toolResult, error) {
	client, err := s.LLM(ctx)
	if err != nil {
		return toolResult{}, err
	}

	// Computer-use processes do not survive a sandbox stop, so they are never
	// assumed to be running.
	if err := tc.Sandbox.EnsureDesktop(ctx); err != nil {
		return toolResult{}, err
	}

	display, err := tc.Sandbox.Display(ctx)
	if err != nil {
		return toolResult{}, fmt.Errorf("could not read screen geometry: %w", err)
	}

	if err := s.openInBrowser(ctx, tc, url); err != nil {
		return toolResult{}, err
	}

	messages := []llm.Message{{
		Role: "system",
		Content: fmt.Sprintf("%s\n\nThe screen is %dx%d pixels. All coordinates are in that space.",
			verifierPrompt, display.Width, display.Height),
	}}

	var observations []string
	recorded := 0

	for step := 1; step <= maxSteps; step++ {
		if err := ctx.Err(); err != nil {
			return toolResult{}, err
		}

		shot, err := tc.Sandbox.CompressedScreenshot(ctx, true)
		if err != nil {
			return toolResult{}, fmt.Errorf("screenshot failed: %w", err)
		}
		recorded++

		// Screenshots outlive the sandbox in R2 so a failed verification can be
		// examined afterwards, which is the fastest way to see why.
		if key, err := s.storeScreenshot(ctx, tc.RunID, shot); err != nil {
			slog.Debug("could not store screenshot", "err", err)
		} else {
			s.emit(ctx, &tc.RunID, eventTypeScreenshot, map[string]any{
				"key":   key,
				"step":  step,
				"width": shot.Width,
			})
		}

		messages = append(messages, llm.Message{
			Role: "user",
			Content: fmt.Sprintf("Goal: %s\nStep %d of %d. Screenshot attached as %s (%d bytes). "+
				"Describe what you see and choose the next action.",
				goal, step, maxSteps, shot.Format, len(shot.Data)),
		})

		resp, err := client.Complete(ctx, llm.PurposeE2E, messages, nil, nil)
		if err != nil {
			return toolResult{}, err
		}

		var decision verifierStep
		if err := json.Unmarshal([]byte(extractJSON(resp.Content)), &decision); err != nil {
			observations = append(observations, "verifier returned unparseable output: "+
				truncate(strings.TrimSpace(resp.Content), 400))
			break
		}

		if decision.Observation != "" {
			observations = append(observations, fmt.Sprintf("step %d: %s", step, decision.Observation))
		}
		messages = append(messages, llm.Message{Role: "assistant", Content: resp.Content})

		if decision.Action == "finish" {
			return verifierOutcome(decision, observations, recorded), nil
		}

		if err := s.applyVerifierAction(ctx, tc, decision); err != nil {
			observations = append(observations, fmt.Sprintf("step %d: action failed: %v", step, err))
			break
		}
	}

	// Reaching the step budget without a verdict is itself a finding: the flow
	// did not visibly complete.
	return verifierOutcome(verifierStep{
		Verdict: "fail",
		Reason:  fmt.Sprintf("did not reach a verdict within %d steps", maxSteps),
	}, observations, recorded), nil
}

func verifierOutcome(decision verifierStep, observations []string, screenshots int) toolResult {
	verdict := decision.Verdict
	if verdict == "" {
		verdict = "fail"
	}

	var b strings.Builder
	fmt.Fprintf(&b, "End-to-end verification: %s\n", strings.ToUpper(verdict))
	if decision.Reason != "" {
		fmt.Fprintf(&b, "%s\n", decision.Reason)
	}
	if len(observations) > 0 {
		b.WriteString("\nWhat it saw:\n")
		for _, o := range observations {
			fmt.Fprintf(&b, "- %s\n", o)
		}
	}
	if verdict != "pass" {
		b.WriteString("\nThe flow is not working. Fix the cause before declaring the goal complete.")
	}

	return toolResult{
		Content: b.String(),
		Preview: fmt.Sprintf("e2e %s · %d screenshots", verdict, screenshots),
	}
}

// applyVerifierAction performs one desktop action.
func (s *Service) applyVerifierAction(ctx context.Context, tc toolContext, step verifierStep) error {
	switch step.Action {
	case "click":
		return tc.Sandbox.Click(ctx, step.X, step.Y)
	case "type":
		return tc.Sandbox.TypeText(ctx, step.Text)
	case "key":
		return tc.Sandbox.PressKey(ctx, step.Key)
	case "scroll":
		direction := step.Direction
		if direction == "" {
			direction = "down"
		}
		return tc.Sandbox.Scroll(ctx, step.X, step.Y, direction, 3)
	case "screenshot", "":
		return nil
	default:
		return fmt.Errorf("unknown action %q", step.Action)
	}
}

// openInBrowser points the sandbox's browser at a URL.
func (s *Service) openInBrowser(ctx context.Context, tc toolContext, url string) error {
	// xdg-open is best-effort across the images; falling back to common browser
	// binaries avoids failing the whole verification over a missing alias.
	script := fmt.Sprintf(
		"(xdg-open %s || firefox %s || google-chrome %s || chromium %s) >/dev/null 2>&1 &",
		shellQuote(url), shellQuote(url), shellQuote(url), shellQuote(url))

	if _, _, err := tc.Sandbox.Exec(ctx, wrapShell(script)); err != nil {
		return fmt.Errorf("could not open the browser: %w", err)
	}
	return nil
}

// extractJSON pulls a JSON object out of a model response that may be wrapped in
// prose or a fenced code block.
func extractJSON(s string) string {
	trimmed := strings.TrimSpace(s)

	if fenced := strings.Index(trimmed, "```"); fenced >= 0 {
		rest := trimmed[fenced+3:]
		if nl := strings.IndexByte(rest, '\n'); nl >= 0 {
			rest = rest[nl+1:]
		}
		if end := strings.Index(rest, "```"); end >= 0 {
			trimmed = strings.TrimSpace(rest[:end])
		}
	}

	start := strings.IndexByte(trimmed, '{')
	end := strings.LastIndexByte(trimmed, '}')
	if start >= 0 && end > start {
		return trimmed[start : end+1]
	}
	return trimmed
}
