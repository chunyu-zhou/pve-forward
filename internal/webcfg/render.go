package webcfg

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"pveforward/internal/store"
)

const (
	// BaseDir 为节点（宿主机或入口虚拟机）上的 Web 配置目录。每个版本写入 BaseDir/web-<摘要>，
	// 校验通过后把 BaseDir/web 链接切换过去，OpenResty 的主配置只 include BaseDir/web/nginx.conf。
	BaseDir = "/etc/pve-forward"
	// StatusPort 为 OpenResty 统计接口端口（只允许宿主机访问）。
	StatusPort = 61080
	dirToken   = "{{WEBDIR}}"
)

// Site 为一个要生成配置的站点（已通过校验）。
type Site struct {
	ID         int64
	Hosts      []string
	Port       int
	HTTPS      bool
	Scheme     string // http | https（转发到目标的协议）
	Target     string // 目标虚拟机内网 IPv4
	TargetPort int
	CertID     int64
	Opts       store.SiteOpts
}

// Cert 为证书与私钥（PEM）。
type Cert struct{ Cert, Key string }

// Config 为节点的全部 Web 转发配置。
type Config struct {
	HTTP, HTTPS  []int
	StatusListen string // 统计接口监听地址，如 127.0.0.1 或入口虚拟机的内网 IP
	StatusAllow  string // 允许访问统计接口的地址（宿主机）
	Version      string // OpenResty 版本（openresty -v），决定 HTTP/2 的写法
	Sites        []Site
	Certs        map[int64]Cert
}

// Rendered 为生成的配置文件（相对版本目录的路径 → 内容）与目录名。
type Rendered struct {
	Dir   string // 版本目录名，如 web-1a2b3c4d5e6f
	Hash  string
	Files map[string]string
}

