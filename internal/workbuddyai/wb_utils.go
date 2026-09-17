// wb_utils.go WorkBuddy 国际版渠道的通用纯函数：请求体改写、SSE 透传/聚合、
// 文案截断、倍率解析。全部为无状态纯函数，不依赖 Client，
// 便于单测覆盖，也避免与国内版 upstream 包互相耦合。
package workbuddyai

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// ---------------------------------------------------------------------------
// 请求体改写
// ---------------------------------------------------------------------------

// PrepareBody 改写发往国际版上游的 chat 请求体。四条改写均有实测依据：
//
//  1. 强制 stream=true      —— stream:false → 400 code=11101 "Non-stream chat request is currently not supported"
//  2. tool_choice 归一化     —— 对象形式 → 400 code=11101 "cannot unmarshal object into Go struct field Request.tool_choice of type string"
//  3. developer → system     —— 否则 → 400 code=11128 "Illegal API invocation from an unapproved channel"
//  4. 保证首条为 system      —— 否则 → 400 code=11128 "first message is not system prompt"
//
// 无法解析为 JSON 对象时原样返回（交由上游给出更准确的报错）。
func PrepareBody(src []byte) []byte {
	if len(src) == 0 {
		return src
	}
	var obj map[string]any
	if err := json.Unmarshal(src, &obj); err != nil {
		return src
	}
	obj["stream"] = true
	normalizeRoles(obj)
	normalizeToolChoice(obj)
	ensureLeadingSystemMessage(obj)
	out, err := json.Marshal(obj)
	if err != nil {
		return src
	}
	return out
}

// ensureLeadingSystemMessage 保证首条消息为 system。
// 仅修复「首条存在但不是 system」的情况；messages 缺失/为空时不干预，
// 让上游自身的参数校验给出更贴切的报错。
func ensureLeadingSystemMessage(obj map[string]any) {
	msgs, ok := obj["messages"].([]any)
	if !ok || len(msgs) == 0 {
		return
	}
	if first, ok := msgs[0].(map[string]any); ok {
		if r, _ := first["role"].(string); r == "system" {
			return
		}
	}
	obj["messages"] = append([]any{map[string]any{
		"role":    "system",
		"content": "You are a helpful assistant.",
	}}, msgs...)
}

// normalizeRoles 将 OpenAI 的 developer 角色改写为 system。
// 上游对 role=developer 一律拒绝（code=11128）。
func normalizeRoles(obj map[string]any) {
	msgs, ok := obj["messages"].([]any)
	if !ok {
		return
	}
	for _, m := range msgs {
		mm, ok := m.(map[string]any)
		if !ok {
			continue
		}
		if r, _ := mm["role"].(string); r == "developer" {
			mm["role"] = "system"
		}
	}
}

// normalizeToolChoice 按上游 string 类型要求改写 tool_choice：
//   - "none" / {"type":"none"}                   → 删 tool_choice + 删 tools/functions
//   - {"type":"auto"|"required"}                 → 字符串 "auto"/"required"
//   - {"type":"function","function":{"name":"x"}} → 字符串 "x"
//   - 其他对象/非标量                              → 删 tool_choice
func normalizeToolChoice(obj map[string]any) {
	suppress := func() {
		delete(obj, "tools")
		delete(obj, "functions")
	}
	tc, present := obj["tool_choice"]
	if !present {
		return
	}
	switch v := tc.(type) {
	case string:
		if strings.EqualFold(strings.TrimSpace(v), "none") {
			delete(obj, "tool_choice")
			suppress()
		}
	case map[string]any:
		typ, _ := v["type"].(string)
		switch strings.ToLower(strings.TrimSpace(typ)) {
		case "none":
			delete(obj, "tool_choice")
			suppress()
		case "auto", "required":
			obj["tool_choice"] = strings.ToLower(strings.TrimSpace(typ))
		case "function":
			name := ""
			if fn, ok := v["function"].(map[string]any); ok {
				name, _ = fn["name"].(string)
			}
			if name == "" {
				name, _ = v["name"].(string)
			}
			if name = strings.TrimSpace(name); name != "" {
				obj["tool_choice"] = name
			} else {
				obj["tool_choice"] = "auto"
			}
		default:
			delete(obj, "tool_choice")
		}
	default:
		delete(obj, "tool_choice")
	}
}

// ---------------------------------------------------------------------------
// SSE 透传 / 聚合
// ---------------------------------------------------------------------------

// Stream 透传上游 SSE 到 w（逐行 flush），保证至少写一个 [DONE]。
// 调用方须先设置过 200 状态码；本函数自设 SSE headers。
func Stream(w http.ResponseWriter, r io.Reader) error {
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("Connection", "keep-alive")
	h.Set("X-Accel-Buffering", "no")
	fl, _ := w.(http.Flusher)
	br := bufio.NewReaderSize(r, 64*1024)
	sawDone := false
	for {
		line, err := br.ReadString('\n')
		if line != "" {
			if strings.HasPrefix(strings.TrimRight(line, "\r\n"), "data: [DONE]") {
				sawDone = true
			}
			if _, werr := io.WriteString(w, line); werr != nil {
				return werr
			}
			if fl != nil {
				fl.Flush()
			}
		}
		if err != nil {
			if err == io.EOF {
				break
			}
			return err
		}
	}
	if !sawDone {
		if _, err := io.WriteString(w, "data: [DONE]\n\n"); err != nil {
			return err
		}
		if fl != nil {
			fl.Flush()
		}
	}
	return nil
}

