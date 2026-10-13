package dnsapi

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"
)

// 阿里云文档中的签名示例
func TestAliyunSignExample(t *testing.T) {
	p := map[string]string{"Format": "XML", "AccessKeyId": "testid", "Action": "DescribeDomainRecords", "SignatureMethod": "HMAC-SHA1",
		"DomainName": "example.com", "SignatureNonce": "f59ed6a9-83fc-473b-9cc6-99c95df3856e", "SignatureVersion": "1.0",
		"Version": "2015-01-09", "Timestamp": "2016-03-24T16:41:54Z"}
	if got := aliyunSign("GET", "testsecret", p); got != "uRpHwaSEt3J+6KQD//svCh/x+pI=" {
		t.Fatalf("签名 %s", got)
	}
}

// fakeDNS 是一个内存中的解析记录表，供各服务商的模拟服务器共用。
type fakeDNS struct {
	mu   sync.Mutex
	next int
	recs map[string]Record // id → 记录
	note map[string]string // id → 备注
}

func (f *fakeDNS) setNote(id, n string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.note[id] = n
}

func (f *fakeDNS) noteOf(id string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.note[id]
}

func newFake() *fakeDNS {
	return &fakeDNS{next: 100, recs: map[string]Record{"7": {ID: "7", Name: "www", Type: "A", Value: "1.1.1.1"}}, note: map[string]string{}}
}

func (f *fakeDNS) list(sub string) []Record {
	f.mu.Lock()
	defer f.mu.Unlock()
	var res []Record
	for _, r := range f.recs {
		if sub == "" || r.Name == sub {
			res = append(res, r)
		}
	}
	return res
}

func (f *fakeDNS) add(r Record) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.next++
	r.ID = strings.TrimSpace(strings.Repeat(" ", 0) + itoa(f.next))
	f.recs[r.ID] = r
	return r.ID
}

func (f *fakeDNS) set(id string, r Record) {
	f.mu.Lock()
	defer f.mu.Unlock()
	r.ID = id
	f.recs[id] = r
}

func (f *fakeDNS) del(id string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, ok := f.recs[id]
	delete(f.recs, id)
	return ok
}

func itoa(n int) string { b, _ := json.Marshal(n); return string(b) }

// exercise 对服务商执行一轮完整操作：测试连接 → 查询 → 新建 → 修改 → 删除。
func exercise(t *testing.T, p Provider, f *fakeDNS) {
	t.Helper()
	ctx := context.Background()
	zones, err := p.Zones(ctx)
	if err != nil || len(zones) != 1 || zones[0] != "example.com" {
		t.Fatalf("Zones %v %v", zones, err)
	}
	rs, err := p.Records(ctx, "example.com", "www")
	if err != nil || len(rs) != 1 || rs[0].Value != "1.1.1.1" || Find(rs, "a") == nil {
		t.Fatalf("Records www %+v %v", rs, err)
	}
	if rs, err := p.Records(ctx, "example.com", "dev"); err != nil || len(rs) != 0 {
		t.Fatalf("Records dev %+v %v", rs, err)
	}
	id, err := p.Create(ctx, "example.com", "dev", "A", "203.0.113.10", "前端开发(alice)")
	if err != nil || id == "" {
		t.Fatalf("Create %q %v", id, err)
	}
	if async, ok := p.(AsyncRemarkProvider); ok {
		if n := f.noteOf(id); n != "" {
			t.Fatalf("创建时不应同步补备注: %q", n)
		}
		if err := async.SetRemark(ctx, "example.com", id, "前端开发(alice)"); err != nil {
			t.Fatal(err)
		}
	}
	if n := f.noteOf(id); n != "前端开发(alice)" {
		t.Fatalf("新建时的备注 %q", n)
	}
	if rs := f.list("dev"); len(rs) != 1 || rs[0].Value != "203.0.113.10" || rs[0].Type != "A" {
		t.Fatalf("新建后 %+v", rs)
	}
	if err := p.Update(ctx, "example.com", id, "dev", "A", "203.0.113.11", "测试环境(alice)"); err != nil {
		t.Fatalf("Update %v", err)
	}
	if async, ok := p.(AsyncRemarkProvider); ok {
		if n := f.noteOf(id); n != "前端开发(alice)" {
			t.Fatalf("更新时不应同步补备注: %q", n)
		}
		if err := async.SetRemark(ctx, "example.com", id, "测试环境(alice)"); err != nil {
			t.Fatal(err)
		}
	}
	if n := f.noteOf(id); n != "测试环境(alice)" {
		t.Fatalf("修改后的备注 %q", n)
	}
	if rs := f.list("dev"); len(rs) != 1 || rs[0].Value != "203.0.113.11" {
		t.Fatalf("修改后 %+v", rs)
	}
	if err := p.Delete(ctx, "example.com", id); err != nil {
		t.Fatalf("Delete %v", err)
	}
	if rs := f.list("dev"); len(rs) != 0 {
		t.Fatalf("删除后 %+v", rs)
	}
}

