package git

import "time"

// 测试包内禁用真实退避等待，避免失败路径用例被拖慢。
func init() { retrySleep = func(time.Duration) {} }
