package nodeops

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"pveforward/internal/proto"
)

// checkOutput 模拟检测脚本的输出。
type checkOutput string

func (e checkOutput) Run(context.Context, string) ([]byte, error) {
	return []byte("##uid\n0\n##os\nDebian GNU/Linux 13 (trixie)|debian|\n##arch\nx86_64\n##apt\nyes\n##systemd\nyes\n" + string(e)), nil
}

var imgID = "sha256:" + strings.Repeat("1", 64)

const (
	sysInstalled = "##openresty\nnginx version: openresty/1.27.1.2\n##runtime\nsystem\n##bin\n/usr/local/openresty/bin/openresty\n"
	notInstalled = "##openresty\n##runtime\n##bin\n"
)

// 与实际环境一致：Nginx Proxy Manager（镜像中没有 openresty 命令）占用 80/443，另有已停止的 openresty 容器
func containers() string {
	return "##containers\n" +
		"nginx-proxy-manager|jc21/nginx-proxy-manager:2.16.0|sha256:" + strings.Repeat("2", 64) + "|bridge|true||80,81,443,\n" +
		"openresty|openresty/openresty:latest|" + imgID + "|bridge|false|nginx version: openresty/1.29.2.5|\n"
}

func listenBy(owner, kind string) string {
	return "##listen\n0.0.0.0:80 users:((\"" + owner + "\",pid=7,fd=6)) " + kind + "\n0.0.0.0:443 users:((\"" + owner + "\",pid=7,fd=7)) " + kind + "\n"
}

func checkFor(t *testing.T, out string, req *proto.WebReq) (*proto.WebResult, map[string]proto.WebCheck) {
	t.Helper()
	if req.Ports == nil {
		req.Ports = []int{80, 443}
	}
	res, err := webCheck(context.Background(), checkOutput(out), req)
	if err != nil {
		t.Fatal(err)
	}
	m := map[string]proto.WebCheck{}
	for _, c := range res.Checks {
		key := c.Name
		if strings.HasPrefix(key, "端口 ") {
			key = "端口"
		}
		m[key] = c
	}
	return res, m
}

// 已安装的 OpenResty（自行安装、本系统的或其他容器的镜像）无论选择哪种安装方式都直接使用，不再安装。
func TestWebCheckExistingOpenResty(t *testing.T) {
	for _, tc := range []struct {
		name, out, kind string
	}{
		{"apt 安装，占用端口的就是它", sysInstalled + "##containers\n" + listenBy("nginx", "or"), "system"},
		{"本系统的容器（host 网络）", "##openresty\nnginx version: openresty/1.31.1.1\n##runtime\ndocker\n##bin\nopenresty/openresty:x\n##containers\n" + listenBy("nginx", "ct:"+webContainer), "docker"},
		{"已停止的 openresty 容器的镜像", notInstalled + containers() + "##listen\n", "container"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			res, checks := checkFor(t, tc.out, &proto.WebReq{Deploy: "docker"})
			if res.Existing == nil || res.Existing.Kind != tc.kind {
				t.Fatalf("existing %+v", res.Existing)
			}
			if !checks["端口"].OK {
				t.Fatalf("将要使用的 OpenResty 占用的端口不应算冲突: %+v", checks["端口"])
			}
			if _, ok := checks["Docker"]; ok {
				t.Fatal("已安装时不应再检查/安装 Docker")
			}
			if c := checks["OpenResty"]; !c.OK || !strings.Contains(c.Detail, "无需重新安装") {
				t.Fatalf("OpenResty %+v", c)
			}
		})
	}
	res, _ := checkFor(t, notInstalled+containers()+"##listen\n", &proto.WebReq{Deploy: "binary"})
	if e := res.Existing; e.Container != "openresty" || e.ImageID != imgID || e.Version != "openresty/1.29.2.5" {
		t.Fatalf("容器 %+v", e)
	}
	if len(res.Containers) != 2 || !res.Containers[0].Running || len(res.Containers[0].Ports) != 3 {
		t.Fatalf("containers %+v", res.Containers)
	}
}

// 被其他容器（如 Nginx Proxy Manager）占用时指明容器，可选择停止；选择停止后端口视为空闲。
func TestWebCheckBusyByContainer(t *testing.T) {
	out := notInstalled + containers() + listenBy("docker-proxy", "ct:nginx-proxy-manager")
	res, checks := checkFor(t, out, &proto.WebReq{Deploy: "docker"})
	if c := checks["端口"]; c.OK || c.Detail != "80、443 被 Docker 容器 nginx-proxy-manager 占用" {
		t.Fatalf("%+v", c)
	}
	if len(res.Busy) != 2 || res.Busy[0].Container != "nginx-proxy-manager" {
		t.Fatalf("busy %+v", res.Busy)
	}
	_, checks = checkFor(t, out, &proto.WebReq{Deploy: "docker", Stop: []string{"nginx-proxy-manager"}})
	if !checks["端口"].OK {
		t.Fatalf("选择停止后应视为空闲 %+v", checks["端口"])
	}
}

