// Package web 嵌入前端构建产物（web/dist）。
package web

import (
	"embed"
	"io/fs"
)

//go:embed all:dist
var dist embed.FS

func FS() fs.FS {
	f, _ := fs.Sub(dist, "dist")
	return f
}
