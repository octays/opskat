package notifycenter

import (
	"context"
	"testing"
)

// 后台(focusedWindows==0)且已注入发送函数时,通知发出;前台时不发。
func TestSendOnlyWhenBackgrounded(t *testing.T) {
	sent := make([]string, 0, 2)
	Init(func(title, body string) error {
		sent = append(sent, title)
		return nil
	})

	Send(context.Background(), "AI 任务完成", "会话 A")
	if len(sent) != 1 {
		t.Fatalf("backgrounded send got %d notifications, want 1", len(sent))
	}

	windowFocused()
	Send(context.Background(), "AI 任务完成", "会话 A")
	if len(sent) != 1 {
		t.Fatalf("foreground send got %d notifications, want still 1", len(sent))
	}

	windowBlurred()
	Send(context.Background(), "AI 任务完成", "会话 A")
	if len(sent) != 2 {
		t.Fatalf("after blur got %d notifications, want 2", len(sent))
	}
}

// 未注入发送函数(测试/未初始化)时静默空操作,不 panic。
func TestSendWithoutInitIsNoop(t *testing.T) {
	Init(nil)
	Send(context.Background(), "AI 任务完成", "会话 A")
}

// LostFocus 多于 Focus(事件乱序/重复)不会把计数打成负数。
func TestBlurNeverUnderflows(t *testing.T) {
	focusedWindows = 0
	windowBlurred()
	windowBlurred()
	if focusedWindows != 0 {
		t.Fatalf("focusedWindows = %d, want 0 (no underflow)", focusedWindows)
	}
}

// 传输终态：完成/失败通知，取消(用户主动)与未知状态不打扰。
func TestTransferFinishedNotifiesDoneAndErrorOnly(t *testing.T) {
	sent := make([]string, 0, 2)
	Init(func(title, body string) error {
		sent = append(sent, title+"/"+body)
		return nil
	})

	TransferFinished(context.Background(), "done", "report.tar.gz")
	TransferFinished(context.Background(), "error", "backup.sql")
	TransferFinished(context.Background(), "cancelled", "movie.mp4")
	TransferFinished(context.Background(), "progress", "weird.mid")

	want := []string{"传输完成/report.tar.gz", "传输失败/backup.sql"}
	if len(sent) != len(want) {
		t.Fatalf("got %v, want %v", sent, want)
	}
	for i := range want {
		if sent[i] != want[i] {
			t.Fatalf("notification %d = %q, want %q", i, sent[i], want[i])
		}
	}
}
