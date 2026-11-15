package monitor

import (
	"context"
	"log/slog"
	"strings"
	"sync"
	"time"

	"sft-tui/internal/pal"
)

const maxLogEvents = 200

type Event struct {
	Sequence uint64    `json:"sequence"`
	Time     time.Time `json:"time"`
	Level    string    `json:"level"`
	Message  string    `json:"message"`
	User     string    `json:"user,omitempty"`
	File     string    `json:"file,omitempty"`
	Detail   string    `json:"detail,omitempty"`
}

type LogQuery struct {
	After uint64    `json:"after,omitempty"`
	Level string    `json:"level,omitempty"`
	User  string    `json:"user,omitempty"`
	File  string    `json:"file,omitempty"`
	Text  string    `json:"text,omitempty"`
	Since time.Time `json:"since,omitempty"`
}

type LogPage struct {
	Events []Event `json:"events"`
	Last   uint64  `json:"last"`
	Lost   bool    `json:"lost"`
}

type Logs struct {
	mu         sync.Mutex
	sequence   uint64
	events     []Event
	writeError string
}

func (l *Logs) appendLocked(e Event) {
	l.sequence++
	e.Sequence = l.sequence
	if len(l.events) == maxLogEvents {
		copy(l.events, l.events[1:])
		l.events[len(l.events)-1] = e
	} else {
		l.events = append(l.events, e)
	}
}

func (l *Logs) Query(q LogQuery) LogPage {
	l.mu.Lock()
	defer l.mu.Unlock()
	p := LogPage{Last: l.sequence}
	if len(l.events) > 0 && q.After > 0 && q.After < l.events[0].Sequence-1 {
		p.Lost = true
	}
	level, file, text := strings.ToUpper(q.Level), strings.ToLower(q.File), strings.ToLower(q.Text)
	for _, e := range l.events {
		if e.Sequence <= q.After ||
			(!q.Since.IsZero() && e.Time.Before(q.Since)) ||
			(q.Level != "" && e.Level != level) ||
			(q.User != "" && e.User != q.User) ||
			(q.File != "" && !strings.Contains(strings.ToLower(e.File), file)) ||
			(q.Text != "" && !strings.Contains(strings.ToLower(e.Message+" "+e.Detail), text)) {
			continue
		}
		p.Events = append(p.Events, e)
	}
	return p
}

// Handler 包装下游 slog.Handler，同步记录有界结构化事件并监控磁盘写入故障。
func (l *Logs) Handler(next slog.Handler) slog.Handler {
	return &logHandler{logs: l, next: next}
}

type logHandler struct {
	logs   *Logs
	next   slog.Handler
	attrs  []slog.Attr
	groups []string
}

func (h *logHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return h.next.Enabled(ctx, level)
}

func (h *logHandler) Handle(ctx context.Context, record slog.Record) error {
	e := Event{
		Time:    record.Time,
		Level:   record.Level.String(),
		Message: pal.TruncateUTF8(record.Message, 2048),
	}
	var parts []string
	var add func(slog.Attr, string)
	add = func(a slog.Attr, prefix string) {
		a.Value = a.Value.Resolve()
		if a.Value.Kind() == slog.KindGroup {
			for _, v := range a.Value.Group() {
				add(v, prefix+a.Key+".")
			}
			return
		}
		value := pal.TruncateUTF8(a.Value.String(), 1024)
		if prefix == "" && a.Key == "user" {
			e.User = value
		}
		if prefix == "" && a.Key == "file" {
			e.File = value
		}
		parts = append(parts, prefix+a.Key+"="+value)
	}
	for _, a := range h.attrs {
		add(a, "")
	}
	prefix := strings.Join(h.groups, ".")
	if prefix != "" {
		prefix += "."
	}
	record.Attrs(func(a slog.Attr) bool { add(a, prefix); return true })
	e.Detail = pal.TruncateUTF8(strings.Join(parts, " "), 2048)

	h.logs.mu.Lock()
	h.logs.appendLocked(e)
	h.logs.mu.Unlock()

	err := h.next.Handle(ctx, record)
	h.logs.mu.Lock()
	if err == nil {
		h.logs.writeError = ""
	} else if msg := err.Error(); msg != h.logs.writeError {
		h.logs.writeError = msg
		h.logs.appendLocked(Event{
			Time:    time.Now(),
			Level:   "ERROR",
			Message: "文件日志写入失败",
			Detail:  pal.TruncateUTF8(msg, 2048),
		})
	}
	h.logs.mu.Unlock()
	return err
}

func (h *logHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	n := *h
	n.next = h.next.WithAttrs(attrs)
	n.attrs = append([]slog.Attr(nil), h.attrs...)
	for _, a := range attrs {
		for i := len(h.groups) - 1; i >= 0; i-- {
			a = slog.Group(h.groups[i], a)
		}
		n.attrs = append(n.attrs, a)
	}
	return &n
}

func (h *logHandler) WithGroup(name string) slog.Handler {
	if name == "" {
		return h
	}
	n := *h
	n.next = h.next.WithGroup(name)
	n.groups = append(append([]string(nil), h.groups...), name)
	return &n
}
