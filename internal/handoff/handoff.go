// Package handoff distills a source Agent session into a structured dossier
// that a target Agent can continue from.
//
// Extraction is deterministic and is deliberately richer than an
// Agent-generated summary: Agora holds the normalized event stream plus the
// provider's raw tool calls, so it can list exactly which files changed, which
// commands ran, which failed, and the live todo list. A model-written handoff
// would only restate a subset of that.
package handoff

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/delve8/agora/internal/adapter"
	"github.com/delve8/agora/internal/event"
	"github.com/delve8/agora/internal/session"
)

const (
	maxRecentRequests = 6
	maxCompleted      = 12
	maxCommands       = 20
	maxErrors         = 12
	maxChars          = 320
)

type Brief struct {
	Objective      string
	RecentRequests []string
	Completed      []string
	Files          []File
	Commands       []Command
	Todos          []Todo
	Errors         []string
	Environment    Environment
	Counts         Counts
}

type File struct {
	Path     string
	Ops      int
	ReadOnly bool
}

type Command struct {
	Command     string
	Description string
	Failed      bool
}

type Todo struct {
	Content string
	Status  string
}

type Environment struct {
	Agent     string
	Workspace string
	Device    string
	NativeID  string
	SessionID string
	StartedAt time.Time
	UpdatedAt time.Time
}

type Counts struct {
	User      int
	Assistant int
	Tool      int
}

// Extract builds the dossier from the source session metadata and its
// normalized event history.
func Extract(source session.Session, events []event.Event) Brief {
	brief := Brief{Environment: Environment{
		Agent:     source.Agent,
		Workspace: source.Workspace,
		Device:    source.DaemonID,
		NativeID:  source.NativeSessionURI(),
		SessionID: source.ID,
		StartedAt: source.CreatedAt,
		UpdatedAt: source.UpdatedAt,
	}}
	files := map[string]*File{}
	var order []string
	var completed []string
	var requests []string
	var commands []Command
	var errors []string
	var todos []Todo
	objective := ""

	touchFile := func(path string, readOnly bool) {
		path = strings.TrimSpace(path)
		if path == "" {
			return
		}
		value, ok := files[path]
		if !ok {
			value = &File{Path: path, ReadOnly: readOnly}
			files[path] = value
			order = append(order, path)
		}
		value.Ops++
		if !readOnly {
			value.ReadOnly = false
		}
	}

	for _, item := range events {
		switch item.Kind {
		case event.KindUser:
			if content := normalize(item.Content); content != "" {
				brief.Counts.User++
				if objective == "" {
					objective = content
				}
				requests = append(requests, content)
			}
		case event.KindAssistant:
			if content := normalize(item.Content); content != "" {
				brief.Counts.Assistant++
				completed = append(completed, content)
			}
		case event.KindTool:
			brief.Counts.Tool++
			if item.IsError {
				errors = append(errors, toolError(item))
			}
			input := parseToolInput(item.ToolInput)
			switch normalizeTool(item.ToolName) {
			case "write", "create", "writefile", "multiedit", "edit", "str_replace", "notebookedit", "apply_patch":
				touchFile(toolFilePath(input), false)
			case "read", "view":
				touchFile(toolFilePath(input), true)
			case "bash", "exec", "exec_command", "shell", "run":
				commands = append(commands, Command{Command: clip(toolCommand(input), maxChars), Description: clip(stringValue(input["description"]), 120), Failed: item.IsError})
			case "todowrite", "todo_write", "todo":
				if parsed := parseTodos(input); len(parsed) > 0 {
					todos = parsed
				}
			}
		case event.KindError:
			errors = append(errors, clip(normalize(item.Content), maxChars))
		}
	}

	brief.Objective = objective
	brief.RecentRequests = tailStrings(requests, maxRecentRequests)
	brief.Completed = tailStrings(completed, maxCompleted)
	brief.Todos = todos
	brief.Errors = dedupeTail(errors, maxErrors)

	values := make([]File, 0, len(order))
	for _, path := range order {
		values = append(values, *files[path])
	}
	sort.SliceStable(values, func(i, j int) bool {
		if values[i].ReadOnly != values[j].ReadOnly {
			return !values[i].ReadOnly
		}
		if values[i].Ops != values[j].Ops {
			return values[i].Ops > values[j].Ops
		}
		return values[i].Path < values[j].Path
	})
	brief.Files = values
	if len(commands) > maxCommands {
		commands = commands[len(commands)-maxCommands:]
	}
	brief.Commands = commands
	return brief
}

