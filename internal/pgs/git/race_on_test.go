//go:build race

package git

// raceEnabled 表示当前测试二进制带 -race（内存度量会被影子内存放大，需跳过）。
const raceEnabled = true
