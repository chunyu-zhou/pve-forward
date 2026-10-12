// Package ports 解析端口分配：单个 / 多个 / 范围。
package ports

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
)

const MaxPerAlloc = 10000

// Parse 解析分配规格。kind: single（"10022"）、multi（"8001,8002"）、range（"20000-20009"）。
// 返回规范化后的规格文本与展开的端口列表。
func Parse(kind, spec string) (string, []int, error) {
	spec = strings.TrimSpace(strings.ReplaceAll(spec, "，", ","))
	switch kind {
	case "single":
		p, err := one(spec)
		if err != nil {
			return "", nil, err
		}
		return strconv.Itoa(p), []int{p}, nil
	case "multi":
		set := map[int]bool{}
		for _, f := range strings.FieldsFunc(spec, func(r rune) bool { return r == ',' || r == ' ' || r == '\n' }) {
			p, err := one(f)
			if err != nil {
				return "", nil, err
			}
			set[p] = true
		}
		if len(set) == 0 {
			return "", nil, fmt.Errorf("请至少输入一个端口")
		}
		var ps []int
		for p := range set {
			ps = append(ps, p)
		}
		sort.Ints(ps)
		if len(ps) > MaxPerAlloc {
			return "", nil, fmt.Errorf("单次最多分配 %d 个端口", MaxPerAlloc)
		}
		ss := make([]string, len(ps))
		for i, p := range ps {
			ss[i] = strconv.Itoa(p)
		}
		return strings.Join(ss, ","), ps, nil
	case "range":
		a, b, ok := strings.Cut(spec, "-")
		if !ok {
			return "", nil, fmt.Errorf("范围格式应为 起始-结束")
		}
		from, err := one(a)
		if err != nil {
			return "", nil, err
		}
		to, err := one(b)
		if err != nil {
			return "", nil, err
		}
		if from > to {
			return "", nil, fmt.Errorf("起始端口不能大于结束端口")
		}
		if to-from+1 > MaxPerAlloc {
			return "", nil, fmt.Errorf("单次最多分配 %d 个端口", MaxPerAlloc)
		}
		ps := make([]int, 0, to-from+1)
		for p := from; p <= to; p++ {
			ps = append(ps, p)
		}
		return fmt.Sprintf("%d-%d", from, to), ps, nil
	}
	return "", nil, fmt.Errorf("未知的分配方式 %q", kind)
}

func one(s string) (int, error) {
	p, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil || p < 1 || p > 65535 {
		return 0, fmt.Errorf("端口 %q 无效（1-65535）", strings.TrimSpace(s))
	}
	return p, nil
}
