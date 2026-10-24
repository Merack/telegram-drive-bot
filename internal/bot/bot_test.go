package bot

import (
	"context"
	"testing"
	"time"

	tele "gopkg.in/telebot.v4"
)

// async 必须立即返回(不阻塞更新循环),并且被 WaitAsyncHandlers 等到
func TestAsyncHandlerRunsOffLoopAndDrains(t *testing.T) {
	b, err := tele.NewBot(tele.Settings{Token: "42:TEST", Offline: true})
	if err != nil {
		t.Fatalf("new bot: %v", err)
	}
	c := b.NewContext(tele.Update{
		ID:      1,
		Message: &tele.Message{ID: 2, Chat: &tele.Chat{ID: 3}, Sender: &tele.User{ID: 4}},
	})

	ran := make(chan struct{})
	h := async(b, func(tele.Context) error {
		time.Sleep(20 * time.Millisecond) // 模拟网络等待
		close(ran)
		return nil
	})

	start := time.Now()
	if err := h(c); err != nil {
		t.Fatalf("async handler returned error: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 10*time.Millisecond {
		t.Fatalf("async handler blocked the caller for %v", elapsed)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if !WaitAsyncHandlers(ctx) {
		t.Fatal("WaitAsyncHandlers returned before the handler finished")
	}
	select {
	case <-ran:
	default:
		t.Fatal("handler did not run")
	}
}

// 错误必须走 bot 的 OnError,而不是被吞掉
func TestAsyncHandlerReportsError(t *testing.T) {
	errCh := make(chan error, 1)
	b, err := tele.NewBot(tele.Settings{
		Token:   "42:TEST",
		Offline: true,
		OnError: func(err error, _ tele.Context) { errCh <- err },
	})
	if err != nil {
		t.Fatalf("new bot: %v", err)
	}
	c := b.NewContext(tele.Update{ID: 1, Message: &tele.Message{ID: 2, Chat: &tele.Chat{ID: 3}}})

	want := context.DeadlineExceeded
	if err := async(b, func(tele.Context) error { return want })(c); err != nil {
		t.Fatalf("async handler returned error: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	WaitAsyncHandlers(ctx)

	select {
	case got := <-errCh:
		if got != want {
			t.Fatalf("OnError got %v, want %v", got, want)
		}
	default:
		t.Fatal("handler error was swallowed")
	}
}
