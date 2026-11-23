package main

import (
	_ "embed"
	"log"

	"github.com/wailsapp/wails/v3/pkg/application"
	"github.com/wailsapp/wails/v3/pkg/events"
)

//go:embed tray.png
var trayIcon []byte

func main() {
	// 1. 创建桌面应用，作为窗口和事件循环的入口。
	app := application.New(application.Options{Name: "CloudNest"})

	// 2. 设置首个窗口，用本地内容验证 WebView2 已完成加载。
	window := app.Window.NewWithOptions(application.WebviewWindowOptions{
		Title:  "CloudNest",
		Width:  960,
		Height: 640,
		HTML:   `<!doctype html><html lang="zh-CN"><head><meta charset="utf-8"><title>CloudNest</title></head><body><main><h1>CloudNest</h1></main></body></html>`,
	})

	// 3. 取消默认关闭动作，隐藏主窗口，保留窗口和页面状态。
	window.RegisterHook(events.Common.WindowClosing, func(event *application.WindowEvent) {
		event.Cancel()
		window.Hide()
	})

	// 4. 左键单击托盘图标或选择打开菜单项时，显示并聚焦同一个窗口。
	showMainWindow := func() {
		window.Show()
		window.Focus()
	}
	tray := app.SystemTray.New()
	tray.SetIcon(trayIcon)
	tray.SetTooltip("CloudNest")
	tray.OnClick(showMainWindow)
	menu := app.Menu.New()
	menu.Add("打开 CloudNest").OnClick(func(_ *application.Context) {
		showMainWindow()
	})
	menu.Add("关于 CloudNest").OnClick(func(_ *application.Context) {
		app.Dialog.Info().AttachToWindow(window).SetTitle("关于 CloudNest").SetMessage("CloudNest 桌面客户端").Show()
	})

	// 5. 用户选择退出时结束整个应用，窗口隐藏后也能通过托盘退出。
	menu.AddSeparator()
	menu.Add("退出 CloudNest").OnClick(func(_ *application.Context) {
		app.Quit()
	})
	tray.SetMenu(menu)

	// 6. 运行事件循环，应用退出后返回，启动失败时报告原因。
	if err := app.Run(); err != nil {
		log.Fatal(err)
	}
}
