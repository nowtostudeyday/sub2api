package handler

import (
	"strconv"
	"strings"
	"sync"
)

// codexInstructionsCacheCapacity 去重缓存容量。
//
// 超出后整体清空而非逐条淘汰：这只是"是否已存过全文"的提示，丢失只会导致多存
// 一份全文（正确性不受影响），换来的是免掉 LRU 链表的实现与锁竞争。
const codexInstructionsCacheCapacity = 4096

// codexInstructionsDedup 记录每个会话最近一次见到的 instructions hash。
//
// 目的：Codex 的 instructions 是 10-20KB 的工具定义且每轮回传，全量入库会让它
// 占据绝大部分存储。首见时存全文，之后只留 hash 用于比对是否漂移。
//
// 进程内缓存，不查库：开关判定在热路径上，多一次 DB 往返不可接受。
// 多实例部署时每个实例各存一份全文，属 5 天窗口内的常量级冗余，可接受。
type codexInstructionsDedup struct {
	mu    sync.Mutex
	seen  map[string]string
	limit int
}

var codexInstructionsSeen = &codexInstructionsDedup{
	seen:  make(map[string]string, codexInstructionsCacheCapacity),
	limit: codexInstructionsCacheCapacity,
}

// shouldStoreFull 判断本轮是否需要存 instructions 全文。
//
// sessionID 为空时退回以 hash 自身为键，语义变为"同一份 instructions 只存一次全文"。
func (d *codexInstructionsDedup) shouldStoreFull(apiKeyID int64, sessionID, hash string) bool {
	if hash == "" {
		return false
	}
	sessionID = strings.TrimSpace(sessionID)
	key := strconv.FormatInt(apiKeyID, 10) + "|"
	if sessionID != "" {
		key += sessionID
	} else {
		key += hash
	}

	d.mu.Lock()
	defer d.mu.Unlock()
	if previous, exists := d.seen[key]; exists && previous == hash {
		return false
	}
	if len(d.seen) >= d.limit {
		d.seen = make(map[string]string, d.limit)
	}
	d.seen[key] = hash
	return true
}
