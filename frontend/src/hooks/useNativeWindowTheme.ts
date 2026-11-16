import { useEffect } from "react";
import { useResolvedTheme } from "@/components/theme-provider";
import { SetWindowAppearance } from "@clients/system";

/**
 * 把应用内主题同步给原生窗口外观：macOS 标题栏底色与全屏 hover 展示条是系统
 * 按窗口外观（NSAppearance）绘制的，不跟随 CSS 主题——应用配了夜间模式而系统
 * 是亮色时不会跟着变，需要显式告知后端。观察 <html> 上的 .dark class（与
 * useResolvedTheme 同源），主题或系统偏好变化都会触发。
 *
 * 同步是尽力而为：桌面桥不可用（vitest 环境）时保持原生现状，不阻断主题切换。
 */
export function useNativeWindowTheme(): void {
  const resolved = useResolvedTheme();
  useEffect(() => {
    void SetWindowAppearance(resolved === "dark").catch(() => undefined);
  }, [resolved]);
}
