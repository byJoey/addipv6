package server

import (
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

const restoreUnitName = "addipv6-restore.service"

// Paths 是 restore 子命令要带的目录参数，写进 systemd unit 里。
type Paths struct {
	ConfigDir string
	StateDir  string
}

// SetPaths 告诉 Server 生成持久化配置时该带哪些参数。
func (s *Server) SetPaths(p Paths) { s.paths = p }

type persistResponse struct {
	OK     bool   `json:"ok"`
	Method string `json:"method"`
	Path   string `json:"path"`
	Detail string `json:"detail,omitempty"`
}

// handlePersist 装一个开机自动恢复的钩子，
// 让这些地址重启后还在，而不是像原来那样写 /tmp 一重启就丢。
func (s *Server) handlePersist(w http.ResponseWriter, r *http.Request) {
	if runtime.GOOS != "linux" {
		writeError(w, http.StatusBadRequest, "只有 Linux 能做持久化")
		return
	}
	exe, err := os.Executable()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "找不到自己的可执行文件路径: "+err.Error())
		return
	}
	exe, _ = filepath.EvalSymlinks(exe)

	args := []string{exe, "restore"}
	if s.paths.ConfigDir != "" {
		args = append(args, "--config-dir", s.paths.ConfigDir)
	}
	if s.paths.StateDir != "" {
		args = append(args, "--state-dir", s.paths.StateDir)
	}
	cmdline := strings.Join(args, " ")

	if hasSystemd() {
		path, err := writeSystemdUnit(cmdline)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		detail, err := enableUnit()
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error()+" "+detail)
			return
		}
		s.log.Info("已装好开机恢复服务", "unit", path)
		writeJSON(w, http.StatusOK, persistResponse{
			OK: true, Method: "systemd", Path: path,
			Detail: "开机会自动跑 " + restoreUnitName + " 把地址加回来",
		})
		return
	}

	path, err := writeRCLocal(cmdline)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, persistResponse{
		OK: true, Method: "rc.local", Path: path,
		Detail: "这台机器没有 systemd，已经写进 rc.local",
	})
}

func hasSystemd() bool {
	if _, err := os.Stat("/run/systemd/system"); err != nil {
		return false
	}
	_, err := exec.LookPath("systemctl")
	return err == nil
}

func writeSystemdUnit(cmdline string) (string, error) {
	unit := fmt.Sprintf(`[Unit]
Description=addipv6 开机恢复 IPv6 地址
Documentation=https://github.com/byJoey/addipv6
After=network-online.target
Wants=network-online.target

[Service]
Type=oneshot
RemainAfterExit=yes
ExecStart=%s
# 网卡偶尔比这个服务起得慢，失败了多试两次。
Restart=on-failure
RestartSec=5s

[Install]
WantedBy=multi-user.target
`, cmdline)

	path := filepath.Join("/etc/systemd/system", restoreUnitName)
	if err := os.WriteFile(path, []byte(unit), 0o644); err != nil {
		return "", fmt.Errorf("写 %s 失败: %w", path, err)
	}
	return path, nil
}

func enableUnit() (string, error) {
	if out, err := exec.Command("systemctl", "daemon-reload").CombinedOutput(); err != nil {
		return string(out), errors.New("systemctl daemon-reload 失败")
	}
	if out, err := exec.Command("systemctl", "enable", restoreUnitName).CombinedOutput(); err != nil {
		return string(out), errors.New("systemctl enable 失败")
	}
	return "", nil
}

func writeRCLocal(cmdline string) (string, error) {
	const path = "/etc/rc.local"
	body, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return "", err
	}
	text := string(body)
	if text == "" {
		text = "#!/bin/sh\n"
	}
	if strings.Contains(text, cmdline) {
		return path, nil
	}
	// exit 0 后面的内容不会执行，得插在它前面。
	line := cmdline + "\n"
	if idx := strings.LastIndex(text, "exit 0"); idx >= 0 {
		text = text[:idx] + line + text[idx:]
	} else {
		if !strings.HasSuffix(text, "\n") {
			text += "\n"
		}
		text += line
	}
	if err := os.WriteFile(path, []byte(text), 0o755); err != nil {
		return "", fmt.Errorf("写 %s 失败: %w", path, err)
	}
	return path, nil
}
