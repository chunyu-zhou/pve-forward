package server

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	"pveforward/internal/update"
)

// UpgradeRequest 由 Run 返回，表示在线更新已就绪，需要以新程序替换当前进程。
type UpgradeRequest struct{ Path, Version string }

func (u *UpgradeRequest) Error() string { return "在线更新到 v" + u.Version }

// updateStatus 为最近一次版本检测的结果（持久化在 settings 表的 update_status 中）。
type updateStatus struct {
	LastCheck int64        `json:"lastCheck"`
	Latest    *update.Info `json:"latest,omitempty"`
	Ready     bool         `json:"ready"` // 新版本的程序文件已发布
	Error     string       `json:"error,omitempty"`
}

// available 返回可以更新到的版本（已发布且高于当前版本），没有则为空。
func (st updateStatus) available() string {
	if st.Latest != nil && st.Ready && update.Compare(st.Latest.Version, Version) > 0 {
		return st.Latest.Version
	}
	return ""
}

// updateJob 为正在进行的在线更新。
type updateJob struct {
	Running bool   `json:"running"`
	Target  string `json:"target,omitempty"`
	Step    string `json:"step,omitempty"`
	Percent int    `json:"percent"`
	Error   string `json:"error,omitempty"`
}

type updater struct {
	mu       sync.Mutex
	checking bool
	job      updateJob
	upgrade  chan *UpgradeRequest
	// 测试用：替换更新来源与安装过程
	source  *update.Source
	install func(ctx context.Context, in *update.Installer, version string, p update.Progress) (string, error)
}

func (s *Server) updateSource() update.Source {
	if s.upd.source != nil {
		return *s.upd.source
	}
	return update.DefaultSource(s.settings().UpdateMirror)
}

func (s *Server) loadUpdateStatus() updateStatus {
	var st updateStatus
	s.db.SettingJSON("update_status", &st)
	return st
}

// checkUpdate 读取最新 version.json，并确认对应版本的程序文件已发布。
func (s *Server) checkUpdate(ctx context.Context) updateStatus {
	s.upd.mu.Lock()
	if s.upd.checking {
		s.upd.mu.Unlock()
		return s.loadUpdateStatus()
	}
	s.upd.checking = true
	s.upd.mu.Unlock()
	defer func() {
		s.upd.mu.Lock()
		s.upd.checking = false
		s.upd.mu.Unlock()
	}()

	src := s.updateSource()
	st := updateStatus{LastCheck: time.Now().Unix()}
	if info, err := src.Latest(ctx); err != nil {
		// 网络错误时保留上次已知的最新版本，界面仍可提示
		prev := s.loadUpdateStatus()
		st.Latest, st.Ready, st.Error = prev.Latest, prev.Ready, err.Error()
	} else {
		st.Latest = info
		if update.Compare(info.Version, Version) > 0 {
			if _, err := src.Checksums(ctx, info.Version); err == nil {
				st.Ready = true
			} else if !errors.Is(err, update.ErrNotPublished) {
				st.Error = err.Error()
			}
		}
	}
	s.db.SetSettingJSON("update_status", st)
	if v := st.available(); v != "" && s.db.Setting("update_notified", "") != v {
		s.db.SetSetting("update_notified", v)
		s.db.AddEvent("info", 0, 0, fmt.Sprintf("发现新版本 v%s（当前 v%s），可在系统设置中更新", v, Version))
	}
	return st
}

// updateLoop 启动 1 分钟后检测一次新版本，之后每小时检测（可在系统设置中关闭）。
func (s *Server) updateLoop(ctx context.Context) {
	timer := time.NewTimer(time.Minute)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}
		if s.settings().UpdateCheck {
			if st := s.checkUpdate(ctx); st.Error != "" {
				log.Printf("检测新版本失败：%s", st.Error)
			}
		}
		timer.Reset(time.Hour)
	}
}

// confirmUpdate 在新版本稳定运行一段时间后标记在线更新成功。
func (s *Server) confirmUpdate(ctx context.Context) {
	select {
	case <-ctx.Done():
		return
	case <-time.After(20 * time.Second):
	}
	if err := update.Confirm(s.dataDir, Version); err != nil {
		log.Printf("在线更新：记录更新状态失败：%v", err)
	}
}

func runtimeKind() string {
	switch {
	case os.Getenv("PVE_FORWARD_RUNTIME") == "docker":
		return "docker"
	case os.Getenv("INVOCATION_ID") != "":
		return "systemd"
	}
	return "binary"
}

func (s *Server) updateInfo() map[string]any {
	st := s.loadUpdateStatus()
	ok, reason := update.Supported()
	s.upd.mu.Lock()
	job, checking := s.upd.job, s.upd.checking
	s.upd.mu.Unlock()
	failed := ""
	if us, err := update.ReadState(s.dataDir); err == nil {
		failed = us.Failed
	}
	set := s.settings()
	return map[string]any{
		"current": Version, "runtime": runtimeKind(), "canUpdate": ok, "reason": reason, "arch": runtime.GOARCH,
		"autoCheck": set.UpdateCheck, "mirror": set.UpdateMirror, "checking": checking,
		"lastCheck": st.LastCheck, "latest": st.Latest, "ready": st.Ready, "error": st.Error,
		"available": st.available(), "job": job, "failed": failed,
		"image": update.Image, "repo": "https://github.com/" + update.Repo,
	}
}

