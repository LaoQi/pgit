package git

import (
	"bytes"
	"crypto/sha1"
	"testing"
)

// 以下用例来自 docs/architecture-review.md 附录 A-2/A-3：
// 修复前会 panic（index out of range），现必须返回错误。

// 畸形 pack：对象 size varint 截断（首字节带续位但后续字节缺失）。
func TestMalformedPackTruncatedSizeVarint(t *testing.T) {
	body := []byte("PACK\x00\x00\x00\x02\x00\x00\x00\x01")
	body = append(body, 0x80) // type 首字节带续位，后续字节缺失
	h := sha1.Sum(body)
	pack := append(append([]byte{}, body...), h[:]...)

	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("panic on malformed pack: %v", r)
		}
	}()
	if _, err := NewPackDecoder(bytes.NewReader(pack)).Decode(); err == nil {
		t.Fatal("truncated pack should return an error")
	}
}

// 畸形 delta：copy 指令声明要读 offset 字节但数据已尽。
func TestMalformedDeltaTruncatedCopy(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("panic on malformed delta: %v", r)
		}
	}()
	// srcSize=0（base 空）、tgtSize=1、copy 指令（0x81）后无数据
	if _, err := ApplyDelta(nil, []byte{0x00, 0x01, 0x81}); err == nil {
		t.Fatal("truncated copy instruction should return an error")
	}
}

// 畸形 delta：头部被截断（无 target size）。
func TestMalformedDeltaTruncatedHeader(t *testing.T) {
	if _, err := ApplyDelta(nil, []byte{0x00}); err == nil {
		t.Fatal("truncated delta header should return an error")
	}
	if _, err := ApplyDelta(nil, nil); err == nil {
		t.Fatal("empty delta should return an error")
	}
}

// 畸形 delta：tgtSize 声明为 2GiB，必须有上限保护（不得预分配后 OOM）。
func TestMalformedDeltaHugeTargetSize(t *testing.T) {
	var d []byte
	d = append(d, encodeVarintLE(0)...)     // srcSize = 0
	d = append(d, encodeVarintLE(2<<30)...) // tgtSize = 2GiB
	if _, err := ApplyDelta(nil, d); err == nil {
		t.Fatal("oversized target should be rejected")
	}
}

// 畸形 pack：ofs-delta 偏移指向 pack 头部之外。
func TestMalformedPackOfsDeltaBadOffset(t *testing.T) {
	body := []byte("PACK\x00\x00\x00\x02\x00\x00\x00\x01")
	// type=OFS_DELTA(6)、size=2；偏移 varint 声明一个极大的值（base 落在 pack 之前）
	body = append(body, byte(packObjOfsDelta<<4|2))
	body = append(body, encodeOfsDelta(1<<31)...)
	body = append(body, 0x78, 0x9c) // 伪 zlib 数据（不会被执行到）
	h := sha1.Sum(body)
	pack := append(append([]byte{}, body...), h[:]...)

	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("panic on bad ofs-delta offset: %v", r)
		}
	}()
	if _, err := NewPackDecoder(bytes.NewReader(pack)).Decode(); err == nil {
		t.Fatal("bad ofs-delta offset should return an error")
	}
}

// 畸形 pack：多对象时 trailer/长度自洽但对象数据缺失。
func TestMalformedPackTruncatedObject(t *testing.T) {
	body := []byte("PACK\x00\x00\x00\x02\x00\x00\x00\x01")
	body = append(body, byte(packObjBlob<<4|10)) // 声明 10 字节 blob，实际无 zlib 数据
	h := sha1.Sum(body)
	pack := append(append([]byte{}, body...), h[:]...)

	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("panic on truncated object: %v", r)
		}
	}()
	if _, err := NewPackDecoder(bytes.NewReader(pack)).Decode(); err == nil {
		t.Fatal("truncated object should return an error")
	}
}

