package main

import (
	"strings"
	"testing"
)

const sampleSRT = "1\n00:00:01,600 --> 00:00:04,200\n敦刻尔克大撤退\n\n2\n00:00:05,000 --> 00:00:08,000\n我们能上去吗，先生？\n"

const sampleASS = "[Script Info]\nTitle: test\n\n[Events]\n" +
	"Format: Layer, Start, End, Style, Name, MarginL, MarginR, MarginV, Effect, Text\n" +
	"Dialogue: 0,0:00:10.00,0:00:14.50,Default,,0,0,0,,宇宙沉默得可怕\\N他们的声音无人回应\n" +
	"Dialogue: 0,0:00:15.00,0:00:18.00,Default,,0,0,0,,{\\i1}特效字幕测试{\\i0}第二行\n"

func TestSRTToVTT(t *testing.T) {
	out, err := toVTT([]byte(sampleSRT), ".srt", "chs")
	if err != nil {
		t.Fatal(err)
	}
	s := string(out)
	if !strings.Contains(s, "00:00:01.600 --> 00:00:04.200") {
		t.Errorf("时间戳未转换：%q", s)
	}
	if !strings.Contains(s, "敦刻尔克大撤退") {
		t.Errorf("文本丢失：%q", s)
	}
}

func TestASSToVTT(t *testing.T) {
	out, err := toVTT([]byte(sampleASS), ".ass", "cht")
	if err != nil {
		t.Fatal(err)
	}
	s := string(out)
	if !strings.Contains(s, "00:00:10.000 --> 00:00:14.500") {
		t.Errorf("ASS 时间轴未转换：%q", s)
	}
	if !strings.Contains(s, "宇宙沉默得可怕\n他们的声音无人回应") {
		t.Errorf("\\N 未转为换行：%q", s)
	}
	if strings.Contains(s, "{\\i1}") {
		t.Errorf("覆盖标签未剥离：%q", s)
	}
	if !strings.Contains(s, "WEBVTT") {
		t.Errorf("缺 WEBVTT 头：%q", s)
	}
}

func TestGBKDecode(t *testing.T) {
	// GBK 编码的「敦刻尔克大撤退」
	gbk := []byte{0xB6, 0xD8, 0xBF, 0xCC, 0xB6, 0xFB, 0xBF, 0xCB, 0xB4, 0xF3, 0xB3, 0xB7, 0xCD, 0xCB}
	got := decodeSubtitle(gbk, "chs")
	if got != "敦刻尔克大撤退" {
		t.Errorf("GBK 解码错误：%q", got)
	}
}

