package responses

import (
	"context"
	"strings"
	"testing"

	"github.com/tidwall/gjson"
)

// Synthetic Codex request: one eager tool, the client tool_search declaration,
// and one completed search round that returned a namespaced deferred tool.
const toolSearchTestRequest = `{
  "model": "claude-test",
  "stream": true,
  "tools": [
    {"type":"function","name":"exec_command","parameters":{"type":"object","properties":{"cmd":{"type":"string"}}}},
    {"type":"tool_search","execution":"client","description":"Search deferred tools","parameters":{"type":"object","properties":{"query":{"type":"string"},"limit":{"type":"number"}},"required":["query"]}}
  ],
  "input": [
    {"type":"message","role":"user","content":[{"type":"input_text","text":"list my calendars"}]},
    {"type":"tool_search_call","call_id":"call_ts1","status":"completed","execution":"client","arguments":{"query":"calendar","limit":1}},
    {"type":"tool_search_output","call_id":"call_ts1","status":"completed","execution":"client","tools":[
      {"type":"namespace","name":"mcp__demo__cal","description":"Calendar","tools":[
        {"type":"function","name":"_list","description":"List calendars","defer_loading":true,"parameters":{"type":"object","properties":{}}}
      ]}
    ]}
  ]
}`

func toolSearchTestClaudeTool(t *testing.T, out []byte, name string) gjson.Result {
	t.Helper()
	tool := gjson.GetBytes(out, `tools.#(name=="`+name+`")`)
	if !tool.Exists() {
		t.Fatalf("tool %q missing; tools=%s", name, gjson.GetBytes(out, "tools.#.name").Raw)
	}
	return tool
}

func TestToolSearch_RequestDeclaresSearchAndDefersDiscoveredTools(t *testing.T) {
	out := ConvertOpenAIResponsesRequestToClaude("claude-test", []byte(toolSearchTestRequest), true)

	search := toolSearchTestClaudeTool(t, out, "tool_search")
	if search.Get("defer_loading").Exists() {
		t.Fatalf("tool_search must not be deferred: %s", search.Raw)
	}
	if search.Get("input_schema.properties.limit").Type == gjson.Null {
		t.Fatalf("tool_search schema lost: %s", search.Raw)
	}
	if eager := toolSearchTestClaudeTool(t, out, "exec_command"); eager.Get("defer_loading").Exists() {
		t.Fatalf("eager tool must not be deferred: %s", eager.Raw)
	}
	discovered := toolSearchTestClaudeTool(t, out, "mcp__demo__cal___list")
	if !discovered.Get("defer_loading").Bool() {
		t.Fatalf("discovered tool must be deferred: %s", discovered.Raw)
	}

	msgs := gjson.GetBytes(out, "messages").Array()
	if len(msgs) != 3 {
		t.Fatalf("messages = %d, want 3: %s", len(msgs), gjson.GetBytes(out, "messages").Raw)
	}
	use := msgs[1].Get("content.0")
	if msgs[1].Get("role").String() != "assistant" || use.Get("type").String() != "tool_use" || use.Get("name").String() != "tool_search" {
		t.Fatalf("search call not converted: %s", msgs[1].Raw)
	}
	if use.Get("input.query").String() != "calendar" || use.Get("input.limit").Int() != 1 {
		t.Fatalf("search input lost: %s", use.Raw)
	}
	result := msgs[2].Get("content.0")
	if result.Get("type").String() != "tool_result" || result.Get("tool_use_id").String() != use.Get("id").String() {
		t.Fatalf("search output not paired: %s", msgs[2].Raw)
	}
	ref := result.Get("content.0")
	if ref.Get("type").String() != "tool_reference" || ref.Get("tool_name").String() != "mcp__demo__cal___list" {
		t.Fatalf("search output must reference the discovered tool: %s", result.Raw)
	}
}

func TestToolSearch_RequestWithoutSearchDeclarationKeepsToolsEager(t *testing.T) {
	req := strings.Replace(toolSearchTestRequest,
		`{"type":"tool_search","execution":"client","description":"Search deferred tools","parameters":{"type":"object","properties":{"query":{"type":"string"},"limit":{"type":"number"}},"required":["query"]}}`,
		`{"type":"function","name":"noop","parameters":{"type":"object","properties":{}}}`, 1)
	out := ConvertOpenAIResponsesRequestToClaude("claude-test", []byte(req), true)
	if gjson.GetBytes(out, `tools.#(defer_loading==true)`).Exists() {
		t.Fatalf("no tool may be deferred without a search tool: %s", gjson.GetBytes(out, "tools").Raw)
	}
	if gjson.GetBytes(out, `messages.2.content.0.content`).Type != gjson.String {
		t.Fatalf("search output must fall back to text: %s", gjson.GetBytes(out, "messages.2").Raw)
	}
}

func TestToolSearch_ServerExecutedSearchIsNotBridged(t *testing.T) {
	req := strings.Replace(toolSearchTestRequest, `"type":"tool_search","execution":"client"`, `"type":"tool_search","execution":"server"`, 1)
	out := ConvertOpenAIResponsesRequestToClaude("claude-test", []byte(req), true)
	if gjson.GetBytes(out, `tools.#(name=="tool_search")`).Exists() {
		t.Fatalf("server-executed search must not become a Claude tool")
	}
}

