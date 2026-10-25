package upstream

import (
	"strings"
	"testing"
)

// xmlTestCall 是 Step 系模型 XML 方言工具调用的典型形态。
const xmlTestCall = "很好，我来执行。\n" + xmlOpenTag + "\n<exec_command>\n<cmd>ls -la /tmp</cmd>\n</exec_command>\n"

// 回归：xmltool.go 曾因接线错误（XML 分支误建 DSML 实例）成为死代码，
// 工具调用块原样落进正文，Codex 侧工具一次都没跑。
func TestXMLRepairConvertsToolCalls(t *testing.T) {
	allowed := map[string]bool{"exec_command": true}
	r := NewXMLRepair(allowed, true)
	if !r.Enabled() {
		t.Fatal("XML 修复器应启用")
	}
	emit, calls := r.Feed(xmlTestCall + xmlCloseTag)
	emit += r.Flush()
	if len(calls) != 1 {
		t.Fatalf("应还原 1 个调用，got %d (emit=%q)", len(calls), emit)
	}
	if calls[0].Name != "exec_command" {
		t.Fatalf("工具名错: %q", calls[0].Name)
	}
	if !strings.Contains(calls[0].Arguments, "ls -la /tmp") {
		t.Fatalf("参数丢: %q", calls[0].Arguments)
	}
	if strings.Contains(emit, "exec_command") {
		t.Fatalf("还原后正文不应再含工具调用 XML: %q", emit)
	}
	if !strings.Contains(emit, "很好") {
		t.Fatalf("普通正文必须保留: %q", emit)
	}
}

// 流式分片喂入（标记被增量边界切开）也必须还原。
func TestXMLRepairStreamingSplit(t *testing.T) {
	allowed := map[string]bool{"exec_command": true}
	r := NewXMLRepair(allowed, true)
	full := xmlTestCall + xmlCloseTag
	cut := 20
	emit1, calls1 := r.Feed(full[:cut])
	emit2, calls2 := r.Feed(full[cut:])
	calls := append(calls1, calls2...)
	if len(calls) != 1 || calls[0].Name != "exec_command" {
		t.Fatalf("分片后应还原 1 个调用，got %+v", calls)
	}
	if !strings.Contains(calls[0].Arguments, "ls -la /tmp") {
		t.Fatalf("参数丢: %q", calls[0].Arguments)
	}
	if emit1+emit2+r.Flush() != "很好，我来执行。\n" {
		t.Fatalf("正文应只剩开场白，got %q", emit1+emit2+r.Flush())
	}
}

// 普通正文（无 XML 块）→ 不转换，一个字节都不能吞。
func TestXMLRepairRejectsPlainText(t *testing.T) {
	allowed := map[string]bool{"exec_command": true}
	r := NewXMLRepair(allowed, true)
	text := "前面是正文，没有任何工具调用。"
	emit, calls := r.Feed(text)
	emit += r.Flush()
	if len(calls) != 0 {
		t.Fatalf("普通正文不应转换，got %+v", calls)
	}
	if emit != text {
		t.Fatalf("必须原文回吐\ngot:  %q\nwant: %q", emit, text)
	}
}

// 非流式聚合路径：RepairAggregatedResponse 的 XML 分支。
func TestXMLRepairAggregated(t *testing.T) {
	allowed := map[string]bool{"exec_command": true}
	resp := map[string]any{
		"choices": []any{
			map[string]any{
				"finish_reason": "stop",
				"message": map[string]any{
					"role":    "assistant",
					"content": xmlTestCall + xmlCloseTag,
				},
			},
		},
	}
	RepairAggregatedResponse(resp, allowed, true)
	choice := resp["choices"].([]any)[0].(map[string]any)
	msg := choice["message"].(map[string]any)
	tcs, ok := msg["tool_calls"].([]any)
	if !ok || len(tcs) != 1 {
		t.Fatalf("应还原出 tool_calls，got %v", msg["tool_calls"])
	}
	fn := tcs[0].(map[string]any)["function"].(map[string]any)
	if fn["name"] != "exec_command" || !strings.Contains(fn["arguments"].(string), "ls -la /tmp") {
		t.Fatalf("调用内容错: %+v", fn)
	}
	if choice["finish_reason"] != "tool_calls" {
		t.Fatalf("finish_reason 应改写为 tool_calls，got %v", choice["finish_reason"])
	}
}

// DSML 方言不能被 XML 改动破坏（两种方言共享状态机后的回归防线）。
func TestDSMLRepairStillWorks(t *testing.T) {
	allowed := map[string]bool{"exec_command": true}
	r := NewMarkupRepair(allowed, true)
	invoke := "<" + markupPipe + "DSML" + markupPipe + " invoke name=\"exec_command\">"
	param := "<" + markupPipe + "DSML" + markupPipe + " parameter name=\"cmd\" string=\"true\">ls" + markupParamClose
	text := markupOpen + "\n" + invoke + "\n" + param + "\n" + markupInvokeClose + "\n" + markupClose
	emit, calls := r.Feed(text)
	emit += r.Flush()
	if len(calls) != 1 || calls[0].Name != "exec_command" {
		t.Fatalf("DSML 应仍能还原，got %+v emit=%q", calls, emit)
	}
	if !strings.Contains(calls[0].Arguments, "ls") {
		t.Fatalf("DSML 参数丢: %q", calls[0].Arguments)
	}
}
