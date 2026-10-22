package server

import (
	"os"
	"testing"

	"pgit/internal/pgs"
)

// TestMain 收敛依赖全局 Settings 的网络相关默认值：
// GitHub 导入用例的 fixture clone_url 指向真实 github.com，导入后会异步触发一次镜像同步，
// 默认 3 次重试 + 120s 停滞超时会把「不可达」变成几十秒的测试耗时（且受网络环境影响）。
// 这里与 pgs 包的 TestMain 对齐：只保留 1 次重试，并把停滞超时压到 1s。
func TestMain(m *testing.M) {
	if pgs.Settings == nil {
		pgs.Settings = &pgs.Setting{}
	}
	pgs.Settings.MirrorRetryAttempts = 1
	pgs.Settings.MirrorStallTimeoutSec = 1
	if pgs.Settings.WebUIPrefix == "" {
		pgs.Settings.WebUIPrefix = "__webui"
	}
	os.Exit(m.Run())
}