func TestDNSPod(t *testing.T) {
	f := newFake()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.ParseForm()
		if r.Form.Get("login_token") != "123,tok" || r.Form.Get("format") != "json" {
			w.Write([]byte(`{"status":{"code":"-1","message":"登录失败"}}`))
			return
		}
		ok := func(extra string) { w.Write([]byte(`{"status":{"code":"1","message":"ok"}` + extra + `}`)) }
		switch r.URL.Path {
		case "/Domain.List":
			ok(`,"domains":[{"name":"example.com"}]`)
		case "/Record.List":
			b, _ := json.Marshal(f.list(r.Form.Get("sub_domain")))
			ok(`,"records":` + strings.ReplaceAll(string(b), `"ID"`, `"id"`))
		case "/Record.Create":
			if r.Form.Get("record_line") != "默认" {
				t.Errorf("record_line %q", r.Form.Get("record_line"))
			}
			id := f.add(Record{Name: r.Form.Get("sub_domain"), Type: r.Form.Get("record_type"), Value: r.Form.Get("value")})
			ok(`,"record":{"id":"` + id + `"}`)
		case "/Record.Modify":
			f.set(r.Form.Get("record_id"), Record{Name: r.Form.Get("sub_domain"), Type: r.Form.Get("record_type"), Value: r.Form.Get("value")})
			ok("")
		case "/Record.Remark":
			f.setNote(r.Form.Get("record_id"), r.Form.Get("remark"))
			ok("")
		case "/Record.Remove":
			f.del(r.Form.Get("record_id"))
			ok("")
		}
	}))
	defer ts.Close()
	p, _ := New("dnspod", map[string]string{"id": "123", "token": "tok"})
	p.(*dnspod).base = ts.URL
	exercise(t, p, f)

	bad, _ := New("dnspod", map[string]string{"id": "1", "token": "x"})
	bad.(*dnspod).base = ts.URL
	if _, err := bad.Zones(context.Background()); err == nil || !strings.Contains(err.Error(), "登录失败") {
		t.Fatalf("错误密钥 %v", err)
	}
}

func TestAliyun(t *testing.T) {
	f := newFake()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		p := map[string]string{}
		for k := range q {
			if k != "Signature" {
				p[k] = q.Get(k)
			}
		}
		if q.Get("AccessKeyId") != "ak" || q.Get("Signature") != aliyunSign("GET", "sk", p) {
			w.WriteHeader(400)
			w.Write([]byte(`{"Code":"SignatureDoesNotMatch","Message":"签名不匹配"}`))
			return
		}
		switch q.Get("Action") {
		case "DescribeDomains":
			w.Write([]byte(`{"Domains":{"Domain":[{"DomainName":"example.com"}]}}`))
		case "DescribeSubDomainRecords":
			sub := strings.TrimSuffix(q.Get("SubDomain"), ".example.com")
			var out []map[string]string
			for _, x := range f.list(sub) {
				out = append(out, map[string]string{"RecordId": x.ID, "RR": x.Name, "Type": x.Type, "Value": x.Value})
			}
			b, _ := json.Marshal(map[string]any{"DomainRecords": map[string]any{"Record": out}})
			w.Write(b)
		case "AddDomainRecord":
			id := f.add(Record{Name: q.Get("RR"), Type: q.Get("Type"), Value: q.Get("Value")})
			w.Write([]byte(`{"RecordId":"` + id + `"}`))
		case "UpdateDomainRecordRemark":
			f.setNote(q.Get("RecordId"), q.Get("Remark"))
			w.Write([]byte(`{}`))
		case "UpdateDomainRecord":
			f.set(q.Get("RecordId"), Record{Name: q.Get("RR"), Type: q.Get("Type"), Value: q.Get("Value")})
			w.Write([]byte(`{}`))
		case "DeleteDomainRecord":
			f.del(q.Get("RecordId"))
			w.Write([]byte(`{}`))
		}
	}))
	defer ts.Close()
	p, _ := New("aliyun", map[string]string{"ak": "ak", "sk": "sk"})
	p.(*aliyun).base = ts.URL
	exercise(t, p, f)

	bad, _ := New("aliyun", map[string]string{"ak": "ak", "sk": "wrong"})
	bad.(*aliyun).base = ts.URL
	if _, err := bad.Zones(context.Background()); err == nil || !strings.Contains(err.Error(), "SignatureDoesNotMatch") {
		t.Fatalf("错误密钥 %v", err)
	}
}

