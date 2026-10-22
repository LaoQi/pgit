package pgs

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"pgit/internal/pgs/git"
)

// 归档原因（与 git.DestructiveRef.Reason 对应）。
const (
	ArchiveReasonDelete = "delete"
	ArchiveReasonFork   = "non-fast-forward"
	// ArchiveReasonManual 是手动触发（POST /api/v1/repos/archives）的快照。
	ArchiveReasonManual = "manual"
)

// ArchiveRef 是归档时记录的单条 ref 快照。
type ArchiveRef struct {
	Name   string  `json:"name"`
	OldOid git.Oid `json:"oldOid"`
	NewOid git.Oid `json:"newOid,omitempty"`
	Reason string  `json:"reason"`
}

// ArchiveRecord 是单个归档（bundle + sidecar）的元数据。
type ArchiveRecord struct {
	ID         string       `json:"id"`
	Repo       string       `json:"repo"`
	Time       time.Time    `json:"time"`
	Reason     string       `json:"reason"`
	Bundle     string       `json:"bundle"`
	SizeBytes  int64        `json:"sizeBytes"`
	Objects    int          `json:"objects"`
	DurationMs int64        `json:"durationMs"`
	Refs       []ArchiveRef `json:"refs"`
	Head       string       `json:"head,omitempty"`
}

// ArchiveManager 负责镜像/中转仓库的破坏性更新归档：先把当前仓库打成 git bundle
// （完整历史，可被 git 直接校验/克隆），再放行 ref 更新。
//
// 目录布局（<archiveDir> 不能以 .git 结尾，否则启动扫描会当成仓库）：
//
//	<archiveDir>/<repoName>/<unixMilli>-<reason>-<ref>.bundle
//	<archiveDir>/<repoName>/<unixMilli>-<reason>-<ref>.json     (sidecar)
type ArchiveManager struct {
	settings *Setting
}

// NewArchiveManager 创建归档管理器（settings 可为 nil，此时读默认配置）。
func NewArchiveManager(settings *Setting) *ArchiveManager {
	return &ArchiveManager{settings: settings}
}

// Enabled 报告归档功能是否开启。
func (am *ArchiveManager) Enabled() bool {
	if am == nil {
		return false
	}
	enabled, _, _, _ := am.settings.ArchiveConf()
	return enabled
}

