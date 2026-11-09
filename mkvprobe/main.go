// mkvprobe 是一次性诊断工具：解析 MKV 的 Tracks 段，打印每条轨的
// 类型 / CodecID / 语言 / 名称 / 默认标志 / 声道数 / 采样率。
// 用法：go run ./mkvprobe <文件.mkv>
package main

import (
	"bytes"
	"fmt"
	"io"
	"math"
	"os"
)

var masterIDs = map[uint64]bool{
	0x1a45dfa3: true, // EBML header
	0x18538067: true, // Segment
	0x114d9b74: true, // MetaSeek
	0x1549a966: true, // Info
	0x1f43b675: true, // Cluster
	0xa0:       true, // BlockGroup
	0x1c53bb6b: true, // Cues
	0x1043a770: true, // Chapters
	0x1254c367: true, // Tags
	0x1941a469: true, // Attachments
}

func readVint(r io.Reader, keepMarker bool) (uint64, int, error) {
	var b [8]byte
	if _, err := io.ReadFull(r, b[:1]); err != nil {
		return 0, 0, err
	}
	first := b[0]
	if first == 0 {
		return 0, 0, fmt.Errorf("bad vint")
	}
	length := 0
	mask := byte(0x80)
	for mask&first == 0 {
		length++
		mask >>= 1
	}
	length++ // 1..8
	val := uint64(first)
	if !keepMarker {
		val &= uint64(mask - 1) // 清掉前置 1 位
	}
	if length > 1 {
		if _, err := io.ReadFull(r, b[1:length]); err != nil {
			return 0, 0, err
		}
		for i := 1; i < length; i++ {
			val = val<<8 | uint64(b[i])
		}
	}
	return val, length, nil
}

type Track struct {
	Number   uint64
	Type     uint64
	CodecID  string
	Lang     string
	Name     string
	Default  bool
	Channels uint64
	Rate     float64
}

var found bool

func scan(r io.Reader, inTracks bool) error {
	for {
		id, _, err := readVint(r, true)
		if err != nil {
			return nil // EOF 即结束
		}
		size, _, err := readVint(r, false)
		if err != nil {
			return nil
		}
		unknown := size == (uint64(1)<<uint64(7*(8-1)))-1 // all-ones 简化判断够用
		switch {
		case id == 0x1654ae6b: // Tracks
			found = true
			if err := scanTracks(r, size, unknown); err != nil {
				return err
			}
			if !unknown {
				return nil
			}
		case inTracks:
			body := make([]byte, size)
			if _, err := io.ReadFull(r, body); err != nil {
				return nil
			}
		case masterIDs[id] || unknown:
			if unknown && !masterIDs[id] {
				return nil // 非已知 master 且尺寸未知，无法跳过，放弃
			}
			if err := scan(r, false); err != nil {
				return err
			}
			if unknown {
				return nil
			}
		default:
			if _, err := io.CopyN(io.Discard, r, int64(size)); err != nil {
				return nil
			}
		}
	}
}

func scanTracks(r io.Reader, size uint64, unknown bool) error {
	var lim io.Reader = r
	if !unknown {
		lim = io.LimitReader(r, int64(size))
	}
	var cur *Track
	for {
		id, _, err := readVint(lim, true)
		if err != nil {
			return nil
		}
		sz, _, err := readVint(lim, false)
		if err != nil {
			return nil
		}
		if id == 0xae { // TrackEntry
			cur = &Track{}
			if err := scanTrackEntry(lim, sz, cur); err != nil {
				return nil
			}
			printTrack(*cur)
			continue
		}
		if _, err := io.CopyN(io.Discard, lim, int64(sz)); err != nil {
			return nil
		}
	}
}

func scanTrackEntry(r io.Reader, size uint64, t *Track) error {
	lim := io.LimitReader(r, int64(size))
	for {
		id, _, err := readVint(lim, true)
		if err != nil {
			return nil
		}
		sz, _, err := readVint(lim, false)
		if err != nil {
			return nil
		}
		body := make([]byte, sz)
		if _, err := io.ReadFull(lim, body); err != nil {
			return nil
		}
		switch id {
		case 0xd7:
			t.Number = beUint(body)
		case 0x83:
			t.Type = beUint(body)
		case 0x86:
			t.CodecID = string(body)
		case 0x22b59c:
			t.Lang = string(body)
		case 0x536e:
			t.Name = string(body)
		case 0x88:
			t.Default = beUint(body) != 0
		case 0xe1: // Audio（只需头两个字段）
			parseAudio(body, t)
		}
	}
}

func parseAudio(b []byte, t *Track) {
	r := io.Reader(bytes.NewReader(b))
	for {
		id, _, err := readVint(r, true)
		if err != nil {
			return
		}
		sz, _, err := readVint(r, false)
		if err != nil {
			return
		}
		body := make([]byte, sz)
		if _, err := io.ReadFull(r, body); err != nil {
			return
		}
		switch id {
		case 0x9f:
			t.Channels = beUint(body)
		case 0xb5:
			t.Rate = floatBe(body)
		}
	}
}

func printTrack(t Track) {
	kind := map[uint64]string{1: "video", 2: "audio", 0x11: "subtitle"}[t.Type]
	if kind == "" {
		kind = fmt.Sprintf("type=%d", t.Type)
	}
	fmt.Printf("track#%d %-9s codec=%-22s lang=%-4s default=%-5v ch=%d rate=%.0f name=%q\n",
		t.Number, kind, t.CodecID, t.Lang, t.Default, t.Channels, t.Rate, t.Name)
}

func beUint(b []byte) uint64 {
	var v uint64
	for _, x := range b {
		v = v<<8 | uint64(x)
	}
	return v
}

func floatBe(b []byte) float64 {
	switch len(b) {
	case 4:
		return float64(math.Float32frombits(uint32(beUint(b))))
	case 8:
		return math.Float64frombits(beUint(b))
	}
	return 0
}

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: mkvprobe <file.mkv>")
		os.Exit(1)
	}
	f, err := os.Open(os.Args[1])
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	defer f.Close()
	if err := scan(f, false); err != nil {
		fmt.Fprintln(os.Stderr, "err:", err)
	}
	if !found {
		fmt.Println("(未找到 Tracks 段)")
	}
}
