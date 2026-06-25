package git

import (
	"bytes"
	"testing"
)

// --- EncodeDelta ↔ ApplyDelta 回环 ---

func TestEncodeDeltaRoundTripSimilar(t *testing.T) {
	base := []byte("hello world, this is the base content for delta testing here")
	target := []byte("hello WORLD, this is the base content for delta testing here")
	d, err := EncodeDelta(base, target)
	if err != nil {
		t.Fatalf("EncodeDelta: %v", err)
	}
	got, err := ApplyDelta(base, d)
	if err != nil {
		t.Fatalf("ApplyDelta: %v", err)
	}
	if !bytes.Equal(got, target) {
		t.Fatalf("roundtrip mismatch:\n got %q\nwant %q", got, target)
	}
	// 相似内容 delta 应明显小于 target 原始大小
	if len(d) >= len(target) {
		t.Fatalf("similar delta %d should be < target %d", len(d), len(target))
	}
}

func TestEncodeDeltaRoundTripIdentical(t *testing.T) {
	base := []byte("identical content, no changes at all here, really no changes\n")
	target := append([]byte(nil), base...)
	d, err := EncodeDelta(base, target)
	if err != nil {
		t.Fatalf("EncodeDelta: %v", err)
	}
	got, err := ApplyDelta(base, d)
	if err != nil {
		t.Fatalf("ApplyDelta: %v", err)
	}
	if !bytes.Equal(got, target) {
		t.Fatal("identical roundtrip mismatch")
	}
	// 全同应是一个 copy 指令，delta 极小
	if len(d) >= len(target) {
		t.Fatalf("identical delta %d should be tiny (< %d)", len(d), len(target))
	}
}

func TestEncodeDeltaRoundTripDifferent(t *testing.T) {
	// base 全 A、target 全 B：无 16 字节窗口匹配 → 全 insert
	base := bytes.Repeat([]byte("A"), 100)
	target := bytes.Repeat([]byte("B"), 100)
	d, err := EncodeDelta(base, target)
	if err != nil {
		t.Fatalf("EncodeDelta: %v", err)
	}
	got, err := ApplyDelta(base, d)
	if err != nil {
		t.Fatalf("ApplyDelta: %v", err)
	}
	if !bytes.Equal(got, target) {
		t.Fatal("different roundtrip mismatch")
	}
	// 全 insert，delta 应约等于 target 大小（header + 拆分指令开销）
	if len(d) < len(target) {
		t.Fatalf("all-insert delta %d expected >= target %d", len(d), len(target))
	}
}

func TestEncodeDeltaInsertSplit(t *testing.T) {
	// base 16 字节全 A，target 300 字节全 B：无匹配 → 全 insert，触发 127 字节拆分
	base := bytes.Repeat([]byte("A"), 16)
	target := bytes.Repeat([]byte("B"), 300)
	d, err := EncodeDelta(base, target)
	if err != nil {
		t.Fatalf("EncodeDelta: %v", err)
	}
	got, err := ApplyDelta(base, d)
	if err != nil {
		t.Fatalf("ApplyDelta: %v", err)
	}
	if !bytes.Equal(got, target) {
		t.Fatalf("insert split mismatch: got %d bytes want %d", len(got), len(target))
	}
}

func TestEncodeDeltaLargeCopy(t *testing.T) {
	// base 大段重复模式，target 复制其中 300 字节段（>127，单条 copy 的 size 多字节编码）
	base := bytes.Repeat([]byte("pattern123"), 100) // 1000 字节
	target := append([]byte(nil), base[100:400]...)
	d, err := EncodeDelta(base, target)
	if err != nil {
		t.Fatalf("EncodeDelta: %v", err)
	}
	got, err := ApplyDelta(base, d)
	if err != nil {
		t.Fatalf("ApplyDelta: %v", err)
	}
	if !bytes.Equal(got, target) {
		t.Fatalf("large copy mismatch: got %d bytes want %d", len(got), len(target))
	}
	if len(d) >= len(target) {
		t.Fatalf("large copy delta %d should be < target %d", len(d), len(target))
	}
}

func TestEncodeDeltaShortBase(t *testing.T) {
	// base < 16 字节窗口：无 hash 索引，target 全 insert
	base := []byte("short")
	target := []byte("short and longer than base but base too short for window")
	d, err := EncodeDelta(base, target)
	if err != nil {
		t.Fatalf("EncodeDelta: %v", err)
	}
	got, err := ApplyDelta(base, d)
	if err != nil {
		t.Fatalf("ApplyDelta: %v", err)
	}
	if !bytes.Equal(got, target) {
		t.Fatal("short base roundtrip mismatch")
	}
}

func TestEncodeDeltaEmptyBase(t *testing.T) {
	if _, err := EncodeDelta(nil, []byte("x")); err == nil {
		t.Fatal("expected error for empty base")
	}
}

func TestEncodeDeltaTargetShorterThanWindow(t *testing.T) {
	// target < 16 字节：无窗口匹配，全 insert
	base := []byte("the quick brown fox jumps over the lazy dog")
	target := []byte("the lazy dog")
	d, err := EncodeDelta(base, target)
	if err != nil {
		t.Fatalf("EncodeDelta: %v", err)
	}
	got, err := ApplyDelta(base, d)
	if err != nil {
		t.Fatalf("ApplyDelta: %v", err)
	}
	if !bytes.Equal(got, target) {
		t.Fatal("short target roundtrip mismatch")
	}
}
