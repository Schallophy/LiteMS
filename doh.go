package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// DNS over HTTPS（RFC 8484）。
//
// 为什么需要：移动网络的 DNS 经常被污染或劫持，表现是域名解析到一个假 IP
// （0.0.0.0 / 一个完全无关的地址），连接立刻失败或者连到别处。刮削源正是
// 典型受害者——api.themoviedb.org 在国内长期被解析到 0.0.0.0。
// 换 [net].dns 指向公共 DNS 只能绕开上游投毒，绕不开链路上的劫持；DoH 把
// 查询塞进 TLS，中间人看不到也改不了。
//
// 这里不接管 net.DefaultResolver：Go 的 Resolver 需要一条能跑 DNS 协议的连接
// （UDP/TCP 53），DoH 是 HTTP，塞不进去。改成只接管 HTTP 出站的 DialContext：
// 解析目标主机名时问 DoH，拿到 IP 后按 IP 建连，TLS 的 SNI 仍然是原域名。

const (
	dnsTypeA    = 1
	dnsTypeAAAA = 28
	dnsClassIN  = 1
)

// defaultDoHEndpoints 是 [net].doh = "auto" 时用的端点，按顺序尝试。
// 国内优先（延迟低、不易被墙），Cloudflare 兜底。
var defaultDoHEndpoints = []string{
	"https://doh.pub/dns-query",
	"https://dns.alidns.com/dns-query",
	"https://1.1.1.1/dns-query",
}

// dohResolver 是一个只做 A/AAAA 查询、带进程内缓存的 DoH 客户端。
type dohResolver struct {
	endpoints []string
	hc        *http.Client
	ttlMax    time.Duration

	seq uint32

	mu    sync.Mutex
	cache map[string]dohEntry
}

type dohEntry struct {
	ips []net.IP
	exp time.Time
}

