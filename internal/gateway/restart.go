package gateway

import (
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"sync"
	"time"
)

// 网关自重启。
//
// 场景：在线更新下载完成后需要重跑 service.sh 才会真正替换二进制，
// 但那个脚本会 kill 掉当前进程 —— 如果直接在请求里执行，
// 响应还没写出去就被杀了，前端只会看到「网络错误」。
//
// 所以拆成两步：
//  1. 先返回响应告诉用户「正在重启」
//  2. 延迟一小会儿再执行重启，给响应留出送达时间
//
// 没有 root 通道时（su 不可用）退回「只重启自己的进程」：
// 至少能让新的 .pending 二进制在下次 service.sh 运行时被应用。

const (
	// restartDelay 是延迟多久再执行重启，给 HTTP 响应留出送达时间。
	restartDelay = 800 * time.Millisecond
	// runtimeDir 是运行目录（二进制从这里跑）。
	runtimeDir = "/data/local/tmp/allin-run"
)

var (
	restartMu  sync.Mutex
	restarting bool
)

// RestartPlan 描述一次重启能做到什么。
type RestartPlan struct {
	// Mode 是重启方式：script（跑 service.sh，能真正应用更新）
	// / self（只重启自己，应用不了模块文件）
	Mode string
	// Reason 给用户看的说明。
	Reason string
}

// planRestart 探测当前环境下能做到哪种重启。
func planRestart() RestartPlan {
	// 注意：不要用 os.Stat 判断 service.sh 在不在。
	// 网关以 uid 2000 运行，/data/adb/modules/allin 是 root 700，
	// 它根本看不到那个目录 —— stat 永远失败，但脚本其实好好的。
	// 只能靠「su 能不能执行」来判断。
	su := findSu()
	if su == "" {
		return RestartPlan{
			Mode:   "self",
			Reason: "当前没有 root 通道，只能重启网关进程；模块文件（新二进制）需要到管理器里点一次「运行」才会应用",
		}
	}
	// 用 su 试探能否读到脚本（root 身份下 stat 才准）
	script := filepath.Join(moduleDir, "service.sh")
	if err := exec.Command(su, "-c", "test -f "+script).Run(); err != nil {
		return RestartPlan{
			Mode:   "self",
			Reason: "有 root 通道但读不到 service.sh，只能重启网关进程",
		}
	}
	return RestartPlan{Mode: "script", Reason: "将通过 service.sh 重启并应用更新"}
}

// findSu 返回可用的 su 路径，没有则空。
func findSu() string {
	for _, su := range []string{"/system/bin/su", "/system/xbin/su", "/debug_ramdisk/su"} {
		if _, err := os.Stat(su); err == nil {
			return su
		}
	}
	// KernelSU / APatch 常见路径
	for _, su := range []string{"/data/adb/ksu/bin/su", "/data/adb/ap/bin/su"} {
		if _, err := os.Stat(su); err == nil {
			return su
		}
	}
	return ""
}

// scheduleRestart 在后台延迟执行重启，立即返回。
//
// 调用方必须先把自己的响应写完再调这个，否则响应会被重启打断。
func scheduleRestart() RestartPlan {
	restartMu.Lock()
	if restarting {
		restartMu.Unlock()
		return RestartPlan{Mode: "already", Reason: "已经在重启中"}
	}
	restarting = true
	restartMu.Unlock()

	plan := planRestart()

	go func() {
		time.Sleep(restartDelay)
		log.Printf("准备重启网关：mode=%s", plan.Mode)
		switch plan.Mode {
		case "script":
			doRestartViaScript()
		case "self":
			doRestartSelf()
		default:
			log.Printf("重启失败：%s", plan.Reason)
		}
	}()

	return plan
}

// doRestartViaScript 以 root 跑 service.sh。
//
// service.sh 内部会 kill 掉旧进程、替换 .pending 二进制、再拉起新的，
// 所以这里只要把它启动起来就行，不要等它结束（它会等系统就绪）。
func doRestartViaScript() {
	su := findSu()
	script := filepath.Join(moduleDir, "service.sh")
	// 用 setsid 让它脱离当前进程组，否则父进程被杀时子进程也会被带走
	cmd := exec.Command(su, "-c", "sh "+script)
	if err := cmd.Start(); err != nil {
		log.Printf("通过 su 启动 service.sh 失败: %v，退回自重启", err)
		doRestartSelf()
	}
}

// doRestartSelf 只重启网关自身进程（没有 root 时用）。
//
// 做法是把运行时二进制重新 exec：先把自己杀掉，让 service.sh 的守护循环
// （如果还活着）把它拉起来；没有守护循环时就用 setsid 直接拉一个新进程。
func doRestartSelf() {
	bin := filepath.Join(runtimeDir, "allin")
	if _, err := os.Stat(bin); err != nil {
		log.Printf("自重启失败：找不到 %s", bin)
		return
	}
	cfg := os.Getenv("ALLIN_CONFIG")
	if cfg == "" {
		cfg = filepath.Join(runtimeDir, "data", "config.json")
	}
	// 用 setsid 脱离进程组，这样本进程退出后新进程能继续跑
	cmd := exec.Command("/system/bin/sh", "-c",
		"setsid "+bin+" -config "+cfg+" >> "+filepath.Join(moduleDir, "gateway.log")+" 2>&1 &")
	cmd.Env = os.Environ()
	_ = cmd.Start()

	// 给新进程一点启动时间，然后退出自己
	time.Sleep(300 * time.Millisecond)
	log.Printf("自重启：退出当前进程（pid=%d）", os.Getpid())
	os.Exit(0)
}

// RestartSupported 判断是否支持自动重启（用于面板显示）。
func RestartSupported() (bool, string) {
	p := planRestart()
	return p.Mode != "", p.Reason
}

// handleRestart 处理面板的「重启网关」请求。
//
//	POST /api/restart
func (s *Server) handleRestart(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "只支持 POST"})
		return
	}
	plan := planRestart()
	if plan.Mode == "" {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{
			"ok": false, "error": plan.Reason,
			"hint": "请到模块管理器里点一次「运行」，或重启手机。",
		})
		return
	}

	// 先写响应，再安排重启 —— 顺序不能反
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":     true,
		"mode":   plan.Mode,
		"reason": plan.Reason,
		"hint":   "网关将在约 1 秒后重启，页面会自动重连。",
	})
	scheduleRestart()
}

// RestartPendingBinaryHint 判断是否有待应用的新二进制（用于面板提示）。
func RestartPendingBinaryHint() bool {
	// 模块目录读不到（uid 2000 vs root 700），只能查自己有权访问的 stage 目录
	if _, err := os.Stat(filepath.Join(runtimeDir, "stage", "allin-update.zip")); err == nil {
		return true
	}
	if su := findSu(); su != "" {
		out, err := exec.Command(su, "-c",
			"ls "+filepath.Join(moduleDir, "bin", "allin.pending")+" "+
				filepath.Join(moduleDir, "service.sh.pending")+" 2>/dev/null").Output()
		if err == nil && len(out) > 0 {
			return true
		}
	}
	return false
}

// pidAlive 判断进程是否还活着（自重启后用不到，留给诊断接口）。
func pidAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	return exec.Command("/system/bin/sh", "-c", "kill -0 "+strconv.Itoa(pid)).Run() == nil
}
