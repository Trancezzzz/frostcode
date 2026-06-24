package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// readOnlyTools returns the non-destructive subset of the toolset, used by
// sub-agents so a delegated investigation can never modify the project.
func readOnlyTools(sb *Sandbox) []Tool {
	var out []Tool
	for _, t := range DefaultTools(sb, nil) {
		if !t.Destructive {
			out = append(out, t)
		}
	}
	return out
}

// subUI captures a sub-agent's final output and surfaces its tool activity to
// the parent terminal (indented), without echoing its prose live.
type subUI struct {
	parent UI
	final  strings.Builder
}

func (s *subUI) AssistantText(t string)             { s.final.WriteString(t) }
func (s *subUI) StreamDelta(t string)               { s.final.WriteString(t) }
func (s *subUI) ReasoningDelta(string)              {}
func (s *subUI) StreamEnd()                         {}
func (s *subUI) ToolStart(name, preview string)     { s.parent.Info(icSub + " " + name + " " + preview) }
func (s *subUI) ToolResult(name, result string)     {}
func (s *subUI) Info(string)                        {}
func (s *subUI) Error(e string)                     { s.parent.Error("subagent: " + e) }
func (s *subUI) Thinking(bool)                      {}
func (s *subUI) TurnStats(time.Duration, int)       {}
func (s *subUI) Todos([]TodoItem)                   {}

// makeTaskTool builds the `task` tool, which delegates a focused, read-only
// investigation to a fresh-context sub-agent and returns its findings.
func makeTaskTool(caller Caller, sb *Sandbox, parent *Agent, parentUI UI) Tool {
	return Tool{
		Name: "task",
		Description: "Delegate a focused, read-only investigation to a sub-agent with its own fresh context. " +
			"Give it a clear, self-contained prompt; it can read/search files (not modify them) and returns its " +
			"findings as text. Use this to explore large areas without cluttering the main conversation.",
		Schema: json.RawMessage(`{"type":"object","properties":{
			"description":{"type":"string","description":"a few words naming the subtask"},
			"prompt":{"type":"string","description":"the full instruction for the sub-agent"}},
			"required":["prompt"]}`),
		Run: func(a map[string]any) (string, error) {
			prompt := str(a, "prompt")
			if strings.TrimSpace(prompt) == "" {
				return "", fmt.Errorf("prompt is required")
			}
			ui := &subUI{parent: parentUI}
			sub := New(parent.Model, caller, readOnlyTools(sb), nil, ui,
				"You are a sub-agent spawned to handle one focused, READ-ONLY task: "+str(a, "description")+
					".\nInvestigate using read/search tools and report concise findings — lead with the answer, "+
					"no preamble or disclaimers. Stay strictly in scope; do not expand the task. You cannot modify files.")
			sub.SetMode(ModePlan)
			if err := sub.Run(context.Background(), prompt); err != nil {
				return "", err
			}
			out := strings.TrimSpace(ui.final.String())
			if out == "" {
				out = "(sub-agent returned no findings)"
			}
			return out, nil
		},
	}
}