// Messages renders the dossier as the settled history of the target session.
// The target reads it as work it already did, so it does not re-derive it; the
// user drives the next turn.
func (b Brief) Messages() []adapter.HandoffMessage {
	return []adapter.HandoffMessage{
		{Role: "user", Content: "把当前工作整理成交接档案：已完成的工作、改动与产物、执行过的命令、待办和阻塞。之后会在另一台设备或另一个 Agent 上继续同一项工作。"},
		{Role: "assistant", Content: b.Render()},
	}
}

func (b Brief) Title() string {
	if b.Objective != "" {
		return "交接 · " + clip(b.Objective, 48)
	}
	if b.Environment.Workspace != "" {
		return "交接 · " + b.Environment.Workspace
	}
	return "交接会话"
}

func (b Brief) Render() string {
	var out strings.Builder
	out.WriteString("# 交接档案（由 Agora 从原始会话抽取）\n")
	if b.Objective != "" {
		out.WriteString("\n## 目标\n" + b.Objective + "\n")
	}
	if len(b.RecentRequests) > 0 {
		out.WriteString("\n## 最近的请求\n")
		for _, value := range b.RecentRequests {
			out.WriteString("- " + oneLine(value) + "\n")
		}
	}
	if len(b.Completed) > 0 {
		out.WriteString("\n## 已完成（结论，无需重新推导）\n")
		for _, value := range b.Completed {
			out.WriteString("- " + oneLine(value) + "\n")
		}
	}
	changed, readOnly := splitFiles(b.Files)
	if len(changed) > 0 {
		out.WriteString("\n## 改动 / 产物\n")
		for _, value := range changed {
			fmt.Fprintf(&out, "- %s（%d 次操作）\n", value.Path, value.Ops)
		}
	}
	if len(readOnly) > 0 {
		out.WriteString("\n## 只读过的文件\n")
		for _, value := range readOnly {
			out.WriteString("- " + value.Path + "\n")
		}
	}
	if len(b.Commands) > 0 {
		out.WriteString("\n## 执行过的命令\n")
		for _, value := range b.Commands {
			line := "- `" + oneLine(value.Command) + "`"
			if value.Description != "" {
				line += " — " + value.Description
			}
			if value.Failed {
				line += " **[失败]**"
			}
			out.WriteString(line + "\n")
		}
	}
	if len(b.Todos) > 0 {
		out.WriteString("\n## 待办\n")
		for _, value := range b.Todos {
			out.WriteString("- [" + value.Status + "] " + oneLine(value.Content) + "\n")
		}
	}
	if len(b.Errors) > 0 {
		out.WriteString("\n## 错误 / 阻塞\n")
		for _, value := range b.Errors {
			out.WriteString("- " + oneLine(value) + "\n")
		}
	}
	out.WriteString("\n## 环境\n")
	fmt.Fprintf(&out, "- Agent: %s\n- 工作区: %s\n", emptyFallback(b.Environment.Agent, "未知"), emptyFallback(b.Environment.Workspace, "未知"))
	if b.Environment.Device != "" {
		fmt.Fprintf(&out, "- 设备: %s\n", b.Environment.Device)
	}
	if b.Environment.StartedAt.IsZero() == false {
		fmt.Fprintf(&out, "- 时间: %s → %s\n", b.Environment.StartedAt.UTC().Format(time.RFC3339), b.Environment.UpdatedAt.UTC().Format(time.RFC3339))
	}
	fmt.Fprintf(&out, "- 统计: 用户 %d / 助手 %d / 工具 %d\n", b.Counts.User, b.Counts.Assistant, b.Counts.Tool)
	fmt.Fprintf(&out, "- 来源会话: %s\n", emptyFallback(b.Environment.SessionID, "未知"))
	return out.String()
}

