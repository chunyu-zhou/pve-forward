package nodeops

import (
	"context"
	"os/exec"
	"strings"
	"testing"
	"time"

	"pveforward/internal/proto"
)

type runtimeCapture struct{ script string }

func (e *runtimeCapture) Run(_ context.Context, script string) ([]byte, error) {
	e.script = script
	return []byte("##version\nopenresty/test\n"), nil
}

func TestCommandOutputBounded(t *testing.T) {
	var streamed strings.Builder
	out := NewCommandOutput(func(s string) {
		if len(s) > 4096 {
			t.Fatal("oversized chunk")
		}
		streamed.WriteString(s)
	})
	text := strings.Repeat("x", 300<<10) + "finished"
	if _, err := out.Write([]byte(text)); err != nil {
		t.Fatal(err)
	}
	if streamed.String() != text || len(out.Bytes()) != 256<<10 || !strings.HasSuffix(string(out.Bytes()), "finished") {
		t.Fatal("incorrect stream or retained tail")
	}
}

func TestCommandOutputUTF8(t *testing.T) {
	var got strings.Builder
	out := NewCommandOutput(func(s string) { got.WriteString(s) })
	text := strings.Repeat("x", 4095) + "正在安装完成"
	for _, b := range []byte(text) {
		out.Write([]byte{b})
	}
	out.Bytes()
	if got.String() != text {
		t.Fatalf("split UTF8 corrupted: %q", got.String())
	}
}

func TestLocalCommandStreamsBeforeExit(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash unavailable")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	observed := make(chan struct{}, 1)
	ctx = WithOutputLog(ctx, func(text string) {
		if strings.Contains(text, "stream-ready") {
			select {
			case observed <- struct{}{}:
			default:
			}
			cancel()
		}
	})
	_, err := (LocalExecutor{}).Run(ctx, "printf stream-ready; while :; do :; done\n")
	select {
	case <-observed:
	default:
		t.Fatal("missing live output")
	}
	if err == nil {
		t.Fatal("expected command failure")
	}
}

func TestWebRuntimeInstall(t *testing.T) {
	for _, deploy := range []string{"binary", "docker"} {
		t.Run(deploy, func(t *testing.T) {
			ex := &runtimeCapture{}
			_, err := Web(context.Background(), ex, &proto.WebReq{Op: "install", Deploy: deploy, SHA256: strings.Repeat("a", 64), Image: DefaultWebImage})
			if err != nil {
				t.Fatal(err)
			}
			for _, want := range []string{"trap rollback_install EXIT", "stop_current", "OLD_RT=$RT", "or_test \"$MAIN\"", "SWITCHED=1", "-previous"} {
				if !strings.Contains(ex.script, want) {
					t.Errorf("missing %q", want)
				}
			}
			if deploy == "binary" && strings.Index(ex.script, "SWAPPED=1") > strings.Index(ex.script, "mv "+BinPrefix+".new/pve-forward-openresty "+BinPrefix) {
				t.Fatal("rollback must be armed before swap")
			}
			if deploy == "docker" && strings.Index(ex.script, "docker pull") > strings.Index(ex.script, "  stop_current\n") {
				t.Fatal("must pull before stopping old runtime")
			}
			if bash, err := exec.LookPath("bash"); err == nil {
				cmd := exec.Command(bash, "-n")
				cmd.Stdin = strings.NewReader(ex.script)
				if out, err := cmd.CombinedOutput(); err != nil {
					t.Fatalf("script syntax: %v %s", err, out)
				}
			}
		})
	}
	for _, req := range []*proto.WebReq{{Op: "install", Deploy: "auto"}, {Op: "install", Deploy: "binary", SHA256: "bad"}, {Op: "install", Deploy: "docker", Image: "x:y;id"}} {
		ex := &runtimeCapture{}
		if _, err := Web(context.Background(), ex, req); err == nil || ex.script != "" {
			t.Fatalf("invalid install executed: %+v", req)
		}
	}
}

type portCheckOutput string

func (e portCheckOutput) Run(context.Context, string) ([]byte, error) {
	return []byte("##uid\n0\n##os\nDebian|debian|\n##arch\nx86_64\n##systemd\nyes\n##managed\n\n##listen\n" + string(e)), nil
}

func TestWebPortOccupancyMessages(t *testing.T) {
	const docker = "0.0.0.0:443 users:((\"docker-proxy\",pid=1,fd=3))\n0.0.0.0:80 users:((\"docker-proxy\",pid=2,fd=3))\n[::]:80 users:((\"docker-proxy\",pid=3,fd=3))\n"
	for _, tc := range []struct {
		name, listen, state, detail string
		ok                          bool
	}{
		{"free", "0.0.0.0:22 users:((\"sshd\",pid=1,fd=3))\n", "未被占用", "", true},
		{"partial", docker, "部分被占用", "80、443 被 docker-proxy 占用", false},
		{"all", docker + "0.0.0.0:8080 users:((\"nginx\",pid=4,fd=3))\n0.0.0.0:8443 users:((\"xxx\",pid=5,fd=3))\n", "全部被占用", "80、443 被 docker-proxy 占用，8080 被 nginx 占用，8443 被 xxx 占用", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			res, err := webCheck(context.Background(), portCheckOutput(tc.listen), &proto.WebReq{Ports: []int{8443, 443, 8080, 80}, Deploy: "binary"})
			if err != nil {
				t.Fatal(err)
			}
			for _, check := range res.Checks {
				if strings.HasPrefix(check.Name, "端口 ") {
					if check.Name != "端口 80 / 443 / 8080 / 8443 "+tc.state || check.Detail != tc.detail || check.OK != tc.ok {
						t.Fatalf("unexpected check: %+v", check)
					}
					return
				}
			}
			t.Fatal("missing port check")
		})
	}
}

func TestValidWebImage(t *testing.T) {
	for _, image := range []string{DefaultWebImage, "registry.example.com:5000/team/openresty:v1", "openresty/openresty@sha256:" + strings.Repeat("a", 64)} {
		if !ValidWebImage(image) {
			t.Errorf("rejected %s", image)
		}
	}
	for _, image := range []string{"", "openresty/openresty", "https://example.com/x:y", "x:y;id", "x:y\n", "-x:y", "x@sha256:bad"} {
		if ValidWebImage(image) {
			t.Errorf("accepted %q", image)
		}
	}
}
