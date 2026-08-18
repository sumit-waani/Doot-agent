package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// ---------------------------------------------------------------- planning

func toolCreateGoalPlan() toolDef {
	return toolDef{
		Name: "create_goal_plan",
		Description: "Propose a structured plan for a goal. The human must approve it before " +
			"any work starts, so make the tasks concrete and ordered. " +
			"Only call this when a plan was asked for.",
		Parameters: object(map[string]any{
			"title": stringProp("Short title for the goal."),
			"goal":  stringProp("What will be true when this is done."),
			"deliverables": arrayProp("Concrete, checkable outcomes.",
				map[string]any{"type": "string"}),
			"tasks": arrayProp("Ordered phases or subtasks.", object(map[string]any{
				"title":  stringProp("Short task title."),
				"detail": stringProp("What this task involves, and how to tell it is done."),
			}, "title")),
		}, "title", "goal", "tasks"),
		Run: func(ctx context.Context, tc toolContext, raw json.RawMessage) (toolResult, error) {
			var args struct {
				Title        string   `json:"title"`
				Goal         string   `json:"goal"`
				Deliverables []string `json:"deliverables"`
				Tasks        []struct {
					Title  string `json:"title"`
					Detail string `json:"detail"`
				} `json:"tasks"`
			}
			if err := decodeArgs(raw, &args); err != nil {
				return toolResult{}, err
			}
			if strings.TrimSpace(args.Title) == "" {
				return toolResult{}, errors.New("title is required")
			}
			if len(args.Tasks) == 0 {
				return toolResult{}, errors.New("a plan needs at least one task")
			}

			plan := Plan{
				Title:        args.Title,
				Goal:         args.Goal,
				Deliverables: args.Deliverables,
			}
			for _, t := range args.Tasks {
				if strings.TrimSpace(t.Title) == "" {
					return toolResult{}, errors.New("every task needs a title")
				}
				plan.Tasks = append(plan.Tasks, Task{Title: t.Title, Detail: t.Detail})
			}

			planID, err := tc.Service.store.createPlan(ctx, tc.Epoch, &tc.RunID, plan, string(raw))
			if err != nil {
				return toolResult{}, err
			}

			stored, err := tc.Service.store.loadPlan(ctx, planID)
			if err != nil {
				return toolResult{}, err
			}

			tc.Service.emit(ctx, &tc.RunID, eventTypePlanProposed, planPayload(stored))

			var b strings.Builder
			fmt.Fprintf(&b, "Plan %d created and awaiting the human's approval.\n\n", planID)
			fmt.Fprintf(&b, "%s\n%s\n\n", stored.Title, stored.Goal)
			for _, t := range stored.Tasks {
				fmt.Fprintf(&b, "%d. %s\n", t.Seq, t.Title)
			}
			b.WriteString("\nStop here. Do not start work until they approve.")

			return toolResult{
				Content:     b.String(),
				Preview:     fmt.Sprintf("%s · %d tasks", stored.Title, len(stored.Tasks)),
				PlanCreated: planID,
			}, nil
		},
	}
}

func toolStartTask() toolDef {
	return toolDef{
		Name:        "start_task",
		Description: "Mark a plan task as in progress before working on it.",
		PlanOnly:    true,
		Parameters: object(map[string]any{
			"task_number": intProp("The task's number in the plan."),
		}, "task_number"),
		Run: func(ctx context.Context, tc toolContext, raw json.RawMessage) (toolResult, error) {
			var args struct {
				TaskNumber int `json:"task_number"`
			}
			if err := decodeArgs(raw, &args); err != nil {
				return toolResult{}, err
			}

			task, plan, err := tc.Service.findTask(ctx, tc.Epoch, args.TaskNumber)
			if err != nil {
				return toolResult{}, err
			}

			if err := tc.Service.store.setTaskStatus(ctx, task.ID, TaskInProgress); err != nil {
				return toolResult{}, err
			}
			if plan.Status == PlanApproved {
				if err := tc.Service.store.setPlanStatus(ctx, plan.ID, PlanInProgress); err != nil {
					return toolResult{}, err
				}
			}

			tc.Service.emitTaskUpdate(ctx, tc.RunID, tc.Epoch)

			return toolResult{
				Content: fmt.Sprintf("Task %d (%s) is now in progress.", task.Seq, task.Title),
				Preview: fmt.Sprintf("started %d. %s", task.Seq, task.Title),
			}, nil
		},
	}
}

