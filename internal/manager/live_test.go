package manager

import (
	"testing"
	"time"
)

// 监控页实时刷新：有效期内使用更短的采集间隔，过期后恢复系统设置的间隔。
func TestLiveInterval(t *testing.T) {
	m := New(nil, nil)
	m.Interval = func() time.Duration { return 60 * time.Second }
	if got := m.interval(); got != 60*time.Second {
		t.Fatalf("默认 %v", got)
	}
	m.Live(time.Second) // 不低于 2 秒
	if got := m.interval(); got != 2*time.Second {
		t.Fatalf("实时 %v", got)
	}
	m.Live(30 * time.Second) // 多个页面时取最短的
	if got := m.interval(); got != 2*time.Second {
		t.Fatalf("取最短 %v", got)
	}
	m.liveMu.Lock()
	m.live[2*time.Second] = time.Now().Add(-time.Second) // 实时页面已关闭
	m.liveMu.Unlock()
	if got := m.interval(); got != 30*time.Second {
		t.Fatalf("实时页面关闭后 %v", got)
	}
	m.Live(120 * time.Second) // 比系统间隔长的请求不影响
	m.liveMu.Lock()
	m.live[30*time.Second] = time.Now().Add(-time.Second)
	m.liveMu.Unlock()
	if got := m.interval(); got != 60*time.Second {
		t.Fatalf("恢复 %v", got)
	}
}

// 采集完成通知：订阅者收到合并后的通知，取消后不再收到。
func TestSubscribe(t *testing.T) {
	m := New(nil, nil)
	ch, cancel := m.Subscribe()
	m.notify()
	m.notify() // 未读取前的多次通知合并为一次
	select {
	case <-ch:
	default:
		t.Fatal("应收到通知")
	}
	select {
	case <-ch:
		t.Fatal("通知应合并")
	default:
	}
	cancel()
	m.notify()
	select {
	case <-ch:
		t.Fatal("取消后不应收到")
	default:
	}
}
