package service

import (
	"context"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"
)

const (
	codexTranscriptQueueCapacity = 4096
	codexTranscriptBatchSize     = 100
	codexTranscriptFlushInterval = time.Second

	codexTranscriptRetentionCheckInterval = 24 * time.Hour
	codexTranscriptRetentionStartupDelay  = 5 * time.Minute
	codexTranscriptRetentionBatchSize     = 5000

	// codexTranscriptEnabledTTL 开关快照有效期。
	//
	// 开关要在每个 Codex 请求进入网关前判定，而 settingRepo.GetValue 是直查 DB 无缓存，
	// 每请求查一次库不可接受。代价是改开关最多 1s 后生效（与风控开关一致）。
	codexTranscriptEnabledTTL = time.Second

	// codexTranscriptEnabledRefreshTimeout 刷开关快照的读库超时。
	codexTranscriptEnabledRefreshTimeout = 2 * time.Second

	// codexTranscriptFlushBackoffMin/Max 连续写库失败的指数退避区间，
	// 避免留痕写入打爆连接池、影响网关主流程。
	codexTranscriptFlushBackoffMin = 2 * time.Second
	codexTranscriptFlushBackoffMax = 60 * time.Second
)

// codexTranscriptEnabledSnapshot 开关快照。
type codexTranscriptEnabledSnapshot struct {
	enabled  bool
	loadedAt time.Time
}

// CodexTranscriptService Codex 对话留痕服务。
//
// 写入端为非阻塞异步批量落库：队列打满即丢并计数，刻意不做同步兜底 —— 对话留痕是
// 可丢的观测数据，绝不能因写库慢拖累网关转发。
// 清理端按保留期分批删除；删除操作幂等，多实例并发执行无害，因此无需选主。
type CodexTranscriptService struct {
	repo           CodexTranscriptRepository
	settingService *SettingService

	queue chan *CodexConversationTurn

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup

	enabledSnapshot atomic.Pointer[codexTranscriptEnabledSnapshot]

	droppedCount uint64
	writeFailed  uint64
	writtenCount uint64
	lastDropLog  atomic.Int64
}

// NewCodexTranscriptService 创建对话留痕服务。
func NewCodexTranscriptService(repo CodexTranscriptRepository, settingService *SettingService) *CodexTranscriptService {
	ctx, cancel := context.WithCancel(context.Background())
	return &CodexTranscriptService{
		repo:           repo,
		settingService: settingService,
		queue:          make(chan *CodexConversationTurn, codexTranscriptQueueCapacity),
		ctx:            ctx,
		cancel:         cancel,
	}
}

// Start 启动异步写入与保留期清理协程。
func (s *CodexTranscriptService) Start() {
	if s == nil || s.repo == nil {
		return
	}
	s.wg.Add(2)
	go s.runWriter()
	go s.runRetentionLoop()
}

// Stop 停止服务并尽量落盘队列中剩余记录。
func (s *CodexTranscriptService) Stop() {
	if s == nil {
		return
	}
	s.cancel()
	s.wg.Wait()
}

// Enabled 返回对话留痕开关（带短 TTL 快照，可安全用于每请求热路径）。
// 入参 ctx 仅保留签名一致性，快照刷新刻意不受其取消影响（见函数内注释）。
func (s *CodexTranscriptService) Enabled(_ context.Context) bool {
	if s == nil || s.repo == nil || s.settingService == nil {
		return false
	}
	now := time.Now()
	if snapshot := s.enabledSnapshot.Load(); snapshot != nil &&
		now.Sub(snapshot.loadedAt) < codexTranscriptEnabledTTL {
		return snapshot.enabled
	}
	// 刷快照不用请求 ctx：请求被取消会让读库失败并被当成"关闭"缓存 1s，
	// 期间所有请求都不留痕。用独立超时 ctx 把快照刷新与单次请求生命周期解耦。
	refreshCtx, cancel := context.WithTimeout(context.Background(), codexTranscriptEnabledRefreshTimeout)
	defer cancel()
	enabled := s.settingService.IsCodexTranscriptEnabled(refreshCtx)
	s.enabledSnapshot.Store(&codexTranscriptEnabledSnapshot{enabled: enabled, loadedAt: now})
	return enabled
}

// Record 非阻塞入队一条对话留痕；队列打满时丢弃并计数。
//
// 调用方必须保证 turn 内的文本已是独立 string（不持有响应缓冲或 chunk 切片），
// 否则整个响应体会被队列元素保活到写库完成。
func (s *CodexTranscriptService) Record(turn *CodexConversationTurn) {
	if s == nil || s.repo == nil || turn == nil {
		return
	}
	if turn.RequestID == "" {
		return // 空 request_id 既违反表约束，也无法反查对应的使用日志
	}
	if turn.CreatedAt.IsZero() {
		turn.CreatedAt = time.Now().UTC()
	}
	select {
	case <-s.ctx.Done():
		return
	default:
	}
	select {
	case s.queue <- turn:
	default:
		atomic.AddUint64(&s.droppedCount, 1)
		s.logDropThrottled()
	}
}

