// Command allin 是手机上的 AI 网关：对外暴露 OpenAI / Anthropic 兼容接口，
// 对内按 provider/model 路由到各家上游，并附带 WorkBuddy 每日签到。
//
// 用法：
//
//	allin -config /data/adb/modules/allin/data/config.json
//	allin -addr 127.0.0.1:6868
package main

import (
	"context"
	"flag"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"syscall"
	"time"

	"allin/internal/config"
	"allin/internal/gateway"
	"allin/internal/netfix"
)

func main() {
	log.SetFlags(log.LstdFlags | log.Lmsgprefix)
	log.SetPrefix("[allin] ")

	defCfg := defaultConfigPath()
	cfgPath := flag.String("config", defCfg, "配置文件路径")
	addr := flag.String("addr", "", "覆盖监听地址，例如 127.0.0.1:6868")
	flag.Parse()

	// Android 上没有 /etc/resolv.conf，必须先装带兜底的 DNS 解析器
	dns := netfix.Setup()
	log.Printf("DNS 服务器: %v", dns)

	store, err := config.NewStore(*cfgPath)
	if err != nil {
		log.Fatalf("加载配置失败: %v", err)
	}
	cfg := store.Snapshot()

	// 日志总开关：接上可控 Writer，并按配置恢复开关状态。
	// 必须在开始输出日志之前接，否则前几行会漏过去。
	gateway.SetupLogging()
	gateway.SetLogEnabled(cfg.Server.LogCaptureEnabled())

	listen := *addr
	if listen == "" {
		listen = net.JoinHostPort(cfg.Server.Host, strconv.Itoa(cfg.Server.Port))
	}

	srv := gateway.New(store)
	// 后台任务：Token 到期前自动刷新
	bgCtx, bgCancel := context.WithCancel(context.Background())
	defer bgCancel()
	srv.Start(bgCtx)
	httpSrv := &http.Server{
		Addr:              listen,
		Handler:           srv,
		ReadHeaderTimeout: 20 * time.Second,
		IdleTimeout:       120 * time.Second,
		// 不设 WriteTimeout：流式响应可能持续很久
	}

	if cfg.Server.AuthKeyOrigin == "first-run-random" {
		log.Printf("首次启动，已生成网关口令：%s", cfg.Server.AuthKey)
	}
	log.Printf("配置文件: %s", *cfgPath)
	log.Printf("监听: http://%s/v1  （模型列表 /v1/models，面板 /）", listen)

	errCh := make(chan error, 1)
	go func() {
		if err := httpSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			errCh <- err
		}
	}()

	// autoSyncModels=true 的供应商在后台同步一次模型目录（只追加，不覆盖）。
	// 放 goroutine 里跑：启动不该被上游网络状况拖住。
	go srv.StartAutoSyncModels(context.Background())

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)

	select {
	case err := <-errCh:
		log.Fatalf("监听失败: %v", err)
	case sig := <-stop:
		log.Printf("收到信号 %v，正在退出", sig)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	if err := httpSrv.Shutdown(ctx); err != nil {
		log.Printf("关闭时出错: %v", err)
	}
	log.Printf("已退出")
}

// defaultConfigPath 优先用 KSU 模块目录，其次工作目录。
func defaultConfigPath() string {
	const moduleData = "/data/adb/modules/allin/data"
	if st, err := os.Stat(moduleData); err == nil && st.IsDir() {
		return filepath.Join(moduleData, "config.json")
	}
	if st, err := os.Stat("/data/adb/modules/allin"); err == nil && st.IsDir() {
		_ = os.MkdirAll(moduleData, 0o700)
		return filepath.Join(moduleData, "config.json")
	}
	wd, err := os.Getwd()
	if err != nil {
		return "config.json"
	}
	return filepath.Join(wd, "config.json")
}
