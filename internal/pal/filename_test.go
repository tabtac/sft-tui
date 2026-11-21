package pal

import "testing"

func TestValidateAndSanitizeWindowsFilename(t *testing.T) {
	invalid := []string{
		"",
		".",
		"..",
		"CON",
		"con",
		"NUL.txt",
		"prn.tar.gz",
		"AUX",
		"COM1",
		"com9.log",
		"LPT1",
		"lpt9.bin",
		"report:2026.txt",
		"a?.txt",
		"a*b.txt",
		`a"b.txt`,
		"a<b.txt",
		"a>b.txt",
		"a|b.txt",
		"trail.",
		"trail ",
		"ctrl\x01.txt",
	}
	for _, name := range invalid {
		if err := ValidateWindowsFilename(name); err == nil {
			t.Errorf("expected ValidateWindowsFilename(%q) to fail", name)
		}
		sanitized := SanitizeWindowsFilename(name)
		if err := ValidateWindowsFilename(sanitized); err != nil {
			t.Errorf("SanitizeWindowsFilename(%q) = %q is still invalid: %v", name, sanitized, err)
		}
	}

	valid := []string{
		"normal.txt",
		"中文报告-2026.pdf",
		"console.txt",
		"COM10.txt",
		"null_file.bin",
		".hidden",
	}
	for _, name := range valid {
		if err := ValidateWindowsFilename(name); err != nil {
			t.Errorf("expected ValidateWindowsFilename(%q) to pass, got %v", name, err)
		}
	}
}
