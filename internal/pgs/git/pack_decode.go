package git

import (
	"bytes"
	"compress/zlib"
	"crypto/sha1"
	"encoding/binary"
	"fmt"
	"io"
)

// ObjectReader 按 oid 读取对象（LooseStore 实现），供 PackDecoder 解 REF_DELTA 时回查已有对象。
type ObjectReader interface {
	Read(oid Oid) (*RawObject, error)
}

// PackDecoder 解析 packfile（入向，含 OFS_DELTA/REF_DELTA 应用，push 用）。
// 对象按 pack 内出现顺序解析；OFS_DELTA 的 base 必须在 pack 内（按偏移引用），
// REF_DELTA 的 base 优先在 pack 内查找，fallback 到 Store（仓库已有对象）。
type PackDecoder struct {
	R        io.Reader
	Store    ObjectReader // 可选：REF_DELTA base 不在 pack 内时回查仓库已有对象
	objects  []*RawObject // 按解析顺序
	byOid    map[Oid]int  // oid -> objects 索引（供 REF_DELTA）
	byOffset map[int]int  // 对象 type 字节起始偏移 -> 索引（供 OFS_DELTA）
}

func NewPackDecoder(r io.Reader, store ...ObjectReader) *PackDecoder {
	d := &PackDecoder{R: r, byOid: map[Oid]int{}, byOffset: map[int]int{}}
	if len(store) > 0 {
		d.Store = store[0]
	}
	return d
}

// Decode 读整个 pack，验证 trailer SHA1，返回所有还原后的对象
func (d *PackDecoder) Decode() ([]*RawObject, error) {
	raw, err := io.ReadAll(d.R)
	if err != nil {
		return nil, err
	}
	if len(raw) < 32 {
		return nil, fmt.Errorf("pack: too short (%d bytes)", len(raw))
	}
	// trailer SHA1 覆盖 header + objects
	body := raw[:len(raw)-20]
	trailer := raw[len(raw)-20:]
	h := sha1.Sum(body)
	if !bytes.Equal(h[:], trailer) {
		return nil, fmt.Errorf("pack: trailer sha1 mismatch")
	}
	if string(body[:4]) != packMagic {
		return nil, fmt.Errorf("pack: bad magic %q", body[:4])
	}
	version := binary.BigEndian.Uint32(body[4:8])
	if version != packVersion {
		return nil, fmt.Errorf("pack: bad version %d", version)
	}
	count := binary.BigEndian.Uint32(body[8:12])
	pos := 12
	for i := uint32(0); i < count; i++ {
		start := pos
		obj, n, err := d.readObject(body, start)
		if err != nil {
			return nil, fmt.Errorf("pack: object %d at %d: %w", i, start, err)
		}
		pos += n
		d.objects = append(d.objects, obj)
		d.byOffset[start] = len(d.objects) - 1
		d.byOid[obj.Oid()] = len(d.objects) - 1
	}
	if pos != len(body) {
		return nil, fmt.Errorf("pack: trailing bytes pos=%d body=%d", pos, len(body))
	}
	return d.objects, nil
}

