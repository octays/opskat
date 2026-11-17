package notifycenter

import (
	"context"

	"github.com/octays/opskat/internal/pkg/transfer"
)

// TransferFinished 传输到达终态时的后台通知：完成/失败告知，取消是用户主动行为
// 不打扰。subject 是传输对象描述（文件名/路径）；错误详情不进通知正文（可能很长，
// 应用内传输面板有完整信息）。
func TransferFinished(ctx context.Context, status, subject string) {
	switch status {
	case transfer.StatusDone:
		Send(ctx, "传输完成", subject)
	case transfer.StatusError:
		Send(ctx, "传输失败", subject)
	}
}
