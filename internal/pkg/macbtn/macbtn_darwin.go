//go:build darwin

// Package macbtn 把 macOS 原生窗口控制按钮（红绿灯）的垂直中心对齐到自定义
// 顶栏的中线。Wails v3 没有移动原生按钮的 API：AppKit 把红绿灯按系统标题栏
// （~28pt）居中，与前端 36px 顶栏的 18pt 中线错位——这里直接设置按钮 frame
// （Electron 的 setWindowButtonPosition 同思路），保留原生外观与行为。
//
// 全屏切换用「还原默认」处理：首次对齐前记录 AppKit 的原始 frame，进全屏时还原，
// 让系统在全屏 hover 展示的菜单栏里自己布局红绿灯；退出/显示时重新对齐。
package macbtn

/*
#cgo CFLAGS: -x objective-c
#import <Cocoa/Cocoa.h>
#import <dispatch/dispatch.h>
#import <objc/runtime.h>

static char kDefaultsKey;

static NSArray *windowButtons(NSWindow *w) {
	return @[[w standardWindowButton:NSWindowCloseButton],
	         [w standardWindowButton:NSWindowMiniaturizeButton],
	         [w standardWindowButton:NSWindowZoomButton]];
}

// saveDefaults 记录按钮的原始 frame（每窗口一次），供进全屏时还原。
static void saveDefaults(NSWindow *w) {
	if (objc_getAssociatedObject(w, &kDefaultsKey) != nil) return;
	NSMutableDictionary *d = [NSMutableDictionary dictionary];
	for (NSButton *b in windowButtons(w)) {
		if (b == nil) continue;
		[d setObject:[NSValue valueWithRect:b.frame] forKey:[NSString stringWithFormat:@"%p", b]];
	}
	objc_setAssociatedObject(w, &kDefaultsKey, d, OBJC_ASSOCIATION_RETAIN);
}

// restoreDefaults 把按钮放回 AppKit 原始位置（全屏展示交给系统布局）。
static void restoreDefaults(NSWindow *w) {
	NSDictionary *d = objc_getAssociatedObject(w, &kDefaultsKey);
	if (d == nil) return;
	for (NSButton *b in windowButtons(w)) {
		if (b == nil) continue;
		NSValue *v = [d objectForKey:[NSString stringWithFormat:@"%p", b]];
		if (v != nil) [b setFrame:[v rectValue]];
	}
}

// alignButtons 把每个可见窗口的三个标准按钮的垂直中心移到「距窗口顶部 targetCenter」。
// 不依赖按钮容器（superview）的坐标系假设：先把按钮中心换算到 contentView 坐标
// （左下原点、y 向上）得到当前距顶距离，再按 superview 是否 flipped 决定平移方向。
// 按目标绝对位置换算，幂等：重复调用与 AppKit 重排后都会被重新校正。
static void alignButtons(double targetCenter) {
	dispatch_async(dispatch_get_main_queue(), ^{
		for (NSWindow *w in [NSApp windows]) {
			if (w.contentView == nil || ![w isVisible]) continue;
			if (w.styleMask & NSWindowStyleMaskFullScreen) continue;
			saveDefaults(w);
			double contentH = w.contentView.bounds.size.height;
			for (NSButton *b in windowButtons(w)) {
				if (b == nil || b.superview == nil) continue;
				NSRect rw = [b convertRect:b.bounds toView:w.contentView];
				if (rw.size.height == 0) continue;
				double curFromTop = contentH - (rw.origin.y + rw.size.height / 2);
				BOOL flipped = [b.superview isFlipped];
				// 非翻转（y 向上）：上移 = origin.y 增大；翻转（y 向下）：上移 = origin.y 减小。
				double dy = flipped ? (targetCenter - curFromTop) : (curFromTop - targetCenter);
				NSRect f = b.frame;
				f.origin.y += dy;
				[b setFrame:f];
			}
		}
	});
}

// restoreAll 把所有窗口的按钮还原到 AppKit 原始位置（进入全屏时调用）。
static void restoreAll() {
	dispatch_async(dispatch_get_main_queue(), ^{
		for (NSWindow *w in [NSApp windows]) {
			if (w.contentView == nil) continue;
			restoreDefaults(w);
		}
	});
}

// setAppearanceAll 把所有应用窗口（含全屏 hover 展示的覆盖层窗口）的 NSAppearance
// 切到亮/暗。原生绘制的背景（标题栏底色、hover bar）按窗口外观着色，不跟随 CSS 主题。
static void setAppearanceAll(int dark) {
	dispatch_async(dispatch_get_main_queue(), ^{
		NSAppearance *a = [NSAppearance appearanceNamed:dark ? NSAppearanceNameDarkAqua : NSAppearanceNameAqua];
		for (NSWindow *w in [NSApp windows]) {
			if (w.contentView == nil) continue;
			[w setAppearance:a];
		}
	});
}

// clearHoverBar 已移除：全屏 hover 展示条的系统底色无法可靠透明化（材质视图的
// alpha 会被系统重置，移出层级后窗口仍自绘底色），主题一致性由 SetWindowAppearance
// 的外观同步承担。
*/
import "C"