func TestWebCheckBusyPorts(t *testing.T) {
	_, checks := checkFor(t, notInstalled+"##containers\n"+listenBy("apache2", "other"), &proto.WebReq{Deploy: "binary"})
	if c := checks["端口"]; c.OK || c.Detail != "80、443 被 apache2 占用" {
		t.Fatalf("%+v", c)
	}
	// 未安装时本机已有 OpenResty 进程（无法识别安装位置）占用
	_, checks = checkFor(t, notInstalled+"##containers\n"+listenBy("nginx", "or"), &proto.WebReq{Deploy: "binary"})
	if c := checks["端口"]; c.OK || !strings.Contains(c.Detail, "另一个 OpenResty") {
		t.Fatalf("%+v", c)
	}
	// 已有 OpenResty 时，其他进程占用的端口仍然冲突
	_, checks = checkFor(t, sysInstalled+"##containers\n##listen\n0.0.0.0:80 users:((\"nginx\",pid=7,fd=6)) or\n0.0.0.0:443 users:((\"caddy\",pid=8,fd=6)) other\n", &proto.WebReq{Deploy: "binary"})
	if c := checks["端口"]; c.OK || c.Detail != "443 被 caddy 占用" {
		t.Fatalf("%+v", c)
	}
}

func TestWebCheckNotInstalled(t *testing.T) {
	_, checks := checkFor(t, notInstalled+"##containers\n##listen\n", &proto.WebReq{Deploy: "manual"})
	if checks["OpenResty"].OK {
		t.Fatal("未安装时选择使用已安装的应失败")
	}
	_, checks = checkFor(t, notInstalled+"##docker\n##containers\n##listen\n", &proto.WebReq{Deploy: "docker", DockerSource: "mirrors.aliyun.com/docker-ce"})
	if c := checks["Docker"]; !c.OK || !strings.Contains(c.Detail, "阿里云") {
		t.Fatalf("%+v", c)
	}
	res, err := webCheck(context.Background(), runOut("##uid\n0\n##os\nAlpine|alpine|\n##arch\nx86_64\n##systemd\n\n##openresty\n##containers\n##listen\n"), &proto.WebReq{Deploy: "docker", Ports: []int{80}})
	if err != nil {
		t.Fatal(err)
	}
	if res.Checks[1].Name != "系统支持" || res.Checks[1].OK {
		t.Fatalf("%+v", res.Checks)
	}
}

type runOut string

func (e runOut) Run(context.Context, string) ([]byte, error) { return []byte(e), nil }

func bashSyntax(t *testing.T, script string) {
	t.Helper()
	if bash, err := exec.LookPath("bash"); err == nil {
		cmd := exec.Command(bash, "-n")
		cmd.Stdin = strings.NewReader(script)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("script syntax: %v %s", err, out)
		}
	}
}

func TestWebInstallScripts(t *testing.T) {
	for _, req := range []*proto.WebReq{
		{Op: "install", Deploy: "docker", Image: PullImage(DefaultWebImage, "docker.1ms.run"), DockerSource: "mirrors.aliyun.com/docker-ce"},
		{Op: "install", Deploy: "docker", Image: DefaultWebImage, DockerSource: DockerSourceDistro},
		{Op: "install", Deploy: "adopt", Image: imgID},
	} {
		ex := &runtimeCapture{}
		if _, err := Web(context.Background(), ex, req); err != nil {
			t.Fatal(err)
		}
		bashSyntax(t, ex.script)
		if req.Deploy == "adopt" {
			if strings.Contains(ex.script, "docker pull") || strings.Contains(ex.script, "install_docker") || !strings.Contains(ex.script, "--entrypoint openresty "+imgID) {
				t.Fatal("使用已有镜像不应下载或安装")
			}
			continue
		}
		for _, want := range []string{"9DC858229FC7DD38854AE2D88D81803C0EBFCD88", "060A61C51B558A7F742B77AAC52FEB6B621E9F35", "SRC=" + shq(req.DockerSource), "docker pull " + shq(req.Image)} {
			if !strings.Contains(ex.script, want) {
				t.Errorf("docker missing %q", want)
			}
		}
	}
	for _, req := range []*proto.WebReq{
		{Op: "install", Deploy: "docker", Image: DefaultWebImage, DockerSource: "evil.example.com"},
		{Op: "install", Deploy: "adopt", Image: "openresty/openresty:latest"},
		{Op: "free", Stop: []string{"x;rm"}},
		{Op: "free", Stop: []string{webContainer}},
		{Op: "restore", Stop: []string{"$(id)"}},
	} {
		ex := &runtimeCapture{}
		if _, err := Web(context.Background(), ex, req); err == nil || ex.script != "" {
			t.Fatalf("invalid op executed: %+v", req)
		}
	}
	for _, req := range []*proto.WebReq{{Op: "free", Stop: []string{"nginx-proxy-manager", "web.1"}}, {Op: "restore", Stop: []string{"nginx-proxy-manager"}}} {
		ex := &runtimeCapture{}
		if _, err := Web(context.Background(), ex, req); err != nil {
			t.Fatal(err)
		}
		bashSyntax(t, ex.script)
		if !strings.Contains(ex.script, "'nginx-proxy-manager'") {
			t.Fatal(ex.script)
		}
		// 先记录原重启策略再停止，恢复时才能还原
		if req.Op == "free" && strings.Index(ex.script, webAdoptedFile) > strings.Index(ex.script, "docker stop") {
			t.Fatal("must record before stopping")
		}
	}
	bashSyntax(t, webLocate+webDetectScript)
	bashSyntax(t, webLocate+webRestoreAdopted)
}