var tc3Re = regexp.MustCompile(`^TC3-HMAC-SHA256 Credential=sid/(\d{4}-\d{2}-\d{2})/dnspod/tc3_request, SignedHeaders=content-type;host, Signature=[0-9a-f]{64}$`)

func TestTencent(t *testing.T) {
	f := newFake()
	var ts *httptest.Server
	ts = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		auth := r.Header.Get("Authorization")
		tsec := r.Header.Get("X-TC-Timestamp")
		var sec int64
		json.Unmarshal([]byte(tsec), &sec)
		u, _ := url.Parse(ts.URL)
		if !tc3Re.MatchString(auth) || auth != tencentAuth("sid", "skey", u.Host, string(body), sec) || r.Header.Get("X-TC-Version") != "2021-03-23" {
			w.Write([]byte(`{"Response":{"Error":{"Code":"AuthFailure.SignatureFailure","Message":"签名错误"}}}`))
			return
		}
		var in map[string]any
		json.Unmarshal(body, &in)
		str := func(k string) string { s, _ := in[k].(string); return s }
		num := func(k string) string { n, _ := in[k].(float64); return itoa(int(n)) }
		resp := func(v any) { b, _ := json.Marshal(map[string]any{"Response": v}); w.Write(b) }
		switch r.Header.Get("X-TC-Action") {
		case "DescribeDomainList":
			resp(map[string]any{"DomainList": []map[string]string{{"Name": "example.com"}}})
		case "DescribeRecordList":
			rs := f.list(str("Subdomain"))
			if len(rs) == 0 {
				resp(map[string]any{"Error": map[string]string{"Code": "ResourceNotFound.NoDataOfRecord", "Message": "记录列表为空"}})
				return
			}
			var out []map[string]any
			for _, x := range rs {
				var id int
				json.Unmarshal([]byte(x.ID), &id)
				out = append(out, map[string]any{"RecordId": id, "Name": x.Name, "Type": x.Type, "Value": x.Value})
			}
			resp(map[string]any{"RecordList": out})
		case "CreateRecord":
			var id int
			rid := f.add(Record{Name: str("SubDomain"), Type: str("RecordType"), Value: str("Value")})
			f.setNote(rid, str("Remark"))
			json.Unmarshal([]byte(rid), &id)
			resp(map[string]any{"RecordId": id})
		case "ModifyRecord":
			f.set(num("RecordId"), Record{Name: str("SubDomain"), Type: str("RecordType"), Value: str("Value")})
			f.setNote(num("RecordId"), str("Remark"))
			resp(map[string]any{})
		case "DeleteRecord":
			f.del(num("RecordId"))
			resp(map[string]any{})
		}
	}))
	defer ts.Close()
	p, _ := New("tencent", map[string]string{"ak": "sid", "sk": "skey"})
	p.(*tencent).base = ts.URL
	exercise(t, p, f)
}

var hwAuthRe = regexp.MustCompile(`^SDK-HMAC-SHA256 Access=hak, SignedHeaders=content-type;host;x-sdk-date, Signature=([0-9a-f]{64})$`)