func (s *Server) getUpdate(w http.ResponseWriter, r *http.Request) { writeJSON(w, s.updateInfo()) }

func (s *Server) postUpdateCheck(w http.ResponseWriter, r *http.Request) {
	if s.limiter.Blocked("update-check", 10, time.Minute) {
		jsonErr(w, http.StatusTooManyRequests, "检测过于频繁，请稍后再试")
		return
	}
	s.limiter.Fail("update-check")
	var req struct {
		Mirror *string `json:"mirror"`
	}
	if r.ContentLength != 0 && !readJSON(w, r, &req) {
		return
	}
	if req.Mirror != nil {
		mirror, ok := validMirror(*req.Mirror)
		if !ok {
			jsonErr(w, http.StatusBadRequest, "下载加速前缀需为 https:// 开头的地址")
			return
		}
		if st := s.settings(); st.UpdateMirror != mirror {
			st.UpdateMirror = mirror
			s.db.SetSettingJSON("settings", st)
		}
	}
	ctx, cancel := context.WithTimeout(r.Context(), time.Minute)
	defer cancel()
	s.checkUpdate(ctx)
	writeJSON(w, s.updateInfo())
}

// postUpdateApply 开始在线更新：后台下载并校验新程序，完成后替换当前进程。
func (s *Server) postUpdateApply(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Version string `json:"version"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	if ok, reason := update.Supported(); !ok && s.upd.install == nil {
		jsonErr(w, http.StatusBadRequest, reason)
		return
	}
	if v := s.loadUpdateStatus().available(); v == "" || v != req.Version {
		jsonErr(w, http.StatusBadRequest, "没有可更新的版本，请先检测更新")
		return
	}
	s.upd.mu.Lock()
	if s.upd.job.Running {
		s.upd.mu.Unlock()
		jsonErr(w, http.StatusConflict, "正在更新中")
		return
	}
	s.upd.job = updateJob{Running: true, Target: req.Version, Step: "准备更新"}
	s.upd.mu.Unlock()
	s.db.AddEvent("info", 0, 0, fmt.Sprintf("管理员 %s 开始在线更新：v%s → v%s", currentUser(r).Username, Version, req.Version))
	go s.runUpdate(req.Version)
	writeJSON(w, s.updateInfo())
}

func (s *Server) setJob(f func(j *updateJob)) {
	s.upd.mu.Lock()
	f(&s.upd.job)
	s.upd.mu.Unlock()
}

func (s *Server) runUpdate(version string) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()
	fail := func(err error) {
		log.Printf("在线更新失败：%v", err)
		s.db.AddEvent("error", 0, 0, fmt.Sprintf("在线更新到 v%s 失败：%v", version, err))
		s.setJob(func(j *updateJob) { j.Running, j.Error, j.Step = false, err.Error(), "更新失败" })
	}
	progress := func(step string, pct int) { s.setJob(func(j *updateJob) { j.Step, j.Percent = step, pct }) }
	install := s.upd.install
	if install == nil {
		install = func(ctx context.Context, in *update.Installer, v string, p update.Progress) (string, error) {
			return in.Install(ctx, v, p)
		}
	}
	path, err := install(ctx, &update.Installer{DataDir: s.dataDir, Source: s.updateSource()}, version, progress)
	if err != nil {
		fail(err)
		return
	}
	progress("备份数据库", 96)
	if err := s.backupDB(version); err != nil {
		fail(fmt.Errorf("备份数据库失败：%w", err))
		return
	}
	exe, _ := os.Executable()
	if err := update.Activate(s.dataDir, version, Version, exe); err != nil {
		fail(err)
		return
	}
	progress("正在重启控制端", 100)
	log.Printf("在线更新：v%s 已下载并校验，正在切换", version)
	s.upd.upgrade <- &UpgradeRequest{Path: path, Version: version}
}

// backupDB 在更新前把数据库备份到 backups/，只保留最近 5 份。
func (s *Server) backupDB(version string) error {
	dir := filepath.Join(s.dataDir, "backups")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	file := filepath.Join(dir, fmt.Sprintf("pre-update-%s-to-%s-%s.db", Version, version, time.Now().Format("20060102-150405")))
	if _, err := s.db.Exec(`VACUUM INTO ?`, file); err != nil {
		return err
	}
	os.Chmod(file, 0o600)
	old, _ := filepath.Glob(filepath.Join(dir, "pre-update-*.db"))
	sort.Slice(old, func(i, j int) bool { return modTime(old[i]).After(modTime(old[j])) })
	for _, f := range old[min(len(old), 5):] {
		os.Remove(f)
	}
	return nil
}

func modTime(p string) time.Time {
	if fi, err := os.Stat(p); err == nil {
		return fi.ModTime()
	}
	return time.Time{}
}

// validMirror 校验下载加速前缀：为空或 https 地址（明文 http 可被中间人替换程序），统一以 / 结尾。
func validMirror(m string) (string, bool) {
	m = strings.TrimSpace(m)
	if m == "" {
		return "", true
	}
	u, err := url.Parse(m)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return "", false
	}
	if !strings.HasSuffix(m, "/") {
		m += "/"
	}
	return m, true
}
