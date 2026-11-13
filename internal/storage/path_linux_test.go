//go:build linux

package storage

import (
	"os"
	"path/filepath"
	"testing"
)

func TestReviewStoreQuestionMarkPathIsLiteral(t *testing.T) {
	root := filepath.Join(t.TempDir(), "literal?directory")
	// BUG: 未编码的文件系统路径直接拼到 SQLite DSN，? 会被解释为 URI 参数分隔符。
	s := reviewOpenStore(t, root)
	reviewPublish(t, s, "alice", "a", nil, 1)
	if _, err := os.Stat(filepath.Join(root, "metadata", "metadata.db")); err != nil {
		t.Errorf("BUG: database was not created at the configured literal path: %v", err)
	}
}
