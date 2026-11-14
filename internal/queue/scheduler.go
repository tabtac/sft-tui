package queue

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sort"
	"time"

	"sft-tui/internal/protocol"
)

var (
	ErrPaused    = errors.New("任务已暂停")
	ErrCancelled = errors.New("任务已取消")
)

// TaskRunner 任务执行契约，解耦传输引擎与队列调度器
type TaskRunner func(ctx context.Context, task Task, op *Operation, progress func(transferred, total uint64, phase State)) error

func (m *Manager) loop() {
	defer close(m.loopDone)
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-m.ctx.Done():
			return
		case <-m.notify:
		case <-ticker.C:
		}
		m.dispatch()
	}
}

func (m *Manager) dispatch() {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.closed || m.hold || m.storageErr != nil || m.runner == nil {
		return
	}

	limit := int(m.cfg.MaxStreams)
	if limit <= 0 {
		limit = 3
	}

	var pending []*Task
	for _, t := range m.tasks {
		if t.State == Waiting && m.active[t.ID] == nil {
			pending = append(pending, t)
		}
	}

	sort.Slice(pending, func(i, j int) bool {
		return pending[i].Created.Before(pending[j].Created)
	})

	for _, t := range pending {
		if len(m.active) >= limit {
			break
		}
		if t.WantCancel {
			t.State = Cancelling
		} else {
			t.State = Preparing
		}
		if err := m.persist(t, false); err != nil {
			return
		}

		r := &running{op: NewOperation(m.ctx)}
		m.active[t.ID] = r
		m.activeCount.Add(1)
		m.wg.Add(1)
		go m.run(*t, r)
	}
}

// Progress 更新任务进度并记录测速采样
func (m *Manager) Progress(id string, transferred, total uint64, phase State) {
	m.mu.Lock()
	defer m.mu.Unlock()

	t := m.tasks[id]
	r := m.active[id]
	if t == nil || r == nil {
		return
	}

	t.Done, t.Size = transferred, total
	if !t.State.Controlling() && t.State != phase && phase != "" {
		t.State = phase
		if err := m.persist(t, false); err != nil {
			_ = r.op.Request(protocol.CancelPause)
		}
	}

	now := time.Now()
	if phase == Transferring && (len(r.samples) == 0 || now.Sub(r.samples[len(r.samples)-1].at) >= 200*time.Millisecond) {
		r.samples = append(r.samples, sample{at: now, bytes: transferred})
		for len(r.samples) > 2 && now.Sub(r.samples[0].at) > 5*time.Second {
			r.samples = r.samples[1:]
		}
	}
}

func (m *Manager) run(task Task, r *running) {
	defer m.wg.Done()
	defer r.op.Close()

	var err error
	started := false

	if task.WantCancel {
		// 取消已落盘检查点
		err = m.cleanCheckpoint(r.op.Context(), task)
	} else {
		if task.Direction == protocol.DirectionUpload && task.SourceModified != 0 {
			info, e := os.Stat(task.Path)
			if e != nil {
				err = e
			} else if !info.Mode().IsRegular() || info.Size() < 0 || uint64(info.Size()) != task.Size || info.ModTime().UnixNano() != task.SourceModified {
				err = fmt.Errorf("源文件已改变，请取消后重新加入队列")
			}
		}

		if err == nil {
			m.mu.Lock()
			runner := m.runner
			m.mu.Unlock()

			if runner != nil {
				started = true
				err = runner(r.op.Context(), task, r.op, func(transferred, total uint64, phase State) {
					m.Progress(task.ID, transferred, total, phase)
				})
			} else {
				err = fmt.Errorf("未配置传输执行器")
			}
		}
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	t := m.tasks[task.ID]
	delete(m.active, task.ID)
	m.activeCount.Add(-1)

	if t == nil {
		m.signal()
		return
	}

	mode := r.op.Mode()
	switch {
	case err == nil:
		if task.WantCancel {
			t.State = Cancelled
			t.WantCancel = false
		} else {
			t.State = Completed
			t.WantCancel = false
			t.Done = t.Size
		}
		t.Error = ""
	case errors.Is(err, ErrPaused) || mode == protocol.CancelPause:
		t.State = Paused
		t.Error = ""
	case errors.Is(err, ErrCancelled) || mode == protocol.CancelAbort:
		t.State = Cancelled
		t.WantCancel = false
		t.Error = ""
	case !started && mode == protocol.CancelPause:
		t.State = Paused
		t.Error = ""
	case !started && mode == protocol.CancelAbort:
		if !t.HasCheckpoint() {
			t.State = Cancelled
			t.WantCancel = false
			t.Error = ""
		} else {
			t.State = Pending
			t.WantCancel = true
			t.Error = "取消尚未完成，稍后重试"
		}
	default:
		t.State = Pending
		t.Error = err.Error()
		if mode == protocol.CancelAbort || task.WantCancel {
			t.WantCancel = true
		}
	}

	history := t.Terminal()
	if e := m.persist(t, history); e != nil {
		t.State = Pending
		t.Error = "状态保存失败：" + e.Error()
	}
	m.signal()
}

func (m *Manager) cleanCheckpoint(ctx context.Context, t Task) error {
	if !t.HasCheckpoint() {
		return nil
	}
	m.mu.Lock()
	runner := m.runner
	m.mu.Unlock()
	if runner == nil {
		return fmt.Errorf("未配置传输执行器")
	}
	return runner(ctx, t, NewOperation(ctx), nil)
}
