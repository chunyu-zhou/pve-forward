package server

import (
	"strings"
	"testing"

	"pveforward/internal/store"
)

// Agent 通过 WSS 连接：访问地址不是 HTTPS 时不能新建或改为 Agent 方式，已接入的 Agent 节点不受影响。
func TestAgentRequiresHTTPS(t *testing.T) {
	s := newEnvTestServer(t)
	setURL := func(u string) {
		st := s.settings()
		st.URL = u
		s.db.SetSettingJSON("settings", st)
	}
	if why := s.agentUnavailable(); !strings.Contains(why, "尚未设置") {
		t.Fatalf("未设置地址：%q", why)
	}
	setURL("http://203.0.113.10")
	if why := s.agentUnavailable(); !strings.Contains(why, "不是 HTTPS") {
		t.Fatalf("http 地址：%q", why)
	}
	if _, err := s.applyConn(&store.Node{}, &nodeIn{Mode: "agent"}); err == nil {
		t.Fatal("http 地址不能新建 Agent 节点")
	}
	ssh := &store.Node{ID: 1, Mode: "ssh"}
	if _, err := s.applyConn(ssh, &nodeIn{Mode: "agent"}); err == nil {
		t.Fatal("http 地址不能改为 Agent 方式")
	}
	if _, err := s.applyConn(&store.Node{ID: 2, Mode: "agent"}, &nodeIn{Mode: "agent"}); err != nil {
		t.Fatalf("已接入的 Agent 节点应可保存：%v", err)
	}
	setURL("https://panel.example.com")
	if why := s.agentUnavailable(); why != "" {
		t.Fatalf("https 地址：%q", why)
	}
	if _, err := s.applyConn(&store.Node{}, &nodeIn{Mode: "agent"}); err != nil {
		t.Fatal(err)
	}
}
