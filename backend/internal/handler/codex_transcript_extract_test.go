//go:build unit

package handler

import (
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
)

// 会话首轮：input 里没有任何模型生成 item，整段都是本轮新增。
func TestExtractCodexTurnDeltaFirstTurn(t *testing.T) {
	body := []byte(`{
		"model":"gpt-5-codex",
		"instructions":"You are Codex.",
		"input":[
			{"type":"message","role":"user","content":[{"type":"input_text","text":"列出当前目录"}]}
		]
	}`)

	draft := extractCodexTurnDelta(body)

	if draft.Model != "gpt-5-codex" {
		t.Fatalf("model = %q, want gpt-5-codex", draft.Model)
	}
	if draft.UserInput != "列出当前目录" {
		t.Fatalf("user input = %q, want 列出当前目录", draft.UserInput)
	}
	if draft.ToolOutput != "" {
		t.Fatalf("tool output = %q, want empty", draft.ToolOutput)
	}
	if draft.TurnKind != service.CodexTurnKindUser {
		t.Fatalf("turn kind = %q, want %q", draft.TurnKind, service.CodexTurnKindUser)
	}
	if draft.InstructionsFull != "You are Codex." || draft.InstructionsHash == "" {
		t.Fatalf("instructions not captured: full=%q hash=%q", draft.InstructionsFull, draft.InstructionsHash)
	}
}

// 用户发新消息：历史里有 assistant 与 reasoning，只能取尾部那条 user。
// 这是最关键的断言 —— 取多了就等于把整段历史重复存一遍。
func TestExtractCodexTurnDeltaNewUserTurnExcludesHistory(t *testing.T) {
	body := []byte(`{
		"model":"gpt-5-codex",
		"input":[
			{"type":"message","role":"user","content":[{"type":"input_text","text":"第一轮提问"}]},
			{"type":"reasoning","summary":[{"type":"summary_text","text":"思考过程"}]},
			{"type":"message","role":"assistant","content":[{"type":"output_text","text":"第一轮回答"}]},
			{"type":"message","role":"user","content":[{"type":"input_text","text":"第二轮提问"}]}
		]
	}`)

	draft := extractCodexTurnDelta(body)

	if draft.UserInput != "第二轮提问" {
		t.Fatalf("user input = %q, want 第二轮提问", draft.UserInput)
	}
	for _, leaked := range []string{"第一轮提问", "第一轮回答", "思考过程"} {
		if strings.Contains(draft.UserInput, leaked) {
			t.Fatalf("user input leaked history %q: %q", leaked, draft.UserInput)
		}
	}
	if draft.TurnKind != service.CodexTurnKindUser {
		t.Fatalf("turn kind = %q, want %q", draft.TurnKind, service.CodexTurnKindUser)
	}
}

// 工具续链（Codex 最常见的形态）：本轮新增只有工具结果，没有人类输入。
// 工具结果必须落在独立列，否则"用户输入"会被命令输出淹没。
func TestExtractCodexTurnDeltaToolChain(t *testing.T) {
	body := []byte(`{
		"model":"gpt-5-codex",
		"input":[
			{"type":"message","role":"user","content":[{"type":"input_text","text":"列出当前目录"}]},
			{"type":"function_call","call_id":"c1","name":"shell","arguments":"{\"command\":[\"ls\"]}"},
			{"type":"function_call_output","call_id":"c1","output":"a.go\nb.go"}
		]
	}`)

	draft := extractCodexTurnDelta(body)

	if draft.UserInput != "" {
		t.Fatalf("user input = %q, want empty on a pure tool turn", draft.UserInput)
	}
	if draft.ToolOutput != "a.go\nb.go" {
		t.Fatalf("tool output = %q, want a.go\\nb.go", draft.ToolOutput)
	}
	if draft.TurnKind != service.CodexTurnKindTool {
		t.Fatalf("turn kind = %q, want %q", draft.TurnKind, service.CodexTurnKindTool)
	}
}

// previous_response_id 续链：客户端不回传历史，input 本身就是增量。
func TestExtractCodexTurnDeltaPreviousResponseIDChain(t *testing.T) {
	body := []byte(`{
		"model":"gpt-5-codex",
		"previous_response_id":"resp_abc",
		"input":[
			{"type":"message","role":"user","content":[{"type":"input_text","text":"接着说"}]}
		]
	}`)

	draft := extractCodexTurnDelta(body)

	if draft.UserInput != "接着说" {
		t.Fatalf("user input = %q, want 接着说", draft.UserInput)
	}
}

