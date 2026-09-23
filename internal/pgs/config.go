package pgs

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"pgit/internal/pgs/git"
)

type Setting struct {
	// mu 保护字段读写：配置可经 SIGHUP 热加载（HotReload），而 handler/中间件会并发读。
	// 直接读字段的旧代码仍可用，但热加载只保证通过方法访问的路径安全。
	mu   sync.RWMutex
	path string `json:"-"`

	Listen       string            `json:"listen"`
	EnableSSH    bool              `json:"enableSSH"`
	SSHHostKey   string            `json:"sshHostKey"`
	SSHPublicKey string            `json:"sshPublicKey"`
	GitRoot      string            `json:"gitRoot"`
	HttpAuth     bool              `json:"httpAuth"`
	SSHAuthType  string            `json:"sshAuthType"`
	Credentials  map[string]string `json:"credentials"`
	WebUIPrefix  string            `json:"webuiPrefix"`
	WebUIAssets  string            `json:"webuiAssets"`
	LogLevel     string            `json:"logLevel"`
	LogFormat    string            `json:"logFormat"`
	// MaxPushBytes 单次 push 请求体上限（字节），0 = 默认 2GiB。
	MaxPushBytes int64 `json:"maxPushBytes"`
	// MaxConcurrentPacks 同时进行的 pack 传输（clone/push/fetch 同步）上限，0 = 默认 4。
	MaxConcurrentPacks int `json:"maxConcurrentPacks"`

	// MirrorStallTimeoutSec 镜像同步「传输停滞」上限（秒）：连续无数据超过该时长即判定失败。
	// 0 = 默认 120 秒。用于取代整体超时，避免大仓库传输被中途掐断。
	MirrorStallTimeoutSec int `json:"mirrorStallTimeoutSec"`
	// MirrorRetryAttempts 镜像同步失败重试次数（含首次），0 = 默认 3。
	MirrorRetryAttempts int `json:"mirrorRetryAttempts"`
	// MirrorRetryBaseDelaySec 重试退避基数（秒），0 = 默认 1。
	MirrorRetryBaseDelaySec int `json:"mirrorRetryBaseDelaySec"`
}

// 传输上限默认值。
const (
	DefaultMaxPushBytes       int64 = 2 << 30 // 2 GiB
	DefaultMaxConcurrentPacks       = 4
)

// LimitPushBytes 返回生效的单次 push 上限。
func (s *Setting) LimitPushBytes() int64 {
	if s == nil {
		return DefaultMaxPushBytes
	}
	s.mu.RLock()
	v := s.MaxPushBytes
	s.mu.RUnlock()
	if v <= 0 {
		return DefaultMaxPushBytes
	}
	return v
}

// MirrorFetchOptions 返回镜像同步的 fetch 超时/重试选项（从配置映射）。
func (s *Setting) MirrorFetchOptions() git.FetchOptions {
	if s == nil {
		return git.FetchOptions{}
	}
	s.mu.RLock()
	stall, attempts, base := s.MirrorStallTimeoutSec, s.MirrorRetryAttempts, s.MirrorRetryBaseDelaySec
	s.mu.RUnlock()
	o := git.FetchOptions{}
	if stall > 0 {
		o.StallTimeout = time.Duration(stall) * time.Second
	}
	if attempts > 0 {
		o.MaxAttempts = attempts
	}
	if base > 0 {
		o.RetryBaseDelay = time.Duration(base) * time.Second
	}
	return o
}

// LimitConcurrentPacks 返回生效的并发 pack 传输上限。
func (s *Setting) LimitConcurrentPacks() int {
	if s == nil {
		return DefaultMaxConcurrentPacks
	}
	s.mu.RLock()
	v := s.MaxConcurrentPacks
	s.mu.RUnlock()
	if v <= 0 {
		return DefaultMaxConcurrentPacks
	}
	return v
}

func (s *Setting) SetConfigPath(path string) {
	s.path = path
}