func TestToolSearch_NonStreamResponseRestoresSearchCall(t *testing.T) {
	sse := strings.Join([]string{
		`data: {"type":"message_start","message":{"id":"msg_1","usage":{"input_tokens":10,"output_tokens":5}}}`,
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"toolu_1","name":"tool_search","input":{}}}`,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"query\":\"calendar\"}"}}`,
		`data: {"type":"content_block_stop","index":0}`,
		`data: {"type":"message_delta","delta":{"stop_reason":"tool_use"}}`,
		`data: {"type":"message_stop"}`,
	}, "\n")
	out := ConvertClaudeResponseToOpenAIResponsesNonStream(context.Background(), "claude-test", []byte(toolSearchTestRequest), nil, []byte(sse), nil)
	item := gjson.GetBytes(out, "output.0")
	if item.Get("type").String() != "tool_search_call" || item.Get("execution").String() != "client" {
		t.Fatalf("want client tool_search_call, got %s", item.Raw)
	}
	if item.Get("call_id").String() != "toolu_1" || item.Get("id").String() != "tsc_toolu_1" {
		t.Fatalf("ids wrong: %s", item.Raw)
	}
	if !item.Get("arguments").IsObject() || item.Get("arguments.query").String() != "calendar" {
		t.Fatalf("arguments must be an object: %s", item.Raw)
	}
}

func TestToolSearch_StreamResponseRestoresSearchCallAndDiscoveredTool(t *testing.T) {
	chunks := []string{
		`data: {"type":"message_start","message":{"id":"msg_2","usage":{"input_tokens":10,"output_tokens":5}}}`,
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"toolu_2","name":"tool_search","input":{}}}`,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"query\":\"cal\"}"}}`,
		`data: {"type":"content_block_stop","index":0}`,
		`data: {"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"toolu_3","name":"mcp__demo__cal___list","input":{}}}`,
		`data: {"type":"content_block_stop","index":1}`,
		`data: {"type":"message_delta","delta":{"stop_reason":"tool_use"}}`,
		`data: {"type":"message_stop"}`,
	}
	var param any
	var added, done, completed gjson.Result
	for _, chunk := range chunks {
		for _, output := range ConvertClaudeResponseToOpenAIResponses(context.Background(), "claude-test", []byte(toolSearchTestRequest), nil, []byte(chunk), &param) {
			event, data := parseClaudeResponsesSSEEvent(t, output)
			if event == "response.function_call_arguments.delta" && data.Get("item_id").String() == "tsc_toolu_2" {
				t.Fatalf("search call must not stream function argument deltas")
			}
			switch {
			case event == "response.output_item.added" && data.Get("item.type").String() == "tool_search_call":
				added = data
			case event == "response.output_item.done" && data.Get("item.type").String() == "tool_search_call":
				done = data
			case event == "response.completed":
				completed = data
			}
		}
	}
	if !added.Exists() || added.Get("item.call_id").String() != "toolu_2" {
		t.Fatalf("missing tool_search_call added event")
	}
	if !done.Exists() || done.Get("item.arguments.query").String() != "cal" || done.Get("item.execution").String() != "client" {
		t.Fatalf("bad tool_search_call done event: %s", done.Raw)
	}
	out := completed.Get("response.output")
	if out.Get("0.type").String() != "tool_search_call" {
		t.Fatalf("completed output.0 = %s", out.Get("0").Raw)
	}
	fc := out.Get("1")
	if fc.Get("type").String() != "function_call" || fc.Get("name").String() != "_list" || fc.Get("namespace").String() != "mcp__demo__cal" {
		t.Fatalf("discovered tool call must restore namespace identity: %s", fc.Raw)
	}
}

func TestToolSearch_DeclaresDeferredPlaceholderBeforeFirstSearch(t *testing.T) {
	req := `{"model":"claude-test","tools":[
	  {"type":"function","name":"exec_command","parameters":{"type":"object","properties":{}}},
	  {"type":"tool_search","execution":"client","parameters":{"type":"object","properties":{"query":{"type":"string"}}}}
	],"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}]}`
	out := ConvertOpenAIResponsesRequestToClaude("claude-test", []byte(req), true)
	anchor := toolSearchTestClaudeTool(t, out, responsesToolSearchPlaceholderName)
	if !anchor.Get("defer_loading").Bool() {
		t.Fatalf("placeholder must be deferred: %s", anchor.Raw)
	}
	if gjson.GetBytes(out, `tools.#(name=="tool_search").defer_loading`).Bool() {
		t.Fatalf("tool_search must stay loaded")
	}
}

func TestToolSearch_NoPlaceholderOnceRealDeferredToolsExist(t *testing.T) {
	out := ConvertOpenAIResponsesRequestToClaude("claude-test", []byte(toolSearchTestRequest), true)
	if gjson.GetBytes(out, `tools.#(name=="`+responsesToolSearchPlaceholderName+`")`).Exists() {
		t.Fatalf("placeholder must not be added when deferred tools exist")
	}
}

func TestToolSearch_NoPlaceholderWithoutSearch(t *testing.T) {
	req := `{"model":"claude-test","tools":[{"type":"function","name":"exec_command","parameters":{"type":"object","properties":{}}}],
	  "input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}]}`
	out := ConvertOpenAIResponsesRequestToClaude("claude-test", []byte(req), true)
	if gjson.GetBytes(out, `tools.#(name=="`+responsesToolSearchPlaceholderName+`")`).Exists() {
		t.Fatalf("placeholder must only be added with tool_search")
	}
}
