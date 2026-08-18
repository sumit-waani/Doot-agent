package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"path"
	"strings"
)

// allTools returns every tool the primary agent can call.
func allTools() []toolDef {
	return []toolDef{
		toolListFiles(),
		toolReadFile(),
		toolWriteFile(),
		toolEditFile(),
		toolSearch(),
		toolExec(),
		toolCommit(),
		toolPushBranch(),
		toolPreviewURL(),
		toolSandboxInfo(),

		toolCreateGoalPlan(),
		toolStartTask(),
		toolCompleteTask(),
		toolSkipTask(),

		toolReviewCode(),
		toolVerifyE2E(),

		toolAskHuman(),
		toolGoalComplete(),
	}
}

// resolvePath keeps tool paths inside the project checkout.
//
// The agent is trusted, but a relative path that escapes the work directory is
// usually a mistake rather than intent, and silently writing outside the repo
// produces changes that no commit will ever capture.
func resolvePath(workDir, p string) (string, error) {
	if strings.TrimSpace(p) == "" {
		return "", fmt.Errorf("path is required")
	}

	joined := p
	if !path.IsAbs(p) {
		joined = path.Join(workDir, p)
	}
	clean := path.Clean(joined)

	if clean != workDir && !strings.HasPrefix(clean, workDir+"/") {
		return "", fmt.Errorf("path %q is outside the project directory %s", p, workDir)
	}
	return clean, nil
}

// ---------------------------------------------------------------- filesystem

func toolListFiles() toolDef {
	return toolDef{
		Name: "list_files",
		Description: "List files and directories in the project. Respects .gitignore. " +
			"Use this to explore before reading.",
		NeedsSandbox: true,
		Parameters: object(map[string]any{
			"path":  stringProp("Directory relative to the project root. Defaults to the root."),
			"depth": intProp("How deep to recurse. Defaults to 2."),
		}),
		Run: func(ctx context.Context, tc toolContext, raw json.RawMessage) (toolResult, error) {
			var args struct {
				Path  string `json:"path"`
				Depth int    `json:"depth"`
			}
			if err := decodeArgs(raw, &args); err != nil {
				return toolResult{}, err
			}
			if args.Depth <= 0 {
				args.Depth = 2
			}

			target, err := resolvePath(tc.Project.WorkDir, orDefault(args.Path, "."))
			if err != nil {
				return toolResult{}, err
			}

			// git ls-files honours .gitignore, which keeps node_modules and build
			// output out of the context window.
			cmd := fmt.Sprintf(
				"cd %s && { git ls-files --cached --others --exclude-standard 2>/dev/null || find . -type f; } "+
					"| awk -F/ 'NF<=%d' | head -400",
				shellQuote(target), args.Depth)

			out, _, err := tc.Sandbox.Exec(ctx, wrapShell(cmd))
			if err != nil {
				return toolResult{}, err
			}

			listing := strings.TrimSpace(out)
			if listing == "" {
				listing = "(no files)"
			}
			return toolResult{
				Content: listing,
				Preview: fmt.Sprintf("%d entries", len(strings.Split(listing, "\n"))),
			}, nil
		},
	}
}

func toolReadFile() toolDef {
	return toolDef{
		Name:         "read_file",
		Description:  "Read a file from the project. Returns numbered lines so edits can reference them.",
		NeedsSandbox: true,
		Parameters: object(map[string]any{
			"path":   stringProp("File path relative to the project root."),
			"offset": intProp("First line to read, 1-based. Defaults to 1."),
			"limit":  intProp("How many lines to read. Defaults to 400."),
		}, "path"),
		Run: func(ctx context.Context, tc toolContext, raw json.RawMessage) (toolResult, error) {
			var args struct {
				Path   string `json:"path"`
				Offset int    `json:"offset"`
				Limit  int    `json:"limit"`
			}
			if err := decodeArgs(raw, &args); err != nil {
				return toolResult{}, err
			}
			if args.Offset <= 0 {
				args.Offset = 1
			}
			if args.Limit <= 0 {
				args.Limit = 400
			}

			target, err := resolvePath(tc.Project.WorkDir, args.Path)
			if err != nil {
				return toolResult{}, err
			}

			cmd := fmt.Sprintf("sed -n '%d,%dp' %s | cat -n | sed 's/^/    /'",
				args.Offset, args.Offset+args.Limit-1, shellQuote(target))

			out, code, err := tc.Sandbox.Exec(ctx, wrapShell(cmd))
			if err != nil {
				return toolResult{}, err
			}
			if code != 0 {
				return toolResult{}, fmt.Errorf("could not read %s: %s", args.Path, strings.TrimSpace(out))
			}
			if strings.TrimSpace(out) == "" {
				return toolResult{
					Content: fmt.Sprintf("%s is empty or has no lines in that range.", args.Path),
					Preview: args.Path + " (empty)",
				}, nil
			}

			return toolResult{
				Content: out,
				Preview: fmt.Sprintf("%s from line %d", args.Path, args.Offset),
			}, nil
		},
	}
}

