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
	Wants        int       `json:"wants"`
	Haves        int       `json:"haves"`
	PackSize     int64     `json:"packSize"`
}

func AppendSyncLog(repoPath string, entry SyncLogEntry) error {
	path := filepath.Join(repoPath, "pgit-sync.jsonl")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	return json.NewEncoder(f).Encode(entry)
}

func ReadSyncLog(repoPath string, limit int) ([]SyncLogEntry, error) {
	path := filepath.Join(repoPath, "pgit-sync.jsonl")
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return []SyncLogEntry{}, nil
		}
		return nil, err
	}
	entries := make([]SyncLogEntry, 0)
	for _, line := range bytes.Split(data, []byte("\n")) {
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var entry SyncLogEntry
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
