package update

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
)

// State 记录数据目录中在线更新的版本，保存在 updates/state.json。
type State struct {
	// Current 为应运行的在线更新版本；为空表示使用程序本体（镜像或安装包中的程序）。
	Current string `json:"current,omitempty"`
	// Previous 为更新前运行的在线更新版本；为空表示更新前运行的是程序本体。
	Previous string `json:"previous,omitempty"`
	// Pending 表示新版本尚未确认正常运行；Attempts 为已尝试启动的次数。
	Pending  bool `json:"pending,omitempty"`
	Attempts int  `json:"attempts,omitempty"`
	// Failed 为最近一次启动失败并已回退的版本。
	Failed string `json:"failed,omitempty"`
}

// Dir 返回在线更新目录。
func Dir(dataDir string) string { return filepath.Join(dataDir, "updates") }

// BinaryPath 返回某个在线更新版本的控制端程序路径。
func BinaryPath(dataDir, version string) string {
	return filepath.Join(Dir(dataDir), version, "pve-forward")
}

// ReadState 读取更新状态；文件不存在时返回空状态。
func ReadState(dataDir string) (*State, error) {
	b, err := os.ReadFile(filepath.Join(Dir(dataDir), "state.json"))
	if errors.Is(err, os.ErrNotExist) {
		return &State{}, nil
	}
	if err != nil {
		return nil, err
	}
	var st State
	if err := json.Unmarshal(b, &st); err != nil {
		return nil, err
	}
	// 只接受合法版本号，防止路径穿越
	if st.Current != "" && !Valid(st.Current) || st.Previous != "" && !Valid(st.Previous) {
		return nil, errors.New("updates/state.json 中的版本号无效")
	}
	return &st, nil
}

// WriteState 原子写入更新状态。
func WriteState(dataDir string, st *State) error {
	dir := Dir(dataDir)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	b, _ := json.MarshalIndent(st, "", "  ")
	tmp := filepath.Join(dir, ".state.json.tmp")
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(dir, "state.json"))
}

// prune 删除除 keep 以外的在线更新版本目录（保留 state.json）。
func prune(dataDir string, keep ...string) {
	entries, err := os.ReadDir(Dir(dataDir))
	if err != nil {
		return
	}
	for _, e := range entries {
		if !e.IsDir() || !Valid(e.Name()) {
			continue
		}
		kept := false
		for _, k := range keep {
			if k != "" && e.Name() == k {
				kept = true
			}
		}
		if !kept {
			os.RemoveAll(filepath.Join(Dir(dataDir), e.Name()))
		}
	}
}

// Release 在正常退出（收到停止信号）时调用：新版本尚未确认就被正常停止
// （如重启容器、重启主机），不算启动失败，下次启动重新尝试。
func Release(dataDir, self string) error {
	st, err := ReadState(dataDir)
	if err != nil || st.Current != self || !st.Pending {
		return err
	}
	st.Attempts = 0
	return WriteState(dataDir, st)
}

// MarkFailed 在无法切换到新版本时调用：回退到更新前的版本并记录失败。
func MarkFailed(dataDir, version string) error {
	st, err := ReadState(dataDir)
	if err != nil {
		return err
	}
	if st.Current != version {
		return nil
	}
	*st = State{Current: st.Previous, Failed: version}
	if err := WriteState(dataDir, st); err != nil {
		return err
	}
	prune(dataDir, st.Current)
	return nil
}

// Confirm 在新版本正常运行一段时间后调用：标记更新成功并清理旧版本。
func Confirm(dataDir, self string) error {
	st, err := ReadState(dataDir)
	if err != nil || st.Current != self || !st.Pending {
		return err
	}
	st.Pending, st.Attempts = false, 0
	if err := WriteState(dataDir, st); err != nil {
		return err
	}
	prune(dataDir, st.Current, st.Previous)
	return nil
}
