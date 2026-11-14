// Package config 负责配置的加载、校验与落盘。
//
// 配置结构与 All In（Java 版）的 config.json 保持兼容，这样原来的配置可以直接搬过来：
//
//	{
//	  "server":    {"host":"127.0.0.1","port":3425,"authKey":"mp-..."},
//	  "providers": [{...}],
//	  "routing":   {"groupsEnabled":false,"bareModelFallback":false},
//	  "usage":     {...}
//	}
package config

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// Server 是监听与鉴权设置。
type Server struct {
	Host    string `json:"host"`
	Port    int    `json:"port"`
	AuthKey string `json:"authKey"`
	// AuthKeyOrigin 记录口令来源：first-run-random 表示首次启动自动生成。
	AuthKeyOrigin string `json:"authKeyOrigin,omitempty"`
}

// Account 是某个供应商下的一个账号（可能是 API Key，也可能是登录态）。
type Account struct {
	ID      string `json:"id"`
	Name    string `json:"name,omitempty"`
	Type    string `json:"type,omitempty"` // token / api / oauth
	Enabled *bool  `json:"enabled,omitempty"`
	Access  string `json:"access,omitempty"`  // 访问令牌 / API Key
	Refresh string `json:"refresh,omitempty"` // 刷新令牌
	UID     string `json:"uid,omitempty"`
	Domain  string `json:"domain,omitempty"`
	Realm   string `json:"realm,omitempty"` // cn / global
	// Email 是账号邮箱：Qoder 的 COSY 信封里要带一份（展示用，可留空）。
	Email   string `json:"email,omitempty"`
	Expires int64  `json:"expires,omitempty"`
	AddedAt int64  `json:"addedAt,omitempty"`
	// 签到记录（WorkBuddy 等支持签到的供应商使用）
	CheckinDate    string `json:"checkinDate,omitempty"`
	CheckinOutcome string `json:"checkinOutcome,omitempty"`
	CheckinCredit  int    `json:"checkinCredit,omitempty"`
	CheckinStreak  int    `json:"checkinStreak,omitempty"`
	// 任务中心记录（WorkBuddy 任务自动化使用）。
	// LastTaskRun 是最近一次跑全套的时间（Unix 秒）；TasksRun 是那次的全套结果，
	// 键为任务名（report/checkin/streak/claim-gift/compensation/makeup/buddy/
	// travel-depart/travel-claim/lottery），值为 ok/already/skipped/locked/failed。
	LastTaskRun int64             `json:"lastTaskRun,omitempty"`
	TasksRun    map[string]string `json:"tasksRun,omitempty"`
	// TaskOutcomeDetail 保存每项任务最近一次的说明文字（上游 msg 或本地判定）。
	TaskOutcomeDetail map[string]string `json:"taskOutcomeDetail,omitempty"`
	// 设备身份（Trae 这类要求设备证明的供应商使用）
	DeviceID   string `json:"deviceId,omitempty"`
	MachineID  string `json:"machineId,omitempty"`
	PublicKey  string `json:"publicKey,omitempty"`
	PrivateKey string `json:"privateKey,omitempty"` // 敏感：不要外发
}

// IsEnabled 默认启用。
func (a *Account) IsEnabled() bool {
	return a == nil || a.Enabled == nil || *a.Enabled
}

// Key 返回可用的凭据：优先 access，其次 refresh。
func (a *Account) Key() string {
	if a == nil {
		return ""
	}
	if a.Access != "" {
		return a.Access
	}
	return a.Refresh
}

// Provider 是一个上游供应商。
type Provider struct {
	ID       string     `json:"id"`
	Name     string     `json:"name,omitempty"`
	Enabled  *bool      `json:"enabled,omitempty"`
	BaseURL  string     `json:"baseUrl,omitempty"`
	Kind     string     `json:"kind,omitempty"` // openai / anthropic / workbuddy / deepseek-web ...
	APIKey   string     `json:"apiKey,omitempty"`
	Accounts []*Account `json:"accounts,omitempty"`
	Models   []string   `json:"models,omitempty"`
	// AutoSyncModels 为真时，网关启动后在后台同步一次模型目录（只追加不覆盖，
	// 口径同 POST /api/providers/sync）。默认 false：目录同步要打上游，不该默认消耗。
	AutoSyncModels bool `json:"autoSyncModels,omitempty"`
	// Headers 是发往上游时附加的固定头（个别供应商需要 X-Domain 之类）。
	Headers map[string]string `json:"headers,omitempty"`
	// HeaderPrefix 供特殊供应商在运行时用账号信息拼头。
	Notes string `json:"notes,omitempty"`
}

// IsEnabled 默认启用。
func (p *Provider) IsEnabled() bool {
	return p == nil || p.Enabled == nil || *p.Enabled
}

// Group 是路由组：按顺序挑目标。
type Group struct {
	ID       string   `json:"id"`
	Name     string   `json:"name,omitempty"`
	Strategy string   `json:"strategy,omitempty"` // order / round-robin
	Targets  []string `json:"targets,omitempty"`  // provider/model
}

// Routing 路由开关。
type Routing struct {
	GroupsEnabled     bool `json:"groupsEnabled"`
	BareModelFallback bool `json:"bareModelFallback"`
}

// PromptConfig 是系统提示词注入配置。
//
//	mode=passthrough  原样转发（默认）
//	mode=inject       在客户端提示词前面插一段（压过它）
//	mode=replace      丢掉客户端自带的，只用自己的
type PromptConfig struct {
	Mode   string `json:"mode,omitempty"`
	File   string `json:"file,omitempty"`
	Inline string `json:"inline,omitempty"`
}

