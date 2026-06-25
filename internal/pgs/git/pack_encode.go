package git

import (
	"bytes"
	"compress/zlib"
	"crypto/sha1"
	"encoding/binary"
	"fmt"
	"hash"
	"io"
)

// pack 类型编号
const (
	packObjCommit   = 1
	packObjTree     = 2
	packObjBlob     = 3
	packObjTag      = 4
	packObjOfsDelta = 6
	packObjRefDelta = 7

	packMagic   = "PACK"
	packVersion = uint32(2)
)

func objTypeToPack(t ObjectType) (byte, error) {
	switch t {
	case ObjCommit:
		return packObjCommit, nil
	case ObjTree:
		return packObjTree, nil
	case ObjBlob:
		return packObjBlob, nil
	case ObjTag:
		return packObjTag, nil
	}
	return 0, fmt.Errorf("unknown obj type %s", t)
}

// PackEncoder 编码 packfile 到 io.Writer。
// 支持非 delta 对象（WriteObject）与 OFS_DELTA（WriteOfsDelta，base 须先于 delta 写入）。
type PackEncoder struct {
	W          io.Writer
	sha        hash.Hash
	count      int
	written    int           // 已写入字节数（含 header），用于 OFS_DELTA 偏移计算
	baseOffset map[Oid]int   // full 对象 oid -> type 字节起始偏移（供 OFS_DELTA 引用）
}

func NewPackEncoder(w io.Writer) *PackEncoder {
	return &PackEncoder{W: w, sha: sha1.New(), baseOffset: map[Oid]int{}}
}

// WriteHeader 写 pack header（PACK + version + objCount）
func (e *PackEncoder) WriteHeader(objCount int) error {
	var buf bytes.Buffer
	buf.WriteString(packMagic)
	_ = binary.Write(&buf, binary.BigEndian, packVersion)
	_ = binary.Write(&buf, binary.BigEndian, uint32(objCount))
	_, err := e.write(buf.Bytes())
	return err
}

// WriteObject 写一个非 delta 对象（commit/tree/blob/tag）。
// 同时记录其 type 字节起始偏移，供后续 WriteOfsDelta 按 oid 引用为 base。
func (e *PackEncoder) WriteObject(obj *RawObject) error {
	start := e.written
	pt, err := objTypeToPack(obj.Type)
	if err != nil {
		return err
	}
	// type(3 bits)+size 变长编码：首字节高 3 位 type、低 4 位 size，MSB 续位
	var hdr []byte
	size := uint64(obj.Size)
	b := byte((pt << 4) | (byte(size) & 0x0f))
	size >>= 4
	for size > 0 {
		b |= 0x80
		hdr = append(hdr, b)
		b = byte(size & 0x7f)
		size >>= 7
	}
	hdr = append(hdr, b)
	if _, err := e.write(hdr); err != nil {
		return err
	}
	// zlib(content)
	var zbuf bytes.Buffer
	zw := zlib.NewWriter(&zbuf)
	if _, err := zw.Write(obj.Content); err != nil {
		zw.Close()
		return fmt.Errorf("pack: zlib write: %w", err)
	}
	if err := zw.Close(); err != nil {
		return fmt.Errorf("pack: zlib close: %w", err)
	}
	_, err = e.write(zbuf.Bytes())
	if err == nil {
		e.baseOffset[obj.Oid()] = start
	}
	return err
}

// WriteTrailer 写 20 字节 SHA1（覆盖 header+objects，不含 trailer 自身）
func (e *PackEncoder) WriteTrailer() error {
	_, err := e.W.Write(e.sha.Sum(nil))
	return err
}

// WriteOfsDelta 写一个 OFS_DELTA 对象：base 按 pack 内字节偏移引用。
// baseOid 必须是此前已通过 WriteObject 写入的 full 对象（单层差分，base 不再是 delta）。
// delta 为 EncodeDelta 的输出；base 类型由解码端从 base 对象继承。
func (e *PackEncoder) WriteOfsDelta(baseOid Oid, delta []byte) error {
	baseStart, ok := e.baseOffset[baseOid]
	if !ok {
		return fmt.Errorf("pack: ofs-delta base %s not written yet", baseOid)
	}
	// delta 对象 type 字节起始偏移（须在写 header 前取，用于计算 ofs 偏移）
	deltaStart := e.written
	// type header: packObjOfsDelta + size=len(delta)
	size := uint64(len(delta))
	b := byte((packObjOfsDelta << 4) | (byte(size) & 0x0f))
	size >>= 4
	for size > 0 {
		b |= 0x80
		if _, err := e.write([]byte{b}); err != nil {
			return err
		}
		b = byte(size & 0x7f)
		size >>= 7
	}
	if _, err := e.write([]byte{b}); err != nil {
		return err
	}
	// ofs-delta 偏移：delta type 字节偏移 - base type 字节偏移
	off := deltaStart - baseStart
	if _, err := e.write(encodeOfsDelta(uint64(off))); err != nil {
		return err
	}
	// zlib(delta)
	var zbuf bytes.Buffer
	zw := zlib.NewWriter(&zbuf)
	if _, err := zw.Write(delta); err != nil {
		zw.Close()
		return fmt.Errorf("pack: ofs-delta zlib write: %w", err)
	}
	if err := zw.Close(); err != nil {
		return fmt.Errorf("pack: ofs-delta zlib close: %w", err)
	}
	_, err := e.write(zbuf.Bytes())
	return err
}

// encodeOfsDelta 编码 git ofs-delta 偏移（big-endian base-128，每续位 +1 补偿）。
// 与 readOfsDelta 互逆。返回值是「当前对象 type 字节偏移 - base type 字节偏移」。
func encodeOfsDelta(off uint64) []byte {
	var buf [16]byte
	pos := len(buf) - 1
	buf[pos] = byte(off) & 0x7f
	tmp := off >> 7
	for tmp != 0 {
		pos--
		tmp--
		buf[pos] = 0x80 | byte(tmp&0x7f)
		tmp >>= 7
	}
	return append([]byte(nil), buf[pos:]...)
}

// write 同时写到 W 与 sha 累积器，并累加 written 计数
func (e *PackEncoder) write(p []byte) (int, error) {
	e.sha.Write(p)
	n, err := e.W.Write(p)
	e.written += n
	return n, err
}