// Snapshot 为一次破坏性更新生成归档包（repoName 用于归档子目录，repoRoot 是被归档仓库的根）。
// 返回归档记录。
//
// 流程：写 <id>.bundle.tmp → fsync → rename → 写 sidecar → prune。
// 任何一步失败都返回 error（调用方据此拒绝覆盖本地 ref）。
func (am *ArchiveManager) Snapshot(repoName, repoRoot string, ev git.DestructiveEvent) (*ArchiveRecord, error) {
	if am == nil || repoName == "" || repoRoot == "" {
		return nil, fmt.Errorf("archive: manager not initialized")
	}
	if !safeArchiveID(repoName) {
		return nil, fmt.Errorf("archive: invalid repository name %q", repoName)
	}
	if len(ev.Refs) == 0 {
		return nil, fmt.Errorf("archive: nothing to snapshot")
	}
	_, dir, keep, maxBytes := am.settings.ArchiveConf()
	start := time.Now()

	repoDir := filepath.Join(dir, repoName)
	if err := os.MkdirAll(repoDir, 0o750); err != nil {
		return nil, fmt.Errorf("archive: create dir: %w", err)
	}

	first := ev.Refs[0]
	// id 以毫秒时间戳开头，天然按时间排序；同毫秒重复触发（极端情况）追加序号避免撞名。
	id := fmt.Sprintf("%d-%s", start.UnixMilli(), archiveIDSlug(first.Reason, first.Name))
	for i := 1; ; i++ {
		if _, err := os.Stat(filepath.Join(repoDir, id+".bundle")); os.IsNotExist(err) {
			break
		}
		id = fmt.Sprintf("%d-%s-%d", start.UnixMilli(), archiveIDSlug(first.Reason, first.Name), i)
	}
	bundleName := id + ".bundle"
	bundlePath := filepath.Join(repoDir, bundleName)
	tmpPath := bundlePath + ".tmp"

	info, err := writeBundleFile(tmpPath, repoRoot)
	if err != nil {
		os.Remove(tmpPath)
		return nil, err
	}
	if err := os.Rename(tmpPath, bundlePath); err != nil {
		os.Remove(tmpPath)
		return nil, fmt.Errorf("archive: rename bundle: %w", err)
	}
	st, err := os.Stat(bundlePath)
	if err != nil {
		return nil, fmt.Errorf("archive: stat bundle: %w", err)
	}

	rec := &ArchiveRecord{
		ID:         id,
		Repo:       repoName,
		Time:       start,
		Reason:     first.Reason,
		Bundle:     bundleName,
		SizeBytes:  st.Size(),
		Objects:    info.Objects,
		DurationMs: time.Since(start).Milliseconds(),
		Head:       info.Head,
	}
	for _, r := range ev.Refs {
		rec.Refs = append(rec.Refs, ArchiveRef{Name: r.Name, OldOid: r.OldOid, NewOid: r.NewOid, Reason: r.Reason})
	}
	if err := writeArchiveSidecar(repoDir, rec); err != nil {
		return nil, err
	}
	if err := am.prune(repoName, keep, maxBytes); err != nil {
		// 清理失败不影响本次归档（已落盘），只记日志由调用方观察。
		return rec, nil
	}
	return rec, nil
}

// writeBundleFile 生成 bundle 到 path（供 Snapshot 复用；fsync 后关闭）。
func writeBundleFile(path, repoRoot string) (*git.BundleInfo, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o640)
	if err != nil {
		return nil, fmt.Errorf("archive: create bundle: %w", err)
	}
	info, err := git.WriteBundle(f, repoRoot)
	if err != nil {
		f.Close()
		return nil, err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return nil, fmt.Errorf("archive: fsync bundle: %w", err)
	}
	if err := f.Close(); err != nil {
		return nil, fmt.Errorf("archive: close bundle: %w", err)
	}
	return info, nil
}

// writeArchiveSidecar 原子写 sidecar JSON（tmp + rename）。
func writeArchiveSidecar(repoDir string, rec *ArchiveRecord) error {
	data, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		return fmt.Errorf("archive: marshal sidecar: %w", err)
	}
	path := filepath.Join(repoDir, rec.ID+".json")
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(data, '\n'), 0o640); err != nil {
		return fmt.Errorf("archive: write sidecar: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("archive: rename sidecar: %w", err)
	}
	return nil
}

// SnapshotManual 为仓库当前的完整 refs 手动生成快照（不受 mirrorArchive 开关限制，
// 供 agent/运维在危险操作前主动备份）。空仓库（无 refs）返回错误。
func (am *ArchiveManager) SnapshotManual(repoName, repoRoot string) (*ArchiveRecord, error) {
	if am == nil || repoName == "" || repoRoot == "" {
		return nil, fmt.Errorf("archive: manager not initialized")
	}
	if !safeArchiveID(repoName) {
		return nil, fmt.Errorf("archive: invalid repository name %q", repoName)
	}
	refs, err := git.NewRefStore(repoRoot).List()
	if err != nil {
		return nil, fmt.Errorf("archive: list refs: %w", err)
	}
	ev := git.DestructiveEvent{RepoRoot: repoRoot}
	for _, r := range refs {
		if r.Name == "HEAD" || !r.Oid.Valid() || r.Oid.IsZero() {
			continue
		}
		ev.Refs = append(ev.Refs, git.DestructiveRef{Name: r.Name, OldOid: r.Oid, Reason: ArchiveReasonManual})
	}
	if len(ev.Refs) == 0 {
		return nil, fmt.Errorf("archive: repository has no refs to snapshot")
	}
	sort.Slice(ev.Refs, func(i, j int) bool { return ev.Refs[i].Name < ev.Refs[j].Name })
	return am.Snapshot(repoName, repoRoot, ev)
}

