package progress

import (
	"bytes"
	"strings"
	"testing"

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
