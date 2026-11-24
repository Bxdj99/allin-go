package gateway

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"allin/internal/provider"
)

// 在线更新：从 GitHub Releases 拉最新版模块包并安装。
//
// 流程：
//  1. GET {repo}/releases/latest 拿最新 tag 与资产列表
//  2. 比对本地版本，有新版才继续
//  3. 下载 zip 到临时目录
//  4. 校验 zip 内 module.prop 的 id 与本模块一致（防止装错模块）
//  5. 解压到 /data/adb/modules/allin 覆盖，保留 data/ 目录
//  6. 提示重启（不自动重启，交给用户决定）

const (
	repoOwner = "Bxdj99"
	repoName  = "allin-go"
	assetName = "AllIn-Go-v"

	moduleDir = "/data/adb/modules/allin"
	dataDir   = "/data/adb/modules/allin/data"
)

// BuildVersion 由编译时注入：-ldflags "-X allin/internal/gateway.BuildVersion=v1.4.5"
var BuildVersion = "dev"

var updateMu sync.Mutex

// UpdateStatus 是一次版本检查的结果。
type UpdateStatus struct {
	Current   string `json:"current"`
	Latest    string `json:"latest"`
	HasUpdate bool   `json:"hasUpdate"`
	URL       string `json:"url,omitempty"`
	Size      int64  `json:"size,omitempty"`
	Note      string `json:"note,omitempty"`
	Error     string `json:"error,omitempty"`
}

// handleUpdateCheck 检查是否有新版。
//
//	GET /api/update/check
func (s *Server) handleUpdateCheck(w http.ResponseWriter, r *http.Request) {
	st := checkUpdate(r.Context())
	writeJSON(w, http.StatusOK, st)
}

// handleUpdateApply 下载并安装最新版。
//
//	POST /api/update/apply
func (s *Server) handleUpdateApply(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "只支持 POST"})
		return
	}
	if !updateMu.TryLock() {
		writeJSON(w, http.StatusConflict, map[string]any{"ok": false, "error": "已有更新任务在进行"})
		return
	}
	defer updateMu.Unlock()

	st := checkUpdate(r.Context())
	if st.Error != "" {
		writeJSON(w, http.StatusBadGateway, map[string]any{"ok": false, "error": st.Error})
		return
	}
	if !st.HasUpdate {
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "message": "已是最新版", "current": st.Current})
		return
	}

	// 下载
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Minute)
	defer cancel()
	tmpDir, err := updateTempDir()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	tmp := filepath.Join(tmpDir, "allin-update.zip")
	if err := downloadFile(ctx, st.URL, tmp); err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{"ok": false, "error": "下载失败: " + err.Error()})
		return
	}

	// 解压前先校验 module.prop 的 id，避免装错模块把系统搞坏
	id, err := peekModuleID(tmp)
	if err != nil || id != "allin" {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"ok": false, "error": "下载的包不是本模块（id=" + id + "），已中止",
		})
		return
	}

	// 把包放到「网关可写、root 可读」的交接目录，由 service.sh 以 root 应用。
	//
	// 不能在这里直接解压覆盖模块目录：网关以 uid 2000 运行，
	// /data/adb/modules/allin 属 root、权限 755，写不进去。
	stageDir := filepath.Join(tmpDir, "stage")
	if err := os.MkdirAll(stageDir, 0o777); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": "准备交接目录失败: " + err.Error()})
		return
	}
	staged := filepath.Join(stageDir, "allin-update.zip")
	if err := copyFile(tmp, staged); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": "放置更新包失败: " + err.Error()})
		return
	}
	_ = os.Chmod(staged, 0o644)
	_ = os.Chmod(stageDir, 0o777)

	_ = os.Remove(tmp)

	// 先把响应写出去，再安排重启。
	//
	// 顺序不能反：重启会 kill 掉本进程，如果先重启，这个响应就发不出去了，
	// 前端只会看到一个网络错误，用户根本不知道更新成功了没有。
	plan := planRestart()
	canAuto := plan.Mode != ""

	msg := "更新包已就绪"
	hint := ""
	if canAuto {
		msg = "更新已下载，网关将在约 1 秒后自动重启并应用"
		hint = plan.Reason
	} else {
		msg = "更新包已就绪，但当前无法自动重启"
		hint = plan.Reason + "；请到模块管理器点一次「运行」，或在面板点「重启网关」。"
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"ok":        true,
		"from":      st.Current,
		"to":        st.Latest,
		"message":   msg,
		"hint":      hint,
		"staged":    staged,
		"autoApply": canAuto,
	})

	if canAuto {
		scheduleRestart()
	}
}

