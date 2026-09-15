package service

import (
	"context"
	"time"
)

// Codex 对话留痕的单列文本上限（按 rune 计，不是字节）。
//
// 上限是代码常量而非配置：这类"列宽/内存保护"在本项目一律固定（参照
// maxModerationExcerptRunes），从根上不存在"配置缺失"这个状态。
//
// tool_output 刻意收得更紧：Codex 的长尾几乎全在工具结果上（cat 大文件、
// cargo build 全量 stderr），放开会让单行膨胀到 MB 级并挤占真正的人类文本预算。
const (
	CodexTranscriptMaxUserInputRunes    = 65536 // 对齐 securityaudit.DefaultFullPromptMaxRunes
	CodexTranscriptMaxOutputRunes       = 65536
	CodexTranscriptMaxReasoningRunes    = 32768
	CodexTranscriptMaxInstructionsRunes = 65536
	CodexTranscriptMaxToolOutputRunes   = 8192
)

// Codex 对话轮次类型。纯工具续链轮的 user_input 为空，分析时可用
// turn_kind <> 'tool' 一把筛掉，只看人类对话。
const (
	CodexTurnKindUser    = "user"
	CodexTurnKindTool    = "tool"
	CodexTurnKindMixed   = "mixed"
	CodexTurnKindUnknown = "unknown"
)

// CodexConversationTurn 一轮 Codex 对话的留痕记录（落库值对象，全部为纯值）。
//
// 只保存"本轮增量"：客户端每轮回传完整历史，全量存会让表随会话长度平方级膨胀。
type CodexConversationTurn struct {
	ID        int64     `json:"id"`
	CreatedAt time.Time `json:"created_at"`

	// RequestID 与 usage_logs.request_id 同值（含 "client:" 前缀），用于反查对应的使用日志。
	RequestID string `json:"request_id"`
	APIKeyID  int64  `json:"api_key_id"`
	UserID    int64  `json:"user_id"`
	SessionID string `json:"session_id"`
	Model     string `json:"model"`
	TurnKind  string `json:"turn_kind"`

	// InstructionsFull 仅会话首见时非空；后续轮次只留 InstructionsHash 用于比对漂移。
	InstructionsFull string `json:"instructions_full"`
	InstructionsHash string `json:"instructions_hash"`

	UserInput           string `json:"user_input"`
	UserInputTruncated  bool   `json:"user_input_truncated"`
	ToolOutput          string `json:"tool_output"`
	ToolOutputTruncated bool   `json:"tool_output_truncated"`

	OutputText      string `json:"output_text"`
	OutputTruncated bool   `json:"output_truncated"`
	OutputReasoning string `json:"output_reasoning"`
}

// CodexTranscriptRepository Codex 对话留痕仓储。
//
// 刻意不提供单条/条件删除：留痕只允许追加与按保留期批量清理。
type CodexTranscriptRepository interface {
	BatchInsert(ctx context.Context, turns []*CodexConversationTurn) (int64, error)
	DeleteBefore(ctx context.Context, cutoff time.Time, batchSize int) (int64, error)
	GetByRequestID(ctx context.Context, requestID string) (*CodexConversationTurn, error)
}