// 出站 DialContext，默认是系统解析。启用 DoH 后由 installDoH 替换，
// 所有 HTTP 客户端（TMDB、图片下载、OMDb）共用同一个。
var netDial = (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext

// activeDoH 非 nil 表示 DoH 已启用，供日志与自检使用。
var activeDoH *dohResolver

// dialForHTTP 是给 http.Transport.DialContext 用的间接层。做成函数而不是
// 直接传 netDial 变量，是为了避免「Transport 先创建、DoH 后启用」时捕获到
// 旧的拨号函数——这里永远读最新的状态。
func dialForHTTP(ctx context.Context, network, addr string) (net.Conn, error) {
	if activeDoH != nil {
		return activeDoH.dialContext(ctx, network, addr)
	}
	return netDial(ctx, network, addr)
}

// installDoH 按配置启用 DoH。返回一句描述，供启动日志打印。
//
// want："" / "off" 关闭；"auto" 用内置端点；其余按逗号分隔的 URL 列表处理。
// 会先做一次真实解析自检，失败就保持系统解析并告警——宁可不生效，也不能让
// 整个程序因为 DNS 被自己搞坏而无法联网。
func installDoH(want string, probeHost string) string {
	want = strings.TrimSpace(want)
	if want == "" || strings.EqualFold(want, "off") || strings.EqualFold(want, "false") {
		return "未启用"
	}

	endpoints := defaultDoHEndpoints
	if !strings.EqualFold(want, "auto") && !strings.EqualFold(want, "on") {
		endpoints = nil
		for _, s := range strings.Split(want, ",") {
			if s = strings.TrimSpace(s); s != "" {
				endpoints = append(endpoints, s)
			}
		}
	}
	if len(endpoints) == 0 {
		return "未启用（端点列表为空）"
	}

	d := &dohResolver{
		endpoints: endpoints,
		ttlMax:    10 * time.Minute,
		cache:     make(map[string]dohEntry),
		// 请求 DoH 端点自身不能再用 DoH 解析，所以这里用裸 Transport（系统解析）。
		hc: &http.Client{
			Timeout: 6 * time.Second,
			Transport: &http.Transport{
				Proxy:               http.ProxyFromEnvironment,
				MaxIdleConns:        4,
				MaxIdleConnsPerHost: 2,
				IdleConnTimeout:     60 * time.Second,
			},
		},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	ips, err := d.lookup(ctx, probeHost)
	if err != nil {
		warnf("[net].doh = %q 自检失败（%v），继续使用系统 DNS", want, err)
		return "未启用（自检失败）"
	}

	activeDoH = d
	return fmt.Sprintf("%s（解析 %s → %s）", d.endpoints[0], probeHost, ips[0])
}

// lookup 解析主机名。命中缓存直接返回；否则依次试端点，A 查不到再查 AAAA。
func (d *dohResolver) lookup(ctx context.Context, host string) ([]net.IP, error) {
	if ip := net.ParseIP(host); ip != nil {
		return []net.IP{ip}, nil
	}
	d.mu.Lock()
	if e, ok := d.cache[host]; ok && time.Now().Before(e.exp) {
		d.mu.Unlock()
		return e.ips, nil
	}
	d.mu.Unlock()

	var firstErr error
	for _, ep := range d.endpoints {
		ips, ttl, err := d.query(ctx, ep, host, dnsTypeA)
		if err == nil && len(ips) > 0 {
			d.store(host, ips, ttl)
			return ips, nil
		}
		if err != nil && firstErr == nil {
			firstErr = err
		}
		// A 没有记录不代表域名不存在，可能只有 AAAA
		ips, ttl, err = d.query(ctx, ep, host, dnsTypeAAAA)
		if err == nil && len(ips) > 0 {
			d.store(host, ips, ttl)
			return ips, nil
		}
		if err != nil && firstErr == nil {
			firstErr = err
		}
	}
	if firstErr == nil {
		firstErr = errors.New("没有 A/AAAA 记录")
	}
	return nil, firstErr
}

// store 写入缓存，TTL 夹在 [30s, ttlMax]：太短会打爆端点，太长会钉死过期结果。
func (d *dohResolver) store(host string, ips []net.IP, ttl uint32) {
	ttl = max(ttl, 30)
	exp := time.Now().Add(min(time.Duration(ttl)*time.Second, d.ttlMax))
	d.mu.Lock()
	d.cache[host] = dohEntry{ips: ips, exp: exp}
	d.mu.Unlock()
}

// query 向单个端点发一次查询。
func (d *dohResolver) query(ctx context.Context, endpoint, host string, qtype uint16) ([]net.IP, uint32, error) {
	id := uint16(atomic.AddUint32(&d.seq, 1))
	body, err := buildDNSQuery(id, host, qtype)
	if err != nil {
		return nil, 0, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Content-Type", "application/dns-message")
	req.Header.Set("Accept", "application/dns-message")

	resp, err := d.hc.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, 0, fmt.Errorf("%s 返回 HTTP %d", endpoint, resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, 16<<10))
	if err != nil {
		return nil, 0, err
	}
	return parseDNSAnswers(data, qtype)
}

// dialContext 先 DoH 解析，再按 IP 建连。解析失败回落系统解析，
// 保证「DoH 挂了不等于断网」。
func (d *dohResolver) dialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return baseDial(ctx, network, addr)
	}
	ips, err := d.lookup(ctx, host)
	if err != nil || len(ips) == 0 {
		return baseDial(ctx, network, addr)
	}

	var lastErr error
	for _, ip := range ips {
		c, err := baseDial(ctx, network, net.JoinHostPort(ip.String(), port))
		if err == nil {
			return c, nil
		}
		lastErr = err
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("无法连接 %s", host)
	}
	return nil, lastErr
}

// baseDial 是不经过 DoH 的原始拨号器。
func baseDial(ctx context.Context, network, addr string) (net.Conn, error) {
	d := net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}
	return d.DialContext(ctx, network, addr)
}

