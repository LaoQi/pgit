package pgs

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
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
	WebUIPrefix string            `json:"webuiPrefix"`
	WebUIAssets string            `json:"webuiAssets"`
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
