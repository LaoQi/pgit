package pgs

import "time"

// RelayLogEntry 是一次「转发到上游」的结果，追加写入 <repo>/pgit-relay.jsonl。
type RelayLogEntry struct {
	Timestamp    time.Time `json:"timestamp"`
	Duration     int64     `json:"duration"`    // 本次转发耗时（毫秒）
	QueueWaitMs  int64     `json:"queueWaitMs"` // 在队列中的等待（毫秒）
	Trigger      string    `json:"trigger"`     // push | manual | retry | startup
	Success      bool      `json:"success"`
	UpToDate     bool      `json:"upToDate"` // 无需变更（未发起推送）
	Objects      int       `json:"objects"`  // 发送对象数
	PackSize     int64     `json:"packSize"` // pack 字节数
	RefsPushed   int       `json:"refsPushed"`
	RefsDeleted  int       `json:"refsDeleted"`
	RefsRejected int       `json:"refsRejected"`
	Error        string    `json:"error,omitempty"`
	Refs         []string  `json:"refs,omitempty"` // 本轮涉及的 ref 名
}

// AppendRelayLog 追加一条转发日志。
func AppendRelayLog(repoPath string, entry RelayLogEntry) error {
	return appendJSONL(repoPath, relayLogFile, entry)
}

// ReadRelayLog 读取转发日志（最新在前，最多 limit 条）。
func ReadRelayLog(repoPath string, limit int) ([]RelayLogEntry, error) {
	return readJSONL[RelayLogEntry](repoPath, relayLogFile, limit)
}
