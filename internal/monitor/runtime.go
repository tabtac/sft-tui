// Package monitor 维护服务运行期连接、任务、吞吐速率与有界历史/日志快照。
package monitor

import (
	"sort"
	"sync"
	"time"

	"sft-tui/internal/config"
	"sft-tui/internal/pal"
)

const Version = 1

type Connection struct {
	ID     uint64    `json:"id"`
	User   string    `json:"user"`
	Remote string    `json:"remote"`
	Since  time.Time `json:"since"`
}

type Task struct {
	ID           uint64    `json:"id"`
	Connection   uint64    `json:"connection"`
	Stream       uint32    `json:"stream"`
	User         string    `json:"user"`
	Direction    string    `json:"direction"`
	File         string    `json:"file"`
	Size         uint64    `json:"size"`
	Offset       uint64    `json:"offset"`
	State        string    `json:"state"`
	Error        string    `json:"error,omitempty"`
	Started      time.Time `json:"started"`
	Finished     time.Time `json:"finished,omitempty"`
	Rate         float64   `json:"rate"`
	sampleAt     time.Time
	sampleOffset uint64
}

type Snapshot struct {
	Version        int           `json:"version"`
	Started        time.Time     `json:"started"`
	At             time.Time     `json:"at"`
	Listen         string        `json:"listen"`
	State          string        `json:"state"`
	DrainStarted   time.Time     `json:"drain_started,omitempty"`
	Connections    int           `json:"connections"`
	OnlineUsers    int           `json:"online_users"`
	ActiveTasks    int           `json:"active_tasks"`
	ReceiveRate    float64       `json:"receive_rate"`
	SendRate       float64       `json:"send_rate"`
	Received       uint64        `json:"received"`
	Sent           uint64        `json:"sent"`
	Failed         uint64        `json:"failed"`
	Completed      uint64        `json:"completed"`
	Peers          []Connection  `json:"peers"`
	Tasks          []Task        `json:"tasks"`
	History        []Task        `json:"history"`
	Truncated      bool          `json:"truncated"`
	PublishedFiles uint64        `json:"published_files"`
	PublishedBytes uint64        `json:"published_bytes"`
	StorageAt      time.Time     `json:"storage_at"`
	StorageError   string        `json:"storage_error,omitempty"`
	Config         config.Server `json:"config"`
}

type trafficSample struct {
	tick           int64
	received, sent uint64
}

type Runtime struct {
	traffic                           [12]trafficSample
	mu                                sync.Mutex
	started                           time.Time
	listen                            string
	state                             string
	drainStarted                      time.Time
	next                              uint64
	peers                             map[uint64]Connection
	tasks                             map[uint64]*Task
	history                           []Task
	drained                           chan struct{}
	storageFiles, storageBytes        uint64
	storageAt                         time.Time
	storageError                      string
	received, sent, failed, completed uint64
}

func New() *Runtime {
	return &Runtime{
		started: time.Now(),
		state:   "running",
		peers:   make(map[uint64]Connection),
		tasks:   make(map[uint64]*Task),
		drained: make(chan struct{}),
	}
}

func (r *Runtime) Listening(address string) {
	r.mu.Lock()
	r.listen = address
	r.mu.Unlock()
}

func (r *Runtime) Connect(user, remote string) (uint64, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.state != "running" {
		return 0, false
	}
	r.next++
	id := r.next
	r.peers[id] = Connection{ID: id, User: user, Remote: remote, Since: time.Now()}
	return id, true
}

func (r *Runtime) Disconnect(id uint64) {
	r.mu.Lock()
	delete(r.peers, id)
	r.mu.Unlock()
}

// Begin 与 Drain 共享同一互斥锁，确保所有已准入流均纳入排空等待集合。
func (r *Runtime) Begin(connection uint64, stream uint32, user, direction string) (uint64, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.state != "running" {
		return 0, false
	}
	r.next++
	id := r.next
	r.tasks[id] = &Task{
		ID:         id,
		Connection: connection,
		Stream:     stream,
		User:       user,
		Direction:  direction,
		State:      "准备",
		Started:    time.Now(),
	}
	return id, true
}

func (r *Runtime) Describe(id uint64, name string, size, offset uint64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if t := r.tasks[id]; t != nil {
		t.File = name
		t.Size = size
		t.Offset = offset
		t.sampleOffset = offset
		t.sampleAt = time.Now()
		t.State = "传输"
	}
}

