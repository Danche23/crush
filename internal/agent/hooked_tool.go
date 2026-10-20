package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"charm.land/fantasy"
	"github.com/charmbracelet/crush/internal/agent/tools"
	"github.com/charmbracelet/crush/internal/hooks"
	"github.com/charmbracelet/crush/internal/permission"
	"github.com/tidwall/sjson"
)

// hookedTool wraps a fantasy.AgentTool to run PreToolUse hooks before
// delegating to the inner tool.
type hookedTool struct {
	inner  fantasy.AgentTool
	runner *hooks.Runner
}

func newHookedTool(inner fantasy.AgentTool, runner *hooks.Runner) *hookedTool {
	return &hookedTool{inner: inner, runner: runner}
}

// wrapToolsWithHooks returns a tool slice with each entry wrapped in a
// hookedTool. Returns the original slice unchanged when runner is nil or
// when isSubAgent is true — sub-agents never fire hooks, the top-level
// invocation of the sub-agent tool itself is wrapped on the caller's side.
func wrapToolsWithHooks(tools []fantasy.AgentTool, runner *hooks.Runner, isSubAgent bool) []fantasy.AgentTool {
	if runner == nil || isSubAgent {
		return tools
	}
	out := make([]fantasy.AgentTool, len(tools))
	for i, tool := range tools {
		out[i] = newHookedTool(tool, runner)
	}
	return out
}

func (h *hookedTool) Info() fantasy.ToolInfo {
	return h.inner.Info()
}

func (h *hookedTool) ProviderOptions() fantasy.ProviderOptions {
	return h.inner.ProviderOptions()
}

func (h *hookedTool) SetProviderOptions(opts fantasy.ProviderOptions) {
	h.inner.SetProviderOptions(opts)
}

func (h *hookedTool) Run(ctx context.Context, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
	pre := h.runner.Run(ctx, hooks.Event{
		Name:      hooks.EventPreToolUse,
		SessionID: tools.GetSessionFromContext(ctx),
		ToolName:  call.Name,
		ToolInput: call.Input,
		ToolUseID: call.ID,
	})
	if pre.Blocked() {
		reason := fmt.Sprintf("Tool call blocked by hook. Reason: %s", pre.Reason)
		if pre.Halt {
			reason = fmt.Sprintf("Turn halted by hook. Reason: %s", pre.Reason)
		}
		resp := fantasy.NewTextErrorResponse(reason)
		// Halt ends the whole turn; a plain deny only blocks this tool
		// call so the model can see the error and try something else.
		resp.StopTurn = pre.Halt
		resp.Metadata = mergeHookMetadata("", pre)
		return resp, nil
	}
	if pre.UpdatedInput != "" {
		call.Input = pre.UpdatedInput
	}
	// An explicit allow from a hook pre-approves the permission prompt for
	// this tool call. Silence falls through to the normal permission flow.
	if pre.Decision == hooks.DecisionAllow {
		ctx = permission.WithHookApproval(ctx, call.ID)
	}

	resp, err := h.inner.Run(ctx, call)
	if err != nil {
		return resp, err
	}

	resp.Content = joinNonEmpty(resp.Content, pre.Context)
	resp.Metadata = mergeHookMetadata(resp.Metadata, pre)
	return resp, nil
}

// joinNonEmpty appends the non-empty parts to content, one per line.
func joinNonEmpty(content string, parts ...string) string {
	for _, p := range parts {
		if p == "" {
			continue
		}
		if content != "" {
			content += "\n"
		}
		content += p
	}
	return content
}

// mergeHookMetadata injects the combined hook metadata for the given
// results into existing tool metadata under the "hook" key. Results
// with no hooks are skipped; when nothing ran, existing is returned
// unchanged.
func mergeHookMetadata(existing string, results ...hooks.AggregateResult) string {
	var meta hooks.HookMetadata
	for _, r := range results {
		if r.HookCount == 0 {
			continue
		}
		meta.HookCount += r.HookCount
		meta.Hooks = append(meta.Hooks, r.Hooks...)
		meta.Halt = meta.Halt || r.Halt
		meta.InputRewrite = meta.InputRewrite || r.UpdatedInput != ""
		if r.Decision > hooks.DecisionNone && meta.Decision != hooks.DecisionDeny.String() {
			meta.Decision = r.Decision.String()
		}
		meta.Reason = strings.TrimSpace(meta.Reason + "\n" + r.Reason)
	}
	if meta.HookCount == 0 {
		return existing
	}
	if meta.Decision == "" {
		meta.Decision = hooks.DecisionNone.String()
	}
	data, err := json.Marshal(meta)
	if err != nil {
		return existing
	}
	if existing == "" {
		existing = "{}"
	}
	merged, err := sjson.SetRaw(existing, "hook", string(data))
	if err != nil {
		return existing
	}
	return merged
}