// checkUpdate 查询 GitHub 最新 release。
func checkUpdate(ctx context.Context) UpdateStatus {
	st := UpdateStatus{Current: BuildVersion}

	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()

	url := "https://api.github.com/repos/" + repoOwner + "/" + repoName + "/releases/latest"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		st.Error = err.Error()
		return st
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "allin-go/"+BuildVersion)

	resp, err := provider.HTTPClient.Do(req)
	if err != nil {
		st.Error = "连不上 GitHub：" + err.Error()
		return st
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		st.Error = "GitHub 返回 " + resp.Status
		return st
	}

	var rel struct {
		TagName string `json:"tag_name"`
		HTMLURL string `json:"html_url"`
		Assets  []struct {
			Name string `json:"name"`
			Size int64  `json:"size"`
			URL  string `json:"browser_download_url"`
		} `json:"assets"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&rel); err != nil {
		st.Error = "解析失败：" + err.Error()
		return st
	}

	st.Latest = rel.TagName
	st.Note = rel.HTMLURL
	for _, a := range rel.Assets {
		if strings.HasPrefix(a.Name, assetName) && strings.HasSuffix(a.Name, ".zip") {
			st.URL = a.URL
			st.Size = a.Size
			break
		}
	}
	// 必须比较大小，不能只判不等：当本地跑的是未发布 Release 的版本（比如刚自己
	// 构建了 v1.18.0，但 GitHub 上最新 Release 还停在 v1.17.1），只判不等会把
	// 旧版本当成"有更新"，用户一点就会降级，丢掉本地这版的功能。
	if st.URL != "" {
		if cmp, ok := compareVer(st.Latest, st.Current); ok {
			st.HasUpdate = cmp > 0
		} else {
			// 版本号解析不了时退回原来的判不等，至少不比现在更差
			st.HasUpdate = normalizeVer(st.Latest) != normalizeVer(st.Current)
		}
	}
	return st
}

// normalizeVer 把 v1.4.4 / v1.4.4-go / 1.4.4 统一成 1.4.4。
func normalizeVer(v string) string {
	v = strings.TrimSpace(strings.ToLower(v))
	v = strings.TrimPrefix(v, "v")
	v = strings.TrimSuffix(v, "-go")
	return v
}

// compareVer 比较两个版本号大小：返回 1 表示 a 比 b 新，-1 表示更旧，0 表示相同。
// ok=false 表示至少有一边解析不出数字版本（调用方应退回字符串比较）。
func compareVer(a, b string) (int, bool) {
	pa, oka := parseVer(a)
	pb, okb := parseVer(b)
	if !oka || !okb {
		return 0, false
	}
	for i := 0; i < 3; i++ {
		if pa[i] != pb[i] {
			if pa[i] > pb[i] {
				return 1, true
			}
			return -1, true
		}
	}
	return 0, true
}

// parseVer 取版本号前三位数字，忽略 -go / -beta 之类的后缀。
func parseVer(v string) ([3]int, bool) {
	var out [3]int
	v = normalizeVer(v)
	if v == "" {
		return out, false
	}
	// 只看主版本部分，遇到非数字非点号就停
	end := len(v)
	for i := 0; i < len(v); i++ {
		c := v[i]
		if (c < '0' || c > '9') && c != '.' {
			end = i
			break
		}
	}
	// 一个数字都没取到（如 "dev"）说明不是版本号，要判失败而不是当成 0.0.0
	if end == 0 || !strings.ContainsAny(v[:end], "0123456789") {
		return out, false
	}
	parts := strings.Split(v[:end], ".")
	for i := 0; i < 3; i++ {
		if i >= len(parts) || parts[i] == "" {
			out[i] = 0
			continue
		}
		n, err := strconv.Atoi(parts[i])
		if err != nil {
			return out, false
		}
		out[i] = n
	}
	return out, true
}

// downloadFile 下载到本地并返回校验值。
func downloadFile(ctx context.Context, url, dst string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", "allin-go/"+BuildVersion)
	resp, err := provider.HTTPClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return &netErr{"下载返回 " + resp.Status}
	}
	f, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = io.Copy(f, io.LimitReader(resp.Body, 64<<20))
	return err
}

type netErr struct{ msg string }

func (e *netErr) Error() string { return e.msg }

// peekModuleID 从 zip 里读 module.prop 的 id。
func peekModuleID(zipPath string) (string, error) {
	out, err := exec.Command("unzip", "-p", zipPath, "module.prop").Output()
	if err != nil {
		return "", err
	}
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "id=") {
			return strings.TrimSpace(strings.TrimPrefix(line, "id=")), nil
		}
	}
	return "", &netErr{"module.prop 里没有 id"}
}

// copyDirWithPending 拷目录，其中名为 allin 的文件写成 allin.pending。
// 运行中的二进制无法直接覆盖（ETXTBSY），必须留给 service.sh 下次启动时替换。
func copyDirWithPending(src, dst string) error {
	return filepath.Walk(src, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, path)
		target := filepath.Join(dst, rel)
		if info.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		if info.Name() == "allin" {
			target += ".pending"
		}
		return copyFile(path, target)
	})
}

func copyDir(src, dst string) error {
	return filepath.Walk(src, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, path)
		target := filepath.Join(dst, rel)
		if info.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		return copyFile(path, target)
	})
}

// sha256Of 计算文件摘要（保留给完整性校验用）。
func sha256Of(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return ""
	}
	return hex.EncodeToString(h.Sum(nil))
}

// updateTempDir 返回一个**当前进程真的能写**的临时目录。
//
// 不能直接用 os.TempDir()：它读的是 TMPDIR 环境变量，
// 而网关以 shell(2000) 身份运行、由别的应用拉起时，
// TMPDIR 可能指向那个应用的私有缓存目录（如
// /data/user/0/<other.app>/cache），网关对它没有写权限，
// 在线更新会直接报 permission denied。
//
// 这里按优先级逐个尝试，第一个能建文件的就用：
//  1. 运行目录（/data/local/tmp/allin-run，模块启动时 chown 给 shell）
//  2. 模块目录下的 tmp/（模块管理器里手动运行时可用）
//  3. 系统 /tmp 与 /data/local/tmp
//  4. 实在没有就退回 os.TempDir()，让错误信息如实暴露
func updateTempDir() (string, error) {
	candidates := make([]string, 0, 6)
	if runtimeDir := os.Getenv("ALLIN_RUN_DIR"); runtimeDir != "" {
		candidates = append(candidates, runtimeDir)
	}
	candidates = append(candidates,
		"/data/local/tmp/allin-run",
		filepath.Join(moduleDir, "tmp"),
		"/data/local/tmp",
		"/tmp",
	)
	for _, dir := range candidates {
		if dir == "" {
			continue
		}
		if err := os.MkdirAll(dir, 0o755); err != nil {
			continue
		}
		probe := filepath.Join(dir, ".allin-write-probe")
		f, err := os.OpenFile(probe, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
		if err != nil {
			continue
		}
		_ = f.Close()
		_ = os.Remove(probe)
		return dir, nil
	}
	fallback := os.TempDir()
	return fallback, fmt.Errorf("找不到可写的临时目录（尝试过 %v，回退 %s 也可能不可写）", candidates, fallback)
}


// copyFile 复制文件到目标路径（目标目录不存在会创建）。
func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	if err := os.MkdirAll(filepath.Dir(dst), 0o777); err != nil {
		return err
	}
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	defer out.Close()
	_, err = io.Copy(out, in)
	return err
}
