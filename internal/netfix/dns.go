// Package netfix 处理 Android 上特殊的网络环境。
//
// 问题：Android 没有 /etc/resolv.conf，Go 的 pure-Go 解析器会直接失败，
// 表现是「curl 能通、Go 程序全部 network error」。
//
// 解决：自己装一个 Resolver，按 环境变量 → net.dns1 → net.dns2 → 公共 DNS 的顺序
// 挑可用的服务器。只影响本进程，不改系统任何东西。
package netfix

import (
	"context"
	"net"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"
)

var (
	once    sync.Once
	servers []string
)

// Setup 安装带兜底的解析器，并返回实际使用的 DNS 列表。
func Setup() []string {
	once.Do(func() {
		servers = detectServers()
		net.DefaultResolver = &net.Resolver{
			PreferGo: true,
			Dial: func(ctx context.Context, network, _ string) (net.Conn, error) {
				dialer := net.Dialer{Timeout: 4 * time.Second}
				// 逐个尝试，并记录**每个**服务器的失败原因。
				//
				// 之前只返回最后一个错误，日志里永远是
				//   dial udp 8.8.8.8:53: connect: network is unreachable
				// 让人误以为是 8.8.8.8 有问题 —— 实际是三个全挂了（断网）。
				// 现在汇总全部失败，一眼看出是「全部不可达」还是「某个 DNS 挂了」。
				var fails []string
				for _, srv := range servers {
					conn, err := dialer.DialContext(ctx, "udp", srv)
					if err == nil {
						return conn, nil
					}
					fails = append(fails, srv+"（"+shortErr(err)+"）")
				}
				if len(fails) == 0 {
					return nil, &net.DNSError{Err: "没有可用的 DNS 服务器", IsNotFound: false}
				}
				detail := strings.Join(fails, "；")
				if len(servers) > 1 && allUnreachable(fails) {
					detail = "全部 DNS 都不可达（大概率是断网或飞行模式）: " + detail
				}
				return nil, &net.DNSError{Err: detail, IsNotFound: false}
			},
		}
	})
	return servers
}

// detectServers 按优先级拼候选列表。
func detectServers() []string {
	var out []string
	add := func(host string) {
		host = strings.TrimSpace(host)
		if host == "" {
			return
		}
		if !strings.Contains(host, ":") {
			host = net.JoinHostPort(host, "53")
		}
		for _, x := range out {
			if x == host {
				return
			}
		}
		out = append(out, host)
	}

	// 1) 显式指定
	for _, h := range strings.Split(os.Getenv("ALLIN_DNS"), ",") {
		add(h)
	}
	// 2) Android 系统属性
	for _, prop := range []string{"net.dns1", "net.dns2", "net.dns3", "net.dns4"} {
		if v := getprop(prop); v != "" && v != "0.0.0.0" {
			add(v)
		}
	}
	// 3) 公共 DNS 兜底（国内可达优先）
	add("223.5.5.5")
	add("119.29.29.29")
	add("8.8.8.8")

	return out
}

// getprop 读一个 Android 系统属性，失败返回空串。
func getprop(name string) string {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "getprop", name).Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// shortErr 把拨号错误压缩成一句可读的话。
func shortErr(err error) string {
	if err == nil {
		return ""
	}
	msg := err.Error()
	switch {
	case strings.Contains(msg, "network is unreachable"):
		return "网络不可达"
	case strings.Contains(msg, "connection refused"):
		return "拒绝连接"
	case strings.Contains(msg, "i/o timeout"), strings.Contains(msg, "deadline exceeded"):
		return "超时"
	case strings.Contains(msg, "no such host"):
		return "查不到主机"
	}
	if len(msg) > 60 {
		msg = msg[:60] + "…"
	}
	return msg
}

// allUnreachable 判断是不是所有服务器都报「网络不可达」。
// 这种情况几乎必然是整个网络断了，而不是某个 DNS 有问题。
func allUnreachable(fails []string) bool {
	if len(fails) == 0 {
		return false
	}
	for _, f := range fails {
		if !strings.Contains(f, "网络不可达") {
			return false
		}
	}
	return true
}
