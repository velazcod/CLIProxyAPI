package responses

import (
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/util"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// Codex declares client-executed tool search as {"type":"tool_search"} with no
// name. Claude has no counterpart for that declaration, so it is exposed as an
// ordinary tool under this name and its calls are restored as
// tool_search_call items. Tools returned by tool_search_output are declared
// with defer_loading and surfaced through tool_reference blocks, which is
// Anthropic's documented client-side tool search contract.
const responsesToolSearchName = "tool_search"

// isResponsesClientToolSearch reports whether tool is a tool_search
// declaration that the client executes. Server-executed search has no Claude
// counterpart and stays unsupported.
func isResponsesClientToolSearch(tool gjson.Result) bool {
	if strings.TrimSpace(tool.Get("type").String()) != "tool_search" {
		return false
	}
	return !strings.EqualFold(strings.TrimSpace(tool.Get("execution").String()), "server")
}

// responsesDeclaresClientToolSearch reports whether a winning client
// tool_search declaration exists in the request.
func responsesDeclaresClientToolSearch(winners map[string]responsesToolDescriptor) bool {
	descriptor, ok := winners[responsesToolSearchName]
	return ok && descriptor.toolType == "tool_search"
}

// responsesToolSearchClaudeName returns the Claude name of the tool_search
// tool, or "" when the request does not declare client tool search.
func responsesToolSearchClaudeName(requestRawJSON []byte) string {
	if len(requestRawJSON) == 0 {
		return ""
	}
	root := gjson.ParseBytes(requestRawJSON)
	winners := responsesToolWinners(root)
	if !responsesDeclaresClientToolSearch(winners) {
		return ""
	}
	return buildClaudeToolNamesWithWinners(root, winners).claudeName(responsesToolSearchName)
}

func convertResponsesToolSearchToolToClaude(tool gjson.Result, claudeName string) ([]byte, bool) {
	name := strings.TrimSpace(claudeName)
	if name == "" {
		name = responsesToolSearchName
	}
	tJSON := []byte(`{"name":"","description":"","input_schema":{"type":"object","properties":{"query":{"type":"string"}},"required":["query"]}}`)
	tJSON, _ = sjson.SetBytes(tJSON, "name", name)
	description := responsesToolDescription(tool)
	if description == "" {
		description = "Search for deferred tools and load the matching tools."
	}
	tJSON, _ = sjson.SetBytes(tJSON, "description", description)
	if parameters := responsesToolParameters(tool); parameters.Exists() && parameters.IsObject() {
		tJSON, _ = sjson.SetRawBytes(tJSON, "input_schema", util.NormalizeClaudeToolInputSchema([]byte(parameters.Raw)))
	}
	return tJSON, true
}

// responsesToolSearchArguments returns tool_search_call arguments as a JSON
// object. Arguments arrive as an object or as a JSON string; anything else is
// kept as the query text.
func responsesToolSearchArguments(item gjson.Result) []byte {
	args := item.Get("arguments")
	switch {
	case args.IsObject():
		return []byte(args.Raw)
	case args.Type == gjson.String:
		if parsed := gjson.Parse(args.String()); gjson.Valid(args.String()) && parsed.IsObject() {
			return []byte(parsed.Raw)
		}
		if text := strings.TrimSpace(args.String()); text != "" {
			out, _ := sjson.SetBytes([]byte(`{}`), "query", text)
			return out
		}
	}
	return []byte(`{}`)
}

// responsesToolSearchOutputIdentities lists the qualified identities of the
// tools returned by one tool_search_output item, in order.
func responsesToolSearchOutputIdentities(item gjson.Result) []string {
	var ids []string
	item.Get("tools").ForEach(func(_, tool gjson.Result) bool {
		if strings.TrimSpace(tool.Get("type").String()) == "namespace" {
			namespaceName := strings.TrimSpace(tool.Get("name").String())
			tool.Get("tools").ForEach(func(_, child gjson.Result) bool {
				if childName := responsesToolName(child); childName != "" {
					ids = append(ids, qualifyResponsesNamespaceToolName(namespaceName, childName))
				}
				return true
			})
			return true
		}
		if name := responsesToolName(tool); name != "" {
			ids = append(ids, name)
		}
		return true
	})
	return ids
}

// convertResponsesToolSearchOutputToClaude builds the tool_result content for
// a tool_search_output: one tool_reference block per returned tool that is
// declared in tools[]. Claude expands each reference into the full definition.
func convertResponsesToolSearchOutputToClaude(item gjson.Result, names claudeToolNames, declared map[string]struct{}) []byte {
	var refs [][]byte
	seen := map[string]bool{}
	for _, identity := range responsesToolSearchOutputIdentities(item) {
		if _, ok := declared[identity]; !ok {
			continue
		}
		claudeName := names.claudeName(identity)
		if claudeName == "" || seen[claudeName] {
			continue
		}
		seen[claudeName] = true
		ref, _ := sjson.SetBytes([]byte(`{"type":"tool_reference","tool_name":""}`), "tool_name", claudeName)
		refs = append(refs, ref)
	}
	if len(refs) == 0 {
		return []byte(`[{"type":"text","text":"No matching tools were found."}]`)
	}
	out := []byte(`[]`)
	for _, ref := range refs {
		out, _ = sjson.SetRawBytes(out, "-1", ref)
	}
	return out
}

func responsesToolSearchCallID(callID string) string {
	return "tsc_" + callID
}

// buildResponsesToolSearchCallItem renders a Claude tool_search tool_use as a
// client-executed Responses tool_search_call with object arguments.
func buildResponsesToolSearchCallItem(callID, args, status string) []byte {
	if status == "" {
		status = "completed"
	}
	item := []byte(`{"id":"","type":"tool_search_call","status":"","execution":"client","call_id":"","arguments":{}}`)
	item, _ = sjson.SetBytes(item, "id", responsesToolSearchCallID(callID))
	item, _ = sjson.SetBytes(item, "status", status)
	item, _ = sjson.SetBytes(item, "call_id", callID)
	if parsed := gjson.Parse(args); gjson.Valid(args) && parsed.IsObject() {
		item, _ = sjson.SetRawBytes(item, "arguments", []byte(parsed.Raw))
	}
	return item
}

// responsesToolSearchPlaceholderName names a deferred tool that is never
// referenced. The first deferred tool in a request invalidates the whole
// Claude prompt cache, while later deferred tools do not. Declaring one from
// the first request keeps the first tool search from rewriting the cache.
const responsesToolSearchPlaceholderName = "tool_search_cache_anchor"

func appendResponsesToolSearchCachePlaceholder(toolItems [][]byte, included map[string]struct{}) [][]byte {
	for _, item := range toolItems {
		if gjson.GetBytes(item, "defer_loading").Bool() {
			return toolItems
		}
	}
	if _, taken := included[responsesToolSearchPlaceholderName]; taken {
		return toolItems
	}
	placeholder := []byte(`{"name":"","description":"Reserved. Never call this tool.","input_schema":{"type":"object","properties":{}},"defer_loading":true}`)
	placeholder, _ = sjson.SetBytes(placeholder, "name", responsesToolSearchPlaceholderName)
	return append(toolItems, placeholder)
}
