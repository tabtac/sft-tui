package pal

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

func TestSecureJoin(t *testing.T) {
	tempDir := t.TempDir()

	// 正常拼接
	p, err := SecureJoin(tempDir, "file.txt")
	if err != nil {
		t.Fatalf("expected success, got: %v", err)
	}
	if filepath.Dir(p) != tempDir {
		t.Errorf("expected parent to be %q, got: %q", tempDir, filepath.Dir(p))
	}

	// 试图逃逸（相对逃逸、绝对路径注入、Windows 盘符注入）
	escapes := []string{
		"../outside.txt",
		"sub/../../outside.txt",
		"/etc/passwd",
		"C:\\Windows\\System32",
		"D:/data.bin",
		"..",
	}
	for _, esc := range escapes {
		_, err := SecureJoin(tempDir, esc)
		if err == nil {
			t.Errorf("expected path traversal error for %q, got nil", esc)
		}
	}
}

func TestEnsureSecureDir(t *testing.T) {
	tempDir := t.TempDir()
	target := filepath.Join(tempDir, "secure_dir")

	if err := EnsureSecureDir(target); err != nil {
		t.Fatalf("EnsureSecureDir failed: %v", err)
	}

	info, err := os.Stat(target)
	if err != nil {
		t.Fatalf("os.Stat failed: %v", err)
	}
	if !info.IsDir() {
		t.Errorf("expected %q to be a directory", target)
	}
}

func TestOpenRootAndSecureSubdir(t *testing.T) {
	tempDir := t.TempDir()
	r, err := OpenRoot(tempDir)
	if err != nil {
		t.Fatalf("OpenRoot failed: %v", err)
	}
	defer r.Close()

	if err := r.EnsureSecureSubdir("subdir"); err != nil {
		t.Fatalf("EnsureSecureSubdir failed: %v", err)
	}
	// 再次调用收紧已有目录权限
	if err := r.EnsureSecureSubdir("subdir"); err != nil {
		t.Fatalf("EnsureSecureSubdir on existing failed: %v", err)
	}

	// 尝试向外部创建软链接作为子目录
	outside := t.TempDir()
	symSub := filepath.Join(tempDir, "sym_sub")
	if err := os.Symlink(outside, symSub); err == nil {
		if err := r.EnsureSecureSubdir("sym_sub"); err == nil {
			t.Error("expected EnsureSecureSubdir to fail on symlink")
		}
	}
}

func TestCopyFileAndStageAndReplace(t *testing.T) {
	tempDir := t.TempDir()
	src := filepath.Join(tempDir, "src.txt")
	dst := filepath.Join(tempDir, "dst.txt")

	// 复制不存在文件应报错
	if err := CopyFile(src, dst); err == nil {
		t.Error("expected error copying non-existent file, got nil")
	}

	// 写入源文件并复制
	testData := []byte("payload for copy test")
	if err := os.WriteFile(src, testData, 0600); err != nil {
		t.Fatalf("WriteFile failed: %v", err)
	}
	if err := CopyFile(src, dst); err != nil {
		t.Fatalf("CopyFile failed: %v", err)
	}
	copiedData, err := os.ReadFile(dst)
	if err != nil || string(copiedData) != string(testData) {
		t.Fatalf("copied data mismatch: got %q, err=%v", string(copiedData), err)
	}

	// StageAndReplace 测试
	stagedDst := filepath.Join(tempDir, "staged.txt")
	stageData := []byte("staged and atomic replaced content")
	if err := StageAndReplace(stagedDst, stageData); err != nil {
		t.Fatalf("StageAndReplace failed: %v", err)
	}
	gotData, err := os.ReadFile(stagedDst)
	if err != nil || string(gotData) != string(stageData) {
		t.Fatalf("staged content mismatch: got %q, err=%v", string(gotData), err)
	}
}