// Snapshot 返回配置的深拷贝视图（不含内部锁，供只读消费）。
func (s *Setting) Snapshot() *SettingView {
	s.mu.RLock()
	defer s.mu.RUnlock()
	v := &SettingView{
		Listen:             s.Listen,
		EnableSSH:          s.EnableSSH,
		SSHHostKey:         s.SSHHostKey,
		SSHPublicKey:       s.SSHPublicKey,
		GitRoot:            s.GitRoot,
		HttpAuth:           s.HttpAuth,
		SSHAuthType:        s.SSHAuthType,
		WebUIPrefix:        s.WebUIPrefix,
		WebUIAssets:        s.WebUIAssets,
		LogLevel:           s.LogLevel,
		LogFormat:          s.LogFormat,
		MaxPushBytes:       s.MaxPushBytes,
		MaxConcurrentPacks: s.MaxConcurrentPacks,
	}
	if s.Credentials != nil {
		v.Credentials = make(map[string]string, len(s.Credentials))
		for k, val := range s.Credentials {
			v.Credentials[k] = val
		}
	}
	return v
}

// SettingView 是 Setting 的无锁只读快照。
type SettingView struct {
	Listen             string
	EnableSSH          bool
	SSHHostKey         string
	SSHPublicKey       string
	GitRoot            string
	HttpAuth           bool
	SSHAuthType        string
	Credentials        map[string]string
	WebUIPrefix        string
	WebUIAssets        string
	LogLevel           string
	LogFormat          string
	MaxPushBytes       int64
	MaxConcurrentPacks int
}

// CredentialsCopy 返回凭据表的副本（鉴权中间件每请求调用，避免与热加载竞态）。
func (s *Setting) CredentialsCopy() map[string]string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.Credentials == nil {
		return nil
	}
	cp := make(map[string]string, len(s.Credentials))
	for k, v := range s.Credentials {
		cp[k] = v
	}
	return cp
}

// WebUI 配置读取（热加载安全）。
func (s *Setting) WebUIConf() (prefix, assets string) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.WebUIPrefix, s.WebUIAssets
}

// hotReloadFrom 把可热加载字段从 src 应用到 s（须持有写锁）。
// 返回被忽略的「需重启」字段名列表。
func (s *Setting) hotReloadFrom(src *Setting) []string {
	var restartNeeded []string
	if s.Listen != src.Listen {
		restartNeeded = append(restartNeeded, "listen")
	}
	if s.EnableSSH != src.EnableSSH {
		restartNeeded = append(restartNeeded, "enableSSH")
	}
	if s.GitRoot != src.GitRoot {
		restartNeeded = append(restartNeeded, "gitRoot")
	}
	if s.HttpAuth != src.HttpAuth {
		restartNeeded = append(restartNeeded, "httpAuth")
	}
	if s.SSHAuthType != src.SSHAuthType {
		restartNeeded = append(restartNeeded, "sshAuthType")
	}
	if s.SSHHostKey != src.SSHHostKey || s.SSHPublicKey != src.SSHPublicKey {
		restartNeeded = append(restartNeeded, "sshHostKey/sshPublicKey")
	}
	if s.WebUIPrefix != src.WebUIPrefix {
		restartNeeded = append(restartNeeded, "webuiPrefix")
	}
	if s.WebUIAssets != src.WebUIAssets {
		restartNeeded = append(restartNeeded, "webuiAssets")
	}

	// 可热加载字段
	s.LogLevel = src.LogLevel
	s.LogFormat = src.LogFormat
	s.MaxPushBytes = src.MaxPushBytes
	s.MaxConcurrentPacks = src.MaxConcurrentPacks
	s.Credentials = src.Credentials
	// 镜像 fetch 超时/重试：下一次同步即生效（无需重启）
	s.MirrorStallTimeoutSec = src.MirrorStallTimeoutSec
	s.MirrorRetryAttempts = src.MirrorRetryAttempts
	s.MirrorRetryBaseDelaySec = src.MirrorRetryBaseDelaySec
	return restartNeeded
}