func toolCompleteTask() toolDef {
	return toolDef{
		Name: "complete_task",
		Description: "Mark a plan task done. Commit the work first. " +
			"Call review_code before this unless the task involved no code changes.",
		PlanOnly: true,
		Parameters: object(map[string]any{
			"task_number": intProp("The task's number in the plan."),
			"commit_sha":  stringProp("Commit this task produced, if any."),
			"summary":     stringProp("One line on what changed."),
		}, "task_number"),
		Run: func(ctx context.Context, tc toolContext, raw json.RawMessage) (toolResult, error) {
			var args struct {
				TaskNumber int    `json:"task_number"`
				CommitSHA  string `json:"commit_sha"`
				Summary    string `json:"summary"`
			}
			if err := decodeArgs(raw, &args); err != nil {
				return toolResult{}, err
			}

			task, plan, err := tc.Service.findTask(ctx, tc.Epoch, args.TaskNumber)
			if err != nil {
				return toolResult{}, err
			}

			if err := tc.Service.store.setTaskStatus(ctx, task.ID, TaskDone); err != nil {
				return toolResult{}, err
			}
			if args.CommitSHA != "" {
				// Recorded so the branch is reconstructible even if the sandbox is
				// later reset.
				if err := tc.Service.store.setTaskCommit(ctx, task.ID, args.CommitSHA); err != nil {
					return toolResult{}, err
				}
			}

			tc.Service.emitTaskUpdate(ctx, tc.RunID, tc.Epoch)

			refreshed, err := tc.Service.store.loadPlan(ctx, plan.ID)
			if err != nil {
				return toolResult{}, err
			}
			done, total := refreshed.Progress()

			content := fmt.Sprintf("Task %d done (%d/%d complete).", task.Seq, done, total)
			if next := refreshed.PendingTask(); next != nil {
				content += fmt.Sprintf(" Next up: %d. %s", next.Seq, next.Title)
			} else {
				content += " Every task is finished. Verify end to end, push, then report completion."
			}

			return toolResult{
				Content: content,
				Preview: fmt.Sprintf("done %d/%d", done, total),
			}, nil
		},
	}
}

func toolSkipTask() toolDef {
	return toolDef{
		Name:        "skip_task",
		Description: "Skip a plan task that turned out to be unnecessary. Explain why.",
		PlanOnly:    true,
		Parameters: object(map[string]any{
			"task_number": intProp("The task's number in the plan."),
			"reason":      stringProp("Why this task is not needed."),
		}, "task_number", "reason"),
		Run: func(ctx context.Context, tc toolContext, raw json.RawMessage) (toolResult, error) {
			var args struct {
				TaskNumber int    `json:"task_number"`
				Reason     string `json:"reason"`
			}
			if err := decodeArgs(raw, &args); err != nil {
				return toolResult{}, err
			}

			task, _, err := tc.Service.findTask(ctx, tc.Epoch, args.TaskNumber)
			if err != nil {
				return toolResult{}, err
			}

			if err := tc.Service.store.setTaskStatus(ctx, task.ID, TaskSkipped); err != nil {
				return toolResult{}, err
			}
			if err := tc.Service.store.setTaskReview(ctx, task.ID, "", "skipped: "+args.Reason); err != nil {
				return toolResult{}, err
			}

			tc.Service.emitTaskUpdate(ctx, tc.RunID, tc.Epoch)

			return toolResult{
				Content: fmt.Sprintf("Task %d skipped: %s", task.Seq, args.Reason),
				Preview: fmt.Sprintf("skipped %d", task.Seq),
			}, nil
		},
	}
}

// ---------------------------------------------------------------- human

