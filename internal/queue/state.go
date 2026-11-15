package queue

import (
	"context"
	"fmt"
	"io"
	"time"

	"sft-tui/internal/protocol"
)

func (m *Manager) saveRecords(tasks []Task, history bool) error {
	m.currentDirty = true
	if history {
		m.historyDirty = true
	}
	if err := m.store.Write(tasks, history); err != nil {
		m.storageErr = err
		m.storageText.Store(err.Error())
		m.hold = true
		return err
	}
	return nil
}

func (m *Manager) persist(t *Task, history bool) error {
	t.Updated = time.Now()
	if err := m.saveRecords([]Task{*t}, history); err != nil {
		return err
	}
	if history {
		m.history = append([]Task{*t}, m.history...)
	}
	if history || !time.Now().Before(m.nextPrune) {
		m.pruneRecords(time.Now())
	}
	return nil
}

func (m *Manager) persistIdle(t *Task, old Task, history bool) error {
	if err := m.persist(t, history); err != nil {
		*t = old
		return err
	}
	return nil
}

// Resume 恢复排队待命
func (m *Manager) Resume(id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	t := m.tasks[id]
	if t == nil {
		return fmt.Errorf("任务不存在")
	}
	if m.active[id] != nil {
		return fmt.Errorf("任务正在运行或等待控制确认")
	}
	if t.Terminal() {
		return fmt.Errorf("已结束的任务请从历史创建新任务")
	}
	if m.closed || m.storageErr != nil {
		return fmt.Errorf("队列不可写或已关闭")
	}

	old := *t
	t.State = Waiting
	t.Error = ""
	if err := m.persistIdle(t, old, false); err != nil {
		return err
	}
	m.hold = false
	m.signal()
	return nil
}

// Pause 暂停任务
func (m *Manager) Pause(id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	t := m.tasks[id]
	if t == nil || t.Terminal() {
		return fmt.Errorf("没有可暂停的任务")
	}
	if t.WantCancel {
		return fmt.Errorf("取消尚未完成，请先重试取消")
	}

	old := *t
	if r := m.active[id]; r != nil {
		if err := r.op.Request(protocol.CancelPause); err != nil {
			return err
		}
		t.State = Pausing
	} else {
		t.State = Paused
		return m.persistIdle(t, old, false)
	}
	return m.persist(t, false)
}

// Cancel 取消任务
func (m *Manager) Cancel(id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	t := m.tasks[id]
	if t == nil || t.Terminal() {
		return fmt.Errorf("没有可取消的任务")
	}

	r := m.active[id]
	if r != nil && r.op.Mode() != 0 && r.op.Mode() != protocol.CancelAbort {
		return fmt.Errorf("等待暂停确认后再取消")
	}

	if r == nil {
		if !t.HasCheckpoint() {
			old := *t
			t.State = Cancelled
			t.WantCancel = false
			t.Error = ""
			return m.persistIdle(t, old, true)
		}
	}

	old := *t
	t.WantCancel = true
	if r != nil {
		t.State = Cancelling
	} else {
		t.State = Waiting
	}
	if err := m.persistIdle(t, old, false); err != nil {
		return err
	}
	if r != nil {
		_ = r.op.Request(protocol.CancelAbort)
	} else if !m.closed && m.storageErr == nil {
		m.hold = false
	}
	m.signal()
	return nil
}

// PauseAll 批量暂停所有任务，支持排空等待
func (m *Manager) PauseAll(ctx context.Context) error {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return io.ErrClosedPipe
	}
	m.hold = true
	var ids []string
	var records []Task
	now := time.Now()
	for id, t := range m.tasks {
		if t.Terminal() {
			continue
		}
		ids = append(ids, id)
		if r := m.active[id]; r != nil {
			if !t.WantCancel {
				_ = r.op.Request(protocol.CancelPause)
				t.State = Pausing
			}
		} else if !t.WantCancel {
			t.State = Paused
		}
		t.Updated = now
		records = append(records, *t)
	}
	if len(records) > 0 {
		if err := m.saveRecords(records, false); err != nil {
			m.mu.Unlock()
			return err
		}
	}
	m.mu.Unlock()

	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	for {
		m.mu.Lock()
		active := len(m.active)
		m.mu.Unlock()
		if active == 0 {
			break
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("暂停未全部确认，等待超时：%w", ctx.Err())
		case <-ticker.C:
		}
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	for _, id := range ids {
		t := m.tasks[id]
		if t != nil && !t.Terminal() && (t.State != Paused || t.WantCancel) {
			return fmt.Errorf("%s 暂停或取消未确认：%s", t.Remote, t.Error)
		}
	}
	return nil
}

// ResumeAll 批量恢复所有处于暂停或可重试待处理状态的任务
func (m *Manager) ResumeAll() error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.closed {
		return io.ErrClosedPipe
	}
	if m.storageErr != nil {
		return m.storageErr
	}

	var records []Task
	var restored []struct {
		ptr *Task
		old Task
	}
	now := time.Now()
	for id, t := range m.tasks {
		if m.active[id] != nil || t.WantCancel || (t.State != Paused && t.State != Pending) {
			continue
		}
		restored = append(restored, struct {
			ptr *Task
			old Task
		}{t, *t})
		t.State = Waiting
		t.Error = ""
		t.Updated = now
		records = append(records, *t)
	}
	if len(records) > 0 {
		if err := m.saveRecords(records, false); err != nil {
			for _, r := range restored {
				*r.ptr = r.old
			}
			return err
		}
	}
	m.hold = false
	m.signal()
	return nil
}

// ClearHistory 清空历史记录
func (m *Manager) ClearHistory() error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.closed {
		return io.ErrClosedPipe
	}
	if _, err := m.store.db.Exec("DELETE FROM history"); err != nil {
		return err
	}
	m.history = nil
	empty := []Task{}
	m.historyView.Store(&empty)
	m.historyDirty = false
	m.currentDirty = true

	for id, t := range m.tasks {
		if t.Terminal() {
			delete(m.tasks, id)
		}
	}
	return nil
}

// UpdateTask 原子修改并持久化运行中任务的检查点元数据（如 TransferID / RequestID / 临时文件信息）
func (m *Manager) UpdateTask(id string, fn func(*Task)) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.closed {
		return io.ErrClosedPipe
	}
	t := m.tasks[id]
	if t == nil {
		return fmt.Errorf("任务不存在")
	}
	old := *t
	fn(t)
	if err := m.persist(t, false); err != nil {
		*t = old
		return err
	}
	return nil
}

// Quiesce 安全静默当前队列，切换身份前排空所有进行中任务
func (m *Manager) Quiesce(ctx context.Context) error {
	err := m.PauseAll(ctx)
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.active) == 0 && m.storageErr == nil {
		return nil
	}
	return err
}
