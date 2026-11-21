package pal

import (
	"fmt"
	"runtime"
	"strings"
)

var windowsReservedNames = map[string]struct{}{
	"CON": {}, "PRN": {}, "AUX": {}, "NUL": {},
	"COM1": {}, "COM2": {}, "COM3": {}, "COM4": {}, "COM5": {}, "COM6": {}, "COM7": {}, "COM8": {}, "COM9": {},
	"LPT1": {}, "LPT2": {}, "LPT3": {}, "LPT4": {}, "LPT5": {}, "LPT6": {}, "LPT7": {}, "LPT8": {}, "LPT9": {},
}

func isWindowsIllegalRune(r rune) bool {
	if r < 0x20 {
		return true
	}
	switch r {
	case '<', '>', ':', '"', '/', '\\', '|', '?', '*':
		return true
	default:
		return false
	}
}

func isWindowsReservedBase(name string) bool {
	stem := name
	if dot := strings.IndexByte(stem, '.'); dot >= 0 {
		stem = stem[:dot]
	}
	stem = strings.TrimRight(stem, " ")
	_, reserved := windowsReservedNames[strings.ToUpper(stem)]
	return reserved
}

// ValidateWindowsFilename 校验单级文件名是否可安全保存为 Windows 本地普通文件
// （拒绝非法字符、NTFS ADS 冒号、尾随点/空格以及 CON/PRN/AUX/NUL/COM1-9/LPT1-9 等保留设备名）。
func ValidateWindowsFilename(name string) error {
	if name == "" || name == "." || name == ".." {
		return fmt.Errorf("本地文件名不能为空或目录保留名：%q", name)
	}
	if strings.HasSuffix(name, ".") || strings.HasSuffix(name, " ") {
		return fmt.Errorf("Windows 本地文件名不能以空格或点结尾：%q；请使用 -save-as 或改名另存", name)
	}
	for _, r := range name {
		if isWindowsIllegalRune(r) {
			return fmt.Errorf("Windows 本地文件名包含非法字符 %q（%s）；请使用 -save-as 或改名另存", r, name)
		}
	}
	if isWindowsReservedBase(name) {
		return fmt.Errorf("Windows 本地文件名不能使用系统保留设备名（%s）；请使用 -save-as 或改名另存", name)
	}
	return nil
}

// ValidateLocalFilename 根据当前操作系统规则校验单级本地下载文件名。
func ValidateLocalFilename(name string) error {
	if runtime.GOOS == "windows" {
		return ValidateWindowsFilename(name)
	}
	if name == "" || name == "." || name == ".." || strings.ContainsAny(name, "/\x00") {
		return fmt.Errorf("非法本地文件名：%q", name)
	}
	return nil
}

// SanitizeWindowsFilename 将逻辑远端文件名映射为安全的 Windows 本地文件名。
func SanitizeWindowsFilename(name string) string {
	var b strings.Builder
	b.Grow(len(name))
	for _, r := range name {
		if isWindowsIllegalRune(r) {
			b.WriteByte('_')
		} else {
			b.WriteRune(r)
		}
	}
	cleaned := strings.TrimRight(b.String(), ". ")
	if cleaned == "" || cleaned == "." || cleaned == ".." {
		return "_file"
	}
	if isWindowsReservedBase(cleaned) {
		cleaned = "_" + cleaned
	}
	return cleaned
}
