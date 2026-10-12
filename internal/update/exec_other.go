//go:build !unix

package update

import "errors"

// Exec 以 path 指向的程序替换当前进程；非 Unix 平台不支持。
func Exec(path string) error { return errors.New("当前平台不支持替换进程") }