func (s *Setting) Output() string {
	out, err := json.MarshalIndent(s, "", "    ")
	if err != nil {
		log.Panic(err)
	}
	return string(out)
}

func (s *Setting) Reload() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.reloadLocked()
}

// HotReload 重新读取配置文件，应用可热加载字段（日志级别/格式、传输上限、凭据），
// 并返回需要重启才能生效的字段名列表。
func (s *Setting) HotReload() ([]string, error) {
	fresh := &Setting{}
	fresh.path = s.path
	if err := fresh.reloadLocked(); err != nil {
		return nil, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	return s.hotReloadFrom(fresh), nil
}

// reloadLocked 读取并校验配置（调用方须持有写锁）。
func (s *Setting) reloadLocked() error {
	data, err := os.ReadFile(s.path)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(data, s); err != nil {
		return err
	}
	if s.GitRoot == "" {
		return fmt.Errorf("gitRoot is required")
	}
	if s.WebUIPrefix == "" {
		s.WebUIPrefix = "__webui"
	}
	if s.WebUIPrefix == "api" || strings.Contains(s.WebUIPrefix, "..") {
		return fmt.Errorf("invalid webuiPrefix: %s", s.WebUIPrefix)
	}
	if s.WebUIAssets != "" {
		if info, err := os.Stat(s.WebUIAssets); err != nil || !info.IsDir() {
			return fmt.Errorf("webuiAssets dir not accessible: %s", s.WebUIAssets)
		}
	}
	if s.MaxPushBytes < 0 {
		return fmt.Errorf("maxPushBytes must be >= 0")
	}
	if s.MaxConcurrentPacks < 0 {
		return fmt.Errorf("maxConcurrentPacks must be >= 0")
	}
	if s.MirrorStallTimeoutSec < 0 {
		return fmt.Errorf("mirrorStallTimeoutSec must be >= 0")
	}
	if s.MirrorRetryAttempts < 0 {
		return fmt.Errorf("mirrorRetryAttempts must be >= 0")
	}
	if s.MirrorRetryBaseDelaySec < 0 {
		return fmt.Errorf("mirrorRetryBaseDelaySec must be >= 0")
	}
	// 传输上限注入 git 包（pgs → git 单向，避免循环依赖）
	// 注意：此处持有写锁，不能调用会取读锁的 LimitPushBytes（自死锁），直接读字段。
	pushLimit := s.MaxPushBytes
	if pushLimit <= 0 {
		pushLimit = DefaultMaxPushBytes
	}
	git.SetMaxReceivePackBytes(pushLimit)

	// 日志：初始化 slog（text/json）+ 把标准库 log 重定向到同一 handler
	debug, err := SetupLogging(s.LogFormat, s.LogLevel)
	if err != nil {
		return err
	}
	// git 包的逐对象 detail 日志跟随 debug 级（pgs → git 单向注入）
	if debug {
		git.SetLogLevel(git.LogDetail)
	} else {
		git.SetLogLevel(git.LogOff)
	}
	return nil
}

var Settings *Setting

func init() {
	workDir, _ := os.Getwd()
	gitRoot := filepath.Join(workDir, "repo")
	hostKey := filepath.Join(workDir, "repo", "hostkey")
	publicKey := filepath.Join(workDir, "repo", "key")
	Settings = &Setting{
		Listen:       "0.0.0.0:3000",
		EnableSSH:    true,
		SSHHostKey:   hostKey,
		SSHPublicKey: publicKey,
		GitRoot:      gitRoot,
		HttpAuth:     false,
		SSHAuthType:  "password",
		Credentials: map[string]string{
			"test": "123456",
		},
		WebUIPrefix: "__webui",
		WebUIAssets: "",
		LogFormat:   FormatText,
	}
}
