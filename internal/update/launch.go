package update

import (
	"log"
	"os"
	"path/filepath"
	"runtime"
)

// envExeced 由启动器在切换到在线更新版本时设置，避免重复切换。
const envExeced = "PVE_FORWARD_UPDATE_EXECED"

// Supported 返回当前平台是否支持在线更新程序及原因。
func Supported() (bool, string) {
	if runtime.GOOS != "linux" {
		return false, "仅 Linux 支持在线更新程序"
	}
	if runtime.GOARCH != "amd64" && runtime.GOARCH != "arm64" {
		return false, "仅 amd64 / arm64 提供在线更新程序"
	}
	return true, ""
}

// Launch 在控制端启动时调用（打开数据库之前）：若数据目录中有比当前程序更新的
// 在线更新版本，则以该版本替换当前进程；该函数在需要切换时不会返回。
//
//   - 程序本体（如新拉取的 Docker 镜像）不旧于在线更新版本时，直接使用本体并清理在线更新。
//   - 新版本上次启动后未能确认正常运行（崩溃退出后被 Docker / systemd 重启），回退到更新前的版本。
func Launch(dataDir, self string) {
	if os.Getenv(envExeced) != "" {
		os.Unsetenv(envExeced)
		return
	}
	if ok, _ := Supported(); !ok {
		return
	}
	target, err := plan(dataDir, self)
	if err != nil {
		log.Printf("在线更新：%v，使用当前程序 v%s 启动", err, self)
		return
	}
	if target == "" {
		return
	}
	if err := Switch(target); err != nil {
		log.Printf("在线更新：切换到 %s 失败：%v，使用当前程序 v%s 启动", target, err, self)
	}
}

// Switch 以在线更新版本替换当前进程。新进程启动时 Launch 不再重复切换，
// 由它确认（Confirm）自身正常运行；若它崩溃退出，被 Docker / systemd 重启后
// 程序本体中的 Launch 会回退到更新前的版本。
func Switch(path string) error {
	os.Setenv(envExeced, "1")
	err := Exec(path)
	os.Unsetenv(envExeced)
	return err
}

// plan 决定启动时要运行的程序，返回空字符串表示使用当前程序。
func plan(dataDir, self string) (string, error) {
	st, err := ReadState(dataDir)
	if err != nil || st.Current == "" {
		return "", err
	}
	if Compare(st.Current, self) <= 0 {
		log.Printf("在线更新：程序本体 v%s 不旧于在线更新版本 v%s，使用程序本体", self, st.Current)
		prune(dataDir)
		return "", os.Remove(filepath.Join(Dir(dataDir), "state.json"))
	}
	if st.Pending && st.Attempts >= 1 {
		failed := st.Current
		log.Printf("在线更新：v%s 未能正常启动，回退到%s", failed, describe(st.Previous))
		*st = State{Current: st.Previous, Failed: failed}
		if err := WriteState(dataDir, st); err != nil {
			return "", err
		}
		prune(dataDir, st.Current)
		if st.Current == "" || Compare(st.Current, self) <= 0 {
			return "", nil
		}
	} else if st.Pending {
		st.Attempts++
		if err := WriteState(dataDir, st); err != nil {
			return "", err
		}
	}
	target := BinaryPath(dataDir, st.Current)
	if fi, err := os.Stat(target); err != nil || !fi.Mode().IsRegular() {
		log.Printf("在线更新：未找到 v%s 的程序文件，使用程序本体 v%s", st.Current, self)
		return "", nil
	}
	return target, nil
}

func describe(v string) string {
	if v == "" {
		return "程序本体"
	}
	return " v" + v
}