func TestAtomicReplaceAndLock(t *testing.T) {
	tempDir := t.TempDir()
	srcFile := filepath.Join(tempDir, "source.txt")
	dstFile := filepath.Join(tempDir, "dest.txt")

	if err := os.WriteFile(srcFile, []byte("content 1"), 0600); err != nil {
		t.Fatalf("failed to write source: %v", err)
	}

	// 测试原子替换
	if err := AtomicReplace(srcFile, dstFile); err != nil {
		t.Fatalf("AtomicReplace failed: %v", err)
	}

	data, err := os.ReadFile(dstFile)
	if err != nil {
		t.Fatalf("failed to read dest: %v", err)
	}
	if string(data) != "content 1" {
		t.Errorf("content mismatch: got %q", string(data))
	}

	// 再次替换已有文件
	if err := os.WriteFile(srcFile, []byte("content 2"), 0600); err != nil {
		t.Fatalf("failed to write source: %v", err)
	}
	if err := AtomicReplace(srcFile, dstFile); err != nil {
		t.Fatalf("AtomicReplace over existing failed: %v", err)
	}
	data, _ = os.ReadFile(dstFile)
	if string(data) != "content 2" {
		t.Errorf("content mismatch: got %q", string(data))
	}

	// 测试锁操作
	lockPath := filepath.Join(tempDir, "test.lock")
	f1, err := OpenLockFile(lockPath)
	if err != nil {
		t.Fatalf("OpenLockFile failed: %v", err)
	}
	defer f1.Close()

	if err := LockFile(f1); err != nil {
		t.Fatalf("LockFile f1 failed: %v", err)
	}

	// 第二个句柄加锁应该失败（非阻塞锁）
	f2, err := OpenLockFile(lockPath)
	if err == nil {
		defer f2.Close()
		if err := LockFile(f2); err == nil {
			t.Errorf("expected LockFile on f2 to fail when f1 holds the lock")
		}
	}

	// 解锁 f1
	if err := UnlockFile(f1); err != nil {
		t.Errorf("UnlockFile f1 failed: %v", err)
	}
}

func TestIPCListenDialAndErrors(t *testing.T) {
	tempDir := t.TempDir()
	endpoint := GetIPCPath(tempDir)

	// 测试 IsIPCNotFound
	if !IsIPCNotFound(os.ErrNotExist) {
		t.Error("expected true for ErrNotExist")
	}
	if !IsIPCNotFound(syscall.ECONNREFUSED) {
		t.Error("expected true for ECONNREFUSED")
	}
	if IsIPCNotFound(errors.New("other error")) {
		t.Error("expected false for generic error")
	}

	// 正常监听与通信测试
	l, err := ListenIPC(endpoint)
	if err != nil {
		t.Fatalf("ListenIPC failed: %v", err)
	}
	defer l.Close()

	done := make(chan struct{})
	go func() {
		defer close(done)
		conn, err := l.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		buf := make([]byte, 4)
		_, _ = conn.Read(buf)
		_, _ = conn.Write([]byte("pong"))
	}()

	conn, err := DialIPC(endpoint, 1*time.Second)
	if err != nil {
		t.Fatalf("DialIPC failed: %v", err)
	}
	defer conn.Close()

	_, _ = conn.Write([]byte("ping"))
	resp := make([]byte, 4)
	_, _ = conn.Read(resp)

	if string(resp) != "pong" {
		t.Errorf("IPC echo mismatch: expected pong, got %s", string(resp))
	}
	<-done

	// 测试在已有实例运行时重复监听
	ep2 := filepath.Join(tempDir, "ep2.sock")
	l2, err := ListenIPC(ep2)
	if err != nil {
		t.Fatalf("ListenIPC ep2 failed: %v", err)
	}
	defer l2.Close()

	go func() {
		c, aErr := l2.Accept()
		if aErr == nil && c != nil {
			c.Close()
		}
	}()
	time.Sleep(20 * time.Millisecond)

	if _, err := ListenIPC(ep2); err == nil {
		t.Error("expected ListenIPC to fail when another instance is already running")
	}

	// 测试非法 socket 路径报错（已有普通文件占用）
	badSocketFile := filepath.Join(tempDir, "regular_file.sock")
	_ = os.WriteFile(badSocketFile, []byte("not a socket"), 0600)
	_, err = ListenIPC(badSocketFile)
	if err == nil {
		t.Error("expected ListenIPC to fail on non-socket regular file")
	}
}
