package git

import "fmt"

// git delta 格式（OFS_DELTA/REF_DELTA 还原）：
//   - 头部：source size varint(LE)、target size varint(LE)
//   - 指令：MSB=1 是 copy，MSB=0 是 insert
//     copy: 0b1xxxxxxx，按 bit 0-3 读 1-4 字节 offset、bit 4-6 读 1-3 字节 size；
//           size=0 表示 0x10000
//     insert: 0b0xxxxxxx（x≠0），后续 x 字节直接拷贝
//   - 0x00 非法

// ApplyDelta 对 base 应用 delta 指令，返回还原后的 target
func ApplyDelta(base, delta []byte) ([]byte, error) {
	pos := 0
	srcSize, n := readVarintLE(delta[pos:])
	pos += n
	if int(srcSize) != len(base) {
		return nil, fmt.Errorf("delta: src size %d != base %d", srcSize, len(base))
	}
	tgtSize, n := readVarintLE(delta[pos:])
	pos += n
	target := make([]byte, 0, tgtSize)
	for pos < len(delta) {
		op := delta[pos]
		pos++
		if op&0x80 != 0 {
			// copy: 读 offset 与 size
			var offset, size uint32
			if op&0x01 != 0 {
				offset |= uint32(delta[pos])
				pos++
			}
			if op&0x02 != 0 {
				offset |= uint32(delta[pos]) << 8
				pos++
			}
			if op&0x04 != 0 {
				offset |= uint32(delta[pos]) << 16
				pos++
			}
			if op&0x08 != 0 {
				offset |= uint32(delta[pos]) << 24
				pos++
			}
			if op&0x10 != 0 {
				size |= uint32(delta[pos])
				pos++
			}
			if op&0x20 != 0 {
				size |= uint32(delta[pos]) << 8
				pos++
			}
			if op&0x40 != 0 {
				size |= uint32(delta[pos]) << 16
				pos++
			}
			if size == 0 {
				size = 0x10000
			}
			if int(offset) > len(base) || int(offset+size) > len(base) {
				return nil, fmt.Errorf("delta: copy out of range off=%d size=%d base=%d", offset, size, len(base))
			}
			target = append(target, base[offset:offset+size]...)
		} else if op != 0 {
			// insert: op 字节直接拷贝
			if pos+int(op) > len(delta) {
				return nil, fmt.Errorf("delta: insert out of range op=%d rem=%d", op, len(delta)-pos)
			}
			target = append(target, delta[pos:pos+int(op)]...)
			pos += int(op)
		} else {
			return nil, fmt.Errorf("delta: illegal 0x00 opcode")
		}
	}
	if int(tgtSize) != len(target) {
		return nil, fmt.Errorf("delta: tgt size %d != actual %d", tgtSize, len(target))
	}
	return target, nil
}

// readVarintLE 读 little-endian base-128 varint（git delta 头部用）
func readVarintLE(b []byte) (uint64, int) {
	var v uint64
	var shift uint
	n := 0
	for n < len(b) {
		c := b[n]
		n++
		v |= uint64(c&0x7f) << shift
		if c&0x80 == 0 {
			break
		}
		shift += 7
	}
	return v, n
}
