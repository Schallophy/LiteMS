package main

import (
	"strings"

	"github.com/mozillazg/go-pinyin"
)

// 拼音搜索支持：中文标题转成「全拼 + 首字母」并入搜索键。
// 爱乐之城 → ailezhicheng / alzc，捕风追影 → bufengzhuiying / bfzy。
// 只覆盖汉字单读音（多音字取默认读音，乐→le），非汉字按折叠规则保留字母数字。

var pyArgs = pinyin.NewArgs() // 默认 Normal 风格、单读音

// pinyinParts 返回字符串的全拼与首字母（非汉字只保留字母数字，标点空白丢弃）。
// 首字母只对含汉字的串有意义：纯英文串的 full 已覆盖，不再产出 "lll" 这种噪音。
func pinyinParts(s string) (full, initials string) {
	var fb, ib strings.Builder
	han := 0
	for _, r := range strings.ToLower(s) {
		if r < 0x80 {
			if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
				fb.WriteRune(r)
			}
			continue
		}
		py := pinyin.Pinyin(string(r), pyArgs)
		if len(py) == 0 || len(py[0]) == 0 || py[0][0] == "" {
			continue
		}
		syl := py[0][0]
		fb.WriteString(syl)
		ib.WriteRune(rune(syl[0]))
		han++
	}
	if han == 0 {
		return fb.String(), ""
	}
	return fb.String(), ib.String()
}