func (r *Runtime) Progress(id, offset uint64, state string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	t := r.tasks[id]
	if t == nil {
		return
	}
	now := time.Now()
	if offset > t.Offset {
		delta := offset - t.Offset
		tick := now.UnixNano() / int64(250*time.Millisecond)
		sample := &r.traffic[tick%int64(len(r.traffic))]
		if sample.tick != tick {
			*sample = trafficSample{tick: tick}
		}
		if t.Direction == "upload" {
			r.received += delta
			sample.received += delta
		} else if t.Direction == "download" {
			r.sent += delta
			sample.sent += delta
		}
	}
	t.Offset = offset
	t.State = state
	if elapsed := now.Sub(t.sampleAt).Seconds(); elapsed >= 0.25 {
		if offset >= t.sampleOffset {
			t.Rate = float64(offset-t.sampleOffset) / elapsed
		}
		t.sampleAt = now
		t.sampleOffset = offset
	}
}

func (r *Runtime) End(id uint64, outcome string, err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	t := r.tasks[id]
	if t == nil {
		return
	}
	delete(r.tasks, id)
	t.Finished = time.Now()
	t.State = outcome
	t.Rate = 0
	if err != nil {
		t.State = "failed"
		t.Error = pal.TruncateUTF8(err.Error(), 2048)
		r.failed++
	} else if outcome == "completed" {
		r.completed++
	}
	r.history = append(r.history, *t)
	if len(r.history) > 100 {
		copy(r.history, r.history[len(r.history)-100:])
		r.history = r.history[:100]
	}
	r.checkDrainedLocked()
}

func (r *Runtime) Drain(immediate bool) <-chan struct{} {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.state == "running" {
		r.state = "draining"
		r.drainStarted = time.Now()
	}
	if immediate {
		r.state = "stopping"
	}
	r.checkDrainedLocked()
	return r.drained
}

func (r *Runtime) checkDrainedLocked() {
	if r.state != "running" && len(r.tasks) == 0 {
		select {
		case <-r.drained:
		default:
			close(r.drained)
		}
	}
}

func (r *Runtime) Storage(files, bytes uint64, err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err != nil {
		r.storageError = pal.TruncateUTF8(err.Error(), 1024)
		return
	}
	r.storageFiles = files
	r.storageBytes = bytes
	r.storageAt = time.Now()
	r.storageError = ""
}

func (r *Runtime) Snapshot() Snapshot {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := time.Now()
	s := Snapshot{
		Version:      Version,
		Started:      r.started,
		At:           now,
		Listen:       r.listen,
		State:        r.state,
		DrainStarted: r.drainStarted,
		Connections:  len(r.peers),
		ActiveTasks:  len(r.tasks),
		Received:     r.received,
		Sent:         r.sent,
		Failed:       r.failed,
		Completed:    r.completed,
	}
	users := make(map[string]bool)
	for _, p := range r.peers {
		users[p.User] = true
		s.Peers = append(s.Peers, p)
	}
	s.OnlineUsers = len(users)
	s.PublishedFiles = r.storageFiles
	s.PublishedBytes = r.storageBytes
	s.StorageAt = r.storageAt
	s.StorageError = r.storageError
	sort.Slice(s.Peers, func(i, j int) bool { return s.Peers[i].ID < s.Peers[j].ID })
	if len(s.Peers) > 256 {
		s.Peers = s.Peers[:256]
		s.Truncated = true
	}
	for _, task := range r.tasks {
		t := *task
		if now.Sub(t.sampleAt) > 3*time.Second {
			t.Rate = 0
		}
		s.Tasks = append(s.Tasks, t)
	}
	sort.Slice(s.Tasks, func(i, j int) bool { return s.Tasks[i].ID < s.Tasks[j].ID })
	if len(s.Tasks) > 256 {
		s.Tasks = s.Tasks[:256]
		s.Truncated = true
	}
	tick := now.UnixNano() / int64(250*time.Millisecond)
	windowStart := time.Unix(0, (tick-7)*int64(250*time.Millisecond))
	if windowStart.Before(r.started) {
		windowStart = r.started
	}
	seconds := now.Sub(windowStart).Seconds()
	if seconds > 0 {
		for _, sample := range r.traffic {
			if sample.tick >= tick-7 && sample.tick <= tick {
				s.ReceiveRate += float64(sample.received) / seconds
				s.SendRate += float64(sample.sent) / seconds
			}
		}
	}
	s.History = append([]Task(nil), r.history...)
	return s
}