// Render 生成配置。内容不变时摘要不变，据此判断是否需要重新下发。
func Render(c Config) *Rendered {
	files := map[string]string{}
	h2Directive := h2On(c.Version)
	var main strings.Builder
	main.WriteString(`# 由 PVE Forward 生成，请勿手动修改
worker_processes auto;
error_log /var/log/pve-forward-web/error.log warn;
events { worker_connections 4096; }
http {
    default_type application/octet-stream;
    server_tokens off;
    sendfile on;
    tcp_nopush on;
    keepalive_timeout 65;
    server_names_hash_bucket_size 128;
    access_log off;
    ssl_protocols TLSv1.2 TLSv1.3;
    ssl_session_cache shared:pvef_ssl:10m;
    ssl_session_timeout 1d;
    proxy_cache_path /var/cache/pve-forward-web levels=1:2 keys_zone=pvef_cache:32m max_size=2g inactive=7d use_temp_path=off;
    map $http_upgrade $connection_upgrade { default upgrade; '' close; }

    # 各站点的请求数与流量（站点通过 $pvef_site 标识）
    lua_shared_dict pvef_stats 8m;
    log_by_lua_block {
        local id = ngx.var.pvef_site
        if id == nil or id == "" then return end
        local d = ngx.shared.pvef_stats
        d:incr("r" .. id, 1, 0)
        d:incr("u" .. id, tonumber(ngx.var.request_length) or 0, 0)
        d:incr("d" .. id, tonumber(ngx.var.bytes_sent) or 0, 0)
    }

`)
	fmt.Fprintf(&main, `    server {
        listen %s:%d;
        allow %s;
        deny all;
        location = /pvef-stats {
            default_type text/plain;
            content_by_lua_block {
                local d = ngx.shared.pvef_stats
                for _, k in ipairs(d:get_keys(0)) do ngx.say(k, " ", d:get(k)) end
            }
        }
        location / { return 404; }
    }

`, c.StatusListen, StatusPort, c.StatusAllow)

	// 未匹配到站点的请求直接断开（HTTPS 拒绝握手，不暴露证书）
	http, https := sortedPorts(c.HTTP), sortedPorts(c.HTTPS)
	if len(http) > 0 {
		main.WriteString("    server {\n")
		for _, p := range http {
			fmt.Fprintf(&main, "        listen %d default_server;\n", p)
		}
		main.WriteString("        server_name _;\n        return 444;\n    }\n")
	}
	if len(https) > 0 {
		main.WriteString("    server {\n")
		for _, p := range https {
			fmt.Fprintf(&main, "        listen %d ssl default_server;\n", p)
		}
		main.WriteString("        server_name _;\n        ssl_reject_handshake on;\n        return 444;\n    }\n")
	}
	main.WriteString("\n    include " + dirToken + "/sites/*.conf;\n}\n")
	files["nginx.conf"] = main.String()

	sites := append([]Site(nil), c.Sites...)
	sort.Slice(sites, func(i, j int) bool { return sites[i].ID < sites[j].ID })
	usedCerts := map[int64]bool{}
	redirectPort := 0
	for _, p := range http {
		if redirectPort == 0 || p == 80 {
			redirectPort = p
		}
	}
	for _, s := range sites {
		files[fmt.Sprintf("sites/%d.conf", s.ID)] = renderSite(s, h2Directive, redirectPort)
		if s.HTTPS {
			usedCerts[s.CertID] = true
		}
		if s.Opts.AuthUser != "" && s.Opts.AuthHash != "" {
			files[fmt.Sprintf("htpasswd/%d", s.ID)] = s.Opts.AuthUser + ":" + s.Opts.AuthHash + "\n"
		}
	}
	for id := range usedCerts {
		if ct, ok := c.Certs[id]; ok {
			files[fmt.Sprintf("certs/%d.crt", id)] = strings.TrimSpace(ct.Cert) + "\n"
			files[fmt.Sprintf("certs/%d.key", id)] = strings.TrimSpace(ct.Key) + "\n"
		}
	}

	// 摘要基于带占位符的内容，再把占位符替换为版本目录
	keys := make([]string, 0, len(files))
	for k := range files {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	h := sha256.New()
	for _, k := range keys {
		fmt.Fprintf(h, "%s\x00%s\x00", k, files[k])
	}
	sum := hex.EncodeToString(h.Sum(nil))
	dir := "web-" + sum[:12]
	for k, v := range files {
		files[k] = strings.ReplaceAll(v, dirToken, BaseDir+"/"+dir)
	}
	return &Rendered{Dir: dir, Hash: sum, Files: files}
}

func sortedPorts(ps []int) []int {
	res := append([]int(nil), ps...)
	sort.Ints(res)
	return res
}

// h2On 判断 OpenResty 是否支持独立的 "http2 on;" 指令（nginx 1.25.1 起）。
func h2On(version string) bool {
	m := regexp.MustCompile(`(\d+)\.(\d+)\.(\d+)`).FindStringSubmatch(version)
	if m == nil {
		return false
	}
	a, _ := strconv.Atoi(m[1])
	b, _ := strconv.Atoi(m[2])
	c, _ := strconv.Atoi(m[3])
	return a > 1 || (a == 1 && (b > 25 || (b == 25 && c >= 1)))
}

// attackRe 为“拦截常见攻击”匹配的请求路径与参数（路径穿越、SQL 注入、脚本注入、敏感文件）。
// $request_uri 未经解码，分隔符可能是 %20、+ 等编码形式。
const attackRe = `(\.\./|\.\.%2f|%2e%2e|%00|<script|%3cscript|\bunion(%[0-9a-f]{2}|[^a-z0-9%])+(all(%[0-9a-f]{2}|[^a-z0-9%])+)?select\b|information_schema|(sleep|benchmark)(\(|%28)|/etc/passwd|/\.git/|/\.env|/\.svn/)`

func renderSite(s Site, h2Directive bool, redirectPort int) string {
	var b strings.Builder
	o := s.Opts
	fmt.Fprintf(&b, "# 站点 %d\n", s.ID)
	if s.HTTPS && o.ForceHTTPS && redirectPort > 0 {
		target := "https://$host"
		if s.Port != 443 {
			target += ":" + strconv.Itoa(s.Port)
		}
		fmt.Fprintf(&b, "server {\n    listen %d;\n    server_name %s;\n    set $pvef_site %d;\n    return 301 %s$request_uri;\n}\n", redirectPort, strings.Join(s.Hosts, " "), s.ID, target)
	}
	b.WriteString("server {\n")
	if s.HTTPS {
		if h2Directive {
			fmt.Fprintf(&b, "    listen %d ssl;\n    http2 on;\n", s.Port)
		} else {
			fmt.Fprintf(&b, "    listen %d ssl http2;\n", s.Port)
		}
	} else {
		fmt.Fprintf(&b, "    listen %d;\n", s.Port)
	}
	fmt.Fprintf(&b, "    server_name %s;\n", strings.Join(s.Hosts, " "))
	fmt.Fprintf(&b, "    set $pvef_site %d;\n", s.ID)
	if s.HTTPS {
		fmt.Fprintf(&b, "    ssl_certificate %s/certs/%d.crt;\n    ssl_certificate_key %s/certs/%d.key;\n", dirToken, s.CertID, dirToken, s.CertID)
		if o.ForceHTTPS {
			b.WriteString("    add_header Strict-Transport-Security \"max-age=31536000\" always;\n")
		}
	}
	bodyMax := o.BodyMax
	if bodyMax <= 0 {
		bodyMax = 100
	}
	fmt.Fprintf(&b, "    client_max_body_size %dm;\n", bodyMax)
	if o.Block {
		fmt.Fprintf(&b, "    if ($request_uri ~* \"%s\") { return 403; }\n", attackRe)
	}
	if o.AuthUser != "" && o.AuthHash != "" {
		fmt.Fprintf(&b, "    auth_basic \"Restricted\";\n    auth_basic_user_file %s/htpasswd/%d;\n", dirToken, s.ID)
	}
	proxy := proxyBlock(s)
	if o.Cache {
		b.WriteString("    location ~* \"\\.(?:css|js|mjs|png|jpe?g|gif|webp|avif|svg|ico|woff2?|ttf|otf|eot|mp4|webm|mp3)$\" {\n")
		b.WriteString("        proxy_cache pvef_cache;\n        proxy_cache_key $scheme$host$request_uri;\n        proxy_cache_valid 200 301 302 10m;\n")
		b.WriteString("        add_header X-Cache-Status $upstream_cache_status;\n")
		b.WriteString(indent(proxy, "        "))
		b.WriteString("    }\n")
	}
	b.WriteString("    location / {\n")
	b.WriteString(indent(proxy, "        "))
	b.WriteString("    }\n")
	if c := strings.TrimSpace(o.Custom); c != "" {
		b.WriteString("    # 自定义配置\n")
		b.WriteString(indent(c+"\n", "    "))
	}
	b.WriteString("}\n")
	return b.String()
}

func proxyBlock(s Site) string {
	o := s.Opts
	var b strings.Builder
	fmt.Fprintf(&b, "proxy_pass %s://%s:%d;\n", s.Scheme, s.Target, s.TargetPort)
	b.WriteString("proxy_http_version 1.1;\nproxy_set_header Host $http_host;\n")
	if o.WS {
		b.WriteString("proxy_set_header Upgrade $http_upgrade;\nproxy_set_header Connection $connection_upgrade;\n")
	} else {
		b.WriteString("proxy_set_header Connection \"\";\n")
	}
	if o.RealIP {
		b.WriteString("proxy_set_header X-Real-IP $remote_addr;\nproxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;\n")
		b.WriteString("proxy_set_header X-Forwarded-Proto $scheme;\nproxy_set_header X-Forwarded-Host $host;\nproxy_set_header X-Forwarded-Port $server_port;\n")
	}
	if s.Scheme == "https" {
		b.WriteString("proxy_ssl_server_name on;\nproxy_ssl_name $host;\n")
	}
	timeout := o.Timeout
	if timeout <= 0 {
		timeout = 60
	}
	fmt.Fprintf(&b, "proxy_connect_timeout %ds;\nproxy_send_timeout %ds;\nproxy_read_timeout %ds;\n", min(timeout, 75), timeout, timeout)
	for _, h := range ParseHeaders(o.Headers) {
		fmt.Fprintf(&b, "proxy_set_header %s \"%s\";\n", h[0], h[1])
	}
	return b.String()
}

func indent(s, pre string) string {
	var b strings.Builder
	for _, l := range strings.Split(strings.TrimRight(s, "\n"), "\n") {
		if strings.TrimSpace(l) == "" {
			b.WriteString("\n")
			continue
		}
		b.WriteString(pre + l + "\n")
	}
	return b.String()
}

var headerNameRe = regexp.MustCompile(`^[A-Za-z0-9-]{1,64}$`)

// ParseHeaders 解析自定义请求头（每行 "名称: 值"）。格式错误的行由 CheckHeaders 报告，这里直接跳过。
func ParseHeaders(text string) [][2]string {
	var res [][2]string
	for _, l := range strings.Split(text, "\n") {
		k, v, ok := strings.Cut(strings.TrimSpace(l), ":")
		k, v = strings.TrimSpace(k), strings.TrimSpace(v)
		if !ok || !headerNameRe.MatchString(k) || strings.ContainsAny(v, "\"\\\r\n") {
			continue
		}
		res = append(res, [2]string{k, v})
	}
	return res
}

// CheckHeaders 校验自定义请求头，返回第一处错误。
func CheckHeaders(text string) error {
	for i, l := range strings.Split(text, "\n") {
		l = strings.TrimSpace(l)
		if l == "" {
			continue
		}
		k, v, ok := strings.Cut(l, ":")
		k, v = strings.TrimSpace(k), strings.TrimSpace(v)
		switch {
		case !ok || !headerNameRe.MatchString(k):
			return fmt.Errorf("自定义请求头第 %d 行格式错误，应为“名称: 值”", i+1)
		case strings.ContainsAny(v, "\"\\"):
			return fmt.Errorf("自定义请求头第 %d 行的值不能包含引号或反斜杠", i+1)
		case strings.EqualFold(k, "Host"):
			return fmt.Errorf("自定义请求头不能修改 Host")
		}
	}
	return nil
}