// GetByRequestID 按使用日志的 request_id 反查对话留痕。
func (s *CodexTranscriptService) GetByRequestID(ctx context.Context, requestID string) (*CodexConversationTurn, error) {
	return s.repo.GetByRequestID(ctx, requestID)
}

// Stats 返回运行计数，便于排查队列容量是否不足。
func (s *CodexTranscriptService) Stats() (written, dropped, failed uint64) {
	if s == nil {
		return 0, 0, 0
	}
	return atomic.LoadUint64(&s.writtenCount),
		atomic.LoadUint64(&s.droppedCount),
		atomic.LoadUint64(&s.writeFailed)
}

// logDropThrottled 丢弃日志限流，5s 一条，避免队列持续打满时刷爆日志。
func (s *CodexTranscriptService) logDropThrottled() {
	now := time.Now().UnixNano()
	last := s.lastDropLog.Load()
	if now-last < int64(5*time.Second) {
		return
	}
	if !s.lastDropLog.CompareAndSwap(last, now) {
		return
	}
	slog.Warn("codex transcript queue full, dropping records",
		"dropped_total", atomic.LoadUint64(&s.droppedCount),
		"queue_capacity", codexTranscriptQueueCapacity)
}

func (s *CodexTranscriptService) runWriter() {
	defer s.wg.Done()

	ticker := time.NewTicker(codexTranscriptFlushInterval)
	defer ticker.Stop()

	batch := make([]*CodexConversationTurn, 0, codexTranscriptBatchSize)
	backoff := time.Duration(0)

	flush := func() {
		if len(batch) == 0 {
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		inserted, err := s.repo.BatchInsert(ctx, batch)
		cancel()
		if err != nil {
			atomic.AddUint64(&s.writeFailed, uint64(len(batch)))
			slog.Warn("codex transcript flush failed", "error", err, "batch", len(batch))
			// 连续失败时指数退避，避免打爆连接池。
			if backoff == 0 {
				backoff = codexTranscriptFlushBackoffMin
			} else if backoff < codexTranscriptFlushBackoffMax {
				backoff *= 2
				if backoff > codexTranscriptFlushBackoffMax {
					backoff = codexTranscriptFlushBackoffMax
				}
			}
			ticker.Reset(backoff)
		} else {
			atomic.AddUint64(&s.writtenCount, uint64(inserted))
			if backoff != 0 {
				backoff = 0
				ticker.Reset(codexTranscriptFlushInterval)
			}
		}
		batch = batch[:0]
	}

	for {
		select {
		case <-s.ctx.Done():
			// 停机前排空队列。
			for {
				select {
				case item := <-s.queue:
					if item == nil {
						continue
					}
					batch = append(batch, item)
					if len(batch) >= codexTranscriptBatchSize {
						flush()
					}
				default:
					flush()
					return
				}
			}
		case item := <-s.queue:
			if item == nil {
				continue
			}
			batch = append(batch, item)
			if len(batch) >= codexTranscriptBatchSize {
				flush()
			}
		case <-ticker.C:
			flush()
		}
	}
}

// runRetentionLoop 按保留期定期删除过期对话留痕。
// 删除操作幂等，多实例并发执行无害，因此无需选主。
func (s *CodexTranscriptService) runRetentionLoop() {
	defer s.wg.Done()

	startupTimer := time.NewTimer(codexTranscriptRetentionStartupDelay)
	defer startupTimer.Stop()
	select {
	case <-s.ctx.Done():
		return
	case <-startupTimer.C:
	}

	ticker := time.NewTicker(codexTranscriptRetentionCheckInterval)
	defer ticker.Stop()

	s.runRetentionOnce()
	for {
		select {
		case <-s.ctx.Done():
			return
		case <-ticker.C:
			s.runRetentionOnce()
		}
	}
}

func (s *CodexTranscriptService) runRetentionOnce() {
	ctx, cancel := context.WithTimeout(s.ctx, 10*time.Minute)
	defer cancel()

	days := 0
	if s.settingService != nil {
		days = s.settingService.GetCodexTranscriptRetentionDays(ctx)
	}
	if days <= 0 {
		return // 0 或负值表示永久保留
	}
	if days > 30 {
		// 对话原文体积远大于普通日志，长保留期容易把库撑爆。
		slog.Warn("codex transcript retention is unusually long; watch table size",
			"retention_days", days)
	}
	cutoff := time.Now().UTC().AddDate(0, 0, -days)
	for {
		deleted, err := s.repo.DeleteBefore(ctx, cutoff, codexTranscriptRetentionBatchSize)
		if err != nil {
			slog.Warn("codex transcript retention cleanup failed", "error", err)
			return
		}
		if deleted == 0 {
			return
		}
	}
}
