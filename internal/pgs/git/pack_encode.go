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

// PackEncoder 编码 packfile 到 io.Writer。本版无 delta（clone 用）。
type PackEncoder struct {
	W     io.Writer
	sha   hash.Hash
	count int
}

func NewPackEncoder(w io.Writer) *PackEncoder {
	return &PackEncoder{W: w, sha: sha1.New()}
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

// WriteObject 写一个非 delta 对象（commit/tree/blob/tag）
func (e *PackEncoder) WriteObject(obj *RawObject) error {
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
	return err
}

// WriteTrailer 写 20 字节 SHA1（覆盖 header+objects，不含 trailer 自身）
func (e *PackEncoder) WriteTrailer() error {
	_, err := e.W.Write(e.sha.Sum(nil))
	return err
}

// write 同时写到 W 与 sha 累积器
func (e *PackEncoder) write(p []byte) (int, error) {
	e.sha.Write(p)
	return e.W.Write(p)
}
