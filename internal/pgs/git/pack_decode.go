package git

import (
	"bufio"
	"bytes"
	"compress/zlib"
	"crypto/sha1"
	"encoding/binary"
	"fmt"
	"hash"
	"io"
	"math"
)

// PackDecoder 流式解析 packfile（入向，含 OFS_DELTA/REF_DELTA 应用，push/fetch 用）。
//
// 与旧实现（先 io.ReadAll 再全量驻留解析）不同，本实现逐对象顺序解析：
//   - 每个对象解析完成后立即交给调用方（DecodeTo 写盘 / Decode 收集），
//     内容不再被解码器持有，内存峰值 ≈ 单个最大对象 + delta 链上的 base；
//   - OFS_DELTA 的 base 按 pack 内字节偏移定位，映射为 oid 后经 Store 回读
//     （base 可能已落盘并被 GC 释放）；
//   - trailer SHA1 在流式读取过程中累积校验，不缓存整个 pack。
//
// 字节计数依赖「zlib 精确消费」：stream 实现 io.ByteReader，flate 见 ByteReader
// 便逐字节读取（不再自行包 bufio 预读），因此 pos 精确等于逻辑消耗字节数。
type PackDecoder struct {
	br     *bufio.Reader
	stream *packStreamReader
	sha    hash.Hash

	// Store 用于 REF_DELTA/OFS_DELTA 的 base 回查，以及 DecodeTo 的落盘目标。
	Store ObjectStore

	offsets map[int64]Oid // 对象 type 字节起始偏移 -> oid（OFS_DELTA base 定位）

	collect   bool               // Decode() 模式：收集对象
	collected map[Oid]*RawObject // 收集到的对象（同时作为 base 回查来源）
	order     []Oid              // 收集顺序
	objects   int                // 已解析对象数
}

func NewPackDecoder(r io.Reader, store ...ObjectStore) *PackDecoder {
	br, ok := r.(*bufio.Reader)
	if !ok {
		br = bufio.NewReader(r)
	}
	d := &PackDecoder{
		br:      br,
		sha:     sha1.New(),
		offsets: map[int64]Oid{},
	}
	d.stream = &packStreamReader{br: br, sha: d.sha}
	if len(store) > 0 {
		d.Store = store[0]
	}
	return d
}

// Decode 流式解析整个 pack，返回全部还原后的对象（内存收集，供测试与小规模调用）。
// 大仓库/不可信输入请优先使用 DecodeTo 以避免全量驻留。
func (d *PackDecoder) Decode() ([]*RawObject, error) {
	d.collect = true
	d.collected = map[Oid]*RawObject{}
	if _, err := d.decodeStream(); err != nil {
		return nil, err
	}
	out := make([]*RawObject, 0, len(d.order))
	for _, oid := range d.order {
		out = append(out, d.collected[oid])
	}
	return out, nil
}

// DecodeTo 流式解析 pack 并把每个对象写入 store，返回写入对象数。
// 对象内容在写入后即被释放，内存占用与 pack 大小无关。
func (d *PackDecoder) DecodeTo(store ObjectStore) (int, error) {
	d.Store = store
	return d.decodeStream()
}

// Count 返回已解析（写入）的对象数。
func (d *PackDecoder) Count() int { return d.objects }

