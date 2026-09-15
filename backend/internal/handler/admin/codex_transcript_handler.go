package admin

import (
	"strings"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/Wei-Shaw/sub2api/internal/pkg/response"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
)

// CodexTranscriptHandler 处理 Codex 对话留痕的查询请求。
type CodexTranscriptHandler struct {
	service *service.CodexTranscriptService
}

// NewCodexTranscriptHandler 创建对话留痕处理器。
func NewCodexTranscriptHandler(svc *service.CodexTranscriptService) *CodexTranscriptHandler {
	return &CodexTranscriptHandler{service: svc}
}

// GetByRequestID 按使用日志的 request_id 反查该轮对话的输入与输出原文。
//
// 使用日志列表刻意不返回原文（对话原文动辄几 KB，会拖慢列表查询），
// 由前端点击"查看对话"时按需拉取单条。
func (h *CodexTranscriptHandler) GetByRequestID(c *gin.Context) {
	if h == nil || h.service == nil {
		response.ErrorFrom(c, infraerrors.NotFound("codex_transcript_disabled", "对话留痕功能未启用"))
		return
	}

	// 用 query 而非路径段：request_id 形如 "client:<uuid>"，含冒号，放路径里需要额外编码。
	requestID := strings.TrimSpace(c.Query("request_id"))
	if requestID == "" {
		response.ErrorFrom(c, infraerrors.BadRequest("codex_transcript_invalid_request_id", "request_id 无效"))
		return
	}

	turn, err := h.service.GetByRequestID(c.Request.Context(), requestID)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	if turn == nil {
		response.ErrorFrom(c, infraerrors.NotFound("codex_transcript_not_found", "该请求没有对话留痕记录"))
		return
	}
	response.Success(c, turn)
}
