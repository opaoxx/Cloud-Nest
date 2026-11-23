package main

import (
	"log"

	"github.com/wailsapp/wails/v3/pkg/application"
)

func main() {
	// 1. 创建桌面应用，作为窗口和事件循环的入口。
	app := application.New(application.Options{Name: "CloudNest"})

	// 2. 设置首个窗口，用本地内容验证 WebView2 已完成加载。
	app.Window.NewWithOptions(application.WebviewWindowOptions{
		Title:  "CloudNest",
		Width:  960,
		Height: 640,
		HTML:   `<!doctype html><html lang="zh-CN"><head><meta charset="utf-8"><title>CloudNest</title></head><body><main><h1>CloudNest</h1></main></body></html>`,
	})

	// 3. 运行事件循环，窗口关闭后退出，启动失败时报告原因。
	if err := app.Run(); err != nil {
		log.Fatal(err)
	}
}
