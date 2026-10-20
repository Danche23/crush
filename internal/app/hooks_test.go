package app

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/charmbracelet/crush/internal/config"
	"github.com/charmbracelet/crush/internal/hooks"
	"github.com/charmbracelet/crush/internal/permission"
	"github.com/stretchr/testify/require"
)

// recordingApp builds an App whose hooks append one line per event to
// a log file, so tests can assert on what fired and in what order.
func recordingApp(t *testing.T) (*App, func() []string) {
	t.Helper()
	dir := t.TempDir()
	logPath := filepath.Join(dir, "hooks.log")
	record := config.HookConfig{Command: `jq -r '[.hook_event_name, .tool_name, .decision // empty] | join(" ")' >> ` + logPath}
	cfg := map[string][]config.HookConfig{
		hooks.EventPermissionRequest: {record},
		hooks.EventPermissionResult:  {record},
	}
	app := &App{
		Permissions:     permission.NewPermissionService(dir, false, nil),
		serviceEventsWG: &sync.WaitGroup{},
		hookRunner:      hooks.NewRunner(hooks.Static(cfg), dir, dir),
	}
	return app, func() []string {
		data, _ := os.ReadFile(logPath)
		text := strings.TrimSpace(string(data))
		if text == "" {
			return nil
		}
		return strings.Split(text, "\n")
	}
}

// waitForLines polls the hook log until it holds at least want lines.
func waitForLines(t *testing.T, read func() []string, want int) []string {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if lines := read(); len(lines) >= want {
			return lines
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("expected %d hook lines, got %v", want, read())
	return nil
}

func TestPermissionHooksPairRequestWithResult(t *testing.T) {
	t.Parallel()
	app, read := recordingApp(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	app.runPermissionHooks(ctx)

	requests := app.Permissions.Subscribe(ctx)
	go func() {
		_, _ = app.Permissions.Request(ctx, permission.CreatePermissionRequest{
			SessionID: "s", ToolCallID: "c1", ToolName: "bash", Action: "execute", Path: t.TempDir(),
			Params: map[string]string{"command": "ls"},
		})
	}()
	pending := (<-requests).Payload
	require.True(t, app.Permissions.Deny(pending))

	lines := waitForLines(t, read, 2)
	require.Equal(t, "PermissionRequest bash", lines[0])
	require.Equal(t, "PermissionResult bash denied", lines[1])
}

func TestPermissionHooksCancelledAndAutoApproved(t *testing.T) {
	t.Parallel()
	app, read := recordingApp(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	app.runPermissionHooks(ctx)

	// An auto-approved session never shows a prompt, so neither hook
	// fires for it.
	app.Permissions.AutoApproveSession("auto")
	granted, err := app.Permissions.Request(ctx, permission.CreatePermissionRequest{
		SessionID: "auto", ToolCallID: "c0", ToolName: "view", Action: "read", Path: t.TempDir(),
	})
	require.NoError(t, err)
	require.True(t, granted)

	requests := app.Permissions.Subscribe(ctx)
	reqCtx, cancelReq := context.WithCancel(ctx)
	go func() {
		_, _ = app.Permissions.Request(reqCtx, permission.CreatePermissionRequest{
			SessionID: "s", ToolCallID: "c2", ToolName: "edit", Action: "write", Path: t.TempDir(),
		})
	}()
	<-requests
	cancelReq()

	lines := waitForLines(t, read, 2)
	require.Equal(t, "PermissionRequest edit", lines[0])
	require.Equal(t, "PermissionResult edit cancelled", lines[1])
}
