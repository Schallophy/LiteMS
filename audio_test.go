package main

import "testing"

// 文件名里的音频标签识别。bad = 主流浏览器内核不带解码器（DTS/TrueHD/Atmos/PCM），
// maybe = 视浏览器而定（AC3/EAC3/DDP），"" = 无标注不提示。
func TestAudioHint(t *testing.T) {
	cases := []struct {
		name string
		want string
	}{
		// bad 组
		{"Interstellar (2014) Remux-1080p DTS-HD MA 5.1.mkv", "bad"},
		{"Movie.2018.2160p.UHD.BluRay.TrueHD.7.1.Atmos.mkv", "bad"},
		{"Film.2020.DTSHD.x264.mkv", "bad"},
		{"Film.2020.DTSMA.mkv", "bad"},
		{"film.dts.x264.mkv", "bad"},
		{"Movie.PCM.2.0.mkv", "bad"},
		{" LPCM.mkv", "bad"},
		{"电影.TrueHD.5.1.中字.mkv", "bad"},
		// maybe 组
		{"捕风追影.2025.1080p.WEB-DL.DDP5.1.H.264.mkv", "maybe"},
		{"Movie.2019.1080p.DD+5.1.mkv", "maybe"},
		{"Movie.2019.1080p.DD5.1.mkv", "maybe"},
		{"Film.2020.EAC3.mkv", "maybe"},
		{"Film.2020.E-AC3.mkv", "maybe"},
		{"film.ac3.mkv", "maybe"},
		{"Movie.Dolby.Digital.mkv", "maybe"},
		// 无标注：不提示
		{"Movie.2019.1080p.AAC.H.264.mp4", ""},
		{"Interstellar (2014).mkv", ""},
		{"捕风追影 (2025).mkv", ""},
		{"Film.2020.FLAC.mkv", ""},
		{"Film.2020.Opus.mkv", ""},
		// 误伤检查：这些普通词不该命中
		{"The.Third.Man.1949.mkv", ""},              // third 不含标签
		{"Kids.1995.mkv", ""},                       // 无标签
		{"Nottingham.2010.x264.mkv", ""},            // ham→无
		{"Broadcast.News.1987.mkv", ""},             // ast 不带边界前的 ac3
		{"Head.On.2004.German.DL.PAL.DVDR.mkv", ""}, // head/on 无
	}
	for _, c := range cases {
		if got := audioHint(c.name); got != c.want {
			t.Errorf("audioHint(%q) = %q, want %q", c.name, got, c.want)
		}
	}
}

// 容器名也参与识别（Some releases only carry the tag in extension or dir name）。
func TestAudioHintContainer(t *testing.T) {
	if got := audioHint("Movies/Interstellar (2014)/movie.mkv", "mkv dts"); got != "bad" {
		t.Errorf("container hint = %q, want bad", got)
	}
}
