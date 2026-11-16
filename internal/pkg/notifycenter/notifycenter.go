// Package notifycenter 把"值得把用户从后台拉回来"的时刻送进系统通知中心
// (macOS 通知中心 / Windows 操作中心)。应用有窗口在焦点时静默跳过——用户看得到
// 应用内提示,通知中心只补后台盲区。
//
// 与应用内 toast(frontend/src/lib/notify.ts)分工:toast 是前台瞬时反馈,
// 这里是后台离场提醒;审批类事件不需要走这里,到达时已由 ActivateWindow 拉回前台。
package notifycenter

import (
	"context"
	"sync"

	"github.com/cago-frame/cago/pkg/logger"
	"github.com/wailsapp/wails/v3/pkg/application"
	"github.com/wailsapp/wails/v3/pkg/events"
	"go.uber.org/zap"
)

var (
	mu     sync.Mutex
	sendFn func(title, body string) error
	// focusedWindows 统计当前持焦的窗口数;0 = 应用整体在后台。
	focusedWindows int
)

// Init 注入实际的系统通知发送函数。main.go 用 Wails notifications 服务包装;
// 未注入(测试)时 Send 是空操作。
func Init(send func(title, body string) error) {
	mu.Lock()
	defer mu.Unlock()
	sendFn = send
}

// TrackWindow 跟踪该窗口的焦点变化,据此得知应用是否在后台。主窗口与工作区窗口
// 都应调用。
func TrackWindow(w application.Window) {
	w.RegisterHook(events.Common.WindowFocus, func(*application.WindowEvent) { windowFocused() })
	w.RegisterHook(events.Common.WindowLostFocus, func(*application.WindowEvent) { windowBlurred() })
}

func windowFocused() {
	mu.Lock()
	defer mu.Unlock()
	focusedWindows++
}

func windowBlurred() {
	mu.Lock()
	defer mu.Unlock()
	if focusedWindows > 0 {
		focusedWindows--
	}
}

// Send 在应用后台时发系统通知;未初始化、在前台或发送失败都不影响业务流,
// 失败仅记日志(title 进日志,body 可能含会话标题等,一并记录无敏感凭据)。
func Send(ctx context.Context, title, body string) {
	mu.Lock()
	send, background := sendFn, focusedWindows == 0
	mu.Unlock()
	if send == nil || !background {
		return
	}
	if err := send(title, body); err != nil {
		logger.Ctx(ctx).Warn("system notification failed", zap.String("title", title), zap.Error(err))
	}
}
