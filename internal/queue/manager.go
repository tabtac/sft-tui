package queue

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"sft-tui/internal/protocol"
)

// Operation 传输控制句柄，支持暂停与取消请求
type Operation struct {
	ctx    context.Context
	cancel context.CancelFunc
	mu     sync.Mutex
	mode   protocol.CancelMode
}

func NewOperation(parent context.Context) *Operation {
	ctx, cancel := context.WithCancel(parent)
	return &Operation{ctx: ctx, cancel: cancel}
}

func (o *Operation) Request(mode protocol.CancelMode) error {
	if mode != protocol.CancelPause && mode != protocol.CancelAbort {
		return fmt.Errorf("非法任务控制模式")
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.mode != 0 && o.mode != mode {
		return fmt.Errorf("已有控制请求等待确认")
	}
	o.mode = mode
	o.cancel()
	return nil
}

func (o *Operation) Mode() protocol.CancelMode {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.mode
}

func (o *Operation) Context() context.Context { return o.ctx }
func (o *Operation) Close()                   { o.cancel() }

type sample struct {
	at    time.Time
	bytes uint64
}

type running struct {
	op      *Operation
	samples []sample
}

// Config 队列配置
type Config struct {
	StateDir     string
	Identity     string
	MaxFileBytes uint64
	MaxStreams   uint16
}

// Manager 客户端任务队列管理器
type Manager struct {
	cfg          Config
	identity     string
	store        *Store
	ctx          context.Context
	cancel       context.CancelFunc
	mu           sync.Mutex
	tasks        map[string]*Task
	history      []Task
	active       map[string]*running
	hold, closed bool
	storageErr   error
	notify       chan struct{}
	loopDone     chan struct{}
	wg           sync.WaitGroup
	closeOnce    sync.Once
	closeErr     error
	currentView  atomic.Pointer[[]Task]
	historyView  atomic.Pointer[[]Task]
	currentDirty bool
	historyDirty bool
	activeCount  atomic.Int64
	nextPrune    time.Time
	runner       TaskRunner
}

func randomID() (string, error) {
	var b [16]byte
	if _, err := io.ReadFull(rand.Reader, b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

func (m *Manager) signal() {
	select {
	case m.notify <- struct{}{}:
	default:
	}
}

// Open 初始化并打开队列管理器
func Open(cfg Config) (*Manager, error) {
	if cfg.StateDir == "" {
		return nil, fmt.Errorf("state directory required")
	}
	if cfg.Identity == "" {
		return nil, fmt.Errorf("identity required")
	}
	if cfg.MaxStreams == 0 {
		cfg.MaxStreams = 3
	}

	s, err := OpenStore(cfg.StateDir, cfg.Identity)
	if err != nil {
		return nil, err
	}

	fail := func(e error) (*Manager, error) {
		s.Close()
		return nil, e
	}

	tasks, err := s.Load(false)
	if err != nil {
		return fail(err)
	}
	history, err := s.Load(true)
	if err != nil {
		return fail(err)
	}

	for _, t := range history {
		if t.Identity != cfg.Identity || protocol.ValidateName(t.Remote) != nil || !filepath.IsAbs(t.Path) {
			return fail(fmt.Errorf("history record does not match identity or path is invalid"))
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	m := &Manager{
		cfg:      cfg,
		identity: cfg.Identity,
		store:    s,
		ctx:      ctx,
		cancel:   cancel,
		tasks:    make(map[string]*Task),
		history:  history,
		active:   make(map[string]*running),
		notify:   make(chan struct{}, 1),
		loopDone: make(chan struct{}),
	}

	for _, t := range tasks {
		if t.Identity != cfg.Identity || protocol.ValidateName(t.Remote) != nil || !filepath.IsAbs(t.Path) {
			cancel()
			return fail(fmt.Errorf("active task record does not match identity or path is invalid"))
		}
		key, err := StateKeyForIdentity(cfg.Identity, t.Direction, t.Remote, t.Path)
		if err != nil || key != t.StateKey || (t.Direction != protocol.DirectionUpload && t.Direction != protocol.DirectionDownload) {
			cancel()
			return fail(fmt.Errorf("队列检查点索引不符"))
		}
		if t.State.Active() {
			t.State = Paused
			t.Error = "上次退出时任务未完成，请选择继续"
		}
		if t.WantCancel {
			t.State = Pending
			t.Error = "上次取消尚未完成，请重试取消"
		}
		taskCopy := t
		m.tasks[t.ID] = &taskCopy
	}

	var restored []Task
	for _, t := range m.tasks {
		restored = append(restored, *t)
	}
	if err := s.Write(restored, false); err != nil {
		cancel()
		return fail(err)
	}

	m.currentDirty = true
	m.historyDirty = true
	m.pruneRecords(time.Now())

	go m.loop()
	return m, nil
}

// SetRunner 设置传输执行器
func (m *Manager) SetRunner(r TaskRunner) {
	m.mu.Lock()
	m.runner = r
	m.mu.Unlock()
	m.signal()
}

// Enqueue 批次加入传输任务
func (m *Manager) Enqueue(specs []Spec) ([]Task, error) {
	if len(specs) == 0 {
		return nil, fmt.Errorf("未选择文件")
	}
	if len(specs) > MaxBatchTasks {
		return nil, fmt.Errorf("单批最多 %d 个文件", MaxBatchTasks)
	}

	batch, err := randomID()
	if err != nil {
		return nil, err
	}

	var tasks []Task
	for _, sp := range specs {
		if sp.Direction != protocol.DirectionUpload && sp.Direction != protocol.DirectionDownload {
			return nil, fmt.Errorf("方向无效")
		}
		if err := protocol.ValidateName(sp.Remote); err != nil {
			return nil, err
		}
		absPath, err := filepath.Abs(sp.Path)
		if err != nil {
			return nil, err
		}
		id, err := randomID()
		if err != nil {
			return nil, err
		}
		key, err := StateKeyForIdentity(m.identity, sp.Direction, sp.Remote, absPath)
		if err != nil {
			return nil, err
		}
		now := time.Now()
		t := Task{
			ID:        id,
			Batch:     batch,
			Identity:  m.identity,
			StateKey:  key,
			Direction: sp.Direction,
			Remote:    sp.Remote,
			Path:      absPath,
			State:     Waiting,
			Size:      sp.Size,
			Created:   now,
			Updated:   now,
		}
		if sp.Skip != "" {
			t.State = Skipped
			t.Error = sp.Skip
		} else if err := m.preflight(&t); err != nil {
			t.State = Pending
			t.Error = err.Error()
		}

		tasks = append(tasks, t)
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	if m.closed {
		return nil, io.ErrClosedPipe
	}
	if m.storageErr != nil {
		return nil, m.storageErr
	}

	unfinished := 0
	for _, old := range m.tasks {
		if !old.Terminal() {
			unfinished++
		}
	}
	added := 0
	for _, t := range tasks {
		if t.State != Skipped {
			added++
		}
	}
	if unfinished+added > MaxUnfinishedTasks {
		return nil, fmt.Errorf("未完成队列最多 %d 项，请先处理已有任务", MaxUnfinishedTasks)
	}

	existing := make(map[string]bool, unfinished)
	for _, old := range m.tasks {
		if !old.Terminal() {
			existing[TargetKey(old.Direction, old.Remote, old.Path)] = true
		}
	}

	seen := make(map[string]bool, added)
	for _, t := range tasks {
		if t.State == Skipped {
			continue
		}
		key := TargetKey(t.Direction, t.Remote, t.Path)
		target := t.Remote
		if t.Direction == protocol.DirectionDownload {
			target = t.Path
		}
		if seen[key] {
			return nil, fmt.Errorf("同批目标重复：%s", target)
		}
		seen[key] = true
		if existing[key] {
			return nil, fmt.Errorf("目标已有未完成任务，请继续原任务：%s", target)
		}
	}

	if err := m.saveRecords(tasks, false); err != nil {
		return nil, err
	}

	for _, t := range tasks {
		taskCopy := t
		m.tasks[t.ID] = &taskCopy
		if t.State == Skipped {
			m.history = append([]Task{t}, m.history...)
			m.historyDirty = true
		}
	}

	m.pruneRecords(time.Now())
	m.hold = false
	m.signal()
	return tasks, nil
}

func (m *Manager) preflight(t *Task) error {
	if t.Direction == protocol.DirectionUpload {
		info, err := os.Stat(t.Path)
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() || info.Size() < 0 || (m.cfg.MaxFileBytes > 0 && uint64(info.Size()) > m.cfg.MaxFileBytes) {
			return fmt.Errorf("上传对象必须是限额内普通文件：%s", t.Path)
		}
		t.Size = uint64(info.Size())
		t.SourceModified = info.ModTime().UnixNano()
		return nil
	}

	if _, err := os.Lstat(t.Path); !os.IsNotExist(err) {
		return fmt.Errorf("下载目标已存在或不可访问：%s", t.Path)
	}

	probe, err := os.CreateTemp(filepath.Dir(t.Path), ".sft-write-check-*")
	if err != nil {
		return fmt.Errorf("下载目录不可写：%w", err)
	}
	name := probe.Name()
	closeErr := probe.Close()
	removeErr := os.Remove(name)
	if closeErr != nil {
		return closeErr
	}
	return removeErr
}

func (m *Manager) HasRunning() bool { return m.activeCount.Load() > 0 }

func (m *Manager) Close() error {
	m.closeOnce.Do(func() { m.closeErr = m.close() })
	return m.closeErr
}

func (m *Manager) close() error {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return nil
	}
	m.closed = true
	m.hold = true
	m.mu.Unlock()

	m.cancel()
	m.signal()
	<-m.loopDone
	m.wg.Wait()
	return m.store.Close()
}

func (m *Manager) Identity() string { return m.identity }

func (m *Manager) pruneRecords(now time.Time) {
	if len(m.history) > HistoryLimit {
		m.history = m.history[:HistoryLimit]
		m.historyDirty = true
	}
	cutoff := now.Add(-HistoryRetention)
	idx := len(m.history)
	for i, t := range m.history {
		if t.Updated.Before(cutoff) {
			idx = i
			break
		}
	}
	if idx < len(m.history) {
		m.history = m.history[:idx]
		m.historyDirty = true
	}
	for id, t := range m.tasks {
		if t.Terminal() && t.Updated.Before(cutoff) {
			delete(m.tasks, id)
			m.currentDirty = true
		}
	}
	var terminal []*Task
	for _, t := range m.tasks {
		if t.Terminal() {
			terminal = append(terminal, t)
		}
	}
	if len(terminal) > HistoryLimit {
		sort.Slice(terminal, func(i, j int) bool {
			if !terminal[i].Updated.Equal(terminal[j].Updated) {
				return terminal[i].Updated.After(terminal[j].Updated)
			}
			return terminal[i].ID < terminal[j].ID
		})
		for _, old := range terminal[HistoryLimit:] {
			delete(m.tasks, old.ID)
		}
		m.currentDirty = true
	}
	m.nextPrune = now.Add(time.Hour)
}

func (m *Manager) Task(id string) *Task {
	m.mu.Lock()
	defer m.mu.Unlock()
	t := m.tasks[id]
	if t == nil {
		return nil
	}
	taskCopy := *t
	return &taskCopy
}
