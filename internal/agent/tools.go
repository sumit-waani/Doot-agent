package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"

	"github.com/sumit-waani/doot/internal/daytona"
	"github.com/sumit-waani/doot/internal/llm"
	"github.com/sumit-waani/doot/internal/project"
)

// toolContext is what a tool implementation gets to work with.
type toolContext struct {
	Service *Service
	RunID   int64
	Epoch   int
	Project project.Project
	Sandbox *daytona.Sandbox
	Setup   daytona.RepoSetup
}

// toolResult is what a tool returns to the model.
type toolResult struct {
	// Content goes back to the model as the tool message.
	Content string

	// Preview is a short form for the UI timeline, so a 5,000-line build log does
	// not have to be rendered on a phone.
	Preview string

	// Control signals that change the loop's flow rather than the conversation.
	ParkForHuman bool
	PlanCreated  int64
	GoalComplete bool
}

// toolFunc implements a tool.
type toolFunc func(ctx context.Context, tc toolContext, args json.RawMessage) (toolResult, error)

// toolDef is a tool's schema plus its implementation.
type toolDef struct {
	Name        string
	Description string
	Parameters  map[string]any
	Run         toolFunc

	// NeedsSandbox tools are skipped when no sandbox is available, with a clear
	// message rather than a nil dereference.
	NeedsSandbox bool

	// PlanOnly tools are only offered while executing an approved plan.
	PlanOnly bool
}

// registry holds the tools available to the primary agent.
type registry struct {
	tools map[string]toolDef
}

func newRegistry() *registry {
	r := &registry{tools: map[string]toolDef{}}
	for _, t := range allTools() {
		r.tools[t.Name] = t
	}
	return r
}

// get looks up a tool by name.
func (r *registry) get(name string) (toolDef, bool) {
	t, ok := r.tools[name]
	return t, ok
}

// schemas returns the tool list for the model, filtered by context.
//
// Plan-management tools are hidden outside plan execution: an agent that can call
// complete_task with no plan will eventually try to.
func (r *registry) schemas(hasPlan bool) []llm.Tool {
	names := make([]string, 0, len(r.tools))
	for name, t := range r.tools {
		if t.PlanOnly && !hasPlan {
			continue
		}
		names = append(names, name)
	}
	// Stable order so the prompt is byte-identical between calls, which keeps
	// prompt caching effective.
	sort.Strings(names)

	out := make([]llm.Tool, 0, len(names))
	for _, name := range names {
		t := r.tools[name]
		out = append(out, llm.Tool{
			Name:        t.Name,
			Description: t.Description,
			Parameters:  t.Parameters,
		})
	}
	return out
}

// ---------------------------------------------------------------- schema helpers

// object builds a JSON Schema object.
func object(props map[string]any, required ...string) map[string]any {
	schema := map[string]any{
		"type":       "object",
		"properties": props,
	}
	if len(required) > 0 {
		schema["required"] = required
	} else {
		schema["required"] = []string{}
	}
	// Models are markedly better at staying inside a schema when extra keys are
	// explicitly disallowed.
	schema["additionalProperties"] = false
	return schema
}

func stringProp(description string) map[string]any {
	return map[string]any{"type": "string", "description": description}
}

func intProp(description string) map[string]any {
	return map[string]any{"type": "integer", "description": description}
}

func boolProp(description string) map[string]any {
	return map[string]any{"type": "boolean", "description": description}
}

func arrayProp(description string, items map[string]any) map[string]any {
	return map[string]any{"type": "array", "description": description, "items": items}
}

func enumProp(description string, values ...string) map[string]any {
	return map[string]any{"type": "string", "description": description, "enum": values}
}

// decodeArgs unmarshals tool arguments, tolerating an empty payload.
//
// Models routinely send "" or "{}" for no-argument tools, and both must work.
func decodeArgs(raw json.RawMessage, dst any) error {
	trimmed := string(raw)
	if trimmed == "" || trimmed == "null" {
		return nil
	}
	if err := json.Unmarshal(raw, dst); err != nil {
		return fmt.Errorf("arguments were not valid JSON: %w", err)
	}
	return nil
}

// errNoProject is returned when a tool needs a project and there is none.
var errNoProject = errors.New("agent: no project exists")
