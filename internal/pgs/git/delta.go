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

// encodeVarintLE 写 little-endian base-128 varint（git delta 头部用，与 readVarintLE 互逆）
func encodeVarintLE(v uint64) []byte {
	var out []byte
	for {
		c := byte(v & 0x7f)
		v >>= 7
		if v != 0 {
			c |= 0x80
		}
		out = append(out, c)
		if v == 0 {
			break
		}
	}
	return out
}

// --- delta 生成（出向 clone 编码用）---

const deltaWindow = 16         // 固定匹配窗口长度
const deltaHashBase uint32 = 31 // 滚动 hash 基数

// deltaHashPow = deltaHashBase^(deltaWindow-1)，滚动 hash 移除旧字节时用
var deltaHashPow = func() uint32 {
	var p uint32 = 1
	for i := 0; i < deltaWindow-1; i++ {
		p *= deltaHashBase
	}
	return p
}()

// hashWindow 算 b[0:deltaWindow] 的完整窗口 hash
func hashWindow(b []byte) uint32 {
	var h uint32
	for i := 0; i < deltaWindow; i++ {
		h = h*deltaHashBase + uint32(b[i])
	}
	return h
}

// rollHash 从 h 移除 out 字节、加入 in 字节（uint32 自然溢出取模）
func rollHash(h uint32, out, in byte) uint32 {
	return (h-uint32(out)*deltaHashPow)*deltaHashBase + uint32(in)
}

// matchLen 从 base[bi] 与 target[ti] 起向后比较，返回最长公共前缀长度
func matchLen(base, target []byte, bi, ti int) int {
	n := 0
	for bi+n < len(base) && ti+n < len(target) && base[bi+n] == target[ti+n] {
		n++
	}
	return n
}

// appendCopyOpOnce 编码单条 copy 指令（offset/size 按 git delta 变长编码，省略零高位字节）
func appendCopyOpOnce(d *[]byte, offset, size uint32) {
	var op byte = 0x80
	var offBytes [4]byte
	nOff := 0
	for {
		offBytes[nOff] = byte(offset)
		nOff++
		offset >>= 8
		if offset == 0 || nOff == 4 {
			break
		}
	}
	for i := 0; i < nOff; i++ {
		op |= byte(1 << i)
	}
	var sizeBytes [3]byte
	nSize := 0
	for {
		sizeBytes[nSize] = byte(size)
		nSize++
		size >>= 8
		if size == 0 || nSize == 3 {
			break
		}
	}
	for i := 0; i < nSize; i++ {
		op |= byte(1 << (4 + i))
	}
	*d = append(*d, op)
	*d = append(*d, offBytes[:nOff]...)
	*d = append(*d, sizeBytes[:nSize]...)
}

// appendCopyOp 编码 copy 指令，size 超过 0xFFFFFF（3 字节上限）自动拆分
func appendCopyOp(d *[]byte, offset, size uint32) {
	for size > 0 {
		chunk := size
		if chunk > 0xFFFFFF {
			chunk = 0xFFFFFF
		}
		appendCopyOpOnce(d, offset, chunk)
		offset += chunk
		size -= chunk
	}
}

// appendInsertOp 编码 insert 指令，单条上限 127 字节，超出自动拆分
func appendInsertOp(d *[]byte, data []byte) {
	for len(data) > 0 {
		n := len(data)
		if n > 127 {
			n = 127
		}
		*d = append(*d, byte(n))
		*d = append(*d, data[:n]...)
		data = data[n:]
	}
}

// EncodeDelta 生成 base → target 的 git delta 字节序列。
// 算法：固定窗口（W=16）滚动 hash 在 base 上建位置索引，
// target 滑动匹配，命中后逐字节比较防冲突并向后扩展最长匹配。
// 匹配长度 ≥ W 发 copy 指令，否则累积为 insert。
// 返回的 delta 可被 ApplyDelta 还原。
func EncodeDelta(base, target []byte) ([]byte, error) {
	if len(base) == 0 {
		return nil, fmt.Errorf("delta: empty base")
	}
	// 1. base 窗口 hash 索引（base 长度 < W 时跳过，target 全 insert）
	index := map[uint32][]int{}
	if len(base) >= deltaWindow {
		h := hashWindow(base)
		index[h] = append(index[h], 0)
		for i := 1; i+deltaWindow <= len(base); i++ {
			h = rollHash(h, base[i-1], base[i+deltaWindow-1])
			index[h] = append(index[h], i)
		}
	}
	// 2. delta header: srcSize + tgtSize（LE varint）
	d := make([]byte, 0, len(target)/4)
	d = append(d, encodeVarintLE(uint64(len(base)))...)
	d = append(d, encodeVarintLE(uint64(len(target)))...)
	// 3. 遍历 target：命中长匹配发 copy，否则累积 insert
	ti := 0
	var pending []byte
	for ti < len(target) {
		var bestLen, bestOff int
		if ti+deltaWindow <= len(target) {
			th := hashWindow(target[ti:])
			if positions, ok := index[th]; ok {
				for _, bi := range positions {
					if l := matchLen(base, target, bi, ti); l > bestLen {
						bestLen = l
						bestOff = bi
					}
				}
			}
		}
		if bestLen >= deltaWindow {
			if len(pending) > 0 {
				appendInsertOp(&d, pending)
				pending = pending[:0]
			}
			appendCopyOp(&d, uint32(bestOff), uint32(bestLen))
			ti += bestLen
		} else {
			pending = append(pending, target[ti])
			ti++
			if len(pending) == 127 {
				appendInsertOp(&d, pending)
				pending = pending[:0]
			}
		}
	}
	if len(pending) > 0 {
		appendInsertOp(&d, pending)
	}
	return d, nil
}
