package storage

import (
	"bytes"
	"context"
	"math"
	"os"
	"strconv"
	"testing"

	"sft-tui/internal/protocol"
)

func TestReviewCheckpointRejectsInvalidRangesWithoutMutation(t *testing.T) {
	// BUG: WriteChunkAt 在读取任务并校验连续偏移/文件大小之前就写盘，
	// 可以制造空洞、覆盖已确认数据、回退检查点及写入 FileSize 之外的数据。
	cases := []struct {
		name   string
		offset uint64
		data   []byte
	}{
		{"rewind", 0, []byte("Z")},
		{"gap", 3, []byte("Z")},
		{"past_eof", 5, []byte("Z")},
		{"overrun", 2, []byte("XYZ")},
		{"empty", 2, nil},
		{"empty_rewind", 0, []byte{}},
		{"negative_after_conversion", math.MaxUint64, []byte("Z")},
		{"offset_above_int64", uint64(math.MaxInt64) + 1, []byte("Z")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			s := reviewOpenStore(t, root)
			task := reviewCreateTask(t, s, "alice", reviewRequest("a.bin", []byte("ABCD"), 1))
			reviewWrite(t, s, task, 0, []byte("AB"))
			before := reviewTask(t, s, task.ID)
			_, err := s.WriteChunkAt(context.Background(), task.ID, tc.offset, tc.data)
			if err == nil {
				t.Error("BUG: invalid DATA range accepted")
			}
			if got := reviewReadFile(t, reviewPartPath(root, task.ID)); !bytes.Equal(got, []byte("AB")) {
				t.Errorf("BUG: rejected write changed bytes to %q", got)
			}
			if after := reviewTask(t, s, task.ID); after.Offset != before.Offset || after.Status != before.Status {
				t.Errorf("BUG: invalid write changed checkpoint/state: offset %d -> %d, status %s -> %s", before.Offset, after.Offset, before.Status, after.Status)
			}
		})
	}
}

func TestReviewInactiveTaskWriteDoesNotTouchFile(t *testing.T) {
	for _, status := range []TaskStatus{TaskPaused, TaskFailed, TaskCancelled, TaskExpired, TaskCompleted} {
		t.Run(string(status), func(t *testing.T) {
			root := t.TempDir()
			s := reviewOpenStore(t, root)
			task := reviewCreateTask(t, s, "alice", reviewRequest("a", []byte("AB"), 1))
			reviewWrite(t, s, task, 0, []byte("A"))
			reviewExec(t, s, "UPDATE tasks SET status=? WHERE id=?", status, IDToHex(task.ID))
			_, err := s.WriteChunkAt(context.Background(), task.ID, 1, []byte("Z"))
			if err == nil {
				t.Error("inactive task write must fail")
			}
			// BUG: SQL 的 status 条件仅阻止更新数据库，文件在此之前已经被修改。
			if got := reviewReadFile(t, reviewPartPath(root, task.ID)); string(got) != "A" {
				t.Errorf("BUG: inactive task file changed to %q", got)
			}
			if got := reviewTask(t, s, task.ID); got.Offset != 1 || got.Status != status {
				t.Errorf("inactive task metadata changed: %+v", got)
			}
		})
	}
}

func TestReviewCancelledWritePreservesConfirmedBytes(t *testing.T) {
	root := t.TempDir()
	s := reviewOpenStore(t, root)
	task := reviewCreateTask(t, s, "alice", reviewRequest("a", []byte("AB"), 1))
	reviewWrite(t, s, task, 0, []byte("A"))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := s.WriteChunkAt(ctx, task.ID, 0, []byte("Z")); err == nil {
		t.Error("cancelled operation must fail")
	}
	// BUG: 取消的 context 只传给 SQL；已确认前缀可能在返回取消错误前被覆盖。
	if got := reviewReadFile(t, reviewPartPath(root, task.ID)); string(got) != "A" {
		t.Errorf("BUG: cancelled write corrupted acknowledged prefix: %q", got)
	}
	if got := reviewTask(t, s, task.ID); got.Offset != 1 {
		t.Errorf("cancelled write advanced checkpoint to %d", got.Offset)
	}
}

func TestReviewCheckpointSQLFailureCanRetry(t *testing.T) {
	root := t.TempDir()
	s := reviewOpenStore(t, root)
	task := reviewCreateTask(t, s, "alice", reviewRequest("a", []byte("AB"), 1))
	reviewWrite(t, s, task, 0, []byte("A"))
	reviewExec(t, s, `CREATE TRIGGER fail_checkpoint BEFORE UPDATE OF offset ON tasks BEGIN SELECT RAISE(FAIL,'checkpoint fault'); END`)
	if _, err := s.WriteChunkAt(context.Background(), task.ID, 1, []byte("B")); err == nil {
		t.Fatal("injected database failure must be returned")
	}
	if got := reviewTask(t, s, task.ID); got.Offset != 1 {
		t.Errorf("SQL failure acknowledged uncommitted bytes: %+v", got)
	}
	// 未确认尾部可留到恢复时截断；相同连续写入应能重试。
	reviewExec(t, s, "DROP TRIGGER fail_checkpoint")
	reviewWrite(t, s, task, 1, []byte("B"))
	if got := reviewReadFile(t, reviewPartPath(root, task.ID)); string(got) != "AB" {
		t.Errorf("retry content=%q", got)
	}
}

func TestReviewUnknownTaskDoesNotWriteExistingPart(t *testing.T) {
	root := t.TempDir()
	s := reviewOpenStore(t, root)
	id := protocol.TransferID{99}
	path := reviewPartPath(root, id)
	if err := os.WriteFile(path, []byte("safe"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.WriteChunkAt(context.Background(), id, 0, []byte("evil")); err == nil {
		t.Error("unknown task write must fail")
	}
	// BUG: 未查询任务存在性就打开同名 .part，错误返回前可破坏孤立文件。
	if got := reviewReadFile(t, path); string(got) != "safe" {
		t.Errorf("BUG: unknown task changed existing part: %q", got)
	}
}

func TestReviewValidChunkBoundaries(t *testing.T) {
	for _, size := range []int{0, 1, 4095, 4096, 4097, 65535, 65536, 65537} {
		t.Run(strconv.Itoa(size), func(t *testing.T) {
			s := reviewOpenStore(t, t.TempDir())
			content := bytes.Repeat([]byte{0xA5}, size)
			task := reviewCreateTask(t, s, "alice", reviewRequest("a", content, 1))
			for offset := 0; offset < size; {
				end := min(offset+4096, size)
				reviewWrite(t, s, task, uint64(offset), content[offset:end])
				offset = end
			}
			if err := s.CommitUpload(context.Background(), task.ID, uint64(size), task.FileSHA256); err != nil {
				t.Fatal(err)
			}
			if got := reviewTask(t, s, task.ID); got.Status != TaskCompleted || got.Offset != uint64(size) {
				t.Errorf("unexpected final state: %+v", got)
			}
		})
	}
}
