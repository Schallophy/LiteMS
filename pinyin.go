package main

import (
	"sort"
	"strings"

	"github.com/mozillazg/go-pinyin"
)

// 拼音搜索支持：中文标题转成「全拼 + 首字母」并入搜索键。
// 爱乐之城 → ailezhicheng / aiyuezhicheng / alzc / ayzc，
// 捕风追影 → bufengzhuiying / bfzy。
//
// 多音字按全部读音展开（乐 lè/yuè、长 zhǎng/cháng），否则 "aiyue" 搜不到
// 《爱乐之城》。组合数是各字读音数的乘积，超过上限时退回默认读音，
// 避免长标题把搜索键撑爆。

const maxPinyinVariants = 16

// pinyinVariants 返回字符串的拼音搜索段：全拼在前、首字母在后，各自有序。
// 纯英文/数字串只产出一个全拼（首字母没有信息量）。
func pinyinVariants(s string) []string {
	var alts [][]string
	han := 0
	for _, r := range strings.ToLower(s) {
		if r < 0x80 {
			if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
				alts = append(alts, []string{string(r)})
			}
			continue
		}
		args := pinyin.NewArgs()
		args.Heteronym = true
		py := pinyin.Pinyin(string(r), args)
		if len(py) == 0 || len(py[0]) == 0 {
			continue
		}
		alt := dedupeReadings(py[0])
		if len(alt) == 0 {
			continue
		}
		alts = append(alts, alt)
		han++
	}
	if han == 0 {
		var b strings.Builder
		for _, a := range alts {
			b.WriteString(a[0])
		}
		return []string{b.String()}
	}

	total := 1
	for _, a := range alts {
		total *= len(a)
		if total > maxPinyinVariants {
			total = 0
			break
		}
	}
	if total == 0 { // 变体太多：只取每个字的默认读音
		for i := range alts {
			alts[i] = alts[i][:1]
		}
	}

	fulls, inits := map[string]bool{}, map[string]bool{}
	var walk func(i int, full, ini string)
	walk = func(i int, full, ini string) {
		if i == len(alts) {
			fulls[full] = true
			inits[ini] = true
			return
		}
		for _, a := range alts[i] {
			walk(i+1, full+a, ini+string(a[0]))
		}
	}
	walk(0, "", "")

	out := make([]string, 0, len(fulls)+len(inits))
	for f := range fulls {
		out = append(out, f)
	}
	sort.Strings(out)
	ini := make([]string, 0, len(inits))
	for i := range inits {
		ini = append(ini, i)
	}
	sort.Strings(ini)
	return append(out, ini...)
}

// dedupeReadings 去重并保持拼音库给的优先顺序。
func dedupeReadings(rs []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(rs))
	for _, r := range rs {
		if r == "" || seen[r] {
			continue
		}
		seen[r] = true
		out = append(out, r)
	}
	return out
}