// Docker 运行方式的重载：容器反复重启、已停止或镜像已更换时必须删除后重新创建（docker start 对重启中的容器会报错），
// 只有运行中且镜像相同才在容器内重载。
func TestWebDockerReload(t *testing.T) {
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("bash unavailable")
	}
	for _, tc := range []struct{ status, image, want string }{
		{"restarting", "img:1", "rm -f run -d"},
		{"exited", "img:1", "rm -f run -d"},
		{"", "", "rm -f run -d"}, // 容器不存在
		{"running", "img:0", "rm -f run -d"},
		{"running", "img:1", "exec"},
	} {
		script := "set -e\n" + webLocate + `RT=docker; IMG=img:1
docker() {
  case "$1" in
    inspect) case "$*" in *State.Status*) [ -n "` + tc.status + `" ] && echo ` + tc.status + ` ;; *Config.Image*) [ -n "` + tc.image + `" ] && echo ` + tc.image + ` ;; esac ;;
    rm) printf 'rm %s ' "$2" >> "$CALLS" ;;
    run) printf 'run %s ' "$2" >> "$CALLS" ;;
    exec) printf 'exec ' >> "$CALLS" ;;
    start) echo 'Error: is restarting' >&2; return 1 ;;
  esac
}
or_reload
cat "$CALLS"
`
		cmd := exec.Command(bash, "-s")
		cmd.Env = append(os.Environ(), "CALLS="+filepath.ToSlash(filepath.Join(t.TempDir(), "calls")))
		cmd.Stdin = strings.NewReader(script)
		out, err := cmd.CombinedOutput()
		if err != nil || strings.TrimSpace(string(out)) != tc.want {
			t.Errorf("%s/%s: %q %v", tc.status, tc.image, out, err)
		}
	}
}

// 公共前置脚本在 set -e 下必须能执行完（没有 OpenResty、Docker 的环境也一样），否则安装、下发脚本会中途退出。
func TestWebLocateRunsUnderSetE(t *testing.T) {
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("bash unavailable")
	}
	cmd := exec.Command(bash, "-s")
	cmd.Stdin = strings.NewReader("set -e\n" + webLocate + webRestoreAdopted + "echo located\n")
	out, err := cmd.CombinedOutput()
	if err != nil || !strings.HasSuffix(strings.TrimSpace(string(out)), "located") || strings.Contains(string(out), "command not found") || strings.Contains(string(out), "Is a directory") {
		t.Fatalf("prelude failed: %v\n%s", err, out)
	}
}

func TestPullImage(t *testing.T) {
	for _, tc := range [][3]string{
		{DefaultWebImage, "", DefaultWebImage},
		{DefaultWebImage, "docker.1ms.run", "docker.1ms.run/openresty/openresty@" + DefaultWebImageDigest},
		{"openresty/openresty:alpine", "docker.m.daocloud.io", "docker.m.daocloud.io/openresty/openresty:alpine"},
		{"nginx:1.27", "dockerproxy.net", "dockerproxy.net/library/nginx:1.27"},
		{"registry.example.com:5000/openresty:test", "docker.1ms.run", "registry.example.com:5000/openresty:test"},
	} {
		got := PullImage(tc[0], tc[1])
		if got != tc[2] || !ValidWebImage(got) {
			t.Errorf("PullImage(%q, %q) = %q", tc[0], tc[1], got)
		}
	}
	if !ValidDockerSource(DockerSourceOfficial) || !ValidDockerSource(DockerSourceDistro) || ValidDockerSource("evil.example.com") || !ValidRegistryMirror("") || ValidRegistryMirror("evil.example.com") {
		t.Fatal("mirror validation")
	}
}
