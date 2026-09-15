package handler

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/securityaudit"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/tidwall/gjson"
)

// codexTranscriptRequestDraft 请求侧抽取结果（本轮增量）。
type codexTranscriptRequestDraft struct {
	Model               string
	TurnKind            string
	UserInput           string
	UserInputTruncated  bool
	ToolOutput          string
	ToolOutputTruncated bool
	InstructionsFull    string
	InstructionsHash    string
}

// codexModelGeneratedItemTypes 模型生成的 input item 类型。
//
// 口径与 service.openAICyberTranscriptItemIsModelGenerated 一致，额外补上 reasoning
// 与 local_shell_call：多轮回放里它们同样是上一轮的产物，不属于本轮新增。
var codexModelGeneratedItemTypes = map[string]struct{}{
	"output_text":      {},
	"function_call":    {},
	"tool_call":        {},
	"custom_tool_call": {},
	"computer_call":    {},
	"local_shell_call": {},
	"reasoning":        {},
}

// codexToolOutputItemTypes 工具结果 item 类型。
var codexToolOutputItemTypes = map[string]struct{}{
	"function_call_output":    {},
	"custom_tool_call_output": {},
	"local_shell_call_output": {},
}

// extractCodexTurnDelta 从 Responses 请求体抽出"本轮增量"。
//
// 算法是尾部切片：客户端每轮回传完整历史，而模型生成的内容一定排在本轮新增之前，
// 所以倒序找到最后一个模型生成 item，它之后的那一段就是本轮新增。
// 这样 N 轮会话只产生 N 条记录，而不是 O(N^2) 的重复历史。
//
// 四种形态都正确：
//   - 会话首轮：无模型生成 item → 取全部
//   - 用户发新消息：[..., assistant, user] → 只取尾部 user
//   - 工具续链：[..., function_call, function_call_output] → 只取工具结果
//   - previous_response_id 续链：客户端不回传历史 → 取全部
//
// 用 gjson 而非 Unmarshal：调用点的 body 已通过 gjson 校验，且 Codex 请求含完整历史
// 可达数十 MB，全量反序列化会在热路径产生巨大分配。
func extractCodexTurnDelta(body []byte) codexTranscriptRequestDraft {
	draft := codexTranscriptRequestDraft{TurnKind: service.CodexTurnKindUnknown}
	if len(body) == 0 {
		return draft
	}
	root := gjson.ParseBytes(body)
	draft.Model = strings.TrimSpace(root.Get("model").String())

	if instructions := root.Get("instructions"); instructions.Exists() {
		if text := strings.TrimSpace(instructions.String()); text != "" {
			draft.InstructionsFull = securityaudit.TrimRunes(text, service.CodexTranscriptMaxInstructionsRunes)
			digest := sha256.Sum256([]byte(text))
			draft.InstructionsHash = hex.EncodeToString(digest[:])
		}
	}

	input := root.Get("input")
	if !input.Exists() {
		return draft
	}
	// input 也可能是纯字符串，此时整体就是本轮用户输入。
	if input.Type == gjson.String {
		draft.setUserInput(input.String())
		draft.TurnKind = codexResolveTurnKind(draft.UserInput, draft.ToolOutput)
		return draft
	}
	if !input.IsArray() {
		return draft
	}

	items := input.Array()
	turnStart := 0
	for i := len(items) - 1; i >= 0; i-- {
		if codexItemIsModelGenerated(items[i]) {
			turnStart = i + 1
			break
		}
	}

	var userParts []string
	var toolParts []string
	for _, item := range items[turnStart:] {
		if item.Type == gjson.String {
			if text := strings.TrimSpace(item.String()); text != "" {
				userParts = append(userParts, text)
			}
			continue
		}
		itemType := strings.ToLower(strings.TrimSpace(item.Get("type").String()))
		if _, isToolOutput := codexToolOutputItemTypes[itemType]; isToolOutput {
			if text := codexItemOutputText(item); text != "" {
				toolParts = append(toolParts, text)
			}
			continue
		}
		// role 为空时按用户输入处理（Responses 允许省略 role 的纯文本 item）。
		role := strings.ToLower(strings.TrimSpace(item.Get("role").String()))
		if role != "" && role != "user" && role != "system" && role != "developer" {
			continue
		}
		if text := codexItemContentText(item); text != "" {
			userParts = append(userParts, text)
		}
	}

	draft.setUserInput(strings.Join(userParts, "\n\n"))
	draft.setToolOutput(strings.Join(toolParts, "\n---\n"))
	draft.TurnKind = codexResolveTurnKind(draft.UserInput, draft.ToolOutput)
	return draft
}

