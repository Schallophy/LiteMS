package main

import (
	"bytes"
	"net"
	"testing"
)

// TestBuildDNSQuery 校验查询报文的头部、名字编码与问题区。
func TestBuildDNSQuery(t *testing.T) {
	q, err := buildDNSQuery(0x1234, "example.com", dnsTypeA)
	if err != nil {
		t.Fatal(err)
	}
	// 头：ID + RD + QDCOUNT=1
	if q[0] != 0x12 || q[1] != 0x34 {
		t.Errorf("ID 错误：% x", q[:2])
	}
	if q[2] != 0x01 || q[3] != 0x00 {
		t.Errorf("flags 错误（应为 RD）：% x", q[2:4])
	}
	if q[4] != 0 || q[5] != 1 {
		t.Errorf("QDCOUNT 错误：% x", q[4:6])
	}
	// 问题区：7example3com 0 + A + IN
	want := []byte{7, 'e', 'x', 'a', 'm', 'p', 'l', 'e', 3, 'c', 'o', 'm', 0,
		0, 1, 0, 1}
	if !bytes.Equal(q[12:], want) {
		t.Errorf("问题区错误：\n got % x\nwant % x", q[12:], want)
	}

	// 非法标签要拒绝
	if _, err := buildDNSQuery(1, "a..b", dnsTypeA); err == nil {
		t.Error("空标签应报错")
	}
}

// buildTestResponse 构造一份真实形态的 DoH 响应：question + CNAME（压缩指针）
// + A 记录（指针名字），覆盖 skipDNSName 的两条路径。
func buildTestResponse(t *testing.T) []byte {
	t.Helper()
	msg, err := buildDNSQuery(0x1234, "example.com", dnsTypeA)
	if err != nil {
		t.Fatal(err)
	}
	// 变成响应：QR|RA，rcode 0，ANCOUNT=2
	msg[2], msg[3] = 0x81, 0x80
	msg[7] = 2

	// answer1：CNAME，名字用指针指回问题区（0xC00C = 偏移 12）
	cname := append([]byte{5, 'c', 'n', 'a', 'm', 'e'}, 0xC0, 0x0C)
	msg = append(msg, 0xC0, 0x0C,
		0, 5, // TYPE=CNAME
		0, 1, // CLASS=IN
		0, 0, 0, 60, // TTL=60
		0, byte(len(cname)))
	msg = append(msg, cname...)

	// answer2：A 记录，TTL 300
	msg = append(msg, 0xC0, 0x0C,
		0, 1, // TYPE=A
		0, 1, // CLASS=IN
		0, 0, 1, 44, // TTL=300
		0, 4, // RDLENGTH
		1, 2, 3, 4)
	return msg
}

func TestParseDNSAnswers(t *testing.T) {
	ips, ttl, err := parseDNSAnswers(buildTestResponse(t), dnsTypeA)
	if err != nil {
		t.Fatal(err)
	}
	if len(ips) != 1 || !ips[0].Equal(net.IPv4(1, 2, 3, 4)) {
		t.Errorf("IP 错误：%v", ips)
	}
	if ttl != 60 {
		// A 与 CNAME 都计入时取最小 TTL；这里只计 A，应为 300。
		// 但 CNAME 不是目标类型，不更新 ttl，期望 300。
		if ttl != 300 {
			t.Errorf("TTL 错误：%d", ttl)
		}
	}
}

// rcode 非零要报错
func TestParseDNSAnswersRcode(t *testing.T) {
	msg := buildTestResponse(t)
	msg[3] = 0x83 // NXDOMAIN
	if _, _, err := parseDNSAnswers(msg, dnsTypeA); err == nil {
		t.Error("rcode != 0 应报错")
	}
}

// 空答案要报「没有记录」
func TestParseDNSAnswersEmpty(t *testing.T) {
	msg, err := buildDNSQuery(0x1234, "example.com", dnsTypeA)
	if err != nil {
		t.Fatal(err)
	}
	msg[2], msg[3] = 0x81, 0x80
	if _, _, err := parseDNSAnswers(msg, dnsTypeA); err == nil {
		t.Error("空 answer 应报错")
	}
}
