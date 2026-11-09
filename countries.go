package main

// 国家/地区的中文名映射。
//
// TMDB 不对国家做本地化：电影返回 production_countries（ISO 码 + 英文名），
// 剧集返回 origin_country（ISO 码），中文用户看到的永远是 "Hong Kong"、"United States"。
// 刮削时在这里映射成中文写进 NFO——NFO 是唯一真相，Kodi / Jellyfin 读到的
// 也一起变中文。查不到的条目原样保留英文，宁可显示原名也不猜。

import "strings"

// isoToZh 覆盖 TMDB 上实际出现过的国家/地区（ISO 3166-1）。
var isoToZh = map[string]string{
	"CN": "中国", "HK": "中国香港", "MO": "中国澳门", "TW": "中国台湾",
	"US": "美国", "GB": "英国", "JP": "日本", "KR": "韩国", "KP": "朝鲜",
	"FR": "法国", "DE": "德国", "IT": "意大利", "ES": "西班牙", "PT": "葡萄牙",
	"RU": "俄罗斯", "CA": "加拿大", "MX": "墨西哥", "BR": "巴西", "AR": "阿根廷",
	"AU": "澳大利亚", "NZ": "新西兰", "IN": "印度", "TH": "泰国", "VN": "越南",
	"SG": "新加坡", "MY": "马来西亚", "ID": "印度尼西亚", "PH": "菲律宾",
	"IE": "爱尔兰", "NL": "荷兰", "BE": "比利时", "CH": "瑞士", "AT": "奥地利",
	"SE": "瑞典", "NO": "挪威", "DK": "丹麦", "FI": "芬兰", "IS": "冰岛",
	"PL": "波兰", "CZ": "捷克", "SK": "斯洛伐克", "HU": "匈牙利", "GR": "希腊",
	"TR": "土耳其", "EG": "埃及", "ZA": "南非", "NG": "尼日利亚", "IL": "以色列",
	"IR": "伊朗", "SA": "沙特阿拉伯", "AE": "阿联酋", "UA": "乌克兰",
	"RO": "罗马尼亚", "MC": "摩纳哥", "LU": "卢森堡", "MT": "马耳他",
	"PA": "巴拿马", "CU": "古巴", "JM": "牙买加", "KE": "肯尼亚", "MA": "摩洛哥",
	"PK": "巴基斯坦", "BD": "孟加拉国", "LK": "斯里兰卡", "MN": "蒙古",
	"NP": "尼泊尔", "KH": "柬埔寨", "LA": "老挝", "MM": "缅甸",
	"KZ": "哈萨克斯坦", "GE": "格鲁吉亚", "AM": "亚美尼亚", "CL": "智利",
	"CO": "哥伦比亚", "PE": "秘鲁", "VE": "委内瑞拉", "UY": "乌拉圭",
}

// nameToZh 兜底：个别数据只有英文名没有 ISO 码时按英文名映射。
var nameToZh = map[string]string{
	"China":            "中国",
	"Hong Kong":        "中国香港",
	"Macao":            "中国澳门",
	"Taiwan":           "中国台湾",
	"United States":    "美国",
	"United Kingdom":   "英国",
	"South Korea":      "韩国",
	"North Korea":      "朝鲜",
	"Czech Republic":   "捷克",
	"Russia":           "俄罗斯",
	"Iran":             "伊朗",
	"Vietnam":          "越南",
	"New Zealand":      "新西兰",
	"Saudi Arabia":     "沙特阿拉伯",
	"United Arab Emir.": "阿联酋",
}

// zhCountry 把国家映射成中文名。优先 ISO 码（无歧义），再英文名，都不认识就原样返回。
func zhCountry(code, name string) string {
	if zh, ok := isoToZh[strings.ToUpper(strings.TrimSpace(code))]; ok {
		return zh
	}
	if zh, ok := nameToZh[strings.TrimSpace(name)]; ok {
		return zh
	}
	return strings.TrimSpace(name)
}
