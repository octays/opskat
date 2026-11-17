import { describe, expect, it } from "vitest";
import { shouldActivateTerminalUrl } from "../terminalRegistry";

// 终端 URL 的打开门槛：裸点击不弹浏览器（误触率高，选中/复制经常点在链接上），
// 只有平台修饰键（⌘/Ctrl）+左键才允许，且右/中键始终不开。
function click({ button = 0, metaKey = false, ctrlKey = false } = {}): MouseEvent {
  return { button, metaKey, ctrlKey } as unknown as MouseEvent;
}

describe("shouldActivateTerminalUrl", () => {
  it("rejects bare left-click (the accidental-hit case)", () => {
    expect(shouldActivateTerminalUrl(click())).toBe(false);
  });

  it("accepts modifier + left-click (⌘ or Ctrl)", () => {
    expect(shouldActivateTerminalUrl(click({ metaKey: true }))).toBe(true);
    expect(shouldActivateTerminalUrl(click({ ctrlKey: true }))).toBe(true);
  });

  it("rejects non-left buttons even with modifier", () => {
    expect(shouldActivateTerminalUrl(click({ button: 2, metaKey: true }))).toBe(false);
    expect(shouldActivateTerminalUrl(click({ button: 1, ctrlKey: true }))).toBe(false);
  });

  it("accepts keyboard-less activation (programmatic/undefined event)", () => {
    expect(shouldActivateTerminalUrl(undefined)).toBe(true);
  });
});