// input 为纯字符串的简写形态。
func TestExtractCodexTurnDeltaStringInput(t *testing.T) {
	draft := extractCodexTurnDelta([]byte(`{"model":"gpt-5-codex","input":"你好"}`))

	if draft.UserInput != "你好" {
		t.Fatalf("user input = %q, want 你好", draft.UserInput)
	}
	if draft.TurnKind != service.CodexTurnKindUser {
		t.Fatalf("turn kind = %q, want %q", draft.TurnKind, service.CodexTurnKindUser)
	}
}

// 同一轮里既有工具结果又有人类补充输入。
func TestExtractCodexTurnDeltaMixedTurn(t *testing.T) {
	body := []byte(`{
		"model":"gpt-5-codex",
		"input":[
			{"type":"message","role":"assistant","content":[{"type":"output_text","text":"上一轮回答"}]},
			{"type":"function_call_output","call_id":"c1","output":"命令输出"},
			{"type":"message","role":"user","content":[{"type":"input_text","text":"顺便看下 README"}]}
		]
	}`)

	draft := extractCodexTurnDelta(body)

	if draft.UserInput != "顺便看下 README" {
		t.Fatalf("user input = %q", draft.UserInput)
	}
	if draft.ToolOutput != "命令输出" {
		t.Fatalf("tool output = %q", draft.ToolOutput)
	}
	if draft.TurnKind != service.CodexTurnKindMixed {
		t.Fatalf("turn kind = %q, want %q", draft.TurnKind, service.CodexTurnKindMixed)
	}
}

// 无 input 字段时不应 panic，且产出空草稿。
func TestExtractCodexTurnDeltaMissingInput(t *testing.T) {
	draft := extractCodexTurnDelta([]byte(`{"model":"gpt-5-codex"}`))

	if draft.UserInput != "" || draft.ToolOutput != "" {
		t.Fatalf("expected empty draft, got %+v", draft)
	}
	if draft.TurnKind != service.CodexTurnKindUnknown {
		t.Fatalf("turn kind = %q, want %q", draft.TurnKind, service.CodexTurnKindUnknown)
	}
}

// 超长输入按 rune 截断并打标记（不能按字节切，会切断多字节字符）。
func TestExtractCodexTurnDeltaTruncatesByRune(t *testing.T) {
	long := strings.Repeat("中", service.CodexTranscriptMaxUserInputRunes+100)
	body := []byte(`{"model":"gpt-5-codex","input":"` + long + `"}`)

	draft := extractCodexTurnDelta(body)

	if !draft.UserInputTruncated {
		t.Fatal("expected UserInputTruncated = true")
	}
	if !strings.HasSuffix(draft.UserInput, "…") {
		t.Fatal("expected truncation ellipsis suffix")
	}
	// 截断后必须仍是合法 UTF-8，且不含替换字符。
	if strings.ContainsRune(draft.UserInput, '�') {
		t.Fatal("truncation split a multi-byte rune")
	}
}

// 非流式响应体的输出抽取：正文与 reasoning 分列。
func TestExtractCodexOutputFromResponsesJSON(t *testing.T) {
	body := []byte(`{
		"output":[
			{"type":"reasoning","summary":[{"type":"summary_text","text":"先看目录结构"}]},
			{"type":"message","content":[{"type":"output_text","text":"目录下有 a.go 和 b.go"}]}
		]
	}`)

	draft := extractCodexOutputFromResponsesJSON(body)

	if draft.OutputText != "目录下有 a.go 和 b.go" {
		t.Fatalf("output text = %q", draft.OutputText)
	}
	if draft.Reasoning != "先看目录结构" {
		t.Fatalf("reasoning = %q", draft.Reasoning)
	}
}

// instructions 去重：同会话同 hash 只在首次返回 true。
func TestCodexInstructionsDedup(t *testing.T) {
	dedup := &codexInstructionsDedup{
		seen:  make(map[string]string, 8),
		limit: 8,
	}

	if !dedup.shouldStoreFull(1, "sess-a", "hash-1") {
		t.Fatal("first sighting should store full instructions")
	}
	if dedup.shouldStoreFull(1, "sess-a", "hash-1") {
		t.Fatal("repeat sighting should not store full instructions")
	}
	// instructions 变化（漂移）时必须重新存全文。
	if !dedup.shouldStoreFull(1, "sess-a", "hash-2") {
		t.Fatal("changed instructions should store full again")
	}
	// 不同会话独立计算。
	if !dedup.shouldStoreFull(1, "sess-b", "hash-1") {
		t.Fatal("different session should store full")
	}
	// 空 hash 表示本轮没有 instructions。
	if dedup.shouldStoreFull(1, "sess-a", "") {
		t.Fatal("empty hash should never store full")
	}
}