// Snapshots 返回某仓库的归档列表（最新在前）。
func (am *ArchiveManager) Snapshots(repoName string) ([]ArchiveRecord, error) {
	if am == nil {
		return nil, nil
	}
	_, dir, _, _ := am.settings.ArchiveConf()
	repoDir := filepath.Join(dir, repoName)
	entries, err := os.ReadDir(repoDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	records := make([]ArchiveRecord, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(repoDir, e.Name()))
		if err != nil {
			continue
		}
		var rec ArchiveRecord
		if err := json.Unmarshal(data, &rec); err != nil {
			continue
		}
		records = append(records, rec)
	}
	sort.Slice(records, func(i, j int) bool {
		if records[i].Time.Equal(records[j].Time) {
			return records[i].ID > records[j].ID
		}
		return records[i].Time.After(records[j].Time)
	})
	return records, nil
}

// Find 按 id 查找归档（id 只允许文件名安全字符，防路径穿越）。
func (am *ArchiveManager) Find(repoName, id string) (*ArchiveRecord, string, error) {
	if !safeArchiveID(id) {
		return nil, "", fmt.Errorf("archive: invalid archive id %q", id)
	}
	_, dir, _, _ := am.settings.ArchiveConf()
	repoDir := filepath.Join(dir, repoName)
	data, err := os.ReadFile(filepath.Join(repoDir, id+".json"))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, "", ErrArchiveNotFound
		}
		return nil, "", err
	}
	var rec ArchiveRecord
	if err := json.Unmarshal(data, &rec); err != nil {
		return nil, "", fmt.Errorf("archive: corrupt sidecar %s: %w", id, err)
	}
	bundlePath := filepath.Join(repoDir, id+".bundle")
	if _, err := os.Stat(bundlePath); err != nil {
		return nil, "", fmt.Errorf("archive: bundle missing for %s: %w", id, err)
	}
	return &rec, bundlePath, nil
}

// Delete 删除单个归档（sidecar + bundle）。id 不合法返回错误，两者都不存在返回 ErrArchiveNotFound。
func (am *ArchiveManager) Delete(repoName, id string) error {
	if am == nil {
		return fmt.Errorf("archive: manager not initialized")
	}
	if !safeArchiveID(id) || !safeArchiveID(repoName) {
		return fmt.Errorf("archive: invalid archive id %q", id)
	}
	_, dir, _, _ := am.settings.ArchiveConf()
	repoDir := filepath.Join(dir, repoName)
	sidecar := filepath.Join(repoDir, id+".json")
	bundle := filepath.Join(repoDir, id+".bundle")
	_, errSidecar := os.Stat(sidecar)
	_, errBundle := os.Stat(bundle)
	if os.IsNotExist(errSidecar) && os.IsNotExist(errBundle) {
		return ErrArchiveNotFound
	}
	// 先删 sidecar：列表不再暴露该条；即使随后删 bundle 失败也不会出现"有记录无文件"。
	os.Remove(sidecar)
	if err := os.Remove(bundle); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("archive: remove bundle: %w", err)
	}
	return nil
}

// Restore 把归档恢复到 destRoot（通常是仓库现有目录）：对象解包合并（幂等），
// 归档记录的 ref 覆盖写回。返回恢复统计。
func (am *ArchiveManager) Restore(destRoot, repoName, id string) (*git.RestoreResult, *ArchiveRecord, error) {
	rec, bundlePath, err := am.Find(repoName, id)
	if err != nil {
		return nil, nil, err
	}
	res, err := git.RestoreBundle(bundlePath, destRoot, rec.Head)
	if err != nil {
		return nil, rec, err
	}
	return res, rec, nil
}

