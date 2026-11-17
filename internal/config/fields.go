package config

import (
	"fmt"
	"reflect"
	"strconv"
	"strings"
)

// NumericField 将配置结构体的 json 字段名与其真实无符号整型位宽绑定，用于界面与向导表单校验。
type NumericField struct {
	Path  string
	value reflect.Value
}

func (f NumericField) Uint() uint64 { return f.value.Uint() }
func (f NumericField) Max() uint64  { return ^uint64(0) >> (64 - f.value.Type().Bits()) }

func (f NumericField) Set(text string) error {
	n, err := strconv.ParseUint(strings.TrimSpace(text), 10, f.value.Type().Bits())
	if err != nil {
		return fmt.Errorf("%s 需要 %d 位非负整数：%w", f.Path, f.value.Type().Bits(), err)
	}
	f.value.SetUint(n)
	return nil
}

func NumericFields(prefix string, target any) []NumericField {
	v := reflect.ValueOf(target).Elem()
	out := make([]NumericField, 0, v.NumField())
	for i := 0; i < v.NumField(); i++ {
		out = append(out, NumericField{prefix + "." + v.Type().Field(i).Tag.Get("json"), v.Field(i)})
	}
	return out
}

func ServerNumericFields(c *Server) []NumericField {
	var out []NumericField
	for _, g := range []struct {
		name  string
		value any
	}{
		{"limits", &c.Limits},
		{"timeouts", &c.Timeouts},
		{"resources", &c.Resources},
		{"bandwidth", &c.Bandwidth},
		{"logging", &c.Logging},
	} {
		out = append(out, NumericFields(g.name, g.value)...)
	}
	return out
}
