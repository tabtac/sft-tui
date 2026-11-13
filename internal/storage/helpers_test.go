package storage

import (
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"sft-tui/internal/protocol"
)

// review 测试断言安全传输应满足的行为。BUG 注释说明当前实现的缺陷，
// 修复前保留失败断言，不能通过 Skip 或反向断言把缺陷变成通过结果。
func reviewOpenStore(t *testing.T, root string) *Store {
	t.Helper()
	s, err := OpenStore(root)
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})
	return s
}

func reviewRequest(name string, content []byte, requestByte byte) protocol.PutRequest {
	return protocol.PutRequest{
		RequestID: protocol.RequestID{requestByte}, FileName: name,
		FileSize: uint64(len(content)), FileSHA256: sha256.Sum256(content),
	}
}

func reviewCreateTask(t *testing.T, s *Store, user string, req protocol.PutRequest) Task {
	t.Helper()
	task, err := s.CreateUploadTask(context.Background(), user, req)
	if err != nil {
		t.Fatalf("CreateUploadTask: %v", err)
	}
	return task
}

func reviewWrite(t *testing.T, s *Store, task Task, offset uint64, data []byte) {
	t.Helper()
	next, err := s.WriteChunkAt(context.Background(), task.ID, offset, data)
	if err != nil || next != offset+uint64(len(data)) {
		t.Fatalf("WriteChunkAt: next=%d err=%v", next, err)
	}
}

func reviewTask(t *testing.T, s *Store, id protocol.TransferID) Task {
	t.Helper()
	task, err := s.GetTask(context.Background(), id)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	return task
}

func reviewPartPath(root string, id protocol.TransferID) string {
	return filepath.Join(root, "staging", IDToHex(id)+".part")
}

func reviewReadFile(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile(%q): %v", path, err)
	}
	return data
}

func reviewPublish(t *testing.T, s *Store, user, name string, content []byte, requestByte byte) Task {
	t.Helper()
	task := reviewCreateTask(t, s, user, reviewRequest(name, content, requestByte))
	if len(content) > 0 {
		reviewWrite(t, s, task, 0, content)
	}
	if err := s.CommitUpload(context.Background(), task.ID, task.FileSize, task.FileSHA256); err != nil {
		t.Fatalf("CommitUpload(%q): %v", name, err)
	}
	return task
}

func reviewExec(t *testing.T, s *Store, query string, args ...any) {
	t.Helper()
	if _, err := s.db.Exec(query, args...); err != nil {
		t.Fatalf("SQL fixture: %v", err)
	}
}

func reviewCount(t *testing.T, s *Store, table string) int {
	t.Helper()
	// table 只来自本测试文件中的固定表名，不接受外部输入。
	var count int
	if err := s.db.QueryRow(fmt.Sprintf("SELECT COUNT(*) FROM %s", table)).Scan(&count); err != nil {
		t.Fatal(err)
	}
	return count
}
