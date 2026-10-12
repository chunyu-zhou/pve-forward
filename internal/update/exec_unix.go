//go:build unix

package update

import (
	"os"
	"syscall"
)

// Exec 以 path 指向的程序替换当前进程，保留命令行参数与环境变量。
func Exec(path string) error {
	return syscall.Exec(path, append([]string{path}, os.Args[1:]...), os.Environ())
}
