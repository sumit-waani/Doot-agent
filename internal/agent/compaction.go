package agent

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"github.com/sumit-waani/doot/internal/llm"
)

// compactionPrompt instructs the summariser.
//
// The summary is the only thing that survives into the next epoch, so it has to
// carry forward decisions and state rather than narrate what happened. Anything
// it omits is effectively forgotten.
const compactionPrompt = `You are compacting a coding agent's working context so it can continue with a smaller transcript.

Write a dense handover note for the agent that will continue this work. It is the ONLY thing carried forward: anything you leave out is lost.

Include:
- The goal, and where the work currently stands against it
- Decisions made and the reasoning behind them, especially ones that would otherwise be re-litigated
- Files created or changed, and what each change was for
- Commands, scripts, ports and paths that matter
- Problems hit and how they were resolved, so they are not repeated
- What remains to be done, concretely
- Anything the human asked for or objected to

Do not include:
- Narration of tool calls or their raw output
- Pleasantries, or a summary of the summary

Write in plain prose and lists. Be specific: names, paths, numbers. Prefer completeness over brevity.`

// shouldCompact reports whether the context is full enough to compact.
//
// The decision uses the prompt token count the API actually charged for, rather
// than a client-side estimate that would drift from the model's real accounting.
func (s *Service) shouldCompact(promptTokens int64) bool {
	window := s.llmContextWindow()
	if window <= 0 || promptTokens <= 0 {
		return false
	}

	threshold := float64(s.cfg.Int("agent.compact_threshold_pct", 80)) / 100
	if threshold <= 0 || threshold >= 1 {
		threshold = 0.8
	}

	return float64(promptTokens) >= float64(window)*threshold
}

// compact summarises the current epoch and rolls onto a new one.
//
// The old epoch is closed, not deleted: its messages stay queryable forever, and
// the summary becomes the first message of the new epoch.
func (s *Service) compact(ctx context.Context, runID *int64, epoch int, reason string) (newEpoch int, err error) {
	client, err := s.LLM(ctx)
	if err != nil {
		return 0, err
	}

	transcript, err := s.store.loadTranscript(ctx, epoch)
	if err != nil {
		return 0, err
	}
	if len(transcript) == 0 {
		return epoch, nil
	}

	rendered := renderTranscript(transcript)

	slog.Info("compacting context", "epoch", epoch, "messages", len(transcript), "reason", reason)
	s.emit(ctx, runID, eventTypeStatus, map[string]string{
		"state":   "compacting",
		"message": "Compressing context…",
	})

	messages := []llm.Message{
		{Role: "system", Content: compactionPrompt},
		{Role: "user", Content: rendered},
	}

	// No tools: this call must produce prose, and offering tools invites the
	// model to try to act instead of summarise.
	resp, err := client.Complete(ctx, llm.PurposeCompaction, messages, nil, nil)
	if err != nil {
		return 0, fmt.Errorf("agent: summarise for compaction: %w", err)
	}

	summary := strings.TrimSpace(resp.Content)
	if summary == "" {
		return 0, fmt.Errorf("agent: compaction produced an empty summary")
	}

	newEpoch, err = s.store.rollEpoch(ctx, epochReasonCompact, summary)
	if err != nil {
		return 0, err
	}

	// The summary is the first message of the new epoch, marked so the UI can
	// render it as a context boundary rather than as something the agent said.
	summaryMsg := llm.Message{
		Role:    "user",
		Content: "Context from earlier in this project, compacted:\n\n" + summary,
	}
	if _, err := s.store.appendMessage(ctx, newEpoch, runID, summaryMsg, true); err != nil {
		return 0, err
	}

	s.emit(ctx, runID, eventTypeEpoch, map[string]any{
		"epoch":    newEpoch,
		"previous": epoch,
		"reason":   epochReasonCompact,
		"summary":  summary,
	})

	slog.Info("context compacted", "from_epoch", epoch, "to_epoch", newEpoch,
		"summary_chars", len(summary))

	return newEpoch, nil
}

// renderTranscript flattens a conversation into text for summarisation.
//
// Tool results are truncated: a full build log adds length without adding
// information the handover note needs, and the summariser call has the same
// context limit as everything else.
func renderTranscript(messages []storedMessage) string {
	const maxToolResult = 2000

	var b strings.Builder
	for _, m := range messages {
		switch m.Role {
		case "system":
			continue

		case "user":
			b.WriteString("## Human\n")
			b.WriteString(m.Content)
			b.WriteString("\n\n")

		case "assistant":
			if content := strings.TrimSpace(m.Content); content != "" {
				b.WriteString("## Agent\n")
				b.WriteString(content)
				b.WriteString("\n\n")
			}
			for _, tc := range m.ToolCalls {
				fmt.Fprintf(&b, "## Agent called %s\n%s\n\n", tc.Name, truncate(tc.Arguments, 600))
			}

		case "tool":
			fmt.Fprintf(&b, "## Result\n%s\n\n", truncate(m.Content, maxToolResult))
		}
	}
	return b.String()
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + fmt.Sprintf("\n…[%d more characters]", len(s)-n)
}
