package handler

import (
	"bytes"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
	"github.com/Wei-Shaw/sub2api/internal/securityaudit"
	middleware2 "github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

// codexTranscriptRequestKey 暂存请求侧抽取结果的 gin context 键。
const codexTranscriptRequestKey = "codex_transcript_request_draft"

// codexTranscriptJSONBufLimit 非流式响应体的缓冲上限。
// 超过即放弃抽取：正常 Responses 响应远小于此，超限说明不是我们要记的形态。
const codexTranscriptJSONBufLimit = 4 << 20

// codexTranscriptRequestIDPrefix 与 usage_logs.request_id 的前缀保持一致。
// 见 service.resolveUsageBillingRequestID：普通请求取 "client:" + ClientRequestID。
const codexTranscriptRequestIDPrefix = "client:"

// stashCodexTranscriptRequest 抽取并暂存本轮请求增量。
//
// 在 handler 读完并校验 body 之后调用：此时 body 已在内存，抽取零额外读取；
// 且位置在透传路径补写 instructions 之前，拿到的是客户端原文。
func stashCodexTranscriptRequest(c *gin.Context, transcripts *service.CodexTranscriptService, body []byte) {
	if c == nil || transcripts == nil || len(body) == 0 {
		return
	}
	if !transcripts.Enabled(c.Request.Context()) {
		return
	}
	defer func() {
		// 抽取失败绝不能影响转发主流程。
		if recovered := recover(); recovered != nil {
			slog.Warn("codex transcript request extraction panicked", "panic", recovered)
		}
	}()
	draft := extractCodexTurnDelta(body)
	c.Set(codexTranscriptRequestKey, &draft)
}

// codexCaptureWriter 包装 gin.ResponseWriter，边转发边抽取模型输出文本。
//
// 内嵌 gin.ResponseWriter 而非持有指针：除 Write/WriteString 外的所有方法
// （Flush/Hijack/CloseNotify/Status/Size 等）都直接继承，不需要逐个转发。
// 只累积抽出来的文本，不缓存原始 SSE 字节。
type codexCaptureWriter struct {
	gin.ResponseWriter

	// mu 保护下面全部可变状态。透传路径的 SSE 心跳 goroutine 与 handler 主流程
	// 在极窄窗口内可能并发写同一个 writer（见 openai_gateway_passthrough.go 的
	// ensureResponseFailedTerminal 与 keepalive 的时序），无竞争时锁开销可忽略。
	mu sync.Mutex

	outputText    strings.Builder
	reasoningText strings.Builder

	partial []byte // 跨 Write 调用的半行残留
	jsonBuf []byte // 仅非流式使用
	mode    int8   // 0=未判定 1=SSE 2=JSON 3=放弃

	// outputCapped 记录累积是否因触顶而提前停止。
	// 必须显式记录：停止累积后 builder 内容不再超限，TrimRunes 的
	// "trimmed != text" 判断会失效，截断标记会丢。
	outputCapped bool
}

func (w *codexCaptureWriter) Write(b []byte) (int, error) {
	w.observe(b)
	return w.ResponseWriter.Write(b)
}

func (w *codexCaptureWriter) WriteString(s string) (int, error) {
	w.observe([]byte(s))
	return w.ResponseWriter.WriteString(s)
}

// observe 解析响应字节，只读不改。
//
// 任何解析异常都必须在这里吞掉：这是网关的响应主路径，留痕失败可以接受，
// 影响客户端收包绝不可以接受。
func (w *codexCaptureWriter) observe(b []byte) {
	w.mu.Lock()
	defer w.mu.Unlock()
	defer func() {
		if recovered := recover(); recovered != nil {
			w.mode = 3
			slog.Warn("codex transcript response capture panicked", "panic", recovered)
		}
	}()
	if w.mode == 3 || len(b) == 0 {
		return
	}
	if w.mode == 0 {
		contentType := strings.ToLower(w.Header().Get("Content-Type"))
		switch {
		case strings.Contains(contentType, "event-stream"):
			w.mode = 1
		case strings.Contains(contentType, "json"):
			w.mode = 2
		default:
			w.mode = 3
			return
		}
	}

	if w.mode == 2 {
		if len(w.jsonBuf)+len(b) > codexTranscriptJSONBufLimit {
			w.mode = 3
			w.jsonBuf = nil
			return
		}
		w.jsonBuf = append(w.jsonBuf, b...)
		return
	}
	w.scanSSE(b)
}

// scanSSE 增量扫描 SSE 行，累积 data 行里的文本增量。
func (w *codexCaptureWriter) scanSSE(b []byte) {
	data := b
	if len(w.partial) > 0 {
		data = append(w.partial, b...)
		w.partial = nil
	}
	for {
		index := bytes.IndexByte(data, '\n')
		if index < 0 {
			break
		}
		w.consumeSSELine(bytes.TrimRight(data[:index], "\r"))
		data = data[index+1:]
	}
	if len(data) > 0 {
		// 残留半行留到下次 Write 拼接；超长说明不是正常 SSE，直接放弃。
		if len(data) > codexTranscriptJSONBufLimit {
			w.mode = 3
			w.partial = nil
			return
		}
		w.partial = append(w.partial[:0], data...)
	}
}

func (w *codexCaptureWriter) consumeSSELine(line []byte) {
	if !bytes.HasPrefix(line, []byte("data:")) {
		return
	}
	payload := bytes.TrimSpace(line[len("data:"):])
	if len(payload) == 0 || payload[0] != '{' {
		return
	}
	// 先按事件类型快筛，避免对无关 delta 帧做无谓的 JSON 取值。
	eventType := gjson.GetBytes(payload, "type").String()
	switch eventType {
	case "response.output_text.delta":
		if !w.appendCapped(&w.outputText, gjson.GetBytes(payload, "delta").String(),
			service.CodexTranscriptMaxOutputRunes) {
			w.outputCapped = true
		}
	case "response.reasoning_text.delta", "response.reasoning_summary_text.delta":
		w.appendCapped(&w.reasoningText, gjson.GetBytes(payload, "delta").String(),
			service.CodexTranscriptMaxReasoningRunes)
	}
}

// appendCapped 追加文本，触顶则停止累积以防单次响应撑爆内存。
// 返回 false 表示本次因触顶被丢弃。
func (w *codexCaptureWriter) appendCapped(builder *strings.Builder, text string, limitRunes int) bool {
	if text == "" {
		return true
	}
	// 按字节粗略判断：UTF-8 下 rune 数不超过字节数，先用字节挡住即可，
	// 精确的 rune 截断在 draft 里统一做。
	if builder.Len() >= limitRunes*4 {
		return false
	}
	builder.WriteString(text)
	return true
}

// outputDraft 汇总响应侧抽取结果，全部拍成独立 string。
func (w *codexCaptureWriter) outputDraft() codexTranscriptOutputDraft {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.mode == 2 && len(w.jsonBuf) > 0 {
		return extractCodexOutputFromResponsesJSON(w.jsonBuf)
	}
	draft := codexTranscriptOutputDraft{}
	draft.setOutputText(w.outputText.String())
	if w.outputCapped {
		draft.OutputTruncated = true
	}
	draft.Reasoning = securityaudit.TrimRunes(
		strings.TrimSpace(w.reasoningText.String()), service.CodexTranscriptMaxReasoningRunes)
	return draft
}

// CodexTranscriptMiddleware 捕获 Codex 每轮的输入与输出原文。
//
// 只包一层 ResponseWriter 就能覆盖全部 HTTP 转发路径（常规/透传、流式/非流式），
// 不需要在每条内部服务路径上单独接文本累积器。
//
// 已知盲区：WebSocket 上游（账号级 openai_*_responses_websockets_v2_* 开关，默认关）。
// WS 连接被 hijack 后不再经过 gin.ResponseWriter，因此不产生留痕；
// 这种情况下会打一条 WARN，避免被误读成"用户没用"。
func (h *OpenAIGatewayHandler) CodexTranscriptMiddleware() gin.HandlerFunc {
	if h == nil {
		return func(c *gin.Context) { c.Next() }
	}
	return codexTranscriptMiddleware(h.codexTranscriptService)
}

func codexTranscriptMiddleware(transcripts *service.CodexTranscriptService) gin.HandlerFunc {
	return func(c *gin.Context) {
		if transcripts == nil || !transcripts.Enabled(c.Request.Context()) {
			c.Next()
			return
		}

		originalWriter := c.Writer
		capture := &codexCaptureWriter{ResponseWriter: originalWriter}
		c.Writer = capture
		defer func() {
			// 先还原 writer，避免外层中间件观察到包装器。
			if c.Writer == capture {
				c.Writer = originalWriter
			}
		}()

		c.Next()

		recordCodexTranscript(c, transcripts, capture)
	}
}

// recordCodexTranscript 组装并入队一条留痕。
func recordCodexTranscript(c *gin.Context, transcripts *service.CodexTranscriptService, capture *codexCaptureWriter) {
	defer func() {
		if recovered := recover(); recovered != nil {
			slog.Warn("codex transcript record panicked", "panic", recovered)
		}
	}()

	value, exists := c.Get(codexTranscriptRequestKey)
	if !exists {
		return
	}
	request, ok := value.(*codexTranscriptRequestDraft)
	if !ok || request == nil {
		return
	}

	// 4xx 是网关本地拒绝（参数非法、模型不允许等），不产生 usage_logs 行，
	// 留痕会成为孤儿；且持有合法 key 者可用故意非法的请求零成本刷库。
	// 上游 5xx 仍记录：那是真实发生过的一轮，失败原因对分析有价值。
	if status := capture.Status(); status >= 400 && status < 500 {
		return
	}

	clientRequestID, _ := c.Request.Context().Value(ctxkey.ClientRequestID).(string)
	clientRequestID = strings.TrimSpace(clientRequestID)
	if clientRequestID == "" {
		return // 没有关联键就无法反查对应的使用日志，记了也没用
	}

	apiKey, ok := middleware2.GetAPIKeyFromContext(c)
	if !ok {
		return
	}
	var userID int64
	if subject, exists := middleware2.GetAuthSubjectFromContext(c); exists {
		userID = subject.UserID
	}

	output := capture.outputDraft()
	if request.UserInput == "" && request.ToolOutput == "" &&
		output.OutputText == "" && output.Reasoning == "" {
		return // 空记录无分析价值（如鉴权失败、请求被拦截）
	}

	sessionID := service.ExtractClientSessionID(c)
	// instructions 每轮回传且有 10-20KB，只在会话首见时存全文。
	instructionsFull := request.InstructionsFull
	if !codexInstructionsSeen.shouldStoreFull(apiKey.ID, sessionID, request.InstructionsHash) {
		instructionsFull = ""
	}

	// 所有文本都是独立 string（TrimRunes/Builder.String 均为拷贝），
	// 队列元素不持有 writer 或响应缓冲，避免闭包保活整个响应体。
	transcripts.Record(&service.CodexConversationTurn{
		CreatedAt:           time.Now().UTC(),
		RequestID:           codexTranscriptRequestIDPrefix + clientRequestID,
		APIKeyID:            apiKey.ID,
		UserID:              userID,
		SessionID:           sessionID,
		Model:               request.Model,
		TurnKind:            request.TurnKind,
		InstructionsFull:    instructionsFull,
		InstructionsHash:    request.InstructionsHash,
		UserInput:           request.UserInput,
		UserInputTruncated:  request.UserInputTruncated,
		ToolOutput:          request.ToolOutput,
		ToolOutputTruncated: request.ToolOutputTruncated,
		OutputText:          output.OutputText,
		OutputTruncated:     output.OutputTruncated,
		OutputReasoning:     output.Reasoning,
	})
}
