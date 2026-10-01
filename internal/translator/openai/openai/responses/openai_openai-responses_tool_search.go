package responses

import (
	"strings"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// Chat Completions has no tool search, so the client-executed Responses
// tool_search declaration is bridged as an ordinary function. Calls to it are
// restored as tool_search_call items, the client runs the search, and the
// tools it returns in tool_search_output are declared as functions on every
// later request.
const responsesToolSearchName = "tool_search"

// isResponsesClientToolSearch reports whether tool is a tool_search
// declaration that the client executes.
func isResponsesClientToolSearch(tool gjson.Result) bool {
	if strings.TrimSpace(tool.Get("type").String()) != "tool_search" {
		return false
	}
	return !strings.EqualFold(strings.TrimSpace(tool.Get("execution").String()), "server")
}

func convertResponsesToolSearchToolToOpenAIChat(tool gjson.Result, overrideName string) ([]byte, bool) {
	name := strings.TrimSpace(overrideName)
	if name == "" {
		name = responsesToolSearchName
	}
	chatTool := []byte(`{"type":"function","function":{"name":"","description":"","parameters":{"type":"object","properties":{"query":{"type":"string"}},"required":["query"]}}}`)
	chatTool, _ = sjson.SetBytes(chatTool, "function.name", name)
	description := responsesToolDescription(tool)
	if description == "" {
		description = "Search for deferred tools and load the matching tools."
	}
	chatTool, _ = sjson.SetBytes(chatTool, "function.description", description)
	if parameters := responsesToolParameters(tool); parameters.Exists() && parameters.IsObject() {
		chatTool, _ = sjson.SetRawBytes(chatTool, "function.parameters", []byte(parameters.Raw))
	}
	return chatTool, true
}

// bridgeResponsesToolSearchItemToFunction rewrites tool_search_call and
// tool_search_output input items into the function_call pair the Chat
// Completions conversion already understands. Other items are returned as is.
func bridgeResponsesToolSearchItemToFunction(item gjson.Result) gjson.Result {
	switch item.Get("type").String() {
	case "tool_search_call":
		call := []byte(`{"type":"function_call","call_id":"","name":"","arguments":""}`)
		call, _ = sjson.SetBytes(call, "call_id", item.Get("call_id").String())
		call, _ = sjson.SetBytes(call, "name", responsesToolSearchName)
		args := item.Get("arguments")
		argsText := "{}"
		switch {
		case args.IsObject():
			argsText = args.Raw
		case args.Type == gjson.String && strings.TrimSpace(args.String()) != "":
			argsText = args.String()
		}
		call, _ = sjson.SetBytes(call, "arguments", argsText)
		return gjson.ParseBytes(call)
	case "tool_search_output":
		var names []string
		item.Get("tools").ForEach(func(_, tool gjson.Result) bool {
			if strings.TrimSpace(tool.Get("type").String()) == "namespace" {
				namespaceName := strings.TrimSpace(tool.Get("name").String())
				tool.Get("tools").ForEach(func(_, child gjson.Result) bool {
					if childName := responsesToolName(child); childName != "" {
						names = append(names, qualifyResponsesNamespaceToolName(namespaceName, childName))
					}
					return true
				})
			} else if name := responsesToolName(tool); name != "" {
				names = append(names, name)
			}
			return true
		})
		text := "No matching tools were found."
		if len(names) > 0 {
			text = "Loaded tools, now available to call: " + strings.Join(names, ", ")
		}
		output := []byte(`{"type":"function_call_output","call_id":"","output":""}`)
		output, _ = sjson.SetBytes(output, "call_id", item.Get("call_id").String())
		output, _ = sjson.SetBytes(output, "output", text)
		return gjson.ParseBytes(output)
	}
	return item
}

func responsesToolSearchCallID(callID string) string {
	return "tsc_" + callID
}

// buildResponsesToolSearchCallItem renders a bridged search function call as a
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
