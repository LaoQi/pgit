package git

import (
	"fmt"
	"io"
)

// pkt-line 协议：4 字节 hex 长度前缀（含自身 4 字节）后跟 payload。
// 0000 = flush，0001 = delim（v2），0002 = response-end（v2）。
// 长度范围 0004-ffff，payload 最长 65516（与 git LARGE_PACKET_DATA 一致）。

const (
	PktFlush       = "0000"
	PktDelim       = "0001"
	PktResponseEnd = "0002"
	maxPktPayload  = 65516 // LARGE_PACKET_DATA：65520(LARGE_PACKET_MAX) - 4
)

// PktWriter 把 payload 封装成 pkt-line 帧写到 io.Writer
type PktWriter struct {
	W io.Writer
}

func NewPktWriter(w io.Writer) *PktWriter { return &PktWriter{W: w} }

// WritePkt 写一个 payload 帧。payload 为 nil 则写 flush
func (p *PktWriter) WritePkt(payload []byte) error {
	if payload == nil {
		_, err := p.W.Write([]byte(PktFlush))
		return err
	}
	if len(payload) > maxPktPayload {
		return fmt.Errorf("pkt: payload too long: %d (max %d)", len(payload), maxPktPayload)
	}
	hdr := []byte(fmt.Sprintf("%04x", len(payload)+4))
	if _, err := p.W.Write(hdr); err != nil {
		return err
	}
	_, err := p.W.Write(payload)
	return err
}

// WritePktString 写字符串帧
func (p *PktWriter) WritePktString(s string) error {
	return p.WritePkt([]byte(s))
}

// WriteFlush 写 flush 包
func (p *PktWriter) WriteFlush() error {
	_, err := p.W.Write([]byte(PktFlush))
	return err
}

// PktReader 从 io.Reader 读 pkt-line 帧
type PktReader struct {
	R   io.Reader
	buf [4]byte
}

func NewPktReader(r io.Reader) *PktReader { return &PktReader{R: r} }

// ReadPkt 读一帧。flush/delim/response-end 返回 (nil, true, nil)；
// 普通帧返回 (payload, false, nil)。
func (p *PktReader) ReadPkt() (payload []byte, isFlush bool, err error) {
	if _, err := io.ReadFull(p.R, p.buf[:]); err != nil {
		return nil, false, err
	}
	hexStr := string(p.buf[:])
	if hexStr == PktFlush || hexStr == PktDelim || hexStr == PktResponseEnd {
		return nil, true, nil
	}
	var n int
	if _, err := fmt.Sscanf(hexStr, "%x", &n); err != nil {
		return nil, false, fmt.Errorf("pkt: bad length %q: %w", hexStr, err)
	}
	if n < 4 {
		return nil, false, fmt.Errorf("pkt: bad length %d", n)
	}
	payload = make([]byte, n-4)
	if _, err := io.ReadFull(p.R, payload); err != nil {
		return nil, false, err
	}
	return payload, false, nil
}

// --- sideband-64k 多路封装 ---
// 每帧前缀 1 字节 channel：1=pack 数据，2=progress，3=error
const (
	SidebandPack     byte = 1
	SidebandProgress byte = 2
	SidebandError    byte = 3

	// SidebandMaxPayload sideband-64k 单帧最大数据量：
	// pkt payload 上限 65516 减去 1 字节 channel = 65515
	SidebandMaxPayload = maxPktPayload - 1
)

// SidebandWriter 把数据按指定 channel 封装成 pkt-line，自动分帧
type SidebandWriter struct {
	pw *PktWriter
	ch byte
}

func NewSidebandWriter(pw *PktWriter, channel byte) *SidebandWriter {
	return &SidebandWriter{pw: pw, ch: channel}
}

// Write 实现标准 io.Writer，每帧 ≤ SidebandMaxPayload
func (s *SidebandWriter) Write(p []byte) (int, error) {
	total := 0
	for len(p) > 0 {
		n := len(p)
		if n > SidebandMaxPayload {
			n = SidebandMaxPayload
		}
		frame := make([]byte, n+1)
		frame[0] = s.ch
		copy(frame[1:], p[:n])
		if err := s.pw.WritePkt(frame); err != nil {
			return total, err
		}
		total += n
		p = p[n:]
	}
	return total, nil
}

// WriteSidebandProgress 便捷写进度消息
func WriteSidebandProgress(pw *PktWriter, msg string) error {
	sw := NewSidebandWriter(pw, SidebandProgress)
	_, err := sw.Write([]byte(msg))
	return err
}
