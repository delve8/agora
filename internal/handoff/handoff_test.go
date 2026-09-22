package handoff

import (
	"strings"
	"testing"
	"time"

	"github.com/delve8/agora/internal/event"
	"github.com/delve8/agora/internal/session"
)

func TestExtractBuildsStructuredBrief(t *testing.T) {
	now := time.Now().UTC()
	events := []event.Event{
		{Kind: event.KindUser, Content: "实现会话管理功能"},
		{Kind: event.KindAssistant, Content: "我先看代码结构。", CreatedAt: now},
		{Kind: event.KindTool, ToolName: "Read", ToolInput: `{"file_path":"/repo/internal/server/server.go"}`},
		{Kind: event.KindTool, ToolName: "Edit", ToolInput: `{"file_path":"/repo/internal/server/server.go","old_string":"a","new_string":"b"}`},
		{Kind: event.KindTool, ToolName: "Write", ToolInput: `{"file_path":"/repo/web/src/SessionManager.tsx","content":"..."}`},
		{Kind: event.KindTool, ToolName: "Bash", ToolInput: `{"command":"go test ./...","description":"run tests"}`},
		{Kind: event.KindTool, ToolName: "Bash", ToolInput: `{"command":"npm run build"}`, IsError: true, ToolOutput: "error: missing module"},
		{Kind: event.KindTool, ToolName: "TodoWrite", ToolInput: `{"todos":[{"content":"补测试","status":"in_progress"},{"content":"更新文档","status":"pending"}]}`},
		{Kind: event.KindUser, Content: "顺手修一下 state 的可见性"},
		{Kind: event.KindAssistant, Content: "已完成会话管理、删除、加星和别名。", CreatedAt: now.Add(time.Minute)},
		{Kind: event.KindError, Content: "session host timeout"},
	}
	source := session.Session{ID: "daemon/d/claude://native-1", DaemonID: "d", Agent: "claude", Workspace: "/repo", CreatedAt: now, UpdatedAt: now}

	brief := Extract(source, events)

	if brief.Objective != "实现会话管理功能" {
		t.Fatalf("objective = %q", brief.Objective)
	}
	if len(brief.RecentRequests) != 2 || brief.RecentRequests[1] != "顺手修一下 state 的可见性" {
		t.Fatalf("recent requests = %+v", brief.RecentRequests)
	}
	if len(brief.Completed) != 2 {
		t.Fatalf("completed = %+v", brief.Completed)
	}
	// server.go was read+edited, so it is a changed file with two operations.
	var serverFile, webFile *File
	for index := range brief.Files {
		switch brief.Files[index].Path {
		case "/repo/internal/server/server.go":
			serverFile = &brief.Files[index]
		case "/repo/web/src/SessionManager.tsx":
			webFile = &brief.Files[index]
		}
	}
	if serverFile == nil || serverFile.Ops != 2 || serverFile.ReadOnly {
		t.Fatalf("server.go = %+v", serverFile)
	}
	if webFile == nil || webFile.ReadOnly {
		t.Fatalf("web file = %+v", webFile)
	}
	// Changed files lead read-only ones.
	if brief.Files[0].ReadOnly {
		t.Fatalf("files not ordered changed-first: %+v", brief.Files)
	}
	if len(brief.Commands) != 2 || brief.Commands[0].Command != "go test ./..." {
		t.Fatalf("commands = %+v", brief.Commands)
	}
	if !brief.Commands[1].Failed {
		t.Fatalf("failed command not marked: %+v", brief.Commands[1])
	}
	if len(brief.Todos) != 2 || brief.Todos[0].Status != "in_progress" {
		t.Fatalf("todos = %+v", brief.Todos)
	}
	if len(brief.Errors) < 2 {
		t.Fatalf("errors = %+v", brief.Errors)
	}

	rendered := brief.Render()
	for _, want := range []string{"交接档案", "## 目标", "## 改动 / 产物", "SessionManager.tsx", "go test ./...", "## 待办", "in_progress", "## 错误 / 阻塞"} {
		if !strings.Contains(rendered, want) {
			t.Fatalf("rendered brief missing %q:\n%s", want, rendered)
		}
	}
	messages := brief.Messages()
	if len(messages) != 2 || messages[0].Role != "user" || messages[1].Role != "assistant" {
		t.Fatalf("messages = %+v", messages)
	}
	if !strings.Contains(messages[1].Content, "交接档案") {
		t.Fatalf("assistant message is not the dossier: %q", messages[1].Content)
	}
}
