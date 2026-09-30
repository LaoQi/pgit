package pgs

import (
	"sync"
	"time"

	"pgit/internal/pgs/git"
)

// relayBase 是「上游基线」的内存视图：最近一次校准（ls-remote）得到的上游 refs。
//
// 中转仓库的准入判定完全基于它：只有当客户端推送的 old-oid 等于基线 oid、
// 且 new-oid 是其后代（或上游本无该 ref 的新建）时才放行本地更新并转发上游。
// 这保证转发永远是一次快进推送，不会与上游冲突。
//
// 校准时机：每次 push 准入前实时校准（权威、可立刻给出 stale base 提示）、
// 启动时校准一次；转发成功后按 ref 推进，避免为了看自己的提交再问一次上游。
type relayBase struct {
	mu   sync.Mutex
	refs map[string]git.Oid
	at   time.Time
}

func newRelayBase() *relayBase {
	return &relayBase{refs: make(map[string]git.Oid)}
}

// Set 整体替换基线（校准后调用）。
func (b *relayBase) Set(refs map[string]git.Oid) {
	cp := make(map[string]git.Oid, len(refs))
	for name, oid := range refs {
		cp[name] = oid
	}
	b.mu.Lock()
	b.refs = cp
	b.at = time.Now()
	b.mu.Unlock()
}

// Advance 把单个 ref 推进到 oid（转发成功后本地与上游一致）。
func (b *relayBase) Advance(ref string, oid git.Oid) {
	b.mu.Lock()
	if b.refs == nil {
		b.refs = make(map[string]git.Oid)
	}
	b.refs[ref] = oid
	b.at = time.Now()
	b.mu.Unlock()
}

// Remove 从基线中删除 ref（删除转发成功后）。
func (b *relayBase) Remove(ref string) {
	b.mu.Lock()
	delete(b.refs, ref)
	b.at = time.Now()
	b.mu.Unlock()
}

// Oid 返回基线中 ref 的 oid；ok=false 表示上游没有该 ref。
func (b *relayBase) Oid(ref string) (git.Oid, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	oid, ok := b.refs[ref]
	return oid, ok
}

// Calibrated 返回是否已校准过及其时间。
func (b *relayBase) Calibrated() (time.Time, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.at, !b.at.IsZero()
}

// Snapshot 返回基线副本与校准时间。
func (b *relayBase) Snapshot() (map[string]git.Oid, time.Time) {
	b.mu.Lock()
	defer b.mu.Unlock()
	cp := make(map[string]git.Oid, len(b.refs))
	for name, oid := range b.refs {
		cp[name] = oid
	}
	return cp, b.at
}

// Len 返回基线中的 ref 数。
func (b *relayBase) Len() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.refs)
}