// readObject 从 body[start] 读一个对象，返回对象 + 消耗字节数
func (d *PackDecoder) readObject(body []byte, start int) (*RawObject, int, error) {
	pos := start
	b := body[pos]
	pos++
	pt := (b >> 4) & 0x07
	size := uint64(b & 0x0f)
	shift := uint(4)
	for b&0x80 != 0 {
		b = body[pos]
		pos++
		size |= uint64(b&0x7f) << shift
		shift += 7
	}
	switch pt {
	case packObjCommit, packObjTree, packObjBlob, packObjTag:
		content, n, err := readZlib(body, pos)
		if err != nil {
			return nil, 0, fmt.Errorf("zlib: %w", err)
		}
		if int(size) != len(content) {
			return nil, 0, fmt.Errorf("size mismatch: header %d actual %d", size, len(content))
		}
		objType, _ := packTypeToObj(pt)
		return NewRawObject(objType, content), (pos - start) + n, nil
	case packObjOfsDelta:
		// 读 offset varint（git ofs-delta 编码：big-endian base-128，每续位 +1 补偿）
		off, m := readOfsDelta(body, pos)
		pos += m
		delta, n, err := readZlib(body, pos)
		if err != nil {
			return nil, 0, fmt.Errorf("ofs-delta zlib: %w", err)
		}
		if int(size) != len(delta) {
			return nil, 0, fmt.Errorf("ofs-delta size mismatch: header %d actual %d", size, len(delta))
		}
		// base 的 type 字节偏移 = 当前对象 type 字节偏移 - off
		basePos := start - off
		baseIdx, ok := d.byOffset[basePos]
		if !ok {
			return nil, 0, fmt.Errorf("ofs-delta: base at byte %d not found (off=%d)", basePos, off)
		}
		base := d.objects[baseIdx]
		content, err := ApplyDelta(base.Content, delta)
		if err != nil {
			return nil, 0, fmt.Errorf("ofs-delta apply: %w", err)
		}
		return NewRawObject(base.Type, content), (pos - start) + n, nil
	case packObjRefDelta:
		if pos+20 > len(body) {
			return nil, 0, fmt.Errorf("ref-delta: short oid")
		}
		baseOid := Oid(fmt.Sprintf("%x", body[pos:pos+20]))
		pos += 20
		delta, n, err := readZlib(body, pos)
		if err != nil {
			return nil, 0, fmt.Errorf("ref-delta zlib: %w", err)
		}
		if int(size) != len(delta) {
			return nil, 0, fmt.Errorf("ref-delta size mismatch: header %d actual %d", size, len(delta))
		}
		var base *RawObject
		if baseIdx, ok := d.byOid[baseOid]; ok {
			base = d.objects[baseIdx]
		} else if d.Store != nil {
			var err2 error
			base, err2 = d.Store.Read(baseOid)
			if err2 != nil {
				return nil, 0, fmt.Errorf("ref-delta: base %s not found (pack nor store)", baseOid)
			}
		} else {
			return nil, 0, fmt.Errorf("ref-delta: base %s not found", baseOid)
		}
		content, err := ApplyDelta(base.Content, delta)
		if err != nil {
			return nil, 0, fmt.Errorf("ref-delta apply: %w", err)
		}
		return NewRawObject(base.Type, content), (pos - start) + n, nil
	default:
		return nil, 0, fmt.Errorf("unknown pack object type %d", pt)
	}
}

func packTypeToObj(pt byte) (ObjectType, error) {
	switch pt {
	case packObjCommit:
		return ObjCommit, nil
	case packObjTree:
		return ObjTree, nil
	case packObjBlob:
		return ObjBlob, nil
	case packObjTag:
		return ObjTag, nil
	}
	return "", fmt.Errorf("bad pack type %d", pt)
}

// readZlib 从 body[pos] 解 zlib，返回 content + 消耗字节数
func readZlib(body []byte, pos int) ([]byte, int, error) {
	br := bytes.NewReader(body[pos:])
	zr, err := zlib.NewReader(br)
	if err != nil {
		return nil, 0, err
	}
	var out bytes.Buffer
	if _, err := io.Copy(&out, zr); err != nil {
		zr.Close()
		return nil, 0, err
	}
	zr.Close()
	// 消耗 = 初始剩余 - 读后剩余
	consumed := (len(body) - pos) - br.Len()
	return out.Bytes(), consumed, nil
}

// readOfsDelta 读 git ofs-delta 偏移 varint。
// 编码（与 git get_delta_base 一致）：
//   off = c & 0x7f
//   while c & 0x80: off += 1; c = next; off = (off<<7) | (c&0x7f)
// 返回解码值与消耗字节数。该值是「当前对象 type 字节偏移 - base type 字节偏移」。
func readOfsDelta(b []byte, pos int) (int, int) {
	var c byte
	var off uint64
	n := 0
	c = b[pos]
	pos++
	n++
	off = uint64(c & 0x7f)
	for c&0x80 != 0 {
		off += 1
		c = b[pos]
		pos++
		n++
		off = (off << 7) | uint64(c&0x7f)
	}
	return int(off), n
}
