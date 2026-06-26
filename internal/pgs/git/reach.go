package git

import (
	"fmt"
)

// gitlinkMode 是 tree entry 中 gitlink（submodule）的 mode，指向不在本仓库的 commit。
const gitlinkMode uint32 = 0o160000

// CollectReachable 从 rootOids 出发，BFS 收集所有可达对象。
// haveOids 指定客户端已有对象：从 have 出发可达的对象将被排除，仅返回增量。
// haveOids 为 nil 时退化为全量收集（兼容旧调用）。
// 返回去重后的对象列表（按 BFS 访问顺序）。gitlink 不入队。
// ZeroOid 跳过；store.Read 失败返回错误（可能损坏仓库）。
func CollectReachable(store *LooseStore, rootOids []Oid, haveOids ...Oid) ([]*RawObject, error) {
	exclude := make(map[Oid]bool)
	if len(haveOids) > 0 {
		exclQueue := make([]Oid, 0, len(haveOids))
		for _, oid := range haveOids {
			if !oid.IsZero() {
				exclQueue = append(exclQueue, oid)
			}
		}
		for len(exclQueue) > 0 {
			oid := exclQueue[0]
			exclQueue = exclQueue[1:]
			if exclude[oid] {
				continue
			}
			if !store.Exists(oid) {
				continue
			}
			exclude[oid] = true
			obj, err := store.Read(oid)
			if err != nil {
				continue
			}
			switch obj.Type {
			case ObjCommit:
				c, err := ParseCommit(obj.Content)
				if err != nil {
					continue
				}
				if !c.Tree.IsZero() {
					exclQueue = append(exclQueue, c.Tree)
				}
				for _, p := range c.Parents {
					if !p.IsZero() {
						exclQueue = append(exclQueue, p)
					}
				}
			case ObjTree:
				tr, err := ParseTree(obj.Content)
				if err != nil {
					continue
				}
				for _, e := range tr.Entries {
					if e.Mode != gitlinkMode && !e.Oid.IsZero() {
						exclQueue = append(exclQueue, e.Oid)
					}
				}
			case ObjTag:
				tg, err := ParseTag(obj.Content)
				if err != nil {
					continue
				}
				if !tg.Object.IsZero() {
					exclQueue = append(exclQueue, tg.Object)
				}
			}
		}
	}

	visited := make(map[Oid]bool)
	queue := make([]Oid, 0, len(rootOids))
	for _, oid := range rootOids {
		queue = append(queue, oid)
	}

	var result []*RawObject

	for len(queue) > 0 {
		oid := queue[0]
		queue = queue[1:]

		if oid.IsZero() {
			continue
		}
		if visited[oid] || exclude[oid] {
			continue
		}

		obj, err := store.Read(oid)
		if err != nil {
			return nil, fmt.Errorf("reach: read %s: %w", oid, err)
		}

		visited[oid] = true
		result = append(result, obj)

		switch obj.Type {
		case ObjCommit:
			c, err := ParseCommit(obj.Content)
			if err != nil {
				return nil, fmt.Errorf("reach: parse commit %s: %w", oid, err)
			}
			if !c.Tree.IsZero() {
				queue = append(queue, c.Tree)
			}
			for _, p := range c.Parents {
				if !p.IsZero() {
					queue = append(queue, p)
				}
			}
		case ObjTree:
			tr, err := ParseTree(obj.Content)
			if err != nil {
				return nil, fmt.Errorf("reach: parse tree %s: %w", oid, err)
			}
			for _, e := range tr.Entries {
				if e.Mode == gitlinkMode {
					continue
				}
				if !e.Oid.IsZero() {
					queue = append(queue, e.Oid)
				}
			}
		case ObjTag:
			tg, err := ParseTag(obj.Content)
			if err != nil {
				return nil, fmt.Errorf("reach: parse tag %s: %w", oid, err)
			}
			if !tg.Object.IsZero() {
				queue = append(queue, tg.Object)
			}
		case ObjBlob:
		}
	}

	return result, nil
}

// CollectReachableRefs 从多个 ref oid 出发收集（便利函数，等价于 CollectReachable）。
func CollectReachableRefs(store *LooseStore, refOids []Oid) ([]*RawObject, error) {
	return CollectReachable(store, refOids)
}
