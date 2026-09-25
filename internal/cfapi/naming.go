package cfapi

import (
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// placeholder 匹配 {n}、{n:3}、{i}、{i:2} 这几种写法。
var placeholder = regexp.MustCompile(`\{(n|i)(?::(\d+))?\}`)

// HasPlaceholder 判断模板里带不带序号占位符。
// 不带的话整批地址会落到同一个名称上，也就是 DNS 轮询。
func HasPlaceholder(tpl string) bool {
	return placeholder.MatchString(tpl)
}

var nameRe = regexp.MustCompile(`^[a-z0-9_]([a-z0-9_-]*[a-z0-9_])?$`)

// ExpandNames 按模板给 count 个地址生成完整的记录名。
//
// 模板里 {n} 从 1 开始、{i} 从 0 开始，冒号后面是补零宽度，比如 {n:3} 出 001。
// 名字没写到顶级域就自动补上 zone；写 @ 或留空表示解析到根域。
func ExpandNames(tpl, zone string, count int) ([]string, error) {
	zone = strings.ToLower(strings.Trim(strings.TrimSpace(zone), "."))
	if zone == "" {
		return nil, errors.New("没选站点，不知道该挂到哪个域名下")
	}
	if count <= 0 {
		return nil, errors.New("数量必须大于 0")
	}
	tpl = strings.TrimSpace(tpl)
	if tpl == "" {
		tpl = "@"
	}

	out := make([]string, 0, count)
	for i := 0; i < count; i++ {
		expanded := placeholder.ReplaceAllStringFunc(tpl, func(m string) string {
			sub := placeholder.FindStringSubmatch(m)
			v := i
			if sub[1] == "n" {
				v = i + 1
			}
			if sub[2] != "" {
				if width, err := strconv.Atoi(sub[2]); err == nil && width > 0 && width <= 12 {
					return fmt.Sprintf("%0*d", width, v)
				}
			}
			return strconv.Itoa(v)
		})
		full, err := Qualify(expanded, zone)
		if err != nil {
			return nil, err
		}
		out = append(out, full)
	}
	return out, nil
}

// Qualify 把一个可能是相对写法的名字补成完整域名并做合法性检查。
func Qualify(name, zone string) (string, error) {
	zone = strings.ToLower(strings.Trim(strings.TrimSpace(zone), "."))
	name = strings.ToLower(strings.Trim(strings.TrimSpace(name), "."))
	if name == "" || name == "@" {
		return zone, nil
	}
	full := name
	if full != zone && !strings.HasSuffix(full, "."+zone) {
		full = name + "." + zone
	}
	if len(full) > 253 {
		return "", fmt.Errorf("域名太长: %s", full)
	}
	for _, label := range strings.Split(full, ".") {
		if label == "" {
			return "", fmt.Errorf("域名里有空的一段: %s", full)
		}
		if len(label) > 63 {
			return "", fmt.Errorf("域名有一段超过 63 字符: %s", label)
		}
		if label == "*" {
			continue
		}
		if !nameRe.MatchString(label) {
			return "", fmt.Errorf("域名里有非法字符: %s", label)
		}
	}
	return full, nil
}
