// Package pveforward 提供嵌入到程序中的版本信息。
//
// 版本号的唯一来源是仓库根目录的 version.json：构建时嵌入二进制，
// 控制端也通过读取 GitHub 上最新的 version.json 判断是否有新版本。
package pveforward

import (
	_ "embed"
	"encoding/json"
)

//go:embed version.json
var versionJSON []byte

// Version 为当前程序版本，如 1.2.3。
var Version = func() string {
	var v struct {
		Version string `json:"version"`
	}
	if json.Unmarshal(versionJSON, &v) != nil || v.Version == "" {
		panic("version.json 缺少 version 字段")
	}
	return v.Version
}()
