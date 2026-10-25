// xmltool.go 上游正文里的「XML 风格工具调用」修复层（第二道防线）。
//
// 背景：WorkBuddy 上游除 DeepSeek 系（见 dsml.go 的 DSML 全角竖线标记）之外，
// 还会由 Step 系等模型出结果。这类模型的工具调用原生语法是 XML：
//
//	<tool_call>
//	<exec_command>
//	<parameter=cmd>ls -la</parameter>
//	</exec_command>
//	</tool_call>
//
// 上游只在请求声明了 tools 时才把这段标记解析成结构化 tool_calls。一旦某次请求
// 没带上 tools（客户端一次性 exec 探测、auto-review 复核回合、或任何 tools 中途
// 丢失的请求），模型仍然想调工具，于是这段 XML 以纯文本落进 content。下游没有
// 任何一层认识它：Codex 只执行 Responses API 的结构化 function_call 条目，于是
// 把 XML 当成普通助手文本写进会话历史，回合直接结束、工具一次都没跑。
//
// 本文件是第二道防线：在出站响应（流式与非流式）里识别这段 XML，还原成标准的
// tool_calls / delta.tool_calls。与 dsml.go 的 MarkupRepair 共用同一状态机
//（Feed/Flush 语义、两级工具名判定、绝不吞字节），只是换了起止标记与块内语法。
package upstream

import (
	"encoding/json"
	"regexp"
	"strings"
)

const (
	xmlOpenTag  = "<tool_call>"
	xmlCloseTag = "</tool_call>"
)

// xmlToolNameRe 与 markupToolNameRe 同形：裸标识符，允许 . : - 以兼容 MCP 工具名。
var xmlToolNameRe = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.:-]{0,63}$`)

// xmlMaxBlockBytes 单块上限，超过即判为正文，避免把大段正文误当工具调用。
const xmlMaxBlockBytes = 256 * 1024

// parseXMLBlock 解析一个工具调用块（xmlOpenTag 与 xmlCloseTag 之间的内容）。
//
// 期望形态：<工具名>...<parameter=参数名>值</parameter>...</工具名>
// 参数名支持两种写法：<parameter name="cmd">值</parameter> 与 <cmd>值</cmd>。
// 成功要求与 DSML 层一致：块内除工具元素与空白外没有别的字节；工具名合法；
// 参数列表闭合。任一不满足返回 ok=false，由调用方原文回吐。
func parseXMLBlock(inner string, allowed map[string]bool) ([]MarkupCall, bool) {
	strict := len(allowed) > 0
	trimmed := strings.TrimSpace(inner)
	if trimmed == "" {
		return nil, false
	}
	open := regexp.MustCompile(`^<([A-Za-z0-9_][A-Za-z0-9_.:-]{0,63})>`)
	m := open.FindStringSubmatch(trimmed)
	if m == nil {
		return nil, false
	}
	name := m[1]
	if strict {
		if !allowed[name] {
			return nil, false
		}
	} else if !xmlToolNameRe.MatchString(name) {
		return nil, false
	}
	rest := trimmed[len(m[0]):]
	closeTag := "</" + name + ">"
	ci := strings.LastIndex(rest, closeTag)
	if ci < 0 {
		return nil, false
	}
	body := rest[:ci]
	if strings.TrimSpace(rest[ci+len(closeTag):]) != "" {
		return nil, false
	}
	args, ok := parseXMLParams(body)
	if !ok {
		return nil, false
	}
	return []MarkupCall{{Name: name, Arguments: args}}, true
}

// parseXMLParams 解析工具元素内部的参数列表，返回 JSON 对象文本。
func parseXMLParams(body string) (string, bool) {
	kv := map[string]string{}
	pos := 0
	for pos < len(body) {
		for pos < len(body) && (body[pos] == 32 || body[pos] == 9 || body[pos] == 10 || body[pos] == 13) {
			pos++
		}
		if pos >= len(body) {
			break
		}
		if body[pos] != 60 {
			return "", false
		}
		gt := strings.IndexByte(body[pos:], 62)
		if gt < 0 {
			return "", false
		}
		tag := body[pos+1 : pos+gt]
		pos += gt + 1
		var pname string
		if strings.HasPrefix(tag, "parameter") {
			nm := regexp.MustCompile(`name\s*=\s*"([^"]*)"`).FindStringSubmatch(tag)
			if nm == nil {
				return "", false
			}
			pname = nm[1]
			end := "</parameter>"
			ei := strings.Index(body[pos:], end)
			if ei < 0 {
				return "", false
			}
			kv[pname] = body[pos : pos+ei]
			pos += ei + len(end)
			continue
		}
		pname = tag
		end := "</" + tag + ">"
		ei := strings.Index(body[pos:], end)
		if ei < 0 {
			return "", false
		}
		kv[pname] = body[pos : pos+ei]
		pos += ei + len(end)
	}
	b, err := json.Marshal(kv)
	if err != nil {
		return "", false
	}
	return string(b), true
}