// ReasoningConfig 是思考强度（reasoning effort）配置。
//
//	mode=auto   客户端带了就用客户端的，没带用 effort
//	mode=force  无条件用 effort，覆盖客户端
type ReasoningConfig struct {
	Mode   string `json:"mode,omitempty"`
	Effort string `json:"effort,omitempty"`
}

// APIKey 是发给客户端的调用凭据（可与主网关口令并存）。
type APIKey struct {
	ID        string `json:"id"`
	Note      string `json:"note,omitempty"`
	Key       string `json:"key"`
	Enabled   *bool  `json:"enabled,omitempty"`
	CreatedAt int64  `json:"createdAt,omitempty"`
}

// IsEnabled 默认启用。
func (k *APIKey) IsEnabled() bool {
	return k == nil || k.Enabled == nil || *k.Enabled
}

// Config 是完整配置。
type Config struct {
	Server    Server      `json:"server"`
	Providers []*Provider `json:"providers"`
	Groups    []*Group    `json:"groups,omitempty"`
	APIKeys   []*APIKey   `json:"apiKeys,omitempty"`
	Routing   Routing     `json:"routing"`
	// Prompt 是网关级系统提示词注入配置（压过客户端自带的 system）
	Prompt PromptConfig `json:"prompt,omitempty"`
	// Reasoning 是思考强度默认值
	Reasoning ReasoningConfig `json:"reasoning,omitempty"`
	// Setup 记录是否完成首次向导，供面板判断。
	Setup struct {
		Done   bool  `json:"done"`
		DoneAt int64 `json:"doneAt,omitempty"`
	} `json:"setup"`
}

// Store 管理配置文件的读写，所有写操作串行化。
type Store struct {
	mu   sync.RWMutex
	path string
	cfg  *Config
}

// NewStore 打开（必要时创建）配置文件。
func NewStore(path string) (*Store, error) {
	s := &Store{path: path}
	if err := s.load(); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *Store) load() error {
	raw, err := os.ReadFile(s.path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	cfg := &Config{}
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, cfg); err != nil {
			return fmt.Errorf("配置解析失败: %w", err)
		}
	}
	if cfg.Server.Port == 0 {
		cfg.Server.Port = 3425
	}
	if cfg.Server.Host == "" {
		// 默认只监听回环：这个服务能代理到付费账号，绝不该默认暴露到局域网
		cfg.Server.Host = "127.0.0.1"
	}
	firstRun := cfg.Server.AuthKey == ""
	if firstRun {
		cfg.Server.AuthKey = generateKey()
		cfg.Server.AuthKeyOrigin = "first-run-random"
	}
	s.cfg = cfg
	if len(raw) == 0 || firstRun {
		if err := s.saveLocked(); err != nil {
			return err
		}
	}
	return nil
}

// generateKey 生成 mp- 前缀的 32 字节随机口令。
func generateKey() string {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		panic("crypto/rand 不可用: " + err.Error())
	}
	return "mp-" + hex.EncodeToString(buf)
}

// Snapshot 返回一份深拷贝，调用方可以随便改。
func (s *Store) Snapshot() *Config {
	s.mu.RLock()
	defer s.mu.RUnlock()
	clone, _ := json.Marshal(s.cfg)
	out := &Config{}
	_ = json.Unmarshal(clone, out)
	return out
}

// Path 返回配置文件路径。
func (s *Store) Path() string { return s.path }

// Save 落盘（先写临时文件再 rename，避免半截文件）。
func (s *Store) Save(cfg *Config) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cfg = cfg
	return s.saveLocked()
}

func (s *Store) saveLocked() error {
	raw, err := json.MarshalIndent(s.cfg, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}

// FindProvider 按 id 找供应商。
func (c *Config) FindProvider(id string) *Provider {
	for _, p := range c.Providers {
		if p != nil && p.ID == id {
			return p
		}
	}
	return nil
}

// FindGroup 按 id 找路由组。
func (c *Config) FindGroup(id string) *Group {
	for _, g := range c.Groups {
		if g != nil && g.ID == id {
			return g
		}
	}
	return nil
}

// SplitModel 把 "provider/model" 拆开。
func SplitModel(model string) (providerID, modelName string, ok bool) {
	i := strings.IndexByte(model, '/')
	if i <= 0 || i == len(model)-1 {
		return "", "", false
	}
	return model[:i], model[i+1:], true
}

// Redacted 返回脱敏副本，供管理接口展示。
func (c *Config) Redacted() *Config {
	clone := c
	out := *clone
	out.Server = clone.Server
	if out.Server.AuthKey != "" {
		out.Server.AuthKey = mask(out.Server.AuthKey)
	}
	out.Providers = make([]*Provider, 0, len(clone.Providers))
	for _, p := range clone.Providers {
		if p == nil {
			continue
		}
		cp := *p
		cp.APIKey = mask(cp.APIKey)
		cp.Accounts = make([]*Account, 0, len(p.Accounts))
		for _, a := range p.Accounts {
			if a == nil {
				continue
			}
			ca := *a
			ca.Access = mask(ca.Access)
			ca.Refresh = mask(ca.Refresh)
			cp.Accounts = append(cp.Accounts, &ca)
		}
		out.Providers = append(out.Providers, &cp)
	}
	return &out
}

func mask(v string) string {
	if v == "" {
		return ""
	}
	if len(v) <= 10 {
		return "***"
	}
	return v[:6] + "..." + v[len(v)-4:]
}
