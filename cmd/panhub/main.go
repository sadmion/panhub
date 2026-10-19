// panhub：多网盘聚合挂载（WebDAV + 浏览 + STRM + 定时任务）。
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"panhub/internal/config"
	"panhub/internal/server"
	"panhub/internal/storage"
	"panhub/internal/store"
	"panhub/internal/task"
	"panhub/internal/vfs"
	"panhub/internal/webui"

	// 注册内置驱动：加新网盘就在这里加一行空导入
	_ "panhub/internal/storage/cloud139"
	_ "panhub/internal/storage/cloud189"
	_ "panhub/internal/storage/local"
	_ "panhub/internal/storage/webdav"
)

const version = "0.1.0"

func main() {
	cfgPath := flag.String("config", "config.yaml", "配置文件路径")
	flag.Parse()

	level := slog.LevelInfo
	if os.Getenv("PANHUB_DEBUG") != "" {
		level = slog.LevelDebug
	}
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: level})))

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		fatal(err)
	}
	if err := os.MkdirAll(cfg.Server.DataDir, 0o755); err != nil {
		fatal(err)
	}
	st, err := store.Open(filepath.Join(cfg.Server.DataDir, "state.json"))
	if err != nil {
		fatal(err)
	}
	if cfg.Server.PublicBaseURL == "" {
		cfg.Server.PublicBaseURL = guessBaseURL(cfg.Server.Addr)
	}

	v := vfs.New()
	seedFromConfig(v, st, cfg)
	loadFromState(v, st)

	runner := task.New(v, st, "panhub "+version, cfg.Server.PublicBaseURL)
	srv := server.New(cfg, v, st, runner, version, webui.FS())

	httpSrv := &http.Server{
		Addr:              cfg.Server.Addr,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 15 * time.Second,
	}

	runner.Start()

	go func() {
		slog.Info("panhub 启动",
			"version", version,
			"addr", cfg.Server.Addr,
			"base_url", cfg.Server.PublicBaseURL,
			"data_dir", cfg.Server.DataDir,
			"drivers", storage.Types(),
		)
		if err := httpSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			fatal(err)
		}
	}()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	<-stop

	slog.Info("正在退出…")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = httpSrv.Shutdown(ctx)
	runner.Stop()
}

func fatal(err error) {
	fmt.Fprintf(os.Stderr, "panhub: %v\n", err)
	os.Exit(1)
}

// seedFromConfig 把 config.yaml 里的挂载点写入 state（仅首次）。
func seedFromConfig(v *vfs.VFS, st *store.Store, cfg *config.Config) {
	existing := map[string]bool{}
	for _, m := range st.Mounts() {
		existing[m.Name] = true
	}
	for _, mc := range cfg.Mounts {
		if existing[mc.Name] {
			continue
		}
		drv, err := storage.New(context.Background(), mc.Type, mc.Config)
		if err != nil {
			slog.Warn("配置文件里的挂载点初始化失败，已跳过", "name", mc.Name, "type", mc.Type, "err", err)
			continue
		}
		_ = drv
		if err := st.UpsertMount(store.Mount{Name: mc.Name, Type: mc.Type, ReadOnly: mc.ReadOnly, Config: mc.Config}); err != nil {
			slog.Warn("写入挂载点失败", "name", mc.Name, "err", err)
		}
	}
}

// loadFromState 从 state.json 恢复所有挂载点。
func loadFromState(v *vfs.VFS, st *store.Store) {
	for _, m := range st.Mounts() {
		drv, err := storage.New(context.Background(), m.Type, m.Config)
		if err != nil {
			slog.Error("挂载点初始化失败", "name", m.Name, "type", m.Type, "err", err)
			continue
		}
		v.Add(&vfs.Mount{Name: m.Name, Type: m.Type, ReadOnly: m.ReadOnly, Driver: drv, Config: m.Config})
	}
}

// guessBaseURL 猜一个局域网可访问的地址，用于生成 STRM 里的播放链接。
func guessBaseURL(addr string) string {
	_, port, err := net.SplitHostPort(addr)
	if err != nil || port == "" {
		port = "5211"
	}
	ip := firstLANIP()
	if ip == "" {
		ip = "127.0.0.1"
	}
	return "http://" + net.JoinHostPort(ip, port)
}

func firstLANIP() string {
	ifaces, err := net.Interfaces()
	if err != nil {
		return ""
	}
	for _, iface := range ifaces {
		if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}
		for _, a := range addrs {
			var ip net.IP
			switch t := a.(type) {
			case *net.IPNet:
				ip = t.IP
			case *net.IPAddr:
				ip = t.IP
			}
			if ip == nil || ip.IsLoopback() {
				continue
			}
			if v4 := ip.To4(); v4 != nil && !strings.HasPrefix(v4.String(), "169.254.") {
				return v4.String()
			}
		}
	}
	return ""
}
