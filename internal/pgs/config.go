package pgs

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"

	"pgit/internal/pgs/git"
)

type Setting struct {
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
	// MaxPushBytes 单次 push 请求体上限（字节），0 = 默认 2GiB。
	MaxPushBytes int64 `json:"maxPushBytes"`
	// MaxConcurrentPacks 同时进行的 pack 传输（clone/push/fetch 同步）上限，0 = 默认 4。
	MaxConcurrentPacks int `json:"maxConcurrentPacks"`
}

// 传输上限默认值。
const (
	DefaultMaxPushBytes       int64 = 2 << 30 // 2 GiB
	DefaultMaxConcurrentPacks       = 4
)

// LimitPushBytes 返回生效的单次 push 上限。
func (s *Setting) LimitPushBytes() int64 {
	if s == nil || s.MaxPushBytes <= 0 {
		return DefaultMaxPushBytes
	}
	return s.MaxPushBytes
}

// LimitConcurrentPacks 返回生效的并发 pack 传输上限。
func (s *Setting) LimitConcurrentPacks() int {
	if s == nil || s.MaxConcurrentPacks <= 0 {
		return DefaultMaxConcurrentPacks
	}
	return s.MaxConcurrentPacks
}

func (s *Setting) SetConfigPath(path string) {
	s.path = path
}

func (s *Setting) Output() string {
	out, err := json.MarshalIndent(s, "", "    ")
	if err != nil {
		log.Panic(err)
	}
	return string(out)
}

func (s *Setting) Reload() error {
	data, err := os.ReadFile(s.path)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(data, &s); err != nil {
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
	// 传输上限注入 git 包（pgs → git 单向，避免循环依赖）
	git.SetMaxReceivePackBytes(s.LimitPushBytes())

	// 日志级别注入 git 包（pgs → git 单向，避免循环依赖）
	switch s.LogLevel {
	case "", "off":
		git.SetLogLevel(git.LogOff)
	case "detail":
		git.SetLogLevel(git.LogDetail)
	default:
		return fmt.Errorf("invalid logLevel: %q (want off|detail)", s.LogLevel)
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
	}
}
