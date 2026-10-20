package app

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/charmbracelet/crush/internal/agent"
	"github.com/charmbracelet/crush/internal/hooks"
	"github.com/charmbracelet/crush/internal/permission"
	"github.com/charmbracelet/crush/internal/session"
)

// sessionEndTimeout bounds the SessionEnd hooks so a slow script cannot
// hold the process open on exit.
const sessionEndTimeout = 2 * time.Second

// CreateSession creates a new session and fires SessionStart. Every
// session creation path goes through here so the hook is consistent.
func (app *App) CreateSession(ctx context.Context, title string) (session.Session, error) {
	sess, err := app.Sessions.Create(ctx, title)
	if err != nil {
		return session.Session{}, err
	}
	app.hookRunner.Run(ctx, hooks.Event{Name: hooks.EventSessionStart, SessionID: sess.ID, Source: "startup"})
	return sess, nil
}

// resolveSession resolves which session to use for a non-interactive
// run: the given session, the most recent top-level one, or a new one.
// Resumed sessions fire SessionStart with source "resume".
func (app *App) resolveSession(ctx context.Context, continueSessionID string, useLast bool) (session.Session, error) {
	var sess session.Session
	var err error
	switch {
	case continueSessionID != "":
		if app.Sessions.IsAgentToolSession(continueSessionID) {
			return session.Session{}, fmt.Errorf("cannot continue an agent tool session: %s", continueSessionID)
		}
		sess, err = app.Sessions.Get(ctx, continueSessionID)
		if err != nil {
			return session.Session{}, fmt.Errorf("session not found: %s", continueSessionID)
		}
		if sess.ParentSessionID != "" {
			return session.Session{}, fmt.Errorf("cannot continue a child session: %s", continueSessionID)
		}
	case useLast:
		sess, err = app.Sessions.GetLast(ctx)
		if err != nil {
			return session.Session{}, fmt.Errorf("no sessions found to continue")
		}
	default:
		return app.CreateSession(ctx, agent.DefaultSessionName)
	}
	app.hookRunner.Run(ctx, hooks.Event{Name: hooks.EventSessionStart, SessionID: sess.ID, Source: "resume"})
	return sess, nil
}

// fireSessionEnd fires SessionEnd for the session the user was working
// in. It runs after agents have stopped so Interrupt precedes it.
func (app *App) fireSessionEnd() {
	ctx, cancel := context.WithTimeout(context.Background(), sessionEndTimeout)
	defer cancel()
	sessionID, _ := app.currentSessionID.Load().(string)
	app.hookRunner.Run(ctx, hooks.Event{
		Name:      hooks.EventSessionEnd,
		SessionID: sessionID,
		Reason:    "exit",
	})
}

// runPermissionHooks fires PermissionRequest when a prompt is shown and
// PermissionResult when that prompt is granted, denied, or cancelled.
// Subscribing to the permission service's own events keeps the hooks
// off the service's lock and out of the agent's path; auto-approved
// calls never show a prompt and so fire neither.
func (app *App) runPermissionHooks(ctx context.Context) {
	requests := app.Permissions.Subscribe(ctx)
	results := app.Permissions.SubscribeNotifications(ctx)
	app.serviceEventsWG.Go(func() {
		pending := make(map[string]hooks.Event)
		onRequest := func(req permission.PermissionRequest) {
			hook := permissionEvent(req)
			pending[req.ToolCallID] = hook
			app.hookRunner.Run(ctx, hook)
		}
		for {
			select {
			case ev, ok := <-requests:
				if !ok {
					return
				}
				onRequest(ev.Payload)
			case ev, ok := <-results:
				if !ok {
					return
				}
				decision := permissionDecision(ev.Payload)
				if decision == "" {
					continue
				}
				// The request and its result arrive on separate
				// channels, so a quick answer can be picked up before
				// the request it answers; the request is always
				// published first, so it is already buffered.
				for len(requests) > 0 {
					onRequest((<-requests).Payload)
				}
				hook, seen := pending[ev.Payload.ToolCallID]
				if !seen {
					continue
				}
				delete(pending, ev.Payload.ToolCallID)
				hook.Name = hooks.EventPermissionResult
				hook.Decision = decision
				app.hookRunner.Run(ctx, hook)
			case <-ctx.Done():
				return
			}
		}
	})
}

func permissionEvent(req permission.PermissionRequest) hooks.Event {
	input, err := json.Marshal(req.Params)
	if err != nil {
		slog.Warn("Failed to encode permission params for hook", "tool", req.ToolName, "error", err)
		input = []byte("{}")
	}
	return hooks.Event{
		Name:      hooks.EventPermissionRequest,
		SessionID: req.SessionID,
		ToolName:  req.ToolName,
		ToolInput: string(input),
		ToolUseID: req.ToolCallID,
	}
}

func permissionDecision(n permission.PermissionNotification) string {
	switch {
	case n.Granted:
		return "granted"
	case n.Denied:
		return "denied"
	case n.Cancelled:
		return "cancelled"
	}
	return ""
}
