import { fireEvent, render, screen, waitFor } from "@testing-library/react";
import { describe, expect, it } from "vitest";
import { TreeSelect, Dialog, DialogContent, DialogTrigger, Button } from "@opskat/ui";

const nodes = [
  { id: 1, label: "web-01", icon: <span data-testid="node-icon" /> },
  { id: 2, label: "web-02" },
];

// 复现 macOS WKWebView 上的真实缺陷：TreeSelect 的下拉 portal 到 body，不在 Radix
// 模态 Dialog 的 DOM 范围内。FocusScope 在 document 上监听 focusin，把聚焦到下拉
// 搜索框视为「焦点逃逸」并同步把焦点抢回对话框——WKWebView 里表现为搜索框完全
// 无法键入。修复方式是在下拉容器上截断焦点事件冒泡；本测试守护这个机制。
describe("TreeSelect search focus inside a modal dialog", () => {
  it("keeps focus on the dropdown search input (not stolen by the dialog focus trap)", async () => {
    render(
      <Dialog defaultOpen>
        <DialogTrigger asChild>
          <Button>open</Button>
        </DialogTrigger>
        <DialogContent>
          <TreeSelect
            value={0}
            onValueChange={() => {}}
            nodes={nodes}
            placeholder="Choose server"
            searchable
            searchPlaceholder="Search..."
          />
        </DialogContent>
      </Dialog>
    );

    fireEvent.click(screen.getByRole("button", { name: /choose server/i }));
    const search = await waitFor(() => screen.getByPlaceholderText("Search..."));

    search.focus();
    await waitFor(() => {
      expect(document.activeElement).toBe(search);
    });

    // 模拟键入路径：聚焦不被抢走时，controlled input 才会收到变更。
    fireEvent.change(search, { target: { value: "web" } });
    expect((search as HTMLInputElement).value).toBe("web");
  });
});
