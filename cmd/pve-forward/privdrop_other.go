//go:build !linux

package main

// dropPrivileges 仅用于 Linux 上的 Docker 镜像。
func dropPrivileges(string) error { return nil }