func (d *PackDecoder) decodeStream() (int, error) {
	// header: PACK + version + count（计入 body SHA1）
	var hdr [12]byte
	if _, err := io.ReadFull(d.stream, hdr[:]); err != nil {
		return 0, fmt.Errorf("pack: read header: %w", err)
	}
	if string(hdr[:4]) != packMagic {
		return 0, fmt.Errorf("pack: bad magic %q", hdr[:4])
	}
	if version := binary.BigEndian.Uint32(hdr[4:8]); version != packVersion {
		return 0, fmt.Errorf("pack: bad version %d", version)
	}
	count := binary.BigEndian.Uint32(hdr[8:12])

	for i := uint32(0); i < count; i++ {
		start := d.stream.pos
		obj, oid, err := d.readObject(start)
		if err != nil {
			return 0, fmt.Errorf("pack: object %d at %d: %w", i, start, err)
		}
		if !oid.Valid() {
			return 0, fmt.Errorf("pack: invalid oid for %s size %d", obj.Type, obj.Size)
		}
		d.offsets[start] = oid
		d.objects++

		if d.collect {
			d.collected[oid] = obj
			d.order = append(d.order, oid)
		}
		if d.Store != nil {
			if _, err := d.Store.Write(obj); err != nil {
				return 0, fmt.Errorf("pack: write %s: %w", oid, err)
			}
		}
	}

	// trailer：body 的 SHA1，直接读底层（不计入 SHA1）
	want := d.sha.Sum(nil)
	trailer := make([]byte, 20)
	if _, err := io.ReadFull(d.br, trailer); err != nil {
		return 0, fmt.Errorf("pack: read trailer: %w", err)
	}
	if !bytes.Equal(want, trailer) {
		return 0, fmt.Errorf("pack: trailer sha1 mismatch")
	}
	// 非 sideband 传输下 pack 之后跟一个 pkt-line flush-pkt，容许并忽略；
	// 其它多余字节视为格式错误。
	rest, err := io.ReadAll(d.br)
	if err != nil {
		return 0, fmt.Errorf("pack: read trailing: %w", err)
	}
	if len(rest) != 0 && !(len(rest) == 4 && string(rest) == PktFlush) {
		return 0, fmt.Errorf("pack: trailing bytes after trailer (%d)", len(rest))
	}
	return d.objects, nil
}

// readObject 从流中读一个对象，返回对象与其 oid。start 是本对象 type 字节的偏移。
func (d *PackDecoder) readObject(start int64) (*RawObject, Oid, error) {
	b, err := d.stream.ReadByte()
	if err != nil {
		return nil, "", fmt.Errorf("truncated object header at %d: %w", start, err)
	}
	pt := (b >> 4) & 0x07
	size := uint64(b & 0x0f)
	shift := uint(4)
	for b&0x80 != 0 {
		if shift >= 64 {
			return nil, "", fmt.Errorf("object size varint too long at %d", start)
		}
		b, err = d.stream.ReadByte()
		if err != nil {
			return nil, "", fmt.Errorf("truncated size varint at %d: %w", start, err)
		}
		size |= uint64(b&0x7f) << shift
		shift += 7
	}
	if size > maxObjectSize {
		return nil, "", fmt.Errorf("object size %d exceeds limit %d at %d", size, maxObjectSize, start)
	}

	switch pt {
	case packObjCommit, packObjTree, packObjBlob, packObjTag:
		content, err := d.readZlibContent(int(size))
		if err != nil {
			return nil, "", err
		}
		objType, _ := packTypeToObj(pt)
		obj := NewRawObject(objType, content)
		return obj, obj.Oid(), nil

	case packObjOfsDelta:
		off, err := d.readOfsDelta()
		if err != nil {
			return nil, "", err
		}
		if off <= 0 || off > start {
			return nil, "", fmt.Errorf("ofs-delta: bad offset %d at %d", off, start)
		}
		delta, err := d.readZlibContent(int(size))
		if err != nil {
			return nil, "", err
		}
		baseOid, ok := d.offsets[start-off]
		if !ok {
			return nil, "", fmt.Errorf("ofs-delta: base at byte %d not found (off=%d)", start-off, off)
		}
		base, err := d.lookupBase(baseOid)
		if err != nil {
			return nil, "", fmt.Errorf("ofs-delta: %w", err)
		}
		content, err := ApplyDelta(base.Content, delta)
		if err != nil {
			return nil, "", fmt.Errorf("ofs-delta apply: %w", err)
		}
		obj := NewRawObject(base.Type, content)
		return obj, obj.Oid(), nil

	case packObjRefDelta:
		oidBytes := make([]byte, 20)
		if _, err := io.ReadFull(d.stream, oidBytes); err != nil {
			return nil, "", fmt.Errorf("ref-delta: short oid: %w", err)
		}
		baseOid := Oid(fmt.Sprintf("%x", oidBytes))
		delta, err := d.readZlibContent(int(size))
		if err != nil {
			return nil, "", err
		}
		base, err := d.lookupBase(baseOid)
		if err != nil {
			return nil, "", fmt.Errorf("ref-delta: %w", err)
		}
		content, err := ApplyDelta(base.Content, delta)
		if err != nil {
			return nil, "", fmt.Errorf("ref-delta apply: %w", err)
		}
		obj := NewRawObject(base.Type, content)
		return obj, obj.Oid(), nil

	default:
		return nil, "", fmt.Errorf("unknown pack object type %d", pt)
	}
}