func TestHuawei(t *testing.T) {
	f := newFake()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		m := hwAuthRe.FindStringSubmatch(r.Header.Get("Authorization"))
		// 用同样的算法重新签名，确认请求在传输中未被改变（路径、查询参数、请求体都参与签名）
		date, _ := time.Parse("20060102T150405Z", r.Header.Get("X-Sdk-Date"))
		chk, _ := http.NewRequest(r.Method, "http://"+r.Host+r.URL.RequestURI(), nil)
		chk.Header.Set("Content-Type", r.Header.Get("Content-Type"))
		huaweiSign(chk, "hak", "hsk", body, date)
		if m == nil || chk.Header.Get("Authorization") != r.Header.Get("Authorization") {
			w.WriteHeader(401)
			w.Write([]byte(`{"error_code":"APIGW.0301","error_msg":"Incorrect IAM authentication information"}`))
			return
		}
		const zid = "ff8080825b8fc86c015b94bc6f8712c3"
		switch {
		case r.Method == "GET" && r.URL.Path == "/v2/zones":
			w.Write([]byte(`{"zones":[{"id":"` + zid + `","name":"example.com."}]}`))
		case r.Method == "GET" && r.URL.Path == "/v2/zones/"+zid+"/recordsets":
			sub := strings.TrimSuffix(r.URL.Query().Get("name"), ".example.com.")
			var out []map[string]any
			for _, x := range f.list(sub) {
				out = append(out, map[string]any{"id": x.ID, "name": x.Name + ".example.com.", "type": x.Type, "records": []string{x.Value}})
			}
			b, _ := json.Marshal(map[string]any{"recordsets": out})
			w.Write(b)
		case r.Method == "POST":
			var in struct {
				Name, Type, Description string
				Records                 []string
			}
			json.Unmarshal(body, &in)
			id := f.add(Record{Name: strings.TrimSuffix(in.Name, ".example.com."), Type: in.Type, Value: in.Records[0]})
			f.setNote(id, in.Description)
			w.Write([]byte(`{"id":"` + id + `"}`))
		case r.Method == "PUT":
			var in struct {
				Name, Type, Description string
				Records                 []string
			}
			json.Unmarshal(body, &in)
			f.set(r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:], Record{Name: strings.TrimSuffix(in.Name, ".example.com."), Type: in.Type, Value: in.Records[0]})
			f.setNote(r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:], in.Description)
			w.Write([]byte(`{}`))
		case r.Method == "DELETE":
			f.del(r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:])
			w.WriteHeader(202)
		}
	}))
	defer ts.Close()
	p, _ := New("huawei", map[string]string{"ak": "hak", "sk": "hsk"})
	p.(*huawei).base = ts.URL
	exercise(t, p, f)
}

func TestCloudflare(t *testing.T) {
	f := newFake()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		res := func(v any) {
			b, _ := json.Marshal(map[string]any{"success": true, "errors": []any{}, "result": v})
			w.Write(b)
		}
		if r.Header.Get("Authorization") != "Bearer cft" {
			w.WriteHeader(403)
			w.Write([]byte(`{"success":false,"errors":[{"code":9109,"message":"Invalid access token"}]}`))
			return
		}
		var in struct{ Type, Name, Content, Comment string }
		json.NewDecoder(r.Body).Decode(&in)
		switch {
		case r.URL.Path == "/zones":
			res([]map[string]string{{"id": "z1", "name": "example.com"}})
		case r.Method == "GET":
			sub := strings.TrimSuffix(r.URL.Query().Get("name"), ".example.com")
			var out []map[string]string
			for _, x := range f.list(sub) {
				out = append(out, map[string]string{"id": x.ID, "type": x.Type, "name": x.Name + ".example.com", "content": x.Value})
			}
			res(out)
		case r.Method == "POST":
			id := f.add(Record{Name: strings.TrimSuffix(in.Name, ".example.com"), Type: in.Type, Value: in.Content})
			f.setNote(id, in.Comment)
			res(map[string]string{"id": id})
		case r.Method == "PUT":
			f.set(r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:], Record{Name: strings.TrimSuffix(in.Name, ".example.com"), Type: in.Type, Value: in.Content})
			f.setNote(r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:], in.Comment)
			res(map[string]string{})
		case r.Method == "DELETE":
			f.del(r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:])
			res(map[string]string{})
		}
	}))
	defer ts.Close()
	p, _ := New("cloudflare", map[string]string{"token": "cft"})
	p.(*cloudflare).base = ts.URL
	exercise(t, p, f)

	bad, _ := New("cloudflare", map[string]string{"token": "x"})
	bad.(*cloudflare).base = ts.URL
	if _, err := bad.Zones(context.Background()); err == nil || !strings.Contains(err.Error(), "Invalid access token") {
		t.Fatalf("错误 Token %v", err)
	}
}

func TestNewRequiresFields(t *testing.T) {
	if _, err := New("aliyun", map[string]string{"ak": "x"}); err == nil || !strings.Contains(err.Error(), "AccessKey Secret") {
		t.Fatalf("缺少参数 %v", err)
	}
	if _, err := New("huawei", map[string]string{"ak": "x", "sk": "y"}); err != nil {
		t.Fatalf("区域可不填 %v", err)
	}
	if _, err := New("bad", nil); err == nil {
		t.Fatal("未知类型应报错")
	}
	if got := Mask("aliyun", map[string]string{"ak": "LTAI5tABCDEFG9Wqz"}); got != "LTAI****9Wqz" {
		t.Fatalf("脱敏 %s", got)
	}
}
