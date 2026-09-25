// Package store 负责配置与状态落盘。
// 两份文件都用「先写临时文件再 rename」的方式更新，中途断电不会留下半截 JSON。
package store

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	// 配置里有 Cloudflare Token，权限必须锁死。
	fileMode = 0o600
	dirMode  = 0o700
)

// CloudflareConfig 是 Cloudflare 侧的默认参数。
//
// AuthMode 为空表示老配置，按 API Token 处理，升级上来不用手动改。
type CloudflareConfig struct {
	AuthMode   string `json:"authMode,omitempty"`
	Email      string `json:"email,omitempty"`
	GlobalKey  string `json:"globalKey,omitempty"`
	Token      string `json:"token,omitempty"`
	ZoneID     string `json:"zoneId,omitempty"`
	ZoneName   string `json:"zoneName,omitempty"`
	RecordName string `json:"recordName,omitempty"`
	TTL        int    `json:"ttl,omitempty"`
	Proxied    bool   `json:"proxied"`
}

// Config 是落盘的配置。
type Config struct {
	Cloudflare CloudflareConfig `json:"cloudflare"`
	Iface      string           `json:"iface,omitempty"`
}

// ManagedAddr 是本工具加上去的一个地址，重启后靠它恢复。
type ManagedAddr struct {
	Iface     string    `json:"iface"`
	Prefix    string    `json:"prefix"`
	CreatedAt time.Time `json:"createdAt"`
}

// State 是运行状态。
type State struct {
	Managed []ManagedAddr `json:"managed"`
	// DefaultSource 记录每块网卡被指定的出口源地址，开机恢复时要重放。
	DefaultSource map[string]string `json:"defaultSource,omitempty"`
}

// Store 同时管配置和状态。所有方法并发安全。
type Store struct {
	mu        sync.RWMutex
	configDir string
	stateDir  string
	config    Config
	state     State
}

// candidateDirs 按优先级给出可写目录：系统级优先，没权限就退回用户目录。
func candidateDirs(sys string, userSub ...string) []string {
	out := []string{sys}
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		out = append(out, filepath.Join(append([]string{home}, userSub...)...))
	}
	out = append(out, filepath.Join(os.TempDir(), "addipv6"))
	return out
}

// pickWritable 挑第一个能建出来的目录。
func pickWritable(cands []string) (string, error) {
	var last error
	for _, d := range cands {
		if err := os.MkdirAll(d, dirMode); err != nil {
			last = err
			continue
		}
		probe := filepath.Join(d, ".wtest")
		if err := os.WriteFile(probe, []byte("x"), fileMode); err != nil {
			last = err
			continue
		}
		_ = os.Remove(probe)
		return d, nil
	}
	if last == nil {
		last = errors.New("没有候选目录")
	}
	return "", fmt.Errorf("找不到可写目录: %w", last)
}

// Open 打开（必要时创建）配置与状态文件。
// configDir / stateDir 传空就走默认位置。
func Open(configDir, stateDir string) (*Store, error) {
	var err error
	if configDir == "" {
		configDir, err = pickWritable(candidateDirs("/etc/addipv6", ".config", "addipv6"))
		if err != nil {
			return nil, err
		}
	} else if err = os.MkdirAll(configDir, dirMode); err != nil {
		return nil, err
	}
	if stateDir == "" {
		stateDir, err = pickWritable(candidateDirs("/var/lib/addipv6", ".local", "share", "addipv6"))
		if err != nil {
			return nil, err
		}
	} else if err = os.MkdirAll(stateDir, dirMode); err != nil {
		return nil, err
	}

	s := &Store{configDir: configDir, stateDir: stateDir}
	s.config = Config{Cloudflare: CloudflareConfig{TTL: 60}}
	s.state = State{DefaultSource: map[string]string{}}
	if err := readJSON(s.configPath(), &s.config); err != nil {
		return nil, fmt.Errorf("读取配置 %s 失败: %w", s.configPath(), err)
	}
	if err := readJSON(s.statePath(), &s.state); err != nil {
		return nil, fmt.Errorf("读取状态 %s 失败: %w", s.statePath(), err)
	}
	if s.state.DefaultSource == nil {
		s.state.DefaultSource = map[string]string{}
	}
	if s.config.Cloudflare.TTL <= 0 {
		s.config.Cloudflare.TTL = 60
	}
	return s, nil
}

func (s *Store) configPath() string { return filepath.Join(s.configDir, "config.json") }
func (s *Store) statePath() string  { return filepath.Join(s.stateDir, "state.json") }

// ConfigPath 给日志和界面展示用。
func (s *Store) ConfigPath() string { return s.configPath() }

// StatePath 给日志和界面展示用。
func (s *Store) StatePath() string { return s.statePath() }

func readJSON(path string, v any) error {
	b, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	if len(strings.TrimSpace(string(b))) == 0 {
		return nil
	}
	return json.Unmarshal(b, v)
}

// writeJSON 原子写：临时文件落盘 + fsync，再 rename 覆盖。
func writeJSON(path string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	b = append(b, '\n')
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer func() {
		tmp.Close()
		os.Remove(tmpName)
	}()
	if err := tmp.Chmod(fileMode); err != nil {
		return err
	}
	if _, err := tmp.Write(b); err != nil {
		return err
	}
	if err := tmp.Sync(); err != nil {
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}

// Config 返回配置副本。
func (s *Store) Config() Config {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.config
}

// UpdateConfig 就地改配置并落盘，fn 里拿到的是可写指针。
func (s *Store) UpdateConfig(fn func(*Config)) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	fn(&s.config)
	return writeJSON(s.configPath(), s.config)
}

// State 返回状态副本。
func (s *Store) State() State {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := State{DefaultSource: map[string]string{}}
	out.Managed = append(out.Managed, s.state.Managed...)
	for k, v := range s.state.DefaultSource {
		out.DefaultSource[k] = v
	}
	return out
}

// TrackAddr 记下一个本工具添加的地址，重复登记会被去重。
func (s *Store) TrackAddr(iface, prefix string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, m := range s.state.Managed {
		if m.Iface == iface && m.Prefix == prefix {
			return nil
		}
	}
	s.state.Managed = append(s.state.Managed, ManagedAddr{
		Iface:     iface,
		Prefix:    prefix,
		CreatedAt: time.Now().UTC(),
	})
	sort.SliceStable(s.state.Managed, func(i, j int) bool {
		if s.state.Managed[i].Iface != s.state.Managed[j].Iface {
			return s.state.Managed[i].Iface < s.state.Managed[j].Iface
		}
		return s.state.Managed[i].Prefix < s.state.Managed[j].Prefix
	})
	return writeJSON(s.statePath(), s.state)
}

// UntrackAddr 取消登记。
func (s *Store) UntrackAddr(iface, prefix string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := s.state.Managed[:0]
	for _, m := range s.state.Managed {
		if m.Iface == iface && m.Prefix == prefix {
			continue
		}
		out = append(out, m)
	}
	s.state.Managed = out
	return writeJSON(s.statePath(), s.state)
}

// IsManaged 判断某个地址是不是本工具加的。
func (s *Store) IsManaged(iface, prefix string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, m := range s.state.Managed {
		if m.Iface == iface && m.Prefix == prefix {
			return true
		}
	}
	return false
}

// SetDefaultSource 记录某块网卡选定的出口源地址。
func (s *Store) SetDefaultSource(iface, addr string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.state.DefaultSource == nil {
		s.state.DefaultSource = map[string]string{}
	}
	s.state.DefaultSource[iface] = addr
	return writeJSON(s.statePath(), s.state)
}