// Aggregate 读取完整 SSE 流，聚合为单个 OpenAI chat.completion 响应。
// 处理 content / reasoning_content / 流式 tool_calls（按 index 合并）。
// 上游 delta 额外含 extra_fields 等字段，此处忽略。
func Aggregate(r io.Reader) (map[string]any, error) {
	br := bufio.NewReaderSize(r, 64*1024)
	var (
		id, model     string
		created       float64
		content       strings.Builder
		reasoning     strings.Builder
		role          = "assistant"
		finishReason  = "stop"
		usage         map[string]any
		gotAnyContent bool
		toolCalls     = map[int]map[string]any{}
		toolOrder     []int
	)
	for {
		line, err := br.ReadString('\n')
		if err != nil && err != io.EOF {
			return nil, err
		}
		line = strings.TrimRight(line, "\r\n")
		if payload, ok := strings.CutPrefix(line, "data: "); ok && payload != "[DONE]" {
			var chunk map[string]any
			if json.Unmarshal([]byte(payload), &chunk) == nil {
				if v, ok := chunk["id"].(string); ok && id == "" {
					id = v
				}
				if v, ok := chunk["model"].(string); ok && model == "" {
					model = v
				}
				if v, ok := chunk["created"].(float64); ok && created == 0 {
					created = v
				}
				if u, ok := chunk["usage"].(map[string]any); ok {
					usage = u
				}
				if ch, ok := chunk["choices"].([]any); ok {
					for _, ci := range ch {
						c, _ := ci.(map[string]any)
						if c == nil {
							continue
						}
						if fr, ok := c["finish_reason"].(string); ok && fr != "" {
							finishReason = fr
						}
						delta, ok := c["delta"].(map[string]any)
						if !ok {
							// 少数上游把完整消息放在 message 里（非 delta）
							if msg, ok := c["message"].(map[string]any); ok && !gotAnyContent {
								if txt, ok := msg["content"].(string); ok {
									content.WriteString(txt)
								}
							}
							continue
						}
						if r2, ok := delta["role"].(string); ok && r2 != "" {
							role = r2
						}
						if txt, ok := delta["content"].(string); ok {
							content.WriteString(txt)
							gotAnyContent = true
						}
						// 国际版推理内容字段为 reasoning_content
						if rc, ok := delta["reasoning_content"].(string); ok {
							reasoning.WriteString(rc)
						}
						tcs, ok := delta["tool_calls"].([]any)
						if !ok {
							continue
						}
						for _, tc := range tcs {
							call, ok := tc.(map[string]any)
							if !ok {
								continue
							}
							idx := 0
							if v, ok := call["index"].(float64); ok {
								idx = int(v)
							}
							merged, seen := toolCalls[idx]
							if !seen {
								merged = map[string]any{"index": idx}
								toolCalls[idx] = merged
								toolOrder = append(toolOrder, idx)
							}
							mergeToolCallDelta(merged, call)
						}
					}
				}
			}
		}
		if err == io.EOF {
			break
		}
	}
	if id == "" {
		id = fmt.Sprintf("chatcmpl-%d", time.Now().UnixNano())
	}
	if created == 0 {
		created = float64(time.Now().Unix())
	}
	message := map[string]any{"role": role, "content": content.String()}
	if reasoning.Len() > 0 {
		message["reasoning_content"] = reasoning.String()
	}
	if len(toolOrder) > 0 {
		sortInts(toolOrder)
		calls := make([]map[string]any, 0, len(toolOrder))
		for _, idx := range toolOrder {
			calls = append(calls, toolCalls[idx])
		}
		message["tool_calls"] = calls
	}
	resp := map[string]any{
		"id":      id,
		"object":  "chat.completion",
		"created": int64(created),
		"model":   model,
		"choices": []any{map[string]any{"index": 0, "message": message, "finish_reason": finishReason}},
	}
	if usage != nil {
		resp["usage"] = usage
	}
	return resp, nil
}

// mergeToolCallDelta 合并流式 tool_call 片段：id/type/name 直覆盖，arguments 拼接。
func mergeToolCallDelta(merged, delta map[string]any) {
	if v, ok := delta["id"].(string); ok && v != "" {
		merged["id"] = v
	}
	if v, ok := delta["type"].(string); ok && v != "" {
		merged["type"] = v
	}
	df, _ := delta["function"].(map[string]any)
	if df == nil {
		return
	}
	mf, _ := merged["function"].(map[string]any)
	if mf == nil {
		mf = map[string]any{}
		merged["function"] = mf
	}
	if v, ok := df["name"].(string); ok && v != "" {
		mf["name"] = v
	}
	if v, ok := df["arguments"].(string); ok && v != "" {
		if prev, _ := mf["arguments"].(string); prev != "" {
			mf["arguments"] = prev + v
		} else {
			mf["arguments"] = v
		}
	}
}

// sortInts 升序排序（元素极少，插入排序足够）。
func sortInts(a []int) {
	for i := 1; i < len(a); i++ {
		for j := i; j > 0 && a[j] < a[j-1]; j-- {
			a[j], a[j-1] = a[j-1], a[j]
		}
	}
}

// ---------------------------------------------------------------------------
// 文案与倍率
// ---------------------------------------------------------------------------

// truncate 截断错误文案（保留可读头部）。
func truncate(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) > n {
		return s[:n]
	}
	return s
}

// parseCredits 解析目录 credits 字段为倍率数值：
// "x0.79 credits" / "x3.47" / "0x" → 0.79 / 3.47 / 0。
func parseCredits(s string) float64 {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0
	}
	s = strings.TrimPrefix(s, "x")
	s = strings.TrimSuffix(s, "x") // "0x" 形式（促销折扣）
	s = strings.TrimSuffix(s, " credits")
	s = strings.TrimSpace(s)
	var v float64
	fmt.Sscanf(s, "%f", &v)
	return v
}