func toolWriteFile() toolDef {
	return toolDef{
		Name: "write_file",
		Description: "Create a file or replace its entire contents. Creates parent directories. " +
			"For a targeted change to an existing file, prefer edit_file.",
		NeedsSandbox: true,
		Parameters: object(map[string]any{
			"path":    stringProp("File path relative to the project root."),
			"content": stringProp("Full file contents."),
		}, "path", "content"),
		Run: func(ctx context.Context, tc toolContext, raw json.RawMessage) (toolResult, error) {
			var args struct {
				Path    string `json:"path"`
				Content string `json:"content"`
			}
			if err := decodeArgs(raw, &args); err != nil {
				return toolResult{}, err
			}

			target, err := resolvePath(tc.Project.WorkDir, args.Path)
			if err != nil {
				return toolResult{}, err
			}

			// Written via a quoted heredoc so the content is never interpreted by
			// the shell: backticks and $ in source code would otherwise expand.
			script := fmt.Sprintf(
				"mkdir -p %s && cat > %s <<'DOOT_EOF_MARKER'\n%s\nDOOT_EOF_MARKER",
				shellQuote(path.Dir(target)), shellQuote(target), args.Content)

			if _, err := tc.Sandbox.ExecCheck(ctx, wrapShell(script)); err != nil {
				return toolResult{}, err
			}

			lines := strings.Count(args.Content, "\n") + 1
			return toolResult{
				Content: fmt.Sprintf("Wrote %s (%d lines).", args.Path, lines),
				Preview: fmt.Sprintf("%s · %d lines", args.Path, lines),
			}, nil
		},
	}
}

func toolEditFile() toolDef {
	return toolDef{
		Name: "edit_file",
		Description: "Replace an exact string in a file. old_string must appear exactly once " +
			"unless replace_all is true. Include surrounding context to make it unique.",
		NeedsSandbox: true,
		Parameters: object(map[string]any{
			"path":        stringProp("File path relative to the project root."),
			"old_string":  stringProp("Exact text to find, including whitespace and indentation."),
			"new_string":  stringProp("Replacement text."),
			"replace_all": boolProp("Replace every occurrence. Defaults to false."),
		}, "path", "old_string", "new_string"),
		Run: func(ctx context.Context, tc toolContext, raw json.RawMessage) (toolResult, error) {
			var args struct {
				Path       string `json:"path"`
				OldString  string `json:"old_string"`
				NewString  string `json:"new_string"`
				ReplaceAll bool   `json:"replace_all"`
			}
			if err := decodeArgs(raw, &args); err != nil {
				return toolResult{}, err
			}
			if args.OldString == "" {
				return toolResult{}, fmt.Errorf("old_string must not be empty; use write_file to create a file")
			}

			target, err := resolvePath(tc.Project.WorkDir, args.Path)
			if err != nil {
				return toolResult{}, err
			}

			// Done in Python rather than sed: the strings are literal, may span
			// lines, and must not be treated as regular expressions.
			script := fmt.Sprintf(`python3 - <<'DOOT_EOF_MARKER'
import io, sys
path = %s
old = %s
new = %s
replace_all = %s

with io.open(path, encoding='utf-8') as f:
    body = f.read()

count = body.count(old)
if count == 0:
    sys.stderr.write('old_string not found in the file\n')
    sys.exit(3)
if count > 1 and not replace_all:
    sys.stderr.write('old_string appears %%d times; add surrounding context or set replace_all\n' %% count)
    sys.exit(4)

body = body.replace(old, new) if replace_all else body.replace(old, new, 1)
with io.open(path, 'w', encoding='utf-8') as f:
    f.write(body)
print('replaced %%d occurrence(s)' %% (count if replace_all else 1))
DOOT_EOF_MARKER`,
				pyLiteral(target), pyLiteral(args.OldString), pyLiteral(args.NewString),
				pyBool(args.ReplaceAll))

			out, code, err := tc.Sandbox.Exec(ctx, wrapShell(script))
			if err != nil {
				return toolResult{}, err
			}
			if code != 0 {
				return toolResult{}, fmt.Errorf("edit failed: %s", strings.TrimSpace(out))
			}

			return toolResult{
				Content: fmt.Sprintf("Edited %s: %s", args.Path, strings.TrimSpace(out)),
				Preview: args.Path,
			}, nil
		},
	}
}

