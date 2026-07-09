package pgs

import (
	"fmt"
	"log"
	"math/rand"
	"sync"
	"time"
)

type SyncManager struct {
	mu      sync.Mutex
	mirrors map[string]*mirrorScheduler
}

type mirrorScheduler struct {
	stop    chan struct{}
	syncMu  sync.Mutex
	syncing bool
}

var SyncMgr *SyncManager

func InitSyncManager() {
	SyncMgr = &SyncManager{
		mirrors: make(map[string]*mirrorScheduler),
	}
}

func (sm *SyncManager) Register(repo *Repository) {
	if repo.Mirror == nil || repo.Mirror.SyncInterval <= 0 {
		return
	}
	sm.mu.Lock()
	if _, ok := sm.mirrors[repo.Name]; ok {
		sm.mu.Unlock()
		return
	}
	s := &mirrorScheduler{stop: make(chan struct{})}
	sm.mirrors[repo.Name] = s
	sm.mu.Unlock()

	go func() {
		delay := time.Duration(1+rand.Intn(10)) * time.Second
		select {
		case <-time.After(delay):
		case <-s.stop:
			return
		}
		sm.doSync(repo.Name, "initial")
		ticker := time.NewTicker(time.Duration(repo.Mirror.SyncInterval) * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				sm.doSync(repo.Name, "scheduled")
			case <-s.stop:
				return
			}
		}
	}()
}

func (sm *SyncManager) Unregister(name string) {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	s, ok := sm.mirrors[name]
	if !ok {
		return
	}
	if s.stop != nil {
		close(s.stop)
	}
	delete(sm.mirrors, name)
}

func (sm *SyncManager) doSync(name string, trigger string) {
	sm.mu.Lock()
	s, ok := sm.mirrors[name]
	sm.mu.Unlock()
	if !ok {
		return
	}
	s.syncMu.Lock()
	if s.syncing {
		s.syncMu.Unlock()
		return
	}
	s.syncing = true
	s.syncMu.Unlock()
	defer func() {
		s.syncMu.Lock()
		s.syncing = false
		s.syncMu.Unlock()
	}()

	start := time.Now()
	result, err := ReposManager.SyncRepository(name)
	entry := SyncLogEntry{
		Timestamp: start,
		Duration:  time.Since(start).Milliseconds(),
		Success:   err == nil,
		Trigger:   trigger,
	}
	if err != nil {
		entry.Error = err.Error()
		log.Printf("sync %s [%s] failed: %v", name, trigger, err)
	} else {
		log.Printf("sync %s [%s] ok: duration=%dms", name, trigger, entry.Duration)
	}
	if result != nil {
		entry.ObjectsFetch = result.ObjectsWritten
		entry.RefsUpdated = result.RefsUpdated
		entry.RefsDeleted = result.RefsDeleted
		entry.UpToDate = result.UpToDate
		entry.Wants = result.Wants
		entry.Haves = result.Haves
		entry.PackSize = result.PackSize
	}
	repo, _ := ReposManager.GetRepository(name)
	if repo != nil {
		if logErr := AppendSyncLog(repo.Path(), entry); logErr != nil {
			log.Printf("append sync log for %s failed: %v", name, logErr)
		}
	}
}

func (sm *SyncManager) SyncNow(name string) (*SyncLogEntry, error) {
	repo, err := ReposManager.GetRepository(name)
	if err != nil {
		return nil, err
	}
	if !repo.IsMirror() {
		return nil, fmt.Errorf("repository %s is not a mirror", name)
	}

	sm.mu.Lock()
	s, ok := sm.mirrors[name]
	if !ok {
		s = &mirrorScheduler{}
		sm.mirrors[name] = s
	}
	sm.mu.Unlock()

	s.syncMu.Lock()
	if s.syncing {
		s.syncMu.Unlock()
		return nil, fmt.Errorf("sync already in progress for %s", name)
	}
	s.syncing = true
	s.syncMu.Unlock()
	defer func() {
		s.syncMu.Lock()
		s.syncing = false
		s.syncMu.Unlock()
	}()

	start := time.Now()
	result, err := ReposManager.SyncRepository(name)
	entry := &SyncLogEntry{
		Timestamp: start,
		Duration:  time.Since(start).Milliseconds(),
		Success:   err == nil,
		Trigger:   "manual",
	}
	if err != nil {
		entry.Error = err.Error()
		log.Printf("sync %s [manual] failed: %v", name, err)
	} else {
		log.Printf("sync %s [manual] ok: duration=%dms", name, entry.Duration)
	}
	if result != nil {
		entry.ObjectsFetch = result.ObjectsWritten
		entry.RefsUpdated = result.RefsUpdated
		entry.RefsDeleted = result.RefsDeleted
		entry.UpToDate = result.UpToDate
		entry.Wants = result.Wants
		entry.Haves = result.Haves
		entry.PackSize = result.PackSize
	}
	AppendSyncLog(repo.Path(), *entry)
	return entry, err
}

func (sm *SyncManager) Stop() {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	for _, s := range sm.mirrors {
		if s.stop != nil {
			close(s.stop)
		}
	}
	sm.mirrors = make(map[string]*mirrorScheduler)
}
