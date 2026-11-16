// Package progress 为无头命令行（CLI）提供不交错并发文件的终端进度条渲染器。
package progress

import (
	"fmt"
	"io"
	"math"
	"os"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/charmbracelet/x/ansi"
	"github.com/charmbracelet/x/term"
	"github.com/mattn/go-isatty"

	"sft-tui/internal/queue"
)

type entry struct {
	label       string
	phase       queue.State
	done, total uint64
	started     time.Time
	initial     uint64
	initialized bool
}

type Display struct {
	mu      sync.Mutex
	status  io.Writer
	results io.Writer
	enabled bool
	ansi    bool
	width   int
	height  int
	lines   int
	entries map[int]*entry
	stop    chan struct{}
	stopped chan struct{}
	once    sync.Once
	restore func()
}

func New(mode string, status, results io.Writer) (*Display, error) {
	if mode != "auto" && mode != "always" && mode != "never" {
		return nil, fmt.Errorf("progress 参数须为 auto、always 或 never")
	}
	d := &Display{
		status:  status,
		results: results,
		width:   100,
		height:  24,
		entries: make(map[int]*entry),
		stop:    make(chan struct{}),
		stopped: make(chan struct{}),
		restore: func() {},
	}
	if f, ok := status.(*os.File); ok && mode != "never" {
		fd := f.Fd()
		if isatty.IsTerminal(fd) || isatty.IsCygwinTerminal(fd) {
			d.restore = enableVT(f)
			d.ansi = true
			if w, h, err := term.GetSize(fd); err == nil && w > 0 && h > 0 {
				d.width, d.height = w, h
			}
		}
	}
	d.enabled = mode == "always" || (mode == "auto" && d.ansi)
	go func() {
		defer close(d.stopped)
		ticker := time.NewTicker(150 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-d.stop:
				return
			case <-ticker.C:
				d.mu.Lock()
				d.draw()
				d.mu.Unlock()
			}
		}
	}()
	return d, nil
}

func (d *Display) Start(id int, label string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.entries[id] = &entry{label: safeLabel(label), phase: queue.Preparing, started: time.Now()}
	d.draw()
}

func (d *Display) Update(id int, done, total uint64, phase queue.State) {
	d.mu.Lock()
	defer d.mu.Unlock()
	e := d.entries[id]
	if e == nil {
		return
	}
	if phase == queue.Transferring && !e.initialized {
		e.initial, e.started, e.initialized = done, time.Now(), true
	}
	if total > 0 && done > total {
		done = total
	}
	e.done, e.total, e.phase = done, total, phase
}

func (d *Display) Finish(id int, message string, failed bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.clear()
	if e := d.entries[id]; e != nil && d.enabled {
		if failed {
			e.phase = queue.Pending
		}
		fmt.Fprintln(d.status, d.line(e))
	}
	delete(d.entries, id)
	if failed {
		fmt.Fprintln(d.status, message)
	} else {
		fmt.Fprintln(d.results, message)
	}
	d.draw()
}

func (d *Display) Close() {
	d.once.Do(func() {
		close(d.stop)
		<-d.stopped
		d.mu.Lock()
		defer d.mu.Unlock()
		d.clear()
		d.restore()
	})
}

func (d *Display) clear() {
	if d.lines > 0 {
		fmt.Fprintf(d.status, "\x1b[%dA\r\x1b[J", d.lines)
		d.lines = 0
	}
}

func (d *Display) draw() {
	if !d.enabled || len(d.entries) == 0 {
		return
	}
	d.clear()
	ids := make([]int, 0, len(d.entries))
	for id := range d.entries {
		ids = append(ids, id)
	}
	sort.Ints(ids)
	limit := len(ids)
	if d.ansi {
		limit = min(limit, max(1, d.height-3))
	}
	for _, id := range ids[:limit] {
		fmt.Fprintln(d.status, d.line(d.entries[id]))
	}
	if d.ansi {
		d.lines = limit
		if limit < len(ids) {
			fmt.Fprintln(d.status, ansi.Truncate(fmt.Sprintf("另有 %d 个并发传输任务", len(ids)-limit), max(1, d.width-1), "…"))
			d.lines++
		}
	}
}

func (d *Display) line(e *entry) string {
	frac := 0.0
	if e.total > 0 {
		frac = math.Min(1, float64(e.done)/float64(e.total))
	} else if e.phase == queue.Completed {
		frac = 1
	}
	const barW = 18
	filled := int(math.Round(frac * barW))
	bar := strings.Repeat("█", filled) + strings.Repeat("░", barW-filled)

	speed := "--"
	if e.initialized {
		if sec := time.Since(e.started).Seconds(); sec >= 0.3 && e.done > e.initial {
			speed = FormatRate(float64(e.done-e.initial) / sec)
		}
	}
	txt := fmt.Sprintf("%-22s [%s] %3.0f%%  %s/%s  %s  (%s)",
		ansi.Truncate(e.label, 22, "…"), bar, frac*100,
		FormatBytes(e.done), FormatBytes(e.total), speed, e.phase)
	if d.ansi {
		return ansi.Truncate(txt, max(1, d.width-1), "…")
	}
	return txt
}

func safeLabel(s string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return '?'
		}
		return r
	}, s)
}

func FormatBytes(n uint64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	units := []string{"KiB", "MiB", "GiB", "TiB"}
	v := float64(n)
	i := -1
	for v >= unit && i < len(units)-1 {
		v /= unit
		i++
	}
	return fmt.Sprintf("%.1f %s", v, units[i])
}

func FormatRate(bps float64) string {
	if bps <= 0 {
		return "--"
	}
	return FormatBytes(uint64(bps)) + "/s"
}
