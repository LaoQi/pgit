package git

import "errors"

// ErrClientAborted 表示客户端在协议交换开始前就主动结束会话——例如 `git ls-remote`
// 读完 ref advertisement 后发一个 flush-pkt 切断连接。这是正常收尾而非协议错误，
// 调用方应据此降级日志（HTTP/SSH 层不再记 ERROR），避免污染日志与告警。
var ErrClientAborted = errors.New("git: client aborted session")
