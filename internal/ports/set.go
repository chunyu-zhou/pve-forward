package ports

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// Set 是由单个端口与端口范围组成的集合，如 "22,8006,10000-20000"。
// 用于节点的可分配端口范围和保留端口。
type Set struct{ spans [][2]int } // 已排序、互不重叠的闭区间

// ParseSet 解析以逗号、空格或换行分隔的端口与范围；空字符串得到空集合。
// 重叠或相邻的部分会合并，String() 返回规范化后的文本。
func ParseSet(spec string) (Set, error) {
	var spans [][2]int
	for _, f := range strings.FieldsFunc(strings.ReplaceAll(spec, "，", ","), func(r rune) bool {
		return r == ',' || r == ' ' || r == '\n' || r == '\t' || r == '\r' || r == '、'
	}) {
		a, b, isRange := strings.Cut(f, "-")
		from, err := one(a)
		if err != nil {
			return Set{}, err
		}
		to := from
		if isRange {
			if to, err = one(b); err != nil {
				return Set{}, err
			}
			if from > to {
				return Set{}, fmt.Errorf("范围 %s 的起始端口大于结束端口", f)
			}
		}
		spans = append(spans, [2]int{from, to})
	}
	sort.Slice(spans, func(i, j int) bool { return spans[i][0] < spans[j][0] })
	var merged [][2]int
	for _, s := range spans {
		if n := len(merged); n > 0 && s[0] <= merged[n-1][1]+1 {
			merged[n-1][1] = max(merged[n-1][1], s[1])
			continue
		}
		merged = append(merged, s)
	}
	return Set{merged}, nil
}

// MustSet 解析已规范化（保存前已校验）的文本，出错时返回空集合。
func MustSet(spec string) Set {
	s, _ := ParseSet(spec)
	return s
}

// Contains 判断端口是否在集合中。
func (s Set) Contains(p int) bool {
	i := sort.Search(len(s.spans), func(i int) bool { return s.spans[i][1] >= p })
	return i < len(s.spans) && s.spans[i][0] <= p
}

// Empty 判断集合是否为空。
func (s Set) Empty() bool { return len(s.spans) == 0 }

// Count 返回集合中的端口数量。
func (s Set) Count() int {
	n := 0
	for _, sp := range s.spans {
		n += sp[1] - sp[0] + 1
	}
	return n
}

// CountExcept 返回集合中不在 other 内的端口数量。
func (s Set) CountExcept(other Set) int {
	n := 0
	for _, sp := range s.spans {
		for p := sp[0]; p <= sp[1]; p++ {
			if !other.Contains(p) {
				n++
			}
		}
	}
	return n
}

// String 返回规范化文本，如 "22,8006,10000-20000"。
func (s Set) String() string {
	parts := make([]string, len(s.spans))
	for i, sp := range s.spans {
		if sp[0] == sp[1] {
			parts[i] = strconv.Itoa(sp[0])
		} else {
			parts[i] = fmt.Sprintf("%d-%d", sp[0], sp[1])
		}
	}
	return strings.Join(parts, ",")
}

// FromList 由端口列表生成集合（连续端口合并为范围）。
func FromList(ps []int) Set {
	ss := make([]string, len(ps))
	for i, p := range ps {
		ss[i] = strconv.Itoa(p)
	}
	return MustSet(strings.Join(ss, ","))
}