func (d *codexTranscriptRequestDraft) setUserInput(text string) {
	text = strings.TrimSpace(text)
	if text == "" {
		return
	}
	trimmed := securityaudit.TrimRunes(text, service.CodexTranscriptMaxUserInputRunes)
	d.UserInput = trimmed
	d.UserInputTruncated = trimmed != text
}

func (d *codexTranscriptRequestDraft) setToolOutput(text string) {
	text = strings.TrimSpace(text)
	if text == "" {
		return
	}
	trimmed := securityaudit.TrimRunes(text, service.CodexTranscriptMaxToolOutputRunes)
	d.ToolOutput = trimmed
	d.ToolOutputTruncated = trimmed != text
}

func codexResolveTurnKind(userInput, toolOutput string) string {
	switch {
	case userInput != "" && toolOutput != "":
		return service.CodexTurnKindMixed
	case userInput != "":
		return service.CodexTurnKindUser
	case toolOutput != "":
		return service.CodexTurnKindTool
	default:
		return service.CodexTurnKindUnknown
	}
}

func codexItemIsModelGenerated(item gjson.Result) bool {
	switch strings.ToLower(strings.TrimSpace(item.Get("role").String())) {
	case "assistant", "model":
		return true
	}
	itemType := strings.ToLower(strings.TrimSpace(item.Get("type").String()))
	_, ok := codexModelGeneratedItemTypes[itemType]
	return ok
}

// codexItemOutputText 取工具结果文本。output 可能是字符串，也可能是结构化数组。
func codexItemOutputText(item gjson.Result) string {
	output := item.Get("output")
	if !output.Exists() {
		return ""
	}
	if output.Type == gjson.String {
		return strings.TrimSpace(output.String())
	}
	var parts []string
	output.ForEach(func(_, value gjson.Result) bool {
		if text := strings.TrimSpace(value.Get("text").String()); text != "" {
			parts = append(parts, text)
		}
		return true
	})
	if len(parts) > 0 {
		return strings.Join(parts, "\n")
	}
	return strings.TrimSpace(output.Raw)
}

// codexItemContentText 取 role-based message 的文本内容。
func codexItemContentText(item gjson.Result) string {
	content := item.Get("content")
	if !content.Exists() {
		return strings.TrimSpace(item.Get("text").String())
	}
	if content.Type == gjson.String {
		return strings.TrimSpace(content.String())
	}
	var parts []string
	content.ForEach(func(_, value gjson.Result) bool {
		if value.Type == gjson.String {
			if text := strings.TrimSpace(value.String()); text != "" {
				parts = append(parts, text)
			}
			return true
		}
		if text := strings.TrimSpace(value.Get("text").String()); text != "" {
			parts = append(parts, text)
		}
		return true
	})
	return strings.Join(parts, "\n\n")
}

// codexTranscriptOutputDraft 响应侧抽取结果。
type codexTranscriptOutputDraft struct {
	OutputText      string
	OutputTruncated bool
	Reasoning       string
}

// extractCodexOutputFromResponsesJSON 从非流式 Responses 响应体抽取模型输出。
func extractCodexOutputFromResponsesJSON(body []byte) codexTranscriptOutputDraft {
	draft := codexTranscriptOutputDraft{}
	if len(body) == 0 {
		return draft
	}
	output := gjson.ParseBytes(body).Get("output")
	if !output.Exists() || !output.IsArray() {
		return draft
	}

	var textParts []string
	var reasoningParts []string
	output.ForEach(func(_, item gjson.Result) bool {
		switch strings.ToLower(strings.TrimSpace(item.Get("type").String())) {
		case "reasoning":
			item.Get("summary").ForEach(func(_, entry gjson.Result) bool {
				if text := strings.TrimSpace(entry.Get("text").String()); text != "" {
					reasoningParts = append(reasoningParts, text)
				}
				return true
			})
		default:
			item.Get("content").ForEach(func(_, entry gjson.Result) bool {
				if text := strings.TrimSpace(entry.Get("text").String()); text != "" {
					textParts = append(textParts, text)
				}
				return true
			})
		}
		return true
	})

	draft.setOutputText(strings.Join(textParts, "\n"))
	draft.Reasoning = securityaudit.TrimRunes(
		strings.Join(reasoningParts, "\n"), service.CodexTranscriptMaxReasoningRunes)
	return draft
}

func (d *codexTranscriptOutputDraft) setOutputText(text string) {
	text = strings.TrimSpace(text)
	if text == "" {
		return
	}
	trimmed := securityaudit.TrimRunes(text, service.CodexTranscriptMaxOutputRunes)
	d.OutputText = trimmed
	d.OutputTruncated = trimmed != text
}
