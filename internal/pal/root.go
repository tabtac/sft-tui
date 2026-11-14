package pal

import (
	"fmt"
	"os"
	"path/filepath"
)

// Root 封装标准库 os.Root，提供锚定根目录的文件系统操作，从内核与系统调用层面阻止符号链接与路径穿越。
type Root struct {
	root *os.Root
	path string
}

// OpenRoot 打开并锁定指定的根目录，在打开前强制执行私有安全权限校验与符号链接排查。
func OpenRoot(path string) (*Root, error) {
	absPath, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("invalid path %q: %w", path, err)
	}
	if err := EnsurePrivateDir(absPath); err != nil {
		return nil, fmt.Errorf("failed to secure root directory %q: %w", absPath, err)
	}

	r, err := os.OpenRoot(absPath)
	if err != nil {
		return nil, fmt.Errorf("open root failed: %w", err)
	}

	return &Root{
		root: r,
		path: absPath,
	}, nil
}

// Path 返回根目录绝对路径
func (r *Root) Path() string {
	return r.path
}

// Close 关闭根目录句柄
func (r *Root) Close() error {
	if r.root == nil {
		return os.ErrClosed
	}
	err := r.root.Close()
	r.root = nil
	return err
}

// EnsureSecureSubdir 在根目录下安全创建或收紧子目录权限，若已有同名符号链接则强制报错拦截。
func (r *Root) EnsureSecureSubdir(sub string) error {
	info, err := r.root.Lstat(sub)
	if err == nil {
		// 已存在：必须排查是否为符号链接
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("security violation: sub-path %q is a symbolic link", sub)
		}
		if !info.IsDir() {
			return fmt.Errorf("security violation: sub-path %q is not a directory", sub)
		}
		// 收紧已有目录权限为私有访问
		return r.secureSubdir(sub)
	}

	if !os.IsNotExist(err) {
		return err
	}

	// 不存在则创建
	if err := r.root.Mkdir(sub, 0700); err != nil {
		return err
	}

	// 创建后二次检验防御竞态
	info, err = r.root.Lstat(sub)
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		_ = r.root.Remove(sub)
		return fmt.Errorf("security violation: created sub-path %q is not a regular directory", sub)
	}
	return r.secureSubdir(sub)
}

// OpenFile 锚定在根目录内安全打开文件
func (r *Root) OpenFile(name string, flag int, perm os.FileMode) (*os.File, error) {
	return r.root.OpenFile(name, flag, perm)
}

// Open 锚定只读打开
func (r *Root) Open(name string) (*os.File, error) {
	return r.root.Open(name)
}

// Lstat 锚定查询，不跟随符号链接
func (r *Root) Lstat(name string) (os.FileInfo, error) {
	return r.root.Lstat(name)
}

// Stat 锚定查询
func (r *Root) Stat(name string) (os.FileInfo, error) {
	return r.root.Stat(name)
}

// Mkdir 锚定创建目录
func (r *Root) Mkdir(name string, perm os.FileMode) error {
	return r.root.Mkdir(name, perm)
}

// MkdirAll 锚定递归创建目录
func (r *Root) MkdirAll(name string, perm os.FileMode) error {
	return r.root.MkdirAll(name, perm)
}

// Remove 锚定删除文件或空目录
func (r *Root) Remove(name string) error {
	return r.root.Remove(name)
}

// Rename 锚定在根目录下原子重命名文件
func (r *Root) Rename(oldName, newName string) error {
	return r.root.Rename(oldName, newName)
}

// Link 锚定在根目录下创建硬链接
func (r *Root) Link(oldName, newName string) error {
	return r.root.Link(oldName, newName)
}
