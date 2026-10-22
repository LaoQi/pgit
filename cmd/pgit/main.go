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

const Version = "1.1.0"

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
	restore := parser.String("", "restore", &argparse.Options{Default: "", Help: "restore a repository from a bundle snapshot, eg: `pgit -c config.json --restore /path/x.bundle --repo my-repo`"})
	restoreRepo := parser.String("", "repo", &argparse.Options{Default: "", Help: "target repository (canonical name or alias) for --restore"})
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

	// 归档恢复（离线运维入口）：--restore <bundle> --repo <name|alias>
	// 行为与 HTTP 的 POST /api/v1/repos/archives/restore 同一条代码路径：
	// 对象解包合并（幂等）+ 归档中的 refs 覆盖写回 + HEAD 指向归档记录的默认分支。
	if *restore != "" {
		if *restoreRepo == "" {
			fmt.Println("--restore requires --repo <name|alias>")
			os.Exit(ConfigError)
		}
		repo, err := pgs.ReposManager.Resolve(*restoreRepo)
		if err != nil {
			fmt.Printf("resolve repository %q failed: %s\n", *restoreRepo, err)
			os.Exit(Failed)
		}
		res, err := pgs.RestoreBundleToRepo(repo, *restore)
		if err != nil {
			fmt.Printf("restore failed: %s\n", err)
			os.Exit(Failed)
		}
		fmt.Printf("restored %s from %s: objects=%d refs=%v head=%s\n",
			repo.Name, *restore, res.Objects, res.Refs, res.Head)
		os.Exit(NoError)
	}

	syncMgr := pgs.NewSyncManager(pgs.ReposManager)
	relayMgr := pgs.NewRelayManager(pgs.ReposManager)
	for _, repo := range pgs.ReposManager.List() {
		if repo.IsMirror() {
			syncMgr.Register(repo)
		}
		if repo.IsRelay() {
			// 启动补推上次未完成的转发，并异步校准上游基线
			relayMgr.Bootstrap(repo)
		}
	}

	ln, err := net.Listen("tcp", pgs.Settings.Listen)
	if err != nil {
		slog.Error("listen failed", "listen", pgs.Settings.Listen, "error", err)
		os.Exit(Failed)
	}

	var sshHandler *server.SSHHandler
	if pgs.Settings.EnableSSH {
		sshHandler, err = server.NewSSHHandler(pgs.Settings.SSHHostKey, pgs.ReposManager, relayMgr)
		if err != nil {
			slog.Error("ssh handler init failed", "error", err)
		}
	}
	httpHandler := server.NewHTTPHandler(pgs.ReposManager, pgs.Settings, syncMgr, relayMgr)

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
		relayMgr.Stop()
		slog.Info("shutdown complete")
		os.Exit(NoError)
	}()

	// SIGHUP 热加载：日志级别/格式、传输上限/同步并发度、HTTP 凭据即时生效；
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
			// 同步任务并发度（mirrorMaxConcurrentSyncs）与中转转发并发度
			// （relayMaxConcurrentPushes）热生效
			syncMgr.SetConcurrency(pgs.Settings.LimitConcurrentSyncs())
			relayMgr.SetConcurrency(pgs.Settings.LimitConcurrentPushes())
			if len(restartNeeded) > 0 {
				slog.Warn("config fields require restart to take effect", "fields", restartNeeded)
			}
		}
	}()

	if err := mux.Serve(); err != nil {
		slog.Info("server stopped", "error", err)
	}
}
