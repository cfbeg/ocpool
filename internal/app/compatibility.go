package app

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
)

type ReasoningOptions struct {
	Effort  string `json:"effort,omitempty"`
	Summary string `json:"summary,omitempty"`
}

// Advisory client metadata has no equivalent in an isolated OpenCode runtime.
type ClientHints struct {
	Include              []string          `json:"include"`
	Metadata             map[string]string `json:"metadata"`
	User                 string            `json:"user"`
	SafetyIdentifier     string            `json:"safety_identifier"`
	PromptCacheKey       string            `json:"prompt_cache_key"`
	PromptCacheRetention string            `json:"prompt_cache_retention"`
	PromptCacheOptions   json.RawMessage   `json:"prompt_cache_options"`
	ServiceTier          string            `json:"service_tier"`
}

type functionDefinition struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	Parameters  map[string]any `json:"parameters"`
	Strict      *bool          `json:"strict,omitempty"`
}
type toolDefinition struct {
	Type string `json:"type"`
	functionDefinition
	Function *functionDefinition `json:"function,omitempty"`
}
type ToolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}
type toolPlan struct {
	definitions []functionDefinition
	choice      string
	selected    string
	parallel    bool
}

var functionName = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,64}$`)

func parseTools(raw, choice json.RawMessage, parallel *bool) (*toolPlan, error) {
	var definitions []toolDefinition
	if len(raw) > 0 && json.Unmarshal(raw, &definitions) != nil {
		return nil, errors.New("tools must be an array")
	}
	plan := &toolPlan{choice: "auto", parallel: parallel == nil || *parallel}
	seen := map[string]bool{}
	aliases := map[string]bool{}
	for _, t := range definitions {
		if t.Type != "function" {
			return nil, errors.New("only function tools are supported")
		}
		f := t.functionDefinition
		if t.Function != nil {
			f = *t.Function
		}
		if !functionName.MatchString(f.Name) || seen[f.Name] {
			return nil, errors.New("tool names must be unique and contain 1 to 64 letters, digits, underscores or hyphens")
		}
		seen[f.Name] = true
		alias := clientToolAlias(f.Name)
		if aliases[alias] {
			return nil, errors.New("tool names collide after OpenCode namespace mapping")
		}
		aliases[alias] = true
		if f.Parameters == nil {
			f.Parameters = map[string]any{"type": "object", "properties": map[string]any{}}
		}
		if f.Parameters["type"] != "object" {
			return nil, fmt.Errorf("tool %s parameters must be an object schema", f.Name)
		}
		plan.definitions = append(plan.definitions, f)
	}
	if len(choice) > 0 && string(choice) != "null" {
		if json.Unmarshal(choice, &plan.choice) != nil {
			var selected toolDefinition
			if json.Unmarshal(choice, &selected) != nil || selected.Type != "function" {
				return nil, errors.New("invalid tool_choice")
			}
			plan.choice = selected.Name
			if selected.Function != nil {
				plan.choice = selected.Function.Name
			}
			if !seen[plan.choice] {
				return nil, errors.New("tool_choice refers to an unknown tool")
			}
			plan.selected = plan.choice
			plan.choice = "required"
		} else if plan.choice != "auto" && plan.choice != "none" && plan.choice != "required" {
			return nil, errors.New("invalid tool_choice")
		}
	}
	if len(definitions) == 0 {
		if plan.choice != "auto" && plan.choice != "none" {
			return nil, errors.New("tool_choice requires tools")
		}
		return nil, nil
	}
	if plan.choice == "none" {
		return nil, nil
	}
	return plan, nil
}

func (p *toolPlan) prompt() string {
	names := []string{}
	for _, f := range p.definitions {
		if p.selected != "" && p.selected != f.Name {
			continue
		}
		names = append(names, f.Name+" = "+clientToolPrefix+clientToolAlias(f.Name))
	}
	raw, _ := json.Marshal(names)
	result := "\nThese tools belong to the API client and execute in the API caller's workspace. Call their ocpool-prefixed names to request client execution. The API client returns the actual result in the next turn. Do not simulate results or use the gateway's built-in tools. Client tool arguments must follow the client's schema and instructions. Preserve file paths exactly as requested, including relative paths and explicitly supplied absolute paths. OpenCode's runtime working directory and workspace root belong to this server and do not define the client workspace. Never prepend that server directory to client tool paths or invent a client workspace root. Tools: " + string(raw)
	if p.choice == "required" {
		result += " You must request a client tool on this turn."
	}
	if !p.parallel {
		result += " Call at most one client tool on this turn."
	}
	return result
}