// 合法 pack 经解码后再编码，仍须完全一致（边界检查不得改变正常路径行为）。
func TestPackDecodeEncodeRoundTripAfterHardening(t *testing.T) {
	blob := makeBlob("roundtrip content\n")
	var buf bytes.Buffer
	enc := NewPackEncoder(&buf)
	if err := enc.WriteHeader(1); err != nil {
		t.Fatal(err)
	}
	if err := enc.WriteObject(blob); err != nil {
		t.Fatal(err)
	}
	if err := enc.WriteTrailer(); err != nil {
		t.Fatal(err)
	}
	objs, err := NewPackDecoder(bytes.NewReader(buf.Bytes())).Decode()
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if len(objs) != 1 || objs[0].Oid() != blob.Oid() {
		t.Fatalf("roundtrip mismatch: %+v", objs)
	}
}

// --- fuzz 目标 ---

func FuzzApplyDelta(f *testing.F) {
	base := []byte("the quick brown fox jumps over the lazy dog")
	d, err := EncodeDelta(base, []byte("the quick brown fox jumps over the lazy cat"))
	if err != nil {
		f.Fatal(err)
	}
	f.Add(base, d)
	f.Add([]byte{}, []byte{0})
	f.Add([]byte("abc"), []byte{0x00, 0x01, 0x81})

	f.Fuzz(func(t *testing.T, base, delta []byte) {
		// 只要求「不 panic、不 OOM」：错误是允许的正常输出
		out, err := ApplyDelta(base, delta)
		if err == nil && out == nil {
			t.Fatal("nil result without error")
		}
	})
}

func FuzzPackDecoder(f *testing.F) {
	var buf bytes.Buffer
	enc := NewPackEncoder(&buf)
	if err := enc.WriteHeader(2); err != nil {
		f.Fatal(err)
	}
	if err := enc.WriteObject(makeBlob("hello\n")); err != nil {
		f.Fatal(err)
	}
	if err := enc.WriteObject(makeTree(nil)); err != nil {
		f.Fatal(err)
	}
	if err := enc.WriteTrailer(); err != nil {
		f.Fatal(err)
	}
	f.Add(buf.Bytes())
	f.Add([]byte("PACK\x00\x00\x00\x02\x00\x00\x00\x01\x80"))

	f.Fuzz(func(t *testing.T, data []byte) {
		dec := NewPackDecoder(bytes.NewReader(data))
		_, _ = dec.Decode() // 不得 panic
	})
}

func FuzzReadOfsDelta(f *testing.F) {
	f.Add([]byte{0x00})
	f.Add([]byte{0x80, 0x80, 0x80})
	f.Add(encodeOfsDelta(12345))

	f.Fuzz(func(t *testing.T, b []byte) {
		d := NewPackDecoder(bytes.NewReader(b))
		off, err := d.readOfsDelta()
		// offset 是「相对当前对象起始」的回退距离，其上界由调用方校验
		if err == nil && (off < 0 || d.stream.pos > int64(len(b))) {
			t.Fatalf("inconsistent result: off=%d pos=%d len=%d", off, d.stream.pos, len(b))
		}
	})
}

func FuzzPktReader(f *testing.F) {
	var buf bytes.Buffer
	pw := NewPktWriter(&buf)
	_ = pw.WritePktString("want 0000000000000000000000000000000000000000\n")
	_ = pw.WriteFlush()
	_ = pw.WritePkt([]byte{1, 2, 3})
	f.Add(buf.Bytes())
	f.Add([]byte("0010abc"))
	f.Add([]byte("0000"))
	f.Add([]byte("0001"))
	f.Add([]byte("zzzz"))

	f.Fuzz(func(t *testing.T, data []byte) {
		pr := NewPktReader(bytes.NewReader(data))
		for i := 0; i < 64; i++ { // 有界循环，避免无限流
			if _, _, err := pr.ReadPkt(); err != nil {
				return
			}
		}
	})
}
