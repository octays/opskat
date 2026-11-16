//go:build !darwin

// Package macbtn 在非 macOS 平台是空实现：红绿灯对齐只对 darwin 的原生窗口按钮有意义。
package macbtn

import "github.com/wailsapp/wails/v3/pkg/application"

// BarHeightPt 与 darwin 实现保持同一常量（非 darwin 平台仅用于文档一致性）。
const BarHeightPt = 36.0

// TrackWindow 在非 darwin 平台不做任何事。
func TrackWindow(_ application.Window) {}

// AlignTrafficLights 在非 darwin 平台不做任何事。
func AlignTrafficLights(_ float64) {}

// SetWindowAppearance 在非 darwin 平台不做任何事。
func SetWindowAppearance(_ bool) {}
