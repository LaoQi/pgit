package git

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Ident 标识人信息
type Ident struct {
	Name      string
	Email     string
	Timestamp int64
	Offset    int // 时区分钟数
}

// Commit 解析后的 commit 视图
type Commit struct {
	Tree      Oid
	Parents   []Oid
	Author    Ident
	Committer Ident
	Message   string
	Extra     []byte // gpgsig 等未识别头，原样保留
}

// TreeEntry tree 的单个条目
type TreeEntry struct {
	Mode uint32
	Name string
	Oid  Oid
}

// Tree 解析后的 tree 视图
type Tree struct{ Entries []TreeEntry }

// Tag 解析后的 annotated tag 视图
type Tag struct {
	Object  Oid
	ObjType ObjectType
	TagName string
	Tagger  Ident
	Message string
}

// ParseCommit 解析 commit 对象内容
func ParseCommit(content []byte) (*Commit, error) {
	c := &Commit{}
	// 分离 header 与 message（空行分隔）
	idx := indexByteSlice(content, []byte("\n\n"))
	var header, msg []byte
	if idx < 0 {
		header = content
	} else {
		header = content[:idx]
		msg = content[idx+2:]
	}
	c.Message = string(msg)
	lines := strings.Split(string(header), "\n")
	for _, line := range lines {
		switch {
		case strings.HasPrefix(line, "tree "):
			c.Tree = Oid(line[5:])
		case strings.HasPrefix(line, "parent "):
			c.Parents = append(c.Parents, Oid(line[7:]))
		case strings.HasPrefix(line, "author "):
			c.Author = parseIdent(line[7:])
		case strings.HasPrefix(line, "committer "):
			c.Committer = parseIdent(line[10:])
		default:
			// 其他头（gpgsig 等多行头这里简化为单行收集）
			c.Extra = append(c.Extra, []byte(line+"\n")...)
		}
	}
	if c.Tree == "" {
		return nil, fmt.Errorf("commit: missing tree")
	}
	return c, nil
}

// parseIdent 解析 "Name <email> timestamp tz"
func parseIdent(s string) Ident {
	var id Ident
	gt := strings.IndexByte(s, '<')
	if gt < 0 {
		return id
	}
	id.Name = strings.TrimSpace(s[:gt])
	gtc := strings.IndexByte(s, '>')
	if gtc < 0 {
		return id
	}
	id.Email = s[gt+1 : gtc]
	rest := strings.TrimSpace(s[gtc+1:])
	parts := strings.Fields(rest)
	if len(parts) >= 1 {
		ts, _ := strconv.ParseInt(parts[0], 10, 64)
		id.Timestamp = ts
	}
	if len(parts) >= 2 {
		// tz 形如 +0800，转分钟
		tz := parts[1]
		if len(tz) == 5 && (tz[0] == '+' || tz[0] == '-') {
			h, _ := strconv.Atoi(tz[1:3])
			m, _ := strconv.Atoi(tz[3:5])
			off := h*60 + m
			if tz[0] == '-' {
				off = -off
			}
			id.Offset = off
		}
	}
	return id
}

// indexByteSlice 查找子切片，返回起始下标，未找到返回 -1
func indexByteSlice(haystack, needle []byte) int {
	if len(needle) == 0 {
		return 0
	}
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if string(haystack[i:i+len(needle)]) == string(needle) {
			return i
		}
	}
	return -1
}

// ParseTree 解析 tree 对象内容
func ParseTree(content []byte) (*Tree, error) {
	t := &Tree{}
	i := 0
	for i < len(content) {
		sp := indexByteByte(content[i:], ' ')
		if sp < 0 {
			return nil, fmt.Errorf("tree: bad entry at %d", i)
		}
		modeStr := string(content[i : i+sp])
		mode, err := strconv.ParseUint(modeStr, 8, 32)
		if err != nil {
			return nil, fmt.Errorf("tree: bad mode %q: %w", modeStr, err)
		}
		i += sp + 1
		nul := indexByteByte(content[i:], 0)
		if nul < 0 {
			return nil, fmt.Errorf("tree: no name terminator at %d", i)
		}
		name := string(content[i : i+nul])
		i += nul + 1
		if i+20 > len(content) {
			return nil, fmt.Errorf("tree: short oid at %d", i)
		}
		oid := Oid(fmt.Sprintf("%x", content[i:i+20]))
		i += 20
		t.Entries = append(t.Entries, TreeEntry{Mode: uint32(mode), Name: name, Oid: oid})
	}
	return t, nil
}

func indexByteByte(haystack []byte, b byte) int {
	for i, c := range haystack {
		if c == b {
			return i
		}
	}
	return -1
}

// ParseTag 解析 annotated tag 对象
func ParseTag(content []byte) (*Tag, error) {
	t := &Tag{}
	idx := indexByteSlice(content, []byte("\n\n"))
	var header, msg []byte
	if idx < 0 {
		header = content
	} else {
		header = content[:idx]
		msg = content[idx+2:]
	}
	t.Message = string(msg)
	for _, line := range strings.Split(string(header), "\n") {
		switch {
		case strings.HasPrefix(line, "object "):
			t.Object = Oid(line[7:])
		case strings.HasPrefix(line, "type "):
			t.ObjType = ObjectType(line[5:])
		case strings.HasPrefix(line, "tag "):
			t.TagName = line[4:]
		case strings.HasPrefix(line, "tagger "):
			t.Tagger = parseIdent(line[7:])
		}
	}
	if t.Object == "" {
		return nil, fmt.Errorf("tag: missing object")
	}
	return t, nil
}

// Time 返回 Ident 对应的 UTC 时间戳（忽略 offset，仅用于展示）
func (id Ident) Time() time.Time {
	return time.Unix(id.Timestamp, 0)
}
