-- Codex（OpenAI Responses 协议）逐轮对话留痕（append-only），默认关闭、短保留。
-- 设计约束：
--   1. 只存"本轮增量"：客户端每轮回传完整历史，全量存会让表随会话长度平方级膨胀，
--      因此只保留 input 尾部那段未被模型生成内容分隔的 item（见 extractCodexTurnDelta）
--   2. instructions 仅会话首见时存全文，后续只留 hash 用于比对是否漂移
--   3. 工具结果（function_call_output）独立列：Codex 是 agent，一次提问后常跟数十轮纯
--      工具续链，混进 user_input 会让"用户输入"绝大部分变成命令 stdout，失去分析价值
--   4. request_id 与 usage_logs.request_id 同值（含 "client:" 前缀），刻意不加外键：
--      本表写入早于 usage_logs 落库（计费走异步队列），且 usage_logs 可被人工清理任务删除
CREATE TABLE IF NOT EXISTS codex_conversation_turns (
    id BIGSERIAL PRIMARY KEY,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),

    request_id VARCHAR(64) NOT NULL,
    api_key_id BIGINT NOT NULL,
    user_id BIGINT NOT NULL DEFAULT 0,
    session_id VARCHAR(255) NOT NULL DEFAULT '',
    model VARCHAR(100) NOT NULL DEFAULT '',
    turn_kind VARCHAR(16) NOT NULL DEFAULT 'unknown',

    instructions_full TEXT NOT NULL DEFAULT '',
    instructions_hash VARCHAR(64) NOT NULL DEFAULT '',

    user_input TEXT NOT NULL DEFAULT '',
    user_input_truncated BOOLEAN NOT NULL DEFAULT FALSE,
    tool_output TEXT NOT NULL DEFAULT '',
    tool_output_truncated BOOLEAN NOT NULL DEFAULT FALSE,

    output_text TEXT NOT NULL DEFAULT '',
    output_truncated BOOLEAN NOT NULL DEFAULT FALSE,
    output_reasoning TEXT NOT NULL DEFAULT '',

    CONSTRAINT chk_codex_turns_request_id_nonempty CHECK (request_id <> ''),
    CONSTRAINT chk_codex_turns_turn_kind
        CHECK (turn_kind IN ('user', 'tool', 'mixed', 'unknown'))
);

-- 与 usage_logs 的 (request_id, api_key_id) 唯一键同构，保证一条使用日志最多对一条
-- 对话记录。failover 重试共用同一 client request id，落库时 DO NOTHING 让首次胜出。
CREATE UNIQUE INDEX IF NOT EXISTS idx_codex_turns_request_api_key_unique
    ON codex_conversation_turns (request_id, api_key_id);

-- 保留期清理：WHERE created_at < cutoff ORDER BY id LIMIT n。
-- append-only 表里 id 序约等于 created_at 序，升序扫最老的行即命中。
CREATE INDEX IF NOT EXISTS idx_codex_turns_created_at_id
    ON codex_conversation_turns (created_at, id);

-- 按会话串联多轮对话
CREATE INDEX IF NOT EXISTS idx_codex_turns_session
    ON codex_conversation_turns (session_id, id)
    WHERE session_id <> '';

-- 默认关闭：存储对话原文属敏感行为，必须由部署方显式开启
INSERT INTO settings (key, value, updated_at)
VALUES ('codex_transcript_enabled', 'false', NOW())
ON CONFLICT (key) DO NOTHING;

INSERT INTO settings (key, value, updated_at)
VALUES ('codex_transcript_retention_days', '5', NOW())
ON CONFLICT (key) DO NOTHING;
