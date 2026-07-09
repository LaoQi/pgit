package pgs

import (
	"fmt"
	"testing"
	"time"
)

func TestAppendAndReadSyncLog(t *testing.T) {
	dir := t.TempDir()

	entry1 := SyncLogEntry{
		Timestamp:    time.Now(),
		Duration:     100,
		Success:      true,
		ObjectsFetch: 5,
		Trigger:      "manual",
	}
	entry2 := SyncLogEntry{
		Timestamp: time.Now().Add(time.Second),
		Duration:  50,
		Success:   true,
		UpToDate:  true,
		Trigger:   "scheduled",
	}

	if err := AppendSyncLog(dir, entry1); err != nil {
		t.Fatal(err)
	}
	if err := AppendSyncLog(dir, entry2); err != nil {
		t.Fatal(err)
	}

	entries, err := ReadSyncLog(dir, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Fatalf("len = %d, want 2", len(entries))
	}
	if entries[0].Trigger != "scheduled" {
		t.Errorf("first entry trigger = %q, want scheduled", entries[0].Trigger)
	}
	if entries[1].Trigger != "manual" {
		t.Errorf("second entry trigger = %q, want manual", entries[1].Trigger)
	}
}

func TestReadSyncLog_Empty(t *testing.T) {
	dir := t.TempDir()
	entries, err := ReadSyncLog(dir, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Errorf("len = %d, want 0", len(entries))
	}
}

func TestReadSyncLog_Limit(t *testing.T) {
	dir := t.TempDir()
	for i := 0; i < 10; i++ {
		AppendSyncLog(dir, SyncLogEntry{
			Timestamp: time.Now().Add(time.Duration(i) * time.Second),
			Success:   true,
			Trigger:   fmt.Sprintf("t%d", i),
		})
	}
	entries, err := ReadSyncLog(dir, 3)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 3 {
		t.Fatalf("len = %d, want 3", len(entries))
	}
	if entries[0].Trigger != "t9" {
		t.Errorf("first = %q, want t9", entries[0].Trigger)
	}
	if entries[2].Trigger != "t7" {
		t.Errorf("third = %q, want t7", entries[2].Trigger)
	}
}
