package repository

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
)

// codexTranscriptRepository Codex 对话留痕仓储（raw SQL，append-only）。
//
// 刻意不实现单条/条件删除：留痕只允许追加与按保留期批量清理。
type codexTranscriptRepository struct {
	db *sql.DB
}

// NewCodexTranscriptRepository 创建对话留痕仓储。
func NewCodexTranscriptRepository(db *sql.DB) service.CodexTranscriptRepository {
	return &codexTranscriptRepository{db: db}
}

// codexTranscriptColumns 与 codexTranscriptScanDest 必须逐一对应。
const codexTranscriptColumns = `id, created_at, request_id, api_key_id, user_id, session_id, model,
turn_kind, instructions_full, instructions_hash, user_input, user_input_truncated,
tool_output, tool_output_truncated, output_text, output_truncated, output_reasoning`

const codexTranscriptInsertColumnCount = 16

func codexTranscriptScanDest(turn *service.CodexConversationTurn) []any {
	return []any{
		&turn.ID,
		&turn.CreatedAt,
		&turn.RequestID,
		&turn.APIKeyID,
		&turn.UserID,
		&turn.SessionID,
		&turn.Model,
		&turn.TurnKind,
		&turn.InstructionsFull,
		&turn.InstructionsHash,
		&turn.UserInput,
		&turn.UserInputTruncated,
		&turn.ToolOutput,
		&turn.ToolOutputTruncated,
		&turn.OutputText,
		&turn.OutputTruncated,
		&turn.OutputReasoning,
	}
}

func codexTranscriptInsertValues(turn *service.CodexConversationTurn) []any {
	createdAt := turn.CreatedAt
	if createdAt.IsZero() {
		createdAt = time.Now().UTC()
	}
	return []any{
		createdAt.UTC(),
		truncateString(turn.RequestID, 64),
		turn.APIKeyID,
		turn.UserID,
		truncateString(turn.SessionID, 255),
		truncateString(turn.Model, 100),
		turn.TurnKind,
		turn.InstructionsFull,
		turn.InstructionsHash,
		turn.UserInput,
		turn.UserInputTruncated,
		turn.ToolOutput,
		turn.ToolOutputTruncated,
		turn.OutputText,
		turn.OutputTruncated,
		turn.OutputReasoning,
	}
}

// BatchInsert 多行插入对话留痕。
//
// 用多行 INSERT 而非 pq.CopyIn：唯一键 (request_id, api_key_id) 需要 ON CONFLICT
// DO NOTHING 来吸收 failover 重试（共用同一 client request id），而 COPY 不支持
// ON CONFLICT。batch 100 × 16 列 = 1600 个占位符，远低于 65535 上限。
func (r *codexTranscriptRepository) BatchInsert(ctx context.Context, turns []*service.CodexConversationTurn) (int64, error) {
	if r == nil || r.db == nil {
		return 0, fmt.Errorf("nil codex transcript repository")
	}
	if len(turns) == 0 {
		return 0, nil
	}

	placeholders := make([]string, 0, len(turns))
	args := make([]any, 0, len(turns)*codexTranscriptInsertColumnCount)
	position := 1
	for _, turn := range turns {
		if turn == nil || turn.RequestID == "" {
			continue
		}
		slots := make([]string, codexTranscriptInsertColumnCount)
		for i := range slots {
			slots[i] = fmt.Sprintf("$%d", position)
			position++
		}
		placeholders = append(placeholders, "("+strings.Join(slots, ",")+")")
		args = append(args, codexTranscriptInsertValues(turn)...)
	}
	if len(placeholders) == 0 {
		return 0, nil
	}

	query := `INSERT INTO codex_conversation_turns (
created_at, request_id, api_key_id, user_id, session_id, model, turn_kind,
instructions_full, instructions_hash, user_input, user_input_truncated,
tool_output, tool_output_truncated, output_text, output_truncated, output_reasoning
) VALUES ` + strings.Join(placeholders, ",") + `
ON CONFLICT (request_id, api_key_id) DO NOTHING`

	res, err := r.db.ExecContext(ctx, query, args...)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// DeleteBefore 分批删除过期留痕，单批走 PK 顺序扫，避免长事务锁表。
func (r *codexTranscriptRepository) DeleteBefore(ctx context.Context, cutoff time.Time, batchSize int) (int64, error) {
	if r == nil || r.db == nil {
		return 0, fmt.Errorf("nil codex transcript repository")
	}
	if batchSize <= 0 {
		batchSize = 5000
	}
	res, err := r.db.ExecContext(ctx, `
WITH batch AS (
  SELECT id FROM codex_conversation_turns WHERE created_at < $1 ORDER BY id LIMIT $2
)
DELETE FROM codex_conversation_turns WHERE id IN (SELECT id FROM batch)`, cutoff.UTC(), batchSize)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// GetByRequestID 按使用日志的 request_id 反查对话留痕；未找到返回 (nil, nil)。
func (r *codexTranscriptRepository) GetByRequestID(ctx context.Context, requestID string) (*service.CodexConversationTurn, error) {
	if r == nil || r.db == nil {
		return nil, fmt.Errorf("nil codex transcript repository")
	}
	requestID = strings.TrimSpace(requestID)
	if requestID == "" {
		return nil, nil
	}

	turn := &service.CodexConversationTurn{}
	row := r.db.QueryRowContext(ctx,
		`SELECT `+codexTranscriptColumns+`
FROM codex_conversation_turns WHERE request_id = $1 ORDER BY id LIMIT 1`, requestID)
	if err := row.Scan(codexTranscriptScanDest(turn)...); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	return turn, nil
}