import (
	"sync/atomic"
	"time"

	"github.com/wailsapp/wails/v3/pkg/application"
	"github.com/wailsapp/wails/v3/pkg/events"
)

// BarHeightPt 是前端顶栏（WindowTitleBar，h-9 = 36px）的 CSS 高度。红绿灯以它
// 的中线对齐；改 h-* 时同步更新这里、frontend/src/styles/globals.css 的
// --offset-top 与 docs/DESIGN.md。
const BarHeightPt = 36.0

// TrackWindow 把窗口的红绿灯生命周期接管到顶栏对齐上。窗口态对齐到顶栏中线，
// 并在显示后补两次延迟对齐——启动期 AppKit 在窗口显示后还会重排一次标题栏，
// 单次对齐会被覆盖（对齐幂等，多跑无害）。进入全屏时还原系统默认位置，让
// hover 展示的菜单栏由系统自己布局红绿灯；退出全屏重新对齐。
// 按钮状态只在进出全屏时写：显示/聚焦路径每次都写会让 AppKit 重排按钮，把刚做的
// 对齐又冲回默认位。主窗口与工作区窗口都应调用。
func TrackWindow(w application.Window) {
	center := BarHeightPt / 2
	alignLater := func() {
		AlignTrafficLights(center)
		for _, delay := range []time.Duration{250 * time.Millisecond, time.Second} {
			time.AfterFunc(delay, func() { AlignTrafficLights(center) })
		}
	}
	w.RegisterHook(events.Common.WindowShow, func(*application.WindowEvent) {
		alignLater()
		applyAppearance()
	})
	w.RegisterHook(events.Common.WindowFocus, func(*application.WindowEvent) { AlignTrafficLights(center) })
	w.RegisterHook(events.Common.WindowFullscreen, func(*application.WindowEvent) {
		RestoreTrafficLights()
		// 全屏 hover 展示条在这步才被系统创建，拿的是系统默认外观——补设应用主题，
		// 延迟踢覆盖切换动画后的创建时点。
		applyAppearance()
		for _, delay := range []time.Duration{500 * time.Millisecond, 2 * time.Second} {
			time.AfterFunc(delay, applyAppearance)
		}
	})
	w.RegisterHook(events.Common.WindowUnFullscreen, func(*application.WindowEvent) {
		setButtons(w, application.ButtonEnabled)
		alignLater()
		applyAppearance()
	})
	// 工作区窗口的显示事件在 NewWithOptions 里就已发出，注册 hook 时已错过，
	// 这里兜底立即对齐一次。
	alignLater()
}

func setButtons(w application.Window, state application.ButtonState) {
	w.SetCloseButtonState(state)
	w.SetMinimiseButtonState(state)
	w.SetMaximiseButtonState(state)
}

// AlignTrafficLights 把所有窗口的红绿灯垂直中心对齐到距窗口顶部 centerPt 点处，
// 主线程异步执行；全屏中的窗口自动跳过。
func AlignTrafficLights(centerPt float64) {
	C.alignButtons(C.double(centerPt))
}

// RestoreTrafficLights 把所有窗口的红绿灯还原到 AppKit 原始位置。
func RestoreTrafficLights() {
	C.restoreAll()
}

// 前端同步过来的主题;未配置(atomic false)时窗口跟随系统外观。
var (
	appearanceConfigured atomic.Bool
	appearanceDark       atomic.Bool
)

// SetWindowAppearance 把应用窗口（含全屏 hover 展示的覆盖层窗口）的原生外观切到
// 亮/暗。原生绘制的背景（标题栏底色、hover bar）不跟随 CSS 主题——应用内主题与
// 系统外观不一致时由前端在主题生效时调用同步；之后新建的窗口在显示时自动沿用。
func SetWindowAppearance(dark bool) {
	appearanceConfigured.Store(true)
	appearanceDark.Store(dark)
	if dark {
		C.setAppearanceAll(1)
	} else {
		C.setAppearanceAll(0)
	}
}

// applyAppearance 把已配置的外观补设到当前所有窗口（新窗口显示时调用）。
func applyAppearance() {
	if appearanceConfigured.Load() {
		SetWindowAppearance(appearanceDark.Load())
	}
}
