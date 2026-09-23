package pgs

import (
	"os"
	"testing"
)

// TestMain 统一降低测试中的镜像同步重试次数：
// 绝大多数用例把镜像指向不可达地址（预期失败），真实退避会显著拖慢测试。
func TestMain(m *testing.M) {
	if Settings == nil {
		Settings = &Setting{}
	}
	Settings.MirrorRetryAttempts = 1
	os.Exit(m.Run())
}
