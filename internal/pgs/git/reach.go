package git

import (
	"fmt"
)

// gitlinkMode 是 tree entry 中 gitlink（submodule）的 mode，指向不在本仓库的 commit。
const gitlinkMode uint32 = 0o160000

// CollectReachable 从 rootOids 出发，BFS 收集所有可达对象（内容全量驻留）。
// 仅适用于小仓库与测试；clone 编码请用 WalkReachable（只读头部）+ encodePack（单遍）。
// haveOids 指定客户端已有对象：从 have 出发可达的对象将被排除，仅返回增量。
// haveOids 为 nil 时退化为全量收集（兼容旧调用）。
// 返回去重后的对象列表（按 BFS 访问顺序）。gitlink 不入队。
// ZeroOid 跳过；store.Read 失败返回错误（可能损坏仓库）。
func CollectReachable(store ObjectStore, rootOids []Oid, haveOids ...Oid) ([]*RawObject, error) {
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
func CollectReachableRefs(store ObjectStore, refOids []Oid) ([]*RawObject, error) {
	return CollectReachable(store, refOids)
}

// ObjectMeta 是对象的轻量元信息（不含内容），供「先规划、后编码」两遍式遍历使用。
type ObjectMeta struct {
	Oid  Oid
	Type ObjectType
	Size int
}

// WalkReachable 从 rootOids 出发 BFS 遍历可达对象，返回对象元信息（不含内容）。
// 与 CollectReachable 语义一致（haveOids 排除集、跳过 gitlink、ZeroOid），
// 但只调用 store.Stat 读取头部，因此内存占用与仓库体积无关。
// visit 若非 nil，则每个对象在发现时回调一次；返回非 nil 错误则中止遍历。
// 返回值 order 为发现顺序的元信息切片（供需要总量的调用方使用）。
func WalkReachable(store ObjectStore, rootOids []Oid, haveOids []Oid, visit func(ObjectMeta) error) ([]ObjectMeta, error) {
	exclude := make(map[Oid]bool)
	if len(haveOids) > 0 {
		queue := make([]Oid, 0, len(haveOids))
		for _, oid := range haveOids {
			if !oid.IsZero() {
				queue = append(queue, oid)
			}
		}
		for len(queue) > 0 {
			oid := queue[0]
			queue = queue[1:]
			if exclude[oid] || !store.Exists(oid) {
				continue
			}
			exclude[oid] = true
			obj, err := store.Read(oid)
			if err != nil {
				continue
			}
			queue = append(queue, refsOf(obj)...)
		}
	}

	visited := make(map[Oid]bool)
	pending := append([]Oid(nil), rootOids...)
	var metas []ObjectMeta

	for len(pending) > 0 {
		oid := pending[0]
		pending = pending[1:]
		if oid.IsZero() || visited[oid] || exclude[oid] {
			continue
		}
		objType, size, err := store.Stat(oid)
		if err != nil {
			return nil, fmt.Errorf("reach: stat %s: %w", oid, err)
		}
		visited[oid] = true
		meta := ObjectMeta{Oid: oid, Type: objType, Size: size}
		metas = append(metas, meta)

		// 需要展开引用时读一次内容（提交/树/标签），blob 直接跳过
		if objType == ObjBlob {
			if visit != nil {
				if err := visit(meta); err != nil {
					return nil, err
				}
			}
			continue
		}
		obj, err := store.Read(oid)
		if err != nil {
			return nil, fmt.Errorf("reach: read %s: %w", oid, err)
		}
		pending = append(pending, refsOf(obj)...)
		if visit != nil {
			if err := visit(meta); err != nil {
				return nil, err
			}
		}
	}
	return metas, nil
}

// refsOf 返回对象引用的子对象 oid（commit 的 tree/parents、tree 的 entries、tag 的 object）。
// 解析失败或无引用时返回 nil（遍历不因此中止）。
func refsOf(obj *RawObject) []Oid {
	var out []Oid
	switch obj.Type {
	case ObjCommit:
		c, err := ParseCommit(obj.Content)
		if err != nil {
			return nil
		}
		if !c.Tree.IsZero() {
			out = append(out, c.Tree)
		}
		for _, p := range c.Parents {
			if !p.IsZero() {
				out = append(out, p)
			}
		}
	case ObjTree:
		tr, err := ParseTree(obj.Content)
		if err != nil {
			return nil
		}
		for _, e := range tr.Entries {
			if e.Mode != gitlinkMode && !e.Oid.IsZero() {
				out = append(out, e.Oid)
			}
		}
	case ObjTag:
		tg, err := ParseTag(obj.Content)
		if err != nil {
			return nil
		}
		if !tg.Object.IsZero() {
			out = append(out, tg.Object)
		}
	}
	return out
}
