package pgs

import (
	"fmt"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// 本包实现极简指标收集与 Prometheus 文本格式输出（不引入第三方依赖）。
// 指标命名遵循 Prometheus 惯例：<namespace>_<subsystem>_<name>_<unit>。

// Metric 是一条带标签的时序（由 Registry 持有）。
type metric struct {
	name   string
	help   string
	kind   string // counter | gauge
	labels []string

	mu     sync.Mutex // 保护 values（采集来自并发请求）
	values map[string]float64
}

// Registry 是进程级指标表。
type Registry struct {
	mu      sync.Mutex
	metrics map[string]*metric
}

// NewRegistry 构造空指标表。
func NewRegistry() *Registry {
	return &Registry{metrics: map[string]*metric{}}
}

// globalRegistry 是进程级默认表（HTTP/同步/协议指标统一登记于此）。
var globalRegistry = NewRegistry()

// DefaultRegistry 返回进程级指标表。
func DefaultRegistry() *Registry { return globalRegistry }

func (r *Registry) get(name, help, kind string, labels []string) *metric {
	m, ok := r.metrics[name]
	if !ok {
		m = &metric{name: name, help: help, kind: kind, labels: labels, values: map[string]float64{}}
		r.metrics[name] = m
	}
	return m
}

func labelKey(values []string) string { return strings.Join(values, "\x00") }

// labelString 生成 Prometheus 标签串，如 {a="1",b="2"}；无标签返回空串。
func (m *metric) labelString(values []string) string {
	if len(m.labels) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteByte('{')
	for i, l := range m.labels {
		if i > 0 {
			b.WriteByte(',')
		}
		v := ""
		if i < len(values) {
			v = values[i]
		}
		b.WriteString(l)
		b.WriteString(`="`)
		b.WriteString(escapeLabel(v))
		b.WriteByte('"')
	}
	b.WriteByte('}')
	return b.String()
}

func escapeLabel(v string) string {
	v = strings.ReplaceAll(v, `\`, `\\`)
	v = strings.ReplaceAll(v, "\n", `\n`)
	return strings.ReplaceAll(v, `"`, `\"`)
}

// Counter 计数器（只增）。
type Counter struct{ m *metric }

// Counter 取出/创建计数器。
func (r *Registry) Counter(name, help string, labels ...string) *Counter {
	r.mu.Lock()
	defer r.mu.Unlock()
	return &Counter{m: r.get(name, help, "counter", labels)}
}

// Add 累加（labelValues 需与 labels 顺序一致）。
func (c *Counter) Add(v float64, labelValues ...string) {
	c.m.add(v, labelValues)
}

// Inc 自增 1。
func (c *Counter) Inc(labelValues ...string) { c.m.add(1, labelValues) }

func (m *metric) add(v float64, labelValues []string) {
	m.mu.Lock()
	m.values[labelKey(labelValues)] += v
	m.mu.Unlock()
}

// Gauge 瞬时值。
type Gauge struct{ m *metric }

// Gauge 取出/创建 gauge。
func (r *Registry) Gauge(name, help string, labels ...string) *Gauge {
	r.mu.Lock()
	defer r.mu.Unlock()
	return &Gauge{m: r.get(name, help, "gauge", labels)}
}

// Set 设置瞬时值。
func (g *Gauge) Set(v float64, labelValues ...string) {
	g.m.mu.Lock()
	g.m.values[labelKey(labelValues)] = v
	g.m.mu.Unlock()
}

// Add 增减瞬时值。
func (g *Gauge) Add(v float64, labelValues ...string) {
	g.m.mu.Lock()
	g.m.values[labelKey(labelValues)] += v
	g.m.mu.Unlock()
}

// Render 输出 Prometheus 文本格式（text/plain; version=0.0.4）。
func (r *Registry) Render() []byte {
	r.mu.Lock()
	defer r.mu.Unlock()

	names := make([]string, 0, len(r.metrics))
	for n := range r.metrics {
		names = append(names, n)
	}
	sort.Strings(names)

	var b strings.Builder
	for _, n := range names {
		m := r.metrics[n]
		fmt.Fprintf(&b, "# HELP %s %s\n", m.name, m.help)
		fmt.Fprintf(&b, "# TYPE %s %s\n", m.name, m.kind)
		m.mu.Lock()
		keys := make([]string, 0, len(m.values))
		for k := range m.values {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			var values []string
			if k != "" {
				values = strings.Split(k, "\x00")
			}
			fmt.Fprintf(&b, "%s%s %g\n", m.name, m.labelString(values), m.values[k])
		}
		m.mu.Unlock()
	}
	return []byte(b.String())
}

// --- 常用指标（延迟初始化，避免包级 init 顺序问题）---

// HTTP 请求指标。
func ObserveHTTPRequest(method string, status int, duration time.Duration) {
	DefaultRegistry().Counter(
		"pgit_http_requests_total",
		"HTTP 请求总数，按方法、状态码与结果分类。",
		"method", "status", "result",
	).Inc(method, fmt.Sprintf("%d", status), statusClass(status))
	DefaultRegistry().Counter(
		"pgit_http_request_duration_seconds_total",
		"HTTP 请求累计耗时（秒）。",
		"method",
	).Add(duration.Seconds(), method)
}

func statusClass(status int) string {
	switch {
	case status >= 500:
		return "error"
	case status >= 400:
		return "client_error"
	default:
		return "ok"
	}
}

// git 传输指标。
func ObserveGitOperation(service, result string) {
	DefaultRegistry().Counter(
		"pgit_git_operations_total",
		"git 传输操作总数（upload-pack/receive-pack），按结果分类。",
		"service", "result",
	).Inc(service, result)
}

// ObservePackBytes 记录传输的 pack 字节数。
func ObservePackBytes(service string, n int64) {
	if n <= 0 {
		return
	}
	DefaultRegistry().Counter(
		"pgit_pack_bytes_total",
		"经 pgit 传输的 pack 字节数。",
		"service",
	).Add(float64(n), service)
}

var inflightPacks atomic.Int64

// PackStarted 记录一次 pack 传输开始（进行中计数 +1）。
func PackStarted() { inflightPacks.Add(1) }

// PackFinished 记录一次 pack 传输结束（进行中计数 -1）。
func PackFinished() { inflightPacks.Add(-1) }

// SetMirrorSyncResult 记录镜像同步结果与最近成功时间。
func SetMirrorSyncResult(repo string, ok bool, objects int, durationMs int64) {
	result := "failure"
	if ok {
		result = "success"
	}
	DefaultRegistry().Counter(
		"pgit_mirror_sync_total",
		"镜像同步总次数，按仓库与结果分类。",
		"repo", "result",
	).Inc(repo, result)
	DefaultRegistry().Counter(
		"pgit_mirror_sync_objects_total",
		"镜像同步累计写入对象数。",
		"repo",
	).Add(float64(objects), repo)
	DefaultRegistry().Gauge(
		"pgit_mirror_sync_duration_ms",
		"最近一次镜像同步耗时（毫秒）。",
		"repo",
	).Set(float64(durationMs), repo)
	if ok {
		DefaultRegistry().Gauge(
			"pgit_mirror_last_success_timestamp_seconds",
			"最近一次镜像同步成功的时间戳（Unix 秒）。",
			"repo",
		).Set(float64(time.Now().Unix()), repo)
	}
}

// SetReposTotal 记录仓库总数。
func SetReposTotal(n int) {
	DefaultRegistry().Gauge("pgit_repositories_total", "仓库总数。").Set(float64(n))
}

// InflightPacks 返回当前进行中的 pack 传输数。
func InflightPacks() int64 { return inflightPacks.Load() }
