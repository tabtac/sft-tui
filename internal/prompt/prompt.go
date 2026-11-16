// Package prompt 提供面向行交互的配置向导对话框（EOF 取消，直接回车采用默认值）。
package prompt

import (
	"bufio"
	"fmt"
	"io"
	"strconv"
	"strings"
)

type Dialog struct {
	in  *bufio.Reader
	out io.Writer
}

func New(in io.Reader, out io.Writer) *Dialog {
	reader, ok := in.(*bufio.Reader)
	if !ok {
		reader = bufio.NewReader(in)
	}
	return &Dialog{in: reader, out: out}
}

func (d *Dialog) Text(label, fallback string, validate func(string) error) (string, error) {
	for {
		fmt.Fprintf(d.out, "%s [%s]：", label, fallback)
		line, err := d.in.ReadString('\n')
		if err != nil {
			return "", fmt.Errorf("交互输入中断，未保存：%w", err)
		}
		value := strings.TrimSpace(line)
		if value == "" {
			value = fallback
		}
		if validate != nil {
			if err := validate(value); err != nil {
				fmt.Fprintln(d.out, "输入无效：", err)
				continue
			}
		}
		return value, nil
	}
}

func Nonempty(value string) error {
	if strings.TrimSpace(value) == "" {
		return fmt.Errorf("不得为空")
	}
	return nil
}

func (d *Dialog) Bool(label string, fallback bool) (bool, error) {
	defaultText := "n"
	if fallback {
		defaultText = "y"
	}
	value, err := d.Text(label+" (y/n)", defaultText, func(s string) error {
		switch strings.ToLower(s) {
		case "y", "yes", "n", "no":
			return nil
		}
		return fmt.Errorf("请输入 y 或 n")
	})
	return strings.EqualFold(value, "y") || strings.EqualFold(value, "yes"), err
}

func (d *Dialog) Uint(label string, fallback, low, high uint64) (uint64, error) {
	value, err := d.Text(label, strconv.FormatUint(fallback, 10), func(s string) error {
		n, err := strconv.ParseUint(s, 10, 64)
		if err != nil || n < low || n > high {
			return fmt.Errorf("请输入 %d–%d 的整数", low, high)
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	return strconv.ParseUint(value, 10, 64)
}

func (d *Dialog) Choose(label string, choices []string) (int, error) {
	if len(choices) == 0 {
		return 0, fmt.Errorf("没有可选项")
	}
	for i, choice := range choices {
		fmt.Fprintf(d.out, "  %d. %s\n", i+1, choice)
	}
	n, err := d.Uint(label, 1, 1, uint64(len(choices)))
	return int(n) - 1, err
}
