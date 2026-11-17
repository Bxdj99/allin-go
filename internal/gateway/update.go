package gateway

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
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
	tmp := filepath.Join(os.TempDir(), "allin-update.zip")
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

	if err := installModule(tmp); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": "安装失败: " + err.Error()})
		return
	}
	_ = os.Remove(tmp)

	writeJSON(w, http.StatusOK, map[string]any{
		"ok":      true,
		"from":    st.Current,
		"to":      st.Latest,
		"message": "已更新到 " + st.Latest + "，重启手机（或在管理器里重启模块）后生效",
	})
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

// installModule 解压覆盖模块目录，保留 data/。
func installModule(zipPath string) error {
	// 备份当前 module.prop，解压失败能回滚
	oldProp, _ := os.ReadFile(filepath.Join(moduleDir, "module.prop"))

	tmpDir := filepath.Join(os.TempDir(), "allin-update-x")
	_ = os.RemoveAll(tmpDir)
	if err := os.MkdirAll(tmpDir, 0o755); err != nil {
		return err
	}
	if out, err := exec.Command("unzip", "-o", "-q", zipPath, "-d", tmpDir).CombinedOutput(); err != nil {
		return &netErr{"解压失败: " + string(out)}
	}
	defer os.RemoveAll(tmpDir)

	// 校验解压结果完整性
	if _, err := os.Stat(filepath.Join(tmpDir, "module.prop")); err != nil {
		return &netErr{"包里缺 module.prop"}
	}
	if _, err := os.Stat(filepath.Join(tmpDir, "bin", "allin")); err != nil {
		return &netErr{"包里缺 bin/allin"}
	}

	// 逐个文件覆盖（不删除 data/ 与日志）
	entries, err := os.ReadDir(tmpDir)
	if err != nil {
		return err
	}
	for _, e := range entries {
		name := e.Name()
		if name == "data" {
			continue // 保护用户数据
		}
		src := filepath.Join(tmpDir, name)
		dst := filepath.Join(moduleDir, name)
		if e.IsDir() {
			// bin/ 下的 allin 同样写成 .pending
			if name == "bin" {
				if err := copyDirWithPending(src, dst); err != nil {
					if oldProp != nil {
						_ = os.WriteFile(filepath.Join(moduleDir, "module.prop"), oldProp, 0o644)
					}
					return err
				}
				continue
			}
			if err := copyDir(src, dst); err != nil {
				// 回滚 module.prop
				if oldProp != nil {
					_ = os.WriteFile(filepath.Join(moduleDir, "module.prop"), oldProp, 0o644)
				}
				return err
			}
			continue
		}
		// 正在运行的二进制不能覆盖自己：写成 .pending，service.sh 下次启动时替换
		if name == "allin" && strings.HasPrefix(src, filepath.Join(tmpDir, "bin")) {
			dst = dst + ".pending"
		}
		if err := copyFile(src, dst); err != nil {
			if oldProp != nil {
				_ = os.WriteFile(filepath.Join(moduleDir, "module.prop"), oldProp, 0o644)
			}
			return err
		}
	}
	// 确保脚本可执行
	for _, sh := range []string{"service.sh", "action.sh", "uninstall.sh"} {
		_ = os.Chmod(filepath.Join(moduleDir, sh), 0o755)
	}
	_ = os.Chmod(filepath.Join(moduleDir, "bin", "allin"), 0o755)

	// 重新生成 update.json 供 KernelSU 管理器识别（如果有这个能力）
	return nil
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	// 先写临时文件再 rename，避免覆盖运行中的二进制失败
	tmp := dst + ".new"
	out, err := os.Create(tmp)
	if err != nil {
		return err
	}
	if _, err = io.Copy(out, in); err != nil {
		out.Close()
		os.Remove(tmp)
		return err
	}
	out.Close()
	if err := os.Rename(tmp, dst); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
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
