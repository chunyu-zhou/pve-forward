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
	m.Live(time.Second) // 不低于 3 秒
	if got := m.interval(); got != 3*time.Second {
		t.Fatalf("实时 %v", got)
	}
	m.Live(30 * time.Second) // 多个页面时取最短的
	if got := m.interval(); got != 3*time.Second {
		t.Fatalf("取最短 %v", got)
	}
	m.liveMu.Lock()
	m.live[3*time.Second] = time.Now().Add(-time.Second) // 实时页面已关闭
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
