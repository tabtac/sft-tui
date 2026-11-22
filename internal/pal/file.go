package pal

import (
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"unicode/utf8"
)

// DigestReaderContext 流式计算输入数据的 SHA-256 与字节数，支持通过 ctx 中断取消。
func DigestReaderContext(ctx context.Context, r io.Reader) ([32]byte, uint64, error) {
	hasher := sha256.New()
	buf := make([]byte, 64*1024)
	var total uint64
	for {
		if err := ctx.Err(); err != nil {
			return [32]byte{}, total, err
		}
		n, err := r.Read(buf)
		if n > 0 {
			_, _ = hasher.Write(buf[:n])
			total += uint64(n)
		}
		if err == io.EOF {
			var sum [32]byte
			copy(sum[:], hasher.Sum(nil))
			return sum, total, nil
		}
		if err != nil {
			return [32]byte{}, total, err
		}
	}
}

// ReadSmall 安全读取有大小上限的普通文件（使用 O_NOFOLLOW/无重解析点打开，消除 Lstat->Open TOCTOU）。
func ReadSmall(path string, maxBytes int64) ([]byte, error) {
	f, err := OpenRegularNoFollow(path, os.O_RDONLY, 0)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("file must be regular: %s", path)
	}
	if info.Size() > maxBytes {
		return nil, fmt.Errorf("file exceeds max allowed size (%d > %d): %s", info.Size(), maxBytes, path)
	}
	data, err := io.ReadAll(io.LimitReader(f, maxBytes+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > maxBytes {
		return nil, fmt.Errorf("file exceeds max allowed size: %s", path)
	}
	return data, nil
}

// CopyFile 安全复制文件内容并落盘同步
func CopyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	inInfo, err := in.Stat()
	if err != nil {
		return err
	}
	if !inInfo.Mode().IsRegular() {
		return fmt.Errorf("source must be a regular file: %s", src)
	}

	if dstInfo, err := os.Stat(dst); err == nil {
		if os.SameFile(inInfo, dstInfo) {
			return fmt.Errorf("source and destination point to the same file: %s", src)
		}
	}

	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0600)
	if err != nil {
		return err
	}
	return syncAndCloseCopy(out, in)
}

func syncAndCloseCopy(out *os.File, in *os.File) error {
	closed := false
	defer func() {
		if !closed {
			_ = out.Close()
		}
	}()

	if _, err := io.Copy(out, in); err != nil {
		return err
	}
	if err := out.Sync(); err != nil {
		return err
	}
	closed = true
	return out.Close()
}

// CheckRegularOrAbsent 校验路径若存在则必须是普通文件（严禁符号链接或目录等非常规文件）
func CheckRegularOrAbsent(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return fmt.Errorf("security violation: %q must be a regular file", path)
	}
	return nil
}

// TruncateUTF8 按字节上限截断 UTF-8 字符串，保持字符边界完整，超长时追加省略号。
func TruncateUTF8(s string, maxBytes int) string {
	if maxBytes <= 0 {
		return ""
	}
	if len(s) <= maxBytes {
		return s
	}
	cut := maxBytes
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + "…"
}

// StageTempFile 在目标同目录下写入临时文件、设置权限并 Sync 落盘，返回临时文件路径。
func StageTempFile(dst string, data []byte, perm os.FileMode) (string, error) {
	if err := CheckRegularOrAbsent(dst); err != nil {
		return "", err
	}
	tmp, err := os.CreateTemp(filepath.Dir(dst), ".stage-*")
	if err != nil {
		return "", err
	}
	tmpName := tmp.Name()
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.Remove(tmpName)
		}
	}()

	if perm != 0 {
		if err := tmp.Chmod(perm); err != nil {
			_ = tmp.Close()
			return "", err
		}
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return "", err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return "", err
	}
	if err := tmp.Close(); err != nil {
		return "", err
	}
	cleanup = false
	return tmpName, nil
}

// StageAndReplaceMode 按指定权限原子写入目标文件；exclusive=true 时若目标已存在则返回错误。
func StageAndReplaceMode(dst string, data []byte, perm os.FileMode, exclusive bool) error {
	tmpName, err := StageTempFile(dst, data, perm)
	if err != nil {
		return err
	}
	defer os.Remove(tmpName)

	if exclusive {
		if err := os.Link(tmpName, dst); err != nil {
			return err
		}
		return SyncParentDir(dst)
	}
	return AtomicReplace(tmpName, dst)
}

// StageAndReplace 以私有权限（0600）写入临时数据并原子覆盖目标文件。
func StageAndReplace(dst string, data []byte) error {
	return StageAndReplaceMode(dst, data, 0600, false)
}
