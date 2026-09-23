package main

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/akamensky/argparse"

	"pgit/internal/pgs"
	"pgit/internal/pgs/server"
)

const Version = "1.0.0"

// shutdownGrace 是优雅关闭的最长等待时间（超过则强制断开残留连接）。
const shutdownGrace = 30 * time.Second

const (
	NoError = iota
	Failed
	ConfigError
	EnvError
)

func ensureGitRoot() error {
	if err := os.MkdirAll(pgs.Settings.GitRoot, os.ModePerm); err != nil {
		return fmt.Errorf("create gitRoot %s failed: %w", pgs.Settings.GitRoot, err)
	}
	return nil
}

func main() {
	parser := argparse.NewParser("pgit", "Personal git server")
	config := parser.String("c", "config", &argparse.Options{Default: "", Help: "config file"})
	version := parser.Flag("v", "version", &argparse.Options{Help: "show version"})
	output := parser.Flag("d", "default", &argparse.Options{Help: "print default config, eg: `pgit -d > config.json`"})
	exportWeb := parser.String("w", "export-webui", &argparse.Options{Default: "", Help: "export embedded webui files to dir, eg: `pgit -w ./webui`"})
	err := parser.Parse(os.Args)
	if err != nil {
		fmt.Print(parser.Usage(err))
		os.Exit(Failed)
	}

	if *version {
		fmt.Printf("pgit version %s\n", Version)
		os.Exit(NoError)
	}
	if *output {
		fmt.Print(pgs.Settings.Output())
		os.Exit(NoError)
	}
	if *exportWeb != "" {
		if err := server.ExportWebUI(*exportWeb); err != nil {
			fmt.Printf("export webui failed: %s\n", err)
			os.Exit(Failed)
		}
		fmt.Printf("webui files exported to %s\n", *exportWeb)
		os.Exit(NoError)
	}
	if *config == "" {
		fmt.Println("Need config file, Run `pgit -h` for help")
		os.Exit(ConfigError)
	}

	pgs.Settings.SetConfigPath(*config)
	if err := pgs.Settings.Reload(); err != nil {
		fmt.Printf("Config parse error: %s\n", err)
		os.Exit(ConfigError)
	}
	if err := ensureGitRoot(); err != nil {
		fmt.Println(err.Error())
		os.Exit(EnvError)
	}

	pgs.InitReposManager(&pgs.RepositoriesManagerConfig{GitRoot: pgs.Settings.GitRoot})

	syncMgr := pgs.NewSyncManager(pgs.ReposManager)
	for _, repo := range pgs.ReposManager.List() {
		if repo.IsMirror() {
			syncMgr.Register(repo)
		}
	}

	ln, err := net.Listen("tcp", pgs.Settings.Listen)
	if err != nil {
		slog.Error("listen failed", "listen", pgs.Settings.Listen, "error", err)
		os.Exit(Failed)
	}

	var sshHandler *server.SSHHandler
	if pgs.Settings.EnableSSH {
		sshHandler, err = server.NewSSHHandler(pgs.Settings.SSHHostKey, pgs.ReposManager)
		if err != nil {
			slog.Error("ssh handler init failed", "error", err)
		}
	}
	httpHandler := server.NewHTTPHandler(pgs.ReposManager, pgs.Settings, syncMgr)

	mux := server.NewMuxServer(ln, pgs.Settings.EnableSSH, sshHandler, httpHandler)
	slog.Info("pgit listening", "listen", pgs.Settings.Listen, "ssh", pgs.Settings.EnableSSH, "version", Version)

	go func() {
		sigCh := make(chan os.Signal, 1)
		signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
		<-sigCh
		slog.Info("shutting down")

		// 优雅关闭：停止接受新连接，等待活动请求/SSH 会话结束（最多 shutdownGrace），
		// 再停止定时同步，最后退出。避免硬切进行中的 push/clone。
		ctx, cancel := context.WithTimeout(context.Background(), shutdownGrace)
		defer cancel()
		if err := mux.Shutdown(ctx); err != nil {
			slog.Warn("graceful shutdown incomplete", "error", err)
		}
		syncMgr.Stop()
		slog.Info("shutdown complete")
		os.Exit(NoError)
	}()

	// SIGHUP 热加载：日志级别/格式、传输上限、HTTP 凭据即时生效；
	// 监听地址/SSH/gitRoot/webui 等需重启的字段会被忽略并提示。
	go func() {
		hupCh := make(chan os.Signal, 1)
		signal.Notify(hupCh, syscall.SIGHUP)
		for range hupCh {
			restartNeeded, err := pgs.Settings.HotReload()
			if err != nil {
				slog.Error("config reload failed", "path", *config, "error", err)
				continue
			}
			slog.Info("config reloaded", "path", *config)
			if len(restartNeeded) > 0 {
				slog.Warn("config fields require restart to take effect", "fields", restartNeeded)
			}
		}
	}()

	if err := mux.Serve(); err != nil {
		slog.Info("server stopped", "error", err)
	}
}
