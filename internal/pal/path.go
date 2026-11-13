package pal

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// ErrPathTraversal 路径穿越错误
var ErrPathTraversal = errors.New("path traversal detected")

// SecureJoin 严格校验并拼接基准目录与相对路径，确保结果严格位于 baseDir 之内。
// 防御 "../" 穿越、绝对路径注入、Windows 盘符注入以及特殊相对路径。
func SecureJoin(baseDir, relPath string) (string, error) {
	cleanBase, err := filepath.Abs(baseDir)
	if err != nil {
		return "", fmt.Errorf("invalid base directory: %w", err)
	}

	// 统一将反斜杠替换为正斜杠，使 Linux 与 Windows 都能统一识别
	normalized := strings.ReplaceAll(relPath, "\\", "/")

	// 拦截 Windows 盘符（如 "C:/Windows"）
	if len(normalized) >= 2 && normalized[1] == ':' {
		c := normalized[0]
		if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') {
			return "", fmt.Errorf("%w: path %q contains volume or drive letter", ErrPathTraversal, relPath)
		}
	}

	// 拦截以 "/" 开头的绝对路径
	if strings.HasPrefix(normalized, "/") {
		return "", fmt.Errorf("%w: absolute path %q is prohibited", ErrPathTraversal, relPath)
	}

	cleanRel := filepath.Clean(normalized)
	if strings.HasPrefix(cleanRel, "..") || cleanRel == ".." {
		return "", fmt.Errorf("%w: path %q attempts to escape base %q", ErrPathTraversal, relPath, baseDir)
	}

	fullPath := filepath.Join(cleanBase, filepath.FromSlash(cleanRel))
	rel, err := filepath.Rel(cleanBase, fullPath)
	if err != nil || strings.HasPrefix(rel, "..") || rel == ".." {
		return "", fmt.Errorf("%w: resolved path %q is outside base %q", ErrPathTraversal, fullPath, cleanBase)
	}

	return fullPath, nil
}

// EnsureSecureDir 确保创建私有目录（权限 0700）
func EnsureSecureDir(dir string) error {
	return os.MkdirAll(dir, 0700)
}