// prune 按「每仓库保留份数」与「全局容量上限」淘汰最旧的归档。
// keep < 0 或 maxBytes < 0 表示不限。
func (am *ArchiveManager) prune(repoName string, keep int, maxBytes int64) error {
	_, dir, _, _ := am.settings.ArchiveConf()
	if keep >= 0 {
		repoDir := filepath.Join(dir, repoName)
		ids, err := archiveIDs(repoDir)
		if err == nil && len(ids) > keep {
			for _, id := range ids[:len(ids)-keep] {
				removeArchive(repoDir, id)
			}
		}
	}
	if maxBytes < 0 {
		return nil
	}
	type item struct {
		repoDir string
		id      string
		size    int64
		mod     time.Time
	}
	var items []item
	var total int64
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		repoDir := filepath.Join(dir, e.Name())
		bundles, _ := os.ReadDir(repoDir)
		for _, b := range bundles {
			if b.IsDir() || !strings.HasSuffix(b.Name(), ".bundle") {
				continue
			}
			st, err := b.Info()
			if err != nil {
				continue
			}
			total += st.Size()
			items = append(items, item{
				repoDir: repoDir,
				id:      strings.TrimSuffix(b.Name(), ".bundle"),
				size:    st.Size(),
				mod:     st.ModTime(),
			})
		}
	}
	if total <= maxBytes {
		return nil
	}
	sort.Slice(items, func(i, j int) bool { return items[i].mod.Before(items[j].mod) })
	for _, it := range items {
		if total <= maxBytes {
			break
		}
		removeArchive(it.repoDir, it.id)
		total -= it.size
	}
	return nil
}

// archiveIDs 返回仓库归档 id（按文件名时间戳升序，最旧在前）。
func archiveIDs(repoDir string) ([]string, error) {
	entries, err := os.ReadDir(repoDir)
	if err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		ids = append(ids, strings.TrimSuffix(e.Name(), ".json"))
	}
	sort.Strings(ids)
	return ids, nil
}

// removeArchive 删除一组归档文件（sidecar 先删，避免列表出现无 bundle 的记录）。
func removeArchive(repoDir, id string) {
	os.Remove(filepath.Join(repoDir, id+".json"))
	os.Remove(filepath.Join(repoDir, id+".bundle"))
}

// archiveIDSlug 生成文件名安全的后缀：<reason>-<ref>。
func archiveIDSlug(reason, ref string) string {
	ref = strings.TrimPrefix(ref, "refs/heads/")
	ref = strings.TrimPrefix(ref, "refs/tags/")
	var b strings.Builder
	for _, r := range ref {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '.', r == '_', r == '-':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	slug := b.String()
	if len(slug) > 40 {
		slug = slug[:40]
	}
	if slug == "" {
		slug = "ref"
	}
	return reason + "-" + slug
}

// RestoreBundleToRepo 从 bundle 文件恢复仓库内容（CLI 入口；与 HTTP 恢复接口同一条路径）。
func RestoreBundleToRepo(repo *Repository, bundlePath string) (*git.RestoreResult, error) {
	if repo == nil {
		return nil, fmt.Errorf("archive: repository is nil")
	}
	return git.RestoreBundle(bundlePath, repo.Path(), "")
}

// destructiveNames 提取破坏性更新的 ref 名（供 sync log 记录）。
func destructiveNames(refs []git.DestructiveRef) []string {
	names := make([]string, 0, len(refs))
	for _, r := range refs {
		names = append(names, r.Name)
	}
	return names
}

// safeArchiveID 只允许服务端生成的 id 形状，拒绝路径分隔符与 ..
func safeArchiveID(id string) bool {
	if id == "" || len(id) > 200 {
		return false
	}
	for _, r := range id {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '.', r == '_', r == '-':
		default:
			return false
		}
	}
	return !strings.Contains(id, "..")
}