// readZlibContent 从流中解压一个 zlib 流，校验解压长度等于声明的 size。
func (d *PackDecoder) readZlibContent(size int) ([]byte, error) {
	zr, err := zlib.NewReader(d.stream)
	if err != nil {
		return nil, fmt.Errorf("zlib init: %w", err)
	}
	defer zr.Close()
	var buf bytes.Buffer
	if _, err := io.Copy(&buf, zr); err != nil {
		return nil, fmt.Errorf("zlib read: %w", err)
	}
	if buf.Len() != size {
		return nil, fmt.Errorf("size mismatch: header %d actual %d", size, buf.Len())
	}
	return buf.Bytes(), nil
}

// lookupBase 按 oid 取 delta 的 base 内容：优先本 pack 已收集对象，其次 Store。
func (d *PackDecoder) lookupBase(oid Oid) (*RawObject, error) {
	if d.collect {
		if o, ok := d.collected[oid]; ok {
			return o, nil
		}
	}
	if d.Store != nil {
		return d.Store.Read(oid)
	}
	return nil, fmt.Errorf("base %s not found (pack nor store)", oid)
}

// readOfsDelta 读 git ofs-delta 偏移 varint（见 readOfsDeltaBytes 说明）。
func (d *PackDecoder) readOfsDelta() (int64, error) {
	var c byte
	var off uint64
	c, err := d.stream.ReadByte()
	if err != nil {
		return 0, fmt.Errorf("ofs-delta: missing offset: %w", err)
	}
	off = uint64(c & 0x7f)
	for c&0x80 != 0 {
		c, err = d.stream.ReadByte()
		if err != nil {
			return 0, fmt.Errorf("ofs-delta: truncated offset: %w", err)
		}
		off += 1
		off = (off << 7) | uint64(c&0x7f)
		if off > math.MaxInt32 {
			return 0, fmt.Errorf("ofs-delta: offset too large (%d)", off)
		}
	}
	return int64(off), nil
}

// packStreamReader 记录逻辑读取位置并累积 body SHA1。
// 实现 io.ByteReader（flate 见之即逐字节读，bufio 预读不可见）→ 位置精确。
type packStreamReader struct {
	br  *bufio.Reader
	sha hash.Hash
	pos int64
}

func (r *packStreamReader) ReadByte() (byte, error) {
	b, err := r.br.ReadByte()
	if err != nil {
		return 0, err
	}
	r.pos++
	_, _ = r.sha.Write([]byte{b})
	return b, nil
}

func (r *packStreamReader) Read(p []byte) (int, error) {
	n, err := r.br.Read(p)
	if n > 0 {
		r.pos += int64(n)
		_, _ = r.sha.Write(p[:n])
	}
	return n, err
}

// maxObjectSize 阻止不可信 size 声明导致的超大预分配。
const maxObjectSize = 1 << 30 // 1 GiB

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
