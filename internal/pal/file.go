package pal

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
)

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

// StageAndReplace 写入临时数据并原子发布到最终目标
func StageAndReplace(dst string, data []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(dst), ".stage-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)

	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}

	return AtomicReplace(tmpName, dst)
}
