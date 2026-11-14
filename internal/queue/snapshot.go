package queue

import (
	"sort"
	"time"
)

// Snapshot 获取队列当前状态的一致性只读快照。
// 专为 TUI 60FPS 渲染设计：当主锁被写事务占用时通过 TryLock 立即返回上一个安全视图，绝不阻塞渲染主循环。
func (m *Manager) Snapshot(history bool) []Task {
	cache := &m.currentView
	if history {
		cache = &m.historyView
	}

	if !m.mu.TryLock() {
		if last := cache.Load(); last != nil {
			return append([]Task(nil), (*last)...)
		}
		return nil
	}
	defer m.mu.Unlock()

	if now := time.Now(); !now.Before(m.nextPrune) {
		m.pruneRecords(now)
	}

	// 脏标记优化：若无变更且当前无活跃测速采样，直接复用上一个快照切片副本
	if last := cache.Load(); last != nil && (history && !m.historyDirty || !history && !m.currentDirty && len(m.active) == 0) {
		return append([]Task(nil), (*last)...)
	}

	if history {
		out := append([]Task(nil), m.history...)
		cache.Store(&out)
		m.historyDirty = false
		return append([]Task(nil), out...)
	}

	out := make([]Task, 0, len(m.tasks))
	now := time.Now()

	for id, t := range m.tasks {
		v := *t
		if r := m.active[id]; r != nil && len(r.samples) > 1 && v.State == Transferring {
			first, last := r.samples[0], r.samples[len(r.samples)-1]
			seconds := last.at.Sub(first.at).Seconds()
			if seconds >= 0.5 && now.Sub(last.at) < 3*time.Second && last.bytes >= first.bytes {
				v.Speed = float64(last.bytes-first.bytes) / seconds
				if v.Speed > 0 && v.Size >= v.Done {
					remSeconds := float64(v.Size-v.Done) / v.Speed
					if remSeconds > 366*24*3600 {
						v.ETA = 366 * 24 * time.Hour
					} else {
						v.ETA = time.Duration(remSeconds * float64(time.Second))
					}
				}
			}
		}
		out = append(out, v)
	}

	sort.Slice(out, func(i, j int) bool {
		if out[i].Created.Equal(out[j].Created) {
			return out[i].ID < out[j].ID
		}
		return out[i].Created.Before(out[j].Created)
	})

	cache.Store(&out)
	m.currentDirty = false
	return append([]Task(nil), out...)
}
