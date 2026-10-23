package server

import (
	"encoding/json"
	"strings"
	"testing"
)

// 转换层最小回归：请求侧 messages/tools 形态、响应侧 output 事件序。
// 完整覆盖在 EquentR 上游测试里（MIT），这里钉住 workbuddy 场景最关键的三条：
// 1) instructions → system 消息 2) function_call 分组 3) 流式事件序含 completed。

func TestConvertRequestBasics(t *testing.T) {
	req := map[string]any{
		"model":        "cn:glm-5.3-flash",
		"instructions": "be terse",
		"input": []any{
			map[string]any{"type": "message", "role": "user", "content": "hi"},
			map[string]any{"type": "function_call", "call_id": "c1", "name": "shell", "arguments": "{}"},
			map[string]any{"type": "function_call_output", "call_id": "c1", "output": "ok"},
		},
		"tools": []any{map[string]any{"type": "function", "name": "shell", "parameters": map[string]any{}}},
	}
	got := ConvertRequest(req, respProxyConfig{})
	msgs, _ := got["messages"].([]any)
	if len(msgs) != 4 { // system + user + assistant(tool_calls) + tool
		t.Fatalf("messages=%d want 4", len(msgs))
	}
	first, _ := msgs[0].(map[string]any)
	if first["role"] != "system" {
		t.Fatalf("first role=%v want system", first["role"])
	}
	mid, _ := msgs[2].(map[string]any)
	if mid["role"] != "assistant" {
		t.Fatalf("call msg role=%v want assistant", mid["role"])
	}
	if _, ok := got["tools"]; !ok {
		t.Fatalf("tools missing: %v", got)
	}
}

func TestConvertResponseBasics(t *testing.T) {
	chat := map[string]any{
		"id": "abc", "created": 1, "model": "glm-5.3-flash",
		"choices": []any{map[string]any{
			"index": 0, "finish_reason": "stop",
			"message": map[string]any{"role": "assistant", "content": "收到", "reasoning_content": "think"},
		}},
		"usage": map[string]any{"prompt_tokens": 10, "completion_tokens": 2, "total_tokens": 12},
	}
	out := ConvertResponse(chat, map[string]any{"model": "cn:glm-5.3-flash"})
	if out["object"] != "response" || out["status"] != "completed" {
		t.Fatalf("bad object/status: %v %v", out["object"], out["status"])
	}
	output, _ := out["output"].([]any)
	if len(output) != 2 {
		t.Fatalf("output=%d want 2 (reasoning+message)", len(output))
	}
	if o0, _ := output[0].(map[string]any); o0["type"] != "reasoning" {
		t.Fatalf("first type=%v want reasoning", o0["type"])
	}
}

func TestStreamingConverterEvents(t *testing.T) {
	c := NewStreamingConverter()
	chunk := []byte("data: {\"id\":\"x\",\"choices\":[{\"delta\":{\"role\":\"assistant\",\"content\":\"\u6536\u5230\"},\"index\":0}]}\n\n")
	events := c.Feed(chunk)
	if len(events) == 0 {
		t.Fatal("no events")
	}
	all := strings.Join(append(events, c.Finish()...), "")
	for _, want := range []string{"response.created", "response.output_text.delta", "response.completed"} {
		if !strings.Contains(all, want) {
			t.Fatalf("missing %s in stream", want)
		}
	}
	var done map[string]any
	for _, ev := range strings.Split(all, "\n\n") {
		if strings.Contains(ev, "response.completed") {
			for _, ln := range strings.Split(ev, "\n") {
				line := strings.TrimSpace(strings.TrimPrefix(ln, "data:"))
				if strings.HasPrefix(line, "{") {
					_ = json.Unmarshal([]byte(line), &done)
				}
			}
		}
	}
	if done == nil || done["type"] != "response.completed" {
		t.Fatalf("completed event malformed: %v", done)
	}
}
