package main

import (
	"fmt"
	"log"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"syscall"

	"github.com/akamensky/argparse"

	"pgit/internal/pgs"
	"pgit/internal/pgs/server"
)

const Version = "1.0.0"

const (
	NoError = iota
	Failed
	ConfigError
	EnvError
)

func checkEnv() error {
	if _, err := exec.LookPath("git"); err != nil {
		return fmt.Errorf("git command not found, install git")
	}
	return nil
}

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

	if err := checkEnv(); err != nil {
		fmt.Println(err.Error())
		os.Exit(EnvError)
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

	ln, err := net.Listen("tcp", pgs.Settings.Listen)
	if err != nil {
		log.Panicf("listen %s failed: %v", pgs.Settings.Listen, err)
	}

	var sshHandler *server.SSHHandler
	if pgs.Settings.EnableSSH {
		sshHandler, err = server.NewSSHHandler(pgs.Settings.SSHHostKey, pgs.Settings.GitRoot, pgs.ReposManager)
		if err != nil {
			log.Printf("SSH handler init failed: %v", err)
		}
	}
	httpHandler := server.NewHTTPHandler(pgs.ReposManager, pgs.Settings)

	mux := server.NewMuxServer(ln, pgs.Settings.EnableSSH, sshHandler, httpHandler)
	log.Printf("pgit listening on %s (SSH: %v)", pgs.Settings.Listen, pgs.Settings.EnableSSH)

	go func() {
		sigCh := make(chan os.Signal, 1)
		signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
		<-sigCh
		log.Printf("shutting down...")
		_ = ln.Close()
		os.Exit(NoError)
	}()

	if err := mux.Serve(); err != nil {
		log.Printf("server stopped: %v", err)
	}
}
