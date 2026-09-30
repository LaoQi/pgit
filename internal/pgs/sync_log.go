package pgs

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"time"
)

type SyncLogEntry struct {
	Timestamp    time.Time `json:"timestamp"`
	Duration     int64     `json:"duration"`
	Success      bool      `json:"success"`
	Error        string    `json:"error,omitempty"`
	ObjectsFetch int       `json:"objectsFetched"`
	RefsUpdated  int       `json:"refsUpdated"`
	RefsDeleted  int       `json:"refsDeleted"`
	UpToDate     bool      `json:"upToDate"`
	Trigger      string    `json:"trigger"`
	QueueWaitMs  int64     `json:"queueWaitMs"`
	Wants        int       `json:"wants"`
	Haves        int       `json:"haves"`
	PackSize     int64     `json:"packSize"`
}

// syncLogFile / relayLogFile 是仓库目录内的 JSONL 日志文件名。
const (
	syncLogFile  = "pgit-sync.jsonl"
	relayLogFile = "pgit-relay.jsonl"
)

// appendJSONL 以 JSON 行追加到 <repoPath>/<name>。
func appendJSONL(repoPath, name string, v any) error {
	path := filepath.Join(repoPath, name)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	return json.NewEncoder(f).Encode(v)
}

// readJSONL 读取 <repoPath>/<name> 的 JSON 行，最新在前，最多 limit 条（limit<=0 表示不限）。
// 文件不存在返回空切片；无法解析的行跳过（容忍半行写入）。
func readJSONL[T any](repoPath, name string, limit int) ([]T, error) {
	data, err := os.ReadFile(filepath.Join(repoPath, name))
	if err != nil {
		if os.IsNotExist(err) {
			return []T{}, nil
		}
		return nil, err
	}
	entries := make([]T, 0)
	for _, line := range bytes.Split(data, []byte("\n")) {
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var entry T
		if err := json.Unmarshal(line, &entry); err != nil {
			continue
		}
		entries = append(entries, entry)
	}
	for i, j := 0, len(entries)-1; i < j; i, j = i+1, j-1 {
		entries[i], entries[j] = entries[j], entries[i]
	}
	if limit > 0 && len(entries) > limit {
		entries = entries[:limit]
	}
	return entries, nil
}

func AppendSyncLog(repoPath string, entry SyncLogEntry) error {
	return appendJSONL(repoPath, syncLogFile, entry)
}

func ReadSyncLog(repoPath string, limit int) ([]SyncLogEntry, error) {
	return readJSONL[SyncLogEntry](repoPath, syncLogFile, limit)
}