func toolSearch() toolDef {
	return toolDef{
		Name:         "search",
		Description:  "Search file contents with a regular expression. Returns matching lines with file and line number.",
		NeedsSandbox: true,
		Parameters: object(map[string]any{
			"pattern": stringProp("Regular expression to search for."),
			"path":    stringProp("Directory to search, relative to the project root. Defaults to the root."),
			"glob":    stringProp("Restrict to matching filenames, e.g. '*.go'."),
		}, "pattern"),
		Run: func(ctx context.Context, tc toolContext, raw json.RawMessage) (toolResult, error) {
			var args struct {
				Pattern string `json:"pattern"`
				Path    string `json:"path"`
				Glob    string `json:"glob"`
			}
			if err := decodeArgs(raw, &args); err != nil {
				return toolResult{}, err
			}
			if strings.TrimSpace(args.Pattern) == "" {
				return toolResult{}, fmt.Errorf("pattern is required")
			}

			target, err := resolvePath(tc.Project.WorkDir, orDefault(args.Path, "."))
			if err != nil {
				return toolResult{}, err
			}

			include := ""
			if args.Glob != "" {
				include = fmt.Sprintf("--include=%s ", shellQuote(args.Glob))
			}

			// grep exits 1 when nothing matches, which is a valid result, not a
			// failure; || true keeps it from being reported as an error.
			cmd := fmt.Sprintf(
				"cd %s && grep -rnI %s-E %s . 2>/dev/null | head -100 || true",
				shellQuote(target), include, shellQuote(args.Pattern))

			out, _, err := tc.Sandbox.Exec(ctx, wrapShell(cmd))
			if err != nil {
				return toolResult{}, err
			}

			matches := strings.TrimSpace(out)
			if matches == "" {
				return toolResult{
					Content: "No matches.",
					Preview: "no matches",
				}, nil
			}
			return toolResult{
				Content: matches,
				Preview: fmt.Sprintf("%d matches", len(strings.Split(matches, "\n"))),
			}, nil
		},
	}
}

// ---------------------------------------------------------------- exec

func toolExec() toolDef {
	return toolDef{
		Name: "exec",
		Description: "Run a shell command in the project directory inside the sandbox. " +
			"Use for builds, tests, package managers and git inspection. " +
			"Long-running servers must be started with a trailing & so they do not block.",
		NeedsSandbox: true,
		Parameters: object(map[string]any{
			"command": stringProp("Shell command to run."),
			"purpose": stringProp("One short line on why, shown in the UI timeline."),
		}, "command"),
		Run: func(ctx context.Context, tc toolContext, raw json.RawMessage) (toolResult, error) {
			var args struct {
				Command string `json:"command"`
				Purpose string `json:"purpose"`
			}
			if err := decodeArgs(raw, &args); err != nil {
				return toolResult{}, err
			}
			if strings.TrimSpace(args.Command) == "" {
				return toolResult{}, fmt.Errorf("command is required")
			}

			script := fmt.Sprintf("cd %s\n%s", shellQuote(tc.Project.WorkDir), args.Command)
			out, code, err := tc.Sandbox.Exec(ctx, wrapShell(script))
			if err != nil {
				return toolResult{}, err
			}

			// A non-zero exit is information for the model, not a tool failure:
			// failing tests are exactly what it needs to see and act on.
			body := strings.TrimSpace(out)
			if body == "" {
				body = "(no output)"
			}
			content := fmt.Sprintf("exit code: %d\n\n%s", code, truncate(body, 20000))

			preview := orDefault(args.Purpose, singleLine(args.Command))
			if code != 0 {
				preview = fmt.Sprintf("exit %d · %s", code, preview)
			}

			return toolResult{Content: content, Preview: preview}, nil
		},
	}
}

func toolSandboxInfo() toolDef {
	return toolDef{
		Name: "sandbox_info",
		Description: "Report the sandbox's real CPU, memory and disk limits, read from cgroups. " +
			"Use this instead of nproc or free, which report the host's values.",
		NeedsSandbox: true,
		Parameters:   object(map[string]any{}),
		Run: func(ctx context.Context, tc toolContext, raw json.RawMessage) (toolResult, error) {
			cores, memBytes, err := tc.Sandbox.Resources(ctx)
			if err != nil {
				return toolResult{}, err
			}

			disk, _, _ := tc.Sandbox.Exec(ctx, wrapShell("df -h / | tail -1"))

			content := fmt.Sprintf("CPU: %.2f cores\nMemory: %.2f GiB\nDisk: %s",
				cores, float64(memBytes)/(1024*1024*1024), strings.TrimSpace(disk))

			return toolResult{
				Content: content,
				Preview: fmt.Sprintf("%.2f cores, %.1f GiB", cores, float64(memBytes)/(1024*1024*1024)),
			}, nil
		},
	}
}

