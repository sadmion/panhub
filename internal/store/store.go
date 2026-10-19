// Package store 把「挂载点 / 任务 / 流媒体密钥」持久化成一个 JSON 文件。
// 之所以不引数据库：单文件方便备份，也方便你直接改。
package store

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Mount 是持久化的挂载点定义。
type Mount struct {
	Name     string         `json:"name"`
	Type     string         `json:"type"`
	ReadOnly bool           `json:"read_only"`
	Config   map[string]any `json:"config"`
}

// Task 是持久化的任务定义与最近一次执行结果。
type Task struct {
	ID         string    `json:"id"`
	Name       string    `json:"name"`
	Type       string    `json:"type"`
	Mount      string    `json:"mount"`
	Dir        string    `json:"dir"`
	Target     string    `json:"target"`
	Mode       string    `json:"mode"`
	Interval   string    `json:"interval"`
	Enabled    bool      `json:"enabled"`
	LastRun    time.Time `json:"last_run"`
	LastResult string    `json:"last_result"`
	LastError  string    `json:"last_error"`
	LastCount  int       `json:"last_count"`
	// LastTarget 是上次实际写入的目录（解析成绝对路径后）。
	// 容器部署时这里显示的是容器内路径，配 LastSample 一起看最直观。
	LastTarget string `json:"last_target"`
	// LastSample 是上次写入的一个示例文件路径。
	LastSample string `json:"last_sample"`
}

// State 是落盘的全部状态。
type State struct {
	StreamKey string  `json:"stream_key"`
	Mounts    []Mount `json:"mounts"`
	Tasks     []Task  `json:"tasks"`
}

// Store 提供并发安全的状态读写。
type Store struct {
	mu   sync.Mutex
	path string
	st   State
}

// Open 加载状态文件，不存在则初始化。
func Open(path string) (*Store, error) {
	s := &Store{path: path}
	raw, err := os.ReadFile(path)
	switch {
	case err == nil:
		if err := json.Unmarshal(raw, &s.st); err != nil {
			return nil, err
		}
	case os.IsNotExist(err):
		// 首次启动
	default:
		return nil, err
	}
	if s.st.StreamKey == "" {
		s.st.StreamKey = randomHex(16)
	}
	if s.st.Mounts == nil {
		s.st.Mounts = []Mount{}
	}
	if s.st.Tasks == nil {
		s.st.Tasks = []Task{}
	}
	// 补齐缺失的任务 ID
	for i := range s.st.Tasks {
		if s.st.Tasks[i].ID == "" {
			s.st.Tasks[i].ID = randomHex(6)
		}
	}
	return s, s.save()
}

func randomHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "fallback" + time.Now().Format("150405")
	}
	return hex.EncodeToString(b)
}

// Save 原子写盘。
func (s *Store) Save() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.save()
}

func (s *Store) save() error {
	if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(s.st, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}

// Snapshot 返回深拷贝快照。
func (s *Store) Snapshot() State {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := State{StreamKey: s.st.StreamKey}
	out.Mounts = append(out.Mounts, s.st.Mounts...)
	out.Tasks = append(out.Tasks, s.st.Tasks...)
	return out
}

// StreamKey 返回流媒体访问密钥。
func (s *Store) StreamKey() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.st.StreamKey
}

// Mounts 返回挂载点副本。
func (s *Store) Mounts() []Mount {
	return s.Snapshot().Mounts
}

// Tasks 返回任务副本。
func (s *Store) Tasks() []Task {
	return s.Snapshot().Tasks
}

// Tag 生成新任务 ID。
func NewTaskID() string { return randomHex(6) }

// UpsertMount 新增或按名字更新挂载点。
func (s *Store) UpsertMount(m Mount) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if m.Config == nil {
		m.Config = map[string]any{}
	}
	for i := range s.st.Mounts {
		if s.st.Mounts[i].Name == m.Name {
			s.st.Mounts[i] = m
			return s.save()
		}
	}
	s.st.Mounts = append(s.st.Mounts, m)
	return s.save()
}

// DeleteMount 删除挂载点。
func (s *Store) DeleteMount(name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := s.st.Mounts[:0]
	found := false
	for _, m := range s.st.Mounts {
		if m.Name == name {
			found = true
			continue
		}
		out = append(out, m)
	}
	if !found {
		return errors.New("mount not found")
	}
	s.st.Mounts = out
	return s.save()
}

// UpsertTask 新增或按 ID 更新任务。
func (s *Store) UpsertTask(t Task) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if t.ID == "" {
		t.ID = randomHex(6)
	}
	for i := range s.st.Tasks {
		if s.st.Tasks[i].ID == t.ID {
			s.st.Tasks[i] = t
			return s.save()
		}
	}
	s.st.Tasks = append(s.st.Tasks, t)
	return s.save()
}

// DeleteTask 删除任务。
func (s *Store) DeleteTask(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := s.st.Tasks[:0]
	found := false
	for _, t := range s.st.Tasks {
		if t.ID == id {
			found = true
			continue
		}
		out = append(out, t)
	}
	if !found {
		return errors.New("task not found")
	}
	s.st.Tasks = out
	return s.save()
}

// RunInfo 是一次任务执行的可观测信息（用于界面展示「到底写到哪」）。
type RunInfo struct {
	Target string // 实际使用的输出目录（绝对路径）
	Sample string // 一个已写入文件的路径示例
}

// RecordTaskRun 记录任务执行结果。
func (s *Store) RecordTaskRun(id, result, errMsg string, count int, info RunInfo) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.st.Tasks {
		if s.st.Tasks[i].ID == id {
			s.st.Tasks[i].LastRun = time.Now()
			s.st.Tasks[i].LastResult = result
			s.st.Tasks[i].LastError = errMsg
			s.st.Tasks[i].LastCount = count
			s.st.Tasks[i].LastTarget = info.Target
			s.st.Tasks[i].LastSample = info.Sample
			return s.save()
		}
	}
	return nil
}

// GetTask 按 ID 取任务。
func (s *Store) GetTask(id string) (Task, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, t := range s.st.Tasks {
		if t.ID == id {
			return t, true
		}
	}
	return Task{}, false
}
