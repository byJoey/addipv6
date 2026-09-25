// addipv6：批量给 VPS 加随机 IPv6，并把它们批量解析到 Cloudflare。
// 带 Web 界面，也可以用 restore 子命令在开机时把地址重新加回来。
package main

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/byJoey/addipv6/internal/netif"
	"github.com/byJoey/addipv6/internal/server"
	"github.com/byJoey/addipv6/internal/store"
)

// version 由构建时的 -ldflags 注入。
var version = "dev"

func main() {
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "restore":
			os.Exit(runRestore(os.Args[2:]))
		case "version", "-v", "--version":
			fmt.Println("addipv6", version, runtime.GOOS+"/"+runtime.GOARCH)
			return
		case "help", "-h", "--help":
			usage()
			return
		}
	}
	os.Exit(runServe(os.Args[1:]))
}

func usage() {
	fmt.Print(`addipv6 ` + version + `

用法：
  addipv6 [参数]           起 Web 界面
  addipv6 restore [参数]   把记录过的地址重新加回网卡（开机时用）
  addipv6 version          看版本

Web 界面常用参数：
  -listen   监听地址，默认 0.0.0.0:8688
  -password 登录密码，不给就自动生成一个并存到配置目录
  -tls-cert / -tls-key     给了就走 HTTPS

项目地址 https://github.com/byJoey/addipv6
`)
}

func newLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
}

// resolvePassword 按「命令行 > 环境变量 > 配置目录里存的 > 新生成」的顺序拿密码。
func resolvePassword(flagPw, configDir string, log *slog.Logger) (string, bool, error) {
	if flagPw != "" {
		return flagPw, false, nil
	}
	if env := strings.TrimSpace(os.Getenv("ADDIPV6_PASSWORD")); env != "" {
		return env, false, nil
	}
	path := filepath.Join(configDir, "password")
	if b, err := os.ReadFile(path); err == nil {
		if pw := strings.TrimSpace(string(b)); pw != "" {
			return pw, false, nil
		}
	}
	buf := make([]byte, 12)
	if _, err := rand.Read(buf); err != nil {
		return "", false, err
	}
	pw := strings.NewReplacer("+", "", "/", "", "=", "").Replace(base64.StdEncoding.EncodeToString(buf))
	if err := os.WriteFile(path, []byte(pw+"\n"), 0o600); err != nil {
		log.Warn("密码存不下来，这次的密码重启后就变了", "err", err)
		return pw, true, nil
	}
	return pw, true, nil
}

func runServe(args []string) int {
	fs := flag.NewFlagSet("addipv6", flag.ExitOnError)
	listen := fs.String("listen", "0.0.0.0:8688", "Web 界面监听地址")
	password := fs.String("password", "", "登录密码，留空则自动生成")
	configDir := fs.String("config-dir", "", "配置目录，默认 /etc/addipv6")
	stateDir := fs.String("state-dir", "", "状态目录，默认 /var/lib/addipv6")
	certFile := fs.String("tls-cert", "", "TLS 证书，配合 -tls-key 使用")
	keyFile := fs.String("tls-key", "", "TLS 私钥")
	restoreOnStart := fs.Bool("restore-on-start", false, "启动时先把记录过的地址加回来")
	fs.Usage = usage
	if err := fs.Parse(args); err != nil {
		return 2
	}

	log := newLogger()
	st, err := store.Open(*configDir, *stateDir)
	if err != nil {
		log.Error("打不开配置", "err", err)
		return 1
	}

	pw, generated, err := resolvePassword(*password, filepath.Dir(st.ConfigPath()), log)
	if err != nil {
		log.Error("生成密码失败", "err", err)
		return 1
	}

	mgr := netif.New()
	if mgr.Platform() != "linux" {
		log.Warn("当前不是 Linux，只能看界面，改不了真网卡", "os", runtime.GOOS)
	} else if os.Geteuid() != 0 {
		log.Warn("不是 root 在跑，加删地址会失败（需要 CAP_NET_ADMIN）")
	}

	useTLS := *certFile != "" && *keyFile != ""
	srv, err := server.New(server.Options{
		Manager: mgr, Store: st, Password: pw,
		Version: version, Secure: useTLS, Logger: log,
	})
	if err != nil {
		log.Error("初始化失败", "err", err)
		return 1
	}
	srv.SetPaths(server.Paths{ConfigDir: *configDir, StateDir: *stateDir})

	if *restoreOnStart {
		ok, bad := srv.Restore()
		log.Info("启动时恢复了一轮", "成功", ok, "失败", bad)
	}

	httpSrv := &http.Server{
		Addr:              *listen,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       60 * time.Second,
		// 批量操作可能跑好几分钟，写超时要给够。
		WriteTimeout: 15 * time.Minute,
		IdleTimeout:  2 * time.Minute,
		ErrorLog:     slog.NewLogLogger(log.Handler(), slog.LevelWarn),
	}

	ln, err := net.Listen("tcp", *listen)
	if err != nil {
		log.Error("端口监听失败", "addr", *listen, "err", err)
		return 1
	}

	scheme := "http"
	if useTLS {
		scheme = "https"
	}
	log.Info("起来了", "地址", scheme+"://"+displayAddr(*listen), "版本", version,
		"配置", st.ConfigPath(), "状态", st.StatePath())
	if generated {
		fmt.Fprintf(os.Stderr, "\n  登录密码: %s\n  （存在 %s，想换就改这个文件或者用 -password）\n\n",
			pw, filepath.Join(filepath.Dir(st.ConfigPath()), "password"))
	}

	errCh := make(chan error, 1)
	go func() {
		if useTLS {
			errCh <- httpSrv.ServeTLS(ln, *certFile, *keyFile)
			return
		}
		errCh <- httpSrv.Serve(ln)
	}()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	select {
	case err := <-errCh:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("服务挂了", "err", err)
			return 1
		}
	case sig := <-stop:
		log.Info("收到信号，准备退出", "signal", sig.String())
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := httpSrv.Shutdown(ctx); err != nil {
			log.Warn("优雅退出超时，直接关", "err", err)
			httpSrv.Close()
		}
	}
	return 0
}

// displayAddr 把 0.0.0.0 换成更好点的提示，方便直接复制到浏览器。
func displayAddr(addr string) string {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return addr
	}
	if host == "" || host == "0.0.0.0" || host == "::" {
		return "<服务器IP>:" + port
	}
	return addr
}

func runRestore(args []string) int {
	fs := flag.NewFlagSet("addipv6 restore", flag.ExitOnError)
	configDir := fs.String("config-dir", "", "配置目录")
	stateDir := fs.String("state-dir", "", "状态目录")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	log := newLogger()
	st, err := store.Open(*configDir, *stateDir)
	if err != nil {
		log.Error("打不开状态文件", "err", err)
		return 1
	}
	mgr := netif.New()
	if mgr.Platform() != "linux" {
		log.Error("只有 Linux 能恢复地址")
		return 1
	}
	// 这里只借 Server 的恢复逻辑，不开监听，所以密码随便给一个。
	srv, err := server.New(server.Options{
		Manager: mgr, Store: st, Password: "restore-only", Version: version, Logger: log,
	})
	if err != nil {
		log.Error("初始化失败", "err", err)
		return 1
	}
	ok, bad := srv.Restore()
	log.Info("恢复完成", "成功", ok, "失败", bad)
	if bad > 0 {
		return 1
	}
	return 0
}