func splitFiles(values []File) (changed, readOnly []File) {
	for _, value := range values {
		if value.ReadOnly {
			readOnly = append(readOnly, value)
		} else {
			changed = append(changed, value)
		}
	}
	return changed, readOnly
}

func normalizeTool(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	value = strings.ReplaceAll(value, "-", "_")
	return value
}

func parseToolInput(value string) map[string]any {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil
	}
	var result map[string]any
	if err := json.Unmarshal([]byte(value), &result); err != nil {
		return nil
	}
	return result
}

func toolFilePath(input map[string]any) string {
	for _, key := range []string{"file_path", "filePath", "path", "notebook_path", "absolute_path"} {
		if value := stringValue(input[key]); value != "" {
			return value
		}
	}
	return ""
}

func toolCommand(input map[string]any) string {
	for _, key := range []string{"command", "cmd", "script"} {
		if value := stringValue(input[key]); value != "" {
			return value
		}
	}
	return ""
}

func parseTodos(input map[string]any) []Todo {
	raw, ok := input["todos"].([]any)
	if !ok {
		return nil
	}
	values := make([]Todo, 0, len(raw))
	for _, item := range raw {
		entry, ok := item.(map[string]any)
		if !ok {
			continue
		}
		content := clip(stringValue(entry["content"]), maxChars)
		if content == "" {
			continue
		}
		values = append(values, Todo{Content: content, Status: strings.TrimSpace(stringValue(entry["status"]))})
	}
	return values
}

func toolError(item event.Event) string {
	detail := normalize(item.ToolOutput)
	if detail == "" {
		detail = normalize(item.Content)
	}
	name := strings.TrimSpace(item.ToolName)
	if name == "" {
		return clip(detail, maxChars)
	}
	return clip(name+": "+detail, maxChars)
}

func stringValue(value any) string {
	switch typed := value.(type) {
	case string:
		return strings.TrimSpace(typed)
	case json.Number:
		return typed.String()
	default:
		return ""
	}
}

func normalize(value string) string {
	return strings.TrimSpace(collapse(value))
}

func collapse(value string) string {
	return strings.Join(strings.Fields(value), " ")
}

func oneLine(value string) string {
	return clip(collapse(value), maxChars)
}

func clip(value string, limit int) string {
	value = strings.TrimSpace(value)
	if limit <= 0 || len([]rune(value)) <= limit {
		return value
	}
	runes := []rune(value)
	return string(runes[:limit]) + "…"
}

func tailStrings(values []string, limit int) []string {
	if limit > 0 && len(values) > limit {
		values = values[len(values)-limit:]
	}
	return values
}

func dedupeTail(values []string, limit int) []string {
	seen := map[string]bool{}
	result := make([]string, 0, len(values))
	for index := len(values) - 1; index >= 0; index-- {
		value := values[index]
		if value == "" || seen[value] {
			continue
		}
		seen[value] = true
		result = append(result, value)
	}
	// reverse back to chronological order
	for left, right := 0, len(result)-1; left < right; left, right = left+1, right-1 {
		result[left], result[right] = result[right], result[left]
	}
	if limit > 0 && len(result) > limit {
		result = result[len(result)-limit:]
	}
	return result
}

func emptyFallback(value, fallback string) string {
	if strings.TrimSpace(value) == "" {
		return fallback
	}
	return value
}
