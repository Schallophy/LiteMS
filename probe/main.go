// M0 网络探测
//
// 用法：
//   ./probe [host]          默认 api.themoviedb.org
//   DNS_ALT=1.2.3.4 ./probe host    指定对照组 DNS
//
// 编译：CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o probe main.go
package main

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"time"
)

func main() {
	target := "api.themoviedb.org"
	if len(os.Args) > 1 {
		target = os.Args[1]
	}
	altDNS := os.Getenv("DNS_ALT")
	if altDNS == "" {
		altDNS = "223.5.5.5"
	}

	fmt.Printf("target    %s\n", target)
	fmt.Printf("runtime   %s/%s  %s\n", runtime.GOOS, runtime.GOARCH, runtime.Version())
	fmt.Printf("local     %s  （出口地址，用于推断网关）\n", localIP())

	// Go 的解析器到底读哪份配置（Termux 的包会 patch 路径）
	for _, p := range []string{"/etc/resolv.conf", os.Getenv("PREFIX") + "/etc/resolv.conf"} {
		if p == "/etc/resolv.conf" || os.Getenv("PREFIX") != "" {
			if b, err := os.ReadFile(p); err == nil {
				fmt.Printf("resolv    %s → %q\n", p, trim(string(b)))
			} else {
				fmt.Printf("resolv    %s → 不可读\n", p)
			}
		}
	}

	// USE_ALT=1：启动前就接管全局解析器，等价于程序内的 [net].dns。
	// 必须在任何解析发生之前设置，否则 DNS-go 那一项仍走原解析器。
	if os.Getenv("USE_ALT") == "1" {
		net.DefaultResolver = newResolver(altDNS)
		fmt.Printf("resolver  USE_ALT 接管 → %s:53\n", altDNS)
	}

	// 1. 系统解析器（纯 Go，CGO=0 时唯一可用）
	dnsOK := probe("DNS-go", func(ctx context.Context) error {
		addrs, err := net.DefaultResolver.LookupHost(ctx, target)
		if err == nil {
			fmt.Printf("          → %v\n", addrs)
		}
		return err
	})

	// 2. 指定 DNS 的对照组
	alt := newResolver(altDNS)
	altOK := probe("DNS-alt["+altDNS+"]", func(ctx context.Context) error {
		addrs, err := alt.LookupHost(ctx, target)
		if err == nil {
			fmt.Printf("          → %v\n", addrs)
		}
		return err
	})

	// 3. HTTPS 直连
	probe("HTTPS", func(ctx context.Context) error {
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "https://"+target+"/", nil)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		fmt.Printf("          → status=%d\n", resp.StatusCode)
		return nil
	})

	// 4. 写盘
	home, _ := os.UserHomeDir()
	p := filepath.Join(home, ".m0-probe")
	start := time.Now()
	if err := os.WriteFile(p, []byte("ok"), 0o644); err != nil {
		fmt.Printf("write     FAIL  %v\n", err)
	} else {
		fi, _ := os.Stat(p)
		_ = os.Remove(p)
		fmt.Printf("write     ok    %d 字节  %v\n", fi.Size(), time.Since(start).Round(time.Microsecond))
	}

	fmt.Println()
	switch {
	case dnsOK && altOK:
		fmt.Println("结论：系统 DNS 与指定 DNS 都通 → 只需要一个可用的 nameserver")
	case !dnsOK && altOK:
		fmt.Printf("结论：系统 DNS 不可用，$PREFIX/etc/resolv.conf 需改成 %s 之类可达的服务器\n", altDNS)
	default:
		fmt.Println("结论：两条路都不通 → 外网 DNS 被阻断，需要代理")
	}
}

// localIP 用 UDP connect 取出口地址。UDP 没有握手，connect 不发包，
// 但内核会据此选路并填入本端地址——不需要任何权限，也不需要读路由表。
func localIP() string {
	c, err := net.Dial("udp", "223.5.5.5:80")
	if err != nil {
		return "(未知)"
	}
	defer c.Close()
	if ua, ok := c.LocalAddr().(*net.UDPAddr); ok {
		return ua.IP.String()
	}
	return "(未知)"
}

func newResolver(dns string) *net.Resolver {
	return &net.Resolver{
		PreferGo: true,
		Dial: func(ctx context.Context, network, _ string) (net.Conn, error) {
			d := net.Dialer{Timeout: 5 * time.Second}
			return d.DialContext(ctx, network, net.JoinHostPort(dns, "53"))
		},
	}
}

func trim(s string) string {
	s = filepath.ToSlash(s)
	out := ""
	for _, line := range splitLines(s) {
		if line == "" {
			continue
		}
		out += line + " | "
	}
	if out == "" {
		return s
	}
	return out
}

func splitLines(s string) []string {
	var out []string
	cur := ""
	for _, r := range s {
		if r == '\n' {
			out = append(out, cur)
			cur = ""
			continue
		}
		cur += string(r)
	}
	out = append(out, cur)
	return out
}

func probe(name string, fn func(context.Context) error) bool {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	start := time.Now()
	if err := fn(ctx); err != nil {
		fmt.Printf("%-14s FAIL  %v  (%v)\n", name, err, time.Since(start).Round(time.Millisecond))
		return false
	}
	fmt.Printf("%-14s ok    %v\n", name, time.Since(start).Round(time.Millisecond))
	return true
}
