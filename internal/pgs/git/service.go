package git

import (
	"bytes"
	"fmt"
	"io"
)

// ServeInfoRefs 生成完整 smart-http info/refs 响应：
//
//	"# service=<service>\n" 帧 + flush + AdvertiseRefs 输出。
//
// service 形如 "git-upload-pack"/"git-receive-pack"。
func ServeInfoRefs(repoRoot string, service string) ([]byte, error) {
	var buf bytes.Buffer
	pw := NewPktWriter(&buf)
	if err := pw.WritePktString("# service=" + service + "\n"); err != nil {
		return nil, err
	}
	if err := pw.WriteFlush(); err != nil {
		return nil, err
	}
	adv, err := AdvertiseRefs(repoRoot, service)
	if err != nil {
		return nil, err
	}
	buf.Write(adv)
	return buf.Bytes(), nil
}

// HandleUploadPack 处理 HTTP POST git-upload-pack（= ServeUploadPack）。
func HandleUploadPack(repoRoot string, in io.Reader, out io.Writer) error {
	return ServeUploadPack(repoRoot, in, out)
}

// HandleReceivePack 处理 HTTP POST git-receive-pack（= ServeReceivePack）。
// 返回逐 ref 更新结果（供调用方触发中转转发等后续动作）。
func HandleReceivePack(repoRoot string, in io.Reader, out io.Writer, opts ...ReceivePackOptions) ([]RefUpdateResult, error) {
	return ServeReceivePack(repoRoot, in, out, opts...)
}

// HandleSSHSession 处理 SSH exec 请求。
// cmdName: "git-upload-pack"/"git-receive-pack"/"git-upload-archive"。
// SSH 单连接：先发 ref advertisement，再走 Serve* 协议交换。
// receive-pack 返回逐 ref 更新结果（opts 仅在 receive-pack 时生效）。
func HandleSSHSession(cmdName string, repoRoot string, ch io.ReadWriter, opts ...ReceivePackOptions) ([]RefUpdateResult, error) {
	switch cmdName {
	case "git-upload-pack":
		adv, err := AdvertiseRefs(repoRoot, "git-upload-pack")
		if err != nil {
			return nil, err
		}
		if _, err := ch.Write(adv); err != nil {
			return nil, fmt.Errorf("ssh: write advertisement: %w", err)
		}
		return nil, ServeUploadPack(repoRoot, ch, ch)
	case "git-receive-pack":
		adv, err := AdvertiseRefs(repoRoot, "git-receive-pack")
		if err != nil {
			return nil, err
		}
		if _, err := ch.Write(adv); err != nil {
			return nil, fmt.Errorf("ssh: write advertisement: %w", err)
		}
		return ServeReceivePack(repoRoot, ch, ch, opts...)
	case "git-upload-archive":
		return nil, fmt.Errorf("git-upload-archive not supported")
	default:
		return nil, fmt.Errorf("unknown ssh service %q", cmdName)
	}
}
