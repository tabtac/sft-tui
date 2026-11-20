package progress

import (
	"bytes"
	"fmt"
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"

	"sft-tui/internal/queue"
)

func TestDisplayAlwaysAndNever(t *testing.T) {
	var status, results bytes.Buffer
	d, err := New("always", &status, &results)
	if err != nil {
		t.Fatal(err)
	}
	d.Start(0, "upload a.bin\x1b[31m")
	d.Update(0, 512, 1024, queue.Transferring)
	d.Finish(0, "ok: a.bin", false)
	d.Close()

	if !strings.Contains(results.String(), "ok: a.bin") {
		t.Fatalf("missing result line: %q", results.String())
	}
	if strings.Contains(status.String(), "\x1b[31m") {
		t.Fatalf("control sequences were not sanitized: %q", status.String())
	}
	if !strings.Contains(status.String(), "50%") {
		t.Fatalf("expected rendered percentage in status output: %q", status.String())
	}

	status.Reset()
	results.Reset()
	quiet, err := New("never", &status, &results)
	if err != nil {
		t.Fatal(err)
	}
	quiet.Start(0, "download b.bin")
	quiet.Update(0, 100, 100, queue.Completed)
	quiet.Finish(0, "done", false)
	quiet.Close()
	if status.Len() != 0 {
		t.Fatalf("never mode unexpectedly wrote status: %q", status.String())
	}
}

func TestProgressCompletionSanitizesUntrustedResultText(t *testing.T) {
	for _, failed := range []bool{false, true} {
		t.Run(fmt.Sprint(failed), func(t *testing.T) {
			var status, results bytes.Buffer
			display, err := New("never", &status, &results)
			if err != nil {
				t.Fatal(err)
			}
			display.Finish(0, "file\x1b[2J\x1b]0;changed-title\x07", failed)
			display.Close()
			// BUG: Start 会清理控制字符，Finish 的结果/错误消息却原样输出，能注入终端控制序列。
			if strings.ContainsAny(status.String()+results.String(), "\x1b\x07") {
				t.Error("BUG: result text injected terminal control sequences")
			}
		})
	}
}

func TestProgressZeroByteAndOverflowRemainBounded(t *testing.T) {
	for _, tc := range []struct {
		name        string
		done, total uint64
		phase       queue.State
		percent     string
	}{
		{"zero_completed", 0, 0, queue.Completed, "100%"},
		{"overflow_clamped", 200, 100, queue.Transferring, "100%"},
		{"unknown_total", 17, 0, queue.Transferring, "0%"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var status, results bytes.Buffer
			display, err := New("always", &status, &results)
			if err != nil {
				t.Fatal(err)
			}
			display.Start(0, "file")
			display.Update(0, tc.done, tc.total, tc.phase)
			display.Finish(0, "done", false)
			display.Close()
			if !strings.Contains(status.String(), tc.percent) {
				t.Errorf("progress missing expected percentage %s: %q", tc.percent, status.String())
			}
		})
	}
}

func TestDisplayWideRuneAlignmentAndPausedFinishPhase(t *testing.T) {
	var status, results bytes.Buffer
	d, err := New("always", &status, &results)
	if err != nil {
		t.Fatal(err)
	}
	d.Start(0, "中文测试文件.bin")
	d.Start(1, "ascii-name.bin")
	d.Update(0, 50, 100, queue.Paused)
	d.Update(1, 50, 100, queue.Transferring)

	d.mu.Lock()
	line0 := d.line(d.entries[0])
	line1 := d.line(d.entries[1])
	d.mu.Unlock()
	idx0 := strings.Index(line0, " [")
	idx1 := strings.Index(line1, " [")
	if idx0 < 0 || idx1 < 0 {
		t.Fatalf("missing progress bar in rendered lines: %q / %q", line0, line1)
	}
	if ansi.StringWidth(line0[:idx0]) != ansi.StringWidth(line1[:idx1]) {
		t.Fatalf("display width mismatch between CJK and ASCII labels: %d != %d (%q vs %q)",
			ansi.StringWidth(line0[:idx0]), ansi.StringWidth(line1[:idx1]), line0, line1)
	}

	d.Finish(0, "paused: 中文测试文件.bin", true)
	d.Close()
	if !strings.Contains(status.String(), "("+string(queue.Paused)+")") {
		t.Fatalf("expected Finish(..., true) to keep paused phase in final line, got: %q", status.String())
	}
}