// buildDNSQuery 手工拼一个查询报文（只含一个问题，递归查询）。
func buildDNSQuery(id uint16, name string, qtype uint16) ([]byte, error) {
	name = strings.TrimSuffix(strings.TrimSpace(name), ".")
	if name == "" {
		return nil, errors.New("空域名")
	}

	b := make([]byte, 0, 64+len(name))
	b = append(b, byte(id>>8), byte(id))
	b = append(b, 0x01, 0x00)             // QR=0, RD=1
	b = append(b, 0, 1)                   // QDCOUNT=1
	b = append(b, 0, 0, 0, 0, 0, 0)       // AN/NS/AR = 0

	for _, label := range strings.Split(name, ".") {
		if len(label) == 0 || len(label) > 63 {
			return nil, fmt.Errorf("非法域名 %q", name)
		}
		b = append(b, byte(len(label)))
		b = append(b, label...)
	}
	b = append(b, 0)                      // 名字结束
	b = append(b, byte(qtype>>8), byte(qtype))
	b = append(b, 0, dnsClassIN)

	return b, nil
}

// parseDNSAnswers 从响应里取指定类型的记录。
//
// 只做「跳过名字 + 读定长头 + 取 RDATA」，不解析 CNAME 链：我们要的是终点的
// 地址，而 name 压缩指针的存在让「跳过」比「解析」更省事也更快。
func parseDNSAnswers(msg []byte, qtype uint16) ([]net.IP, uint32, error) {
	if len(msg) < 12 {
		return nil, 0, errors.New("响应过短")
	}
	if rcode := msg[3] & 0x0F; rcode != 0 {
		return nil, 0, fmt.Errorf("DNS 返回 rcode=%d", rcode)
	}
	qd := int(msg[4])<<8 | int(msg[5])
	an := int(msg[6])<<8 | int(msg[7])

	off := 12
	var err error
	for i := 0; i < qd; i++ {
		if off, err = skipDNSName(msg, off); err != nil {
			return nil, 0, err
		}
		off += 4 // TYPE + CLASS
		if off > len(msg) {
			return nil, 0, errors.New("报文越界（question）")
		}
	}

	var ips []net.IP
	ttl := uint32(300)
	for i := 0; i < an; i++ {
		if off, err = skipDNSName(msg, off); err != nil {
			return nil, 0, err
		}
		if off+10 > len(msg) {
			return nil, 0, errors.New("报文越界（answer 头）")
		}
		typ := uint16(msg[off])<<8 | uint16(msg[off+1])
		recTTL := uint32(msg[off+4])<<24 | uint32(msg[off+5])<<16 |
			uint32(msg[off+6])<<8 | uint32(msg[off+7])
		rdlen := int(msg[off+8])<<8 | int(msg[off+9])
		off += 10
		if off+rdlen > len(msg) {
			return nil, 0, errors.New("报文越界（RDATA）")
		}
		if typ == qtype {
			switch typ {
			case dnsTypeA:
				if rdlen == 4 {
					ips = append(ips, net.IPv4(msg[off], msg[off+1], msg[off+2], msg[off+3]))
				}
			case dnsTypeAAAA:
				if rdlen == 16 {
					ip := make(net.IP, 16)
					copy(ip, msg[off:off+16])
					ips = append(ips, ip)
				}
			}
			if recTTL < ttl {
				ttl = recTTL
			}
		}
		off += rdlen
	}

	if len(ips) == 0 {
		return nil, 0, fmt.Errorf("没有 type=%d 的记录", qtype)
	}
	return ips, ttl, nil
}

// skipDNSName 返回名字之后第一个字节的下标，兼容压缩指针（0xC0）。
func skipDNSName(msg []byte, off int) (int, error) {
	for {
		if off >= len(msg) {
			return 0, errors.New("报文越界（name）")
		}
		l := int(msg[off])
		if l == 0 {
			return off + 1, nil
		}
		if l&0xC0 == 0xC0 {
			// 指针固定两字节，且只在原地结束
			if off+1 >= len(msg) {
				return 0, errors.New("报文越界（指针）")
			}
			return off + 2, nil
		}
		off += 1 + l
	}
}