// ---------------------------------------------------------------- git

func toolCommit() toolDef {
	return toolDef{
		Name: "commit_changes",
		Description: "Stage everything and commit on the doot branch. " +
			"Commit after each task rather than batching. Returns the commit SHA, " +
			"or reports that there was nothing to commit.",
		NeedsSandbox: true,
		Parameters: object(map[string]any{
			"message": stringProp("Commit message. First line under 72 characters."),
		}, "message"),
		Run: func(ctx context.Context, tc toolContext, raw json.RawMessage) (toolResult, error) {
			var args struct {
				Message string `json:"message"`
			}
			if err := decodeArgs(raw, &args); err != nil {
				return toolResult{}, err
			}
			if strings.TrimSpace(args.Message) == "" {
				return toolResult{}, fmt.Errorf("message is required")
			}

			sha, err := tc.Sandbox.CommitAll(ctx, tc.Project.WorkDir, args.Message)
			if err != nil {
				return toolResult{}, err
			}
			if sha == "" {
				return toolResult{
					Content: "Nothing to commit; the working tree is clean.",
					Preview: "nothing to commit",
				}, nil
			}

			short := sha
			if len(short) > 8 {
				short = short[:8]
			}
			return toolResult{
				Content: fmt.Sprintf("Committed %s: %s", short, singleLine(args.Message)),
				Preview: short,
			}, nil
		},
	}
}

func toolPushBranch() toolDef {
	return toolDef{
		Name: "push_branch",
		Description: "Push the doot branch to the remote and, if enabled, try to open a pull request. " +
			"Call once the goal is complete, not after every task. " +
			"A failed pull request is not a failure: the branch is still pushed.",
		NeedsSandbox: true,
		Parameters: object(map[string]any{
			"title": stringProp("Pull request title. Defaults to the plan title."),
			"body":  stringProp("Pull request body: what changed and why."),
		}),
		Run: func(ctx context.Context, tc toolContext, raw json.RawMessage) (toolResult, error) {
			var args struct {
				Title string `json:"title"`
				Body  string `json:"body"`
			}
			if err := decodeArgs(raw, &args); err != nil {
				return toolResult{}, err
			}
			return tc.Service.pushAndOpenPR(ctx, tc, args.Title, args.Body)
		},
	}
}

func toolPreviewURL() toolDef {
	return toolDef{
		Name: "preview_url",
		Description: "Get a shareable link to a port inside the sandbox. " +
			"Use after starting the dev server, to show the human the running app.",
		NeedsSandbox: true,
		Parameters: object(map[string]any{
			"port": intProp("Port inside the sandbox. Defaults to the project's dev port."),
		}),
		Run: func(ctx context.Context, tc toolContext, raw json.RawMessage) (toolResult, error) {
			var args struct {
				Port int `json:"port"`
			}
			if err := decodeArgs(raw, &args); err != nil {
				return toolResult{}, err
			}

			port := args.Port
			if port == 0 {
				port = tc.Project.DevPort
			}
			if port == 0 {
				return toolResult{}, fmt.Errorf("no port given and the project has no dev port configured")
			}

			url, err := tc.Service.projects.PreviewURL(ctx, port)
			if err != nil {
				return toolResult{}, err
			}

			tc.Service.emit(ctx, &tc.RunID, eventTypePreview, map[string]any{
				"url":  url,
				"port": port,
			})

			return toolResult{
				Content: fmt.Sprintf("Preview URL for port %d: %s", port, url),
				Preview: fmt.Sprintf("port %d", port),
			}, nil
		},
	}
}

// ---------------------------------------------------------------- shell helpers

func wrapShell(script string) string {
	return "bash -lc " + shellQuote(script)
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// pyLiteral renders a Go string as a Python literal, so arbitrary file content
// can be embedded without escaping bugs.
func pyLiteral(s string) string {
	encoded, err := json.Marshal(s)
	if err != nil {
		return `""`
	}
	return string(encoded)
}

func pyBool(b bool) string {
	if b {
		return "True"
	}
	return "False"
}

func orDefault(v, def string) string {
	if strings.TrimSpace(v) == "" {
		return def
	}
	return v
}
