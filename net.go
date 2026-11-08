package main

import (
	"context"
	"fmt"
	"net"
	"os"
	"strings"
	"time"
)

// 网络层自检与接管（文档 13.2）。
//
// 背景：本项目用 GOOS=linux + CGO_ENABLED=0 编译，DNS 走 Go 自己的解析器，
// 它只读 /etc/resolv.conf——而 Termux 上这个文件不存在，于是回退到 [::1]:53，
// 直接 connection refused。表现为"编译成功、运行无网络"。
//
// 因此每个联网命令启动时都先做一次解析自检，失败则接管 net.DefaultResolver，
// 依次尝试：系统默认 → 本机网关 → 公共 DNS。全部失败才报警。

// dnsProbeHost 是自检用的域名。要求稳定、全球可解析、响应快。
const dnsProbeHost = "www.baidu.com"

// SetupNet 按配置准备网络：接管 DNS、注入代理环境变量。
// 返回实际使用的 DNS 描述，便于日志与排错。
func SetupNet(cfg *Config) string {
	if p := strings.TrimSpace(cfg.Net.Proxy); p != "" {
		for _, k := range []string{"HTTP_PROXY", "HTTPS_PROXY", "http_proxy", "https_proxy"} {
			os.Setenv(k, p)
		}
		logf("代理 %s", p)
	}
	return setupDNS(cfg)
}

// setupDNS 决定用哪个 DNS 服务器。
func setupDNS(cfg *Config) string {
	want := strings.TrimSpace(cfg.Net.DNS)

	// 显式指定：用它，但必须验证真的能解析，否则配了等于没配
	if want != "" && !strings.EqualFold(want, "auto") {
		r := resolverFor(want)
		if dnsWorks(r, dnsProbeHost) {
			net.DefaultResolver = r
			return want + "（配置指定）"
		}
		warnf("[net].dns = %q 不可用，回退到自动探测", want)
	}

	// auto：先看系统默认是否可用（桌面 Linux/macOS 上通常可用）
	if dnsWorks(net.DefaultResolver, dnsProbeHost) {
		return "系统默认"
	}

	// 系统 DNS 不可用：猜网关。UDP connect 不发包，但内核会选路并填入本端地址，
	// 同网段的 .1 通常就是网关（文档 13.2.5）。
	if local := localIPv4(); local != "" {
		if gw := guessGateway(local); gw != "" {
			r := resolverFor(gw)
			if dnsWorks(r, dnsProbeHost) {
				net.DefaultResolver = r
				return fmt.Sprintf("网关 %s（由本机 %s 推断）", gw, local)
			}
		}
	}

	// 最后试公共 DNS：部分网络允许直连 53 端口，只是系统没把地址暴露出来
	for _, s := range []string{"223.5.5.5", "1.1.1.1", "8.8.8.8"} {
		r := resolverFor(s)
		if dnsWorks(r, dnsProbeHost) {
			net.DefaultResolver = r
			return s + "（公共 DNS）"
		}
	}

	warnf("DNS 自检失败：所有解析途径都不可用。刮削会全部超时。")
	warnf("  排查：在配置里显式指定 [net].dns = \"<局域网 DNS 或网关 IP>\"，例如 \"192.168.1.1\"")
	warnf("  说明：%s", dnsHint())
	return "不可用"
}

// resolverFor 构造一个直接问指定服务器的解析器，绕过系统配置查找。
func resolverFor(server string) *net.Resolver {
	addr := server
	if _, _, err := net.SplitHostPort(addr); err != nil {
		addr = net.JoinHostPort(server, "53")
	}
	return &net.Resolver{
		PreferGo: true,
		Dial: func(ctx context.Context, network, _ string) (net.Conn, error) {
			d := net.Dialer{Timeout: 4 * time.Second}
			return d.DialContext(ctx, network, addr)
		},
	}
}

// dnsWorks 用 4 秒超时试解析一个域名。
func dnsWorks(r *net.Resolver, host string) bool {
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	_, err := r.LookupHost(ctx, host)
	return err == nil
}

// localIPv4 通过 UDP connect 拿本机出口地址（不实际发包）。
func localIPv4() string {
	c, err := net.Dial("udp4", "8.8.8.8:80")
	if err != nil {
		return ""
	}
	defer c.Close()
	if a, ok := c.LocalAddr().(*net.UDPAddr); ok {
		if ip := a.IP.To4(); ip != nil {
			return ip.String()
		}
	}
	return ""
}

// guessGateway 由本机地址推断同网段的网关（a.b.c.d → a.b.c.1）。
func guessGateway(local string) string {
	ip := net.ParseIP(local).To4()
	if ip == nil {
		return ""
	}
	return fmt.Sprintf("%d.%d.%d.1", ip[0], ip[1], ip[2])
}

// dnsHint 给出当前环境的补充说明，帮助定位是哪一种失败。
func dnsHint() string {
	var b strings.Builder
	if _, err := os.Stat("/etc/resolv.conf"); err != nil {
		b.WriteString("/etc/resolv.conf 不存在（Termux 上属正常现象）")
	} else {
		b.WriteString("/etc/resolv.conf 存在")
	}
	if p := os.Getenv("PREFIX"); p != "" {
		if _, err := os.Stat(p + "/etc/resolv.conf"); err == nil {
			b.WriteString("；$PREFIX/etc/resolv.conf 存在（只对 GOOS=android 的构建有效）")
		}
	}
	return b.String()
}