func toolAskHuman() toolDef {
	return toolDef{
		Name: "ask_human",
		Description: "Pause and ask the human a question. The run stops until they reply, " +
			"so use it when you are genuinely blocked rather than to check in. " +
			"Always prefer asking over guessing on anything irreversible.",
		Parameters: object(map[string]any{
			"question": stringProp("What you need to know. Be specific and give them the context to answer."),
			"options": arrayProp("Concrete choices, if the answer is a decision between alternatives.",
				map[string]any{"type": "string"}),
		}, "question"),
		Run: func(ctx context.Context, tc toolContext, raw json.RawMessage) (toolResult, error) {
			var args struct {
				Question string   `json:"question"`
				Options  []string `json:"options"`
			}
			if err := decodeArgs(raw, &args); err != nil {
				return toolResult{}, err
			}
			if strings.TrimSpace(args.Question) == "" {
				return toolResult{}, errors.New("question is required")
			}

			// Surfaces as an ordinary message in the conversation. There is no
			// separate notification system: the run visibly waits.
			body := args.Question
			if len(args.Options) > 0 {
				body += "\n\n" + strings.Join(prefixed(args.Options, "- "), "\n")
			}

			if err := tc.Service.appendAssistantNote(ctx, tc.Epoch, tc.RunID, body); err != nil {
				return toolResult{}, err
			}

			return toolResult{
				Content:      "Asked the human and paused. Their answer will arrive as the next message.",
				Preview:      singleLine(args.Question),
				ParkForHuman: true,
			}, nil
		},
	}
}

func toolGoalComplete() toolDef {
	return toolDef{
		Name: "goal_complete",
		Description: "Declare the goal finished. Call only after verifying end to end and pushing. " +
			"Include the preview URL if there is a running app.",
		Parameters: object(map[string]any{
			"summary":     stringProp("What was built, and anything the human should know."),
			"preview_url": stringProp("Link to the running app, if there is one."),
		}, "summary"),
		Run: func(ctx context.Context, tc toolContext, raw json.RawMessage) (toolResult, error) {
			var args struct {
				Summary    string `json:"summary"`
				PreviewURL string `json:"preview_url"`
			}
			if err := decodeArgs(raw, &args); err != nil {
				return toolResult{}, err
			}

			if plan, err := tc.Service.store.activePlan(ctx, tc.Epoch); err == nil {
				if err := tc.Service.store.setPlanStatus(ctx, plan.ID, PlanCompleted); err != nil {
					return toolResult{}, err
				}
				tc.Service.emitTaskUpdate(ctx, tc.RunID, tc.Epoch)
			}

			body := args.Summary
			if args.PreviewURL != "" {
				body += "\n\nPreview: " + args.PreviewURL
			}
			if err := tc.Service.appendAssistantNote(ctx, tc.Epoch, tc.RunID, body); err != nil {
				return toolResult{}, err
			}

			return toolResult{
				Content:      "Goal marked complete.",
				Preview:      "goal complete",
				GoalComplete: true,
			}, nil
		},
	}
}

func prefixed(items []string, prefix string) []string {
	out := make([]string, 0, len(items))
	for _, s := range items {
		out = append(out, prefix+s)
	}
	return out
}

// findTask resolves a task number within the active plan.
func (s *Service) findTask(ctx context.Context, epoch, number int) (Task, Plan, error) {
	plan, err := s.store.activePlan(ctx, epoch)
	if err != nil {
		return Task{}, Plan{}, err
	}
	for _, t := range plan.Tasks {
		if t.Seq == number {
			return t, plan, nil
		}
	}
	return Task{}, plan, fmt.Errorf("plan has no task numbered %d (it has %d tasks)", number, len(plan.Tasks))
}

// planPayload renders a plan for the UI.
func planPayload(p Plan) map[string]any {
	tasks := make([]map[string]any, 0, len(p.Tasks))
	for _, t := range p.Tasks {
		tasks = append(tasks, map[string]any{
			"seq":     t.Seq,
			"title":   t.Title,
			"detail":  t.Detail,
			"status":  t.Status,
			"verdict": t.ReviewVerdict,
		})
	}

	done, total := p.Progress()
	return map[string]any{
		"id":           p.ID,
		"title":        p.Title,
		"goal":         p.Goal,
		"deliverables": p.Deliverables,
		"status":       p.Status,
		"tasks":        tasks,
		"done":         done,
		"total":        total,
	}
}
