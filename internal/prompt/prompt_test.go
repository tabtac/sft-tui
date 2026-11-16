package prompt

import (
	"bytes"
	"strings"
	"testing"
)

func TestDialogPromptsDefaultsValidationAndEOF(t *testing.T) {
	var out bytes.Buffer
	d := New(strings.NewReader("\n\nbad\n2\n"), &out)

	got, err := d.Text("主机", "localhost", Nonempty)
	if err != nil || got != "localhost" {
		t.Fatalf("Text default = %q, %v", got, err)
	}
	ok, err := d.Bool("启用", true)
	if err != nil || !ok {
		t.Fatalf("Bool default = %v, %v", ok, err)
	}
	idx, err := d.Choose("操作", []string{"查看", "新建"})
	if err != nil || idx != 1 {
		t.Fatalf("Choose = %d, %v", idx, err)
	}
	_, err = d.Text("截断", "default", nil)
	if err == nil {
		t.Fatal("expected EOF error on empty trailing input")
	}
}
