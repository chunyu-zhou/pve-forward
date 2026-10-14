package dnsapi

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

// now 便于测试固定时间。
var now = time.Now

// ---------------- DNSPod（dnsapi.cn，ID,Token） ----------------

type dnspod struct {
	hc    *http.Client
	base  string
	token string
}

func (d *dnspod) call(ctx context.Context, action string, form url.Values, out any) error {
	form.Set("login_token", d.token)
	form.Set("format", "json")
	form.Set("lang", "cn")
	form.Set("error_on_empty", "no")
	req, _ := http.NewRequestWithContext(ctx, "POST", d.base+"/"+action, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	b, code, err := do(d.hc, req)
	if err != nil {
		return err
	}
	var st struct {
		Status struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"status"`
	}
	if json.Unmarshal(b, &st) != nil {
		return fmt.Errorf("DNSPod 返回 HTTP %d: %s", code, trimErr(string(b)))
	}
	if st.Status.Code != "1" {
		return fmt.Errorf("DNSPod：%s（%s）", st.Status.Message, st.Status.Code)
	}
	if out != nil {
		return json.Unmarshal(b, out)
	}
	return nil
}

func (d *dnspod) Zones(ctx context.Context) ([]string, error) {
	var r struct {
		Domains []struct {
			Name string `json:"name"`
		} `json:"domains"`
	}
	if err := d.call(ctx, "Domain.List", url.Values{}, &r); err != nil {
		return nil, err
	}
	var res []string
	for _, x := range r.Domains {
		res = append(res, x.Name)
	}
	return res, nil
}

func (d *dnspod) Records(ctx context.Context, zone, sub string) ([]Record, error) {
	var r struct {
		Records []struct {
			ID    json.Number `json:"id"`
			Name  string      `json:"name"`
			Type  string      `json:"type"`
			Value string      `json:"value"`
		} `json:"records"`
	}
	if err := d.call(ctx, "Record.List", url.Values{"domain": {zone}, "sub_domain": {sub}}, &r); err != nil {
		return nil, err
	}
	var res []Record
	for _, x := range r.Records {
		if strings.EqualFold(x.Name, sub) {
			res = append(res, Record{ID: x.ID.String(), Name: x.Name, Type: x.Type, Value: x.Value})
		}
	}
	return res, nil
}

func (d *dnspod) Create(ctx context.Context, zone, sub, typ, value, remark string) (string, error) {
	var r struct {
		Record struct {
			ID json.Number `json:"id"`
		} `json:"record"`
	}
	if err := d.call(ctx, "Record.Create", url.Values{"domain": {zone}, "sub_domain": {sub}, "record_type": {typ}, "record_line": {"默认"}, "value": {value}}, &r); err != nil {
		return "", err
	}
	id := r.Record.ID.String()
	return id, nil
}

func (d *dnspod) SetRemark(ctx context.Context, zone, id, remark string) error {
	if remark == "" {
		return nil
	}
	return d.call(ctx, "Record.Remark", url.Values{"domain": {zone}, "record_id": {id}, "remark": {remark}}, nil)
}

func (d *dnspod) Update(ctx context.Context, zone, id, sub, typ, value, remark string) error {
	if err := d.call(ctx, "Record.Modify", url.Values{"domain": {zone}, "record_id": {id}, "sub_domain": {sub}, "record_type": {typ}, "record_line": {"默认"}, "value": {value}}, nil); err != nil {
		return err
	}
	return nil
}

func (d *dnspod) SetStatus(ctx context.Context, zone, id string, enabled bool) error {
	return d.call(ctx, "Record.Status", url.Values{"domain": {zone}, "record_id": {id}, "status": {map[bool]string{true: "enable", false: "disable"}[enabled]}}, nil)
}

func (d *dnspod) Delete(ctx context.Context, zone, id string) error {
	err := d.call(ctx, "Record.Remove", url.Values{"domain": {zone}, "record_id": {id}}, nil)
	if err != nil && strings.Contains(err.Error(), "（8）") { // 记录不存在
		return nil
	}
	return err
}

// ---------------- 阿里云 DNS（RPC 风格，HMAC-SHA1） ----------------

type aliyun struct {
	hc     *http.Client
	base   string
	ak, sk string
}

// aliyunEncode 按阿里云签名要求编码（RFC 3986）。
func aliyunEncode(s string) string {
	return strings.NewReplacer("+", "%20", "*", "%2A", "%7E", "~").Replace(url.QueryEscape(s))
}

// aliyunSign 计算 RPC 签名。
func aliyunSign(method, secret string, params map[string]string) string {
	keys := make([]string, 0, len(params))
	for k := range params {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var parts []string
	for _, k := range keys {
		parts = append(parts, aliyunEncode(k)+"="+aliyunEncode(params[k]))
	}
	str := method + "&" + aliyunEncode("/") + "&" + aliyunEncode(strings.Join(parts, "&"))
	m := hmac.New(sha1.New, []byte(secret+"&"))
	m.Write([]byte(str))
	return base64.StdEncoding.EncodeToString(m.Sum(nil))
}

func (a *aliyun) call(ctx context.Context, action string, params map[string]string, out any) error {
	p := map[string]string{"Format": "JSON", "Version": "2015-01-09", "AccessKeyId": a.ak, "SignatureMethod": "HMAC-SHA1",
		"Timestamp": now().UTC().Format("2006-01-02T15:04:05Z"), "SignatureVersion": "1.0", "SignatureNonce": nonce(), "Action": action}
	for k, v := range params {
		p[k] = v
	}
	p["Signature"] = aliyunSign("GET", a.sk, p)
	q := url.Values{}
	for k, v := range p {
		q.Set(k, v)
	}
	req, _ := http.NewRequestWithContext(ctx, "GET", a.base+"/?"+q.Encode(), nil)
	b, code, err := do(a.hc, req)
	if err != nil {
		return err
	}
	if code/100 != 2 {
		var e struct{ Code, Message string }
		if json.Unmarshal(b, &e) == nil && e.Code != "" {
			return fmt.Errorf("阿里云 DNS：%s（%s）", e.Message, e.Code)
		}
		return fmt.Errorf("阿里云 DNS 返回 HTTP %d: %s", code, trimErr(string(b)))
	}
	if out != nil {
		return json.Unmarshal(b, out)
	}
	return nil
}

func (a *aliyun) Zones(ctx context.Context) ([]string, error) {
	var r struct {
		Domains struct {
			Domain []struct{ DomainName string } `json:"Domain"`
		} `json:"Domains"`
	}
	if err := a.call(ctx, "DescribeDomains", map[string]string{"PageSize": "100"}, &r); err != nil {
		return nil, err
	}
	var res []string
	for _, x := range r.Domains.Domain {
		res = append(res, x.DomainName)
	}
	return res, nil
}

func (a *aliyun) Records(ctx context.Context, zone, sub string) ([]Record, error) {
	var r struct {
		DomainRecords struct {
			Record []struct{ RecordId, RR, Type, Value string } `json:"Record"`
		} `json:"DomainRecords"`
	}
	if err := a.call(ctx, "DescribeSubDomainRecords", map[string]string{"SubDomain": sub + "." + zone, "DomainName": zone, "PageSize": "100"}, &r); err != nil {
		return nil, err
	}
	var res []Record
	for _, x := range r.DomainRecords.Record {
		if strings.EqualFold(x.RR, sub) {
			res = append(res, Record{ID: x.RecordId, Name: x.RR, Type: x.Type, Value: x.Value})
		}
	}
	return res, nil
}

func (a *aliyun) Create(ctx context.Context, zone, sub, typ, value, remark string) (string, error) {
	var r struct{ RecordId string }
	if err := a.call(ctx, "AddDomainRecord", map[string]string{"DomainName": zone, "RR": sub, "Type": typ, "Value": value}, &r); err != nil {
		return "", err
	}
	return r.RecordId, nil
}

func (a *aliyun) SetRemark(ctx context.Context, zone, id, remark string) error {
	if remark == "" {
		return nil
	}
	if err := a.call(ctx, "UpdateDomainRecordRemark", map[string]string{"RecordId": id, "Remark": remark}, nil); err != nil {
		if strings.Contains(err.Error(), "Forbidden") || strings.Contains(err.Error(), "NoPermission") {
			return fmt.Errorf("%w（AccessKey 缺少 alidns:UpdateDomainRecordRemark 权限，请在 RAM 中授权）", err)
		}
		return err
	}
	return nil
}

func (a *aliyun) Update(ctx context.Context, zone, id, sub, typ, value, remark string) error {
	err := a.call(ctx, "UpdateDomainRecord", map[string]string{"RecordId": id, "RR": sub, "Type": typ, "Value": value}, nil)
	if err != nil && !strings.Contains(err.Error(), "DomainRecordDuplicate") { // 记录值未变化时仍更新备注
		return err
	}
	return nil
}

func (a *aliyun) SetStatus(ctx context.Context, zone, id string, enabled bool) error {
	return a.call(ctx, "SetDomainRecordStatus", map[string]string{"RecordId": id, "Status": map[bool]string{true: "Enable", false: "Disable"}[enabled]}, nil)
}

func (a *aliyun) Delete(ctx context.Context, zone, id string) error {
	err := a.call(ctx, "DeleteDomainRecord", map[string]string{"RecordId": id}, nil)
	if err != nil && strings.Contains(err.Error(), "DomainRecordNotBelongToUser") {
		return nil
	}
	return err
}

// ---------------- 腾讯云 DNSPod（API 3.0，TC3-HMAC-SHA256） ----------------

type tencent struct {
	hc     *http.Client
	base   string
	ak, sk string
}

func hmacSHA256(key []byte, s string) []byte {
	m := hmac.New(sha256.New, key)
	m.Write([]byte(s))
	return m.Sum(nil)
}

func sha256Hex(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

// tencentAuth 计算 TC3-HMAC-SHA256 签名的 Authorization 头。
func tencentAuth(ak, sk, host, payload string, ts int64) string {
	const service = "dnspod"
	date := time.Unix(ts, 0).UTC().Format("2006-01-02")
	canonical := "POST\n/\n\ncontent-type:application/json; charset=utf-8\nhost:" + host + "\n\ncontent-type;host\n" + sha256Hex([]byte(payload))
	scope := date + "/" + service + "/tc3_request"
	str := "TC3-HMAC-SHA256\n" + strconv.FormatInt(ts, 10) + "\n" + scope + "\n" + sha256Hex([]byte(canonical))
	key := hmacSHA256(hmacSHA256(hmacSHA256([]byte("TC3"+sk), date), service), "tc3_request")
	sig := hex.EncodeToString(hmacSHA256(key, str))
	return "TC3-HMAC-SHA256 Credential=" + ak + "/" + scope + ", SignedHeaders=content-type;host, Signature=" + sig
}

func (t *tencent) call(ctx context.Context, action string, params map[string]any, out any) error {
	payload, _ := json.Marshal(params)
	u, _ := url.Parse(t.base)
	ts := now().Unix()
	req, _ := http.NewRequestWithContext(ctx, "POST", t.base+"/", strings.NewReader(string(payload)))
	req.Header.Set("Content-Type", "application/json; charset=utf-8")
	req.Header.Set("X-TC-Action", action)
	req.Header.Set("X-TC-Version", "2021-03-23")
	req.Header.Set("X-TC-Timestamp", strconv.FormatInt(ts, 10))
	req.Header.Set("Authorization", tencentAuth(t.ak, t.sk, u.Host, string(payload), ts))
	b, code, err := do(t.hc, req)
	if err != nil {
		return err
	}
	var r struct {
		Response struct {
			Error *struct{ Code, Message string } `json:"Error"`
		} `json:"Response"`
	}
	if json.Unmarshal(b, &r) != nil {
		return fmt.Errorf("腾讯云 DNS 返回 HTTP %d: %s", code, trimErr(string(b)))
	}
	if e := r.Response.Error; e != nil {
		return &apiError{"腾讯云 DNS", e.Code, e.Message}
	}
	if out != nil {
		var w struct {
			Response json.RawMessage `json:"Response"`
		}
		json.Unmarshal(b, &w)
		return json.Unmarshal(w.Response, out)
	}
	return nil
}

type apiError struct{ who, code, msg string }

func (e *apiError) Error() string { return fmt.Sprintf("%s：%s（%s）", e.who, e.msg, e.code) }

func (t *tencent) Zones(ctx context.Context) ([]string, error) {
	var r struct {
		DomainList []struct{ Name string } `json:"DomainList"`
	}
	if err := t.call(ctx, "DescribeDomainList", map[string]any{"Limit": 3000}, &r); err != nil {
		return nil, err
	}
	var res []string
	for _, x := range r.DomainList {
		res = append(res, x.Name)
	}
	return res, nil
}

func (t *tencent) Records(ctx context.Context, zone, sub string) ([]Record, error) {
	var r struct {
		RecordList []struct {
			RecordId   uint64
			Name, Type string
			Value      string
		} `json:"RecordList"`
	}
	err := t.call(ctx, "DescribeRecordList", map[string]any{"Domain": zone, "Subdomain": sub, "Limit": 3000}, &r)
	if e, ok := err.(*apiError); ok && e.code == "ResourceNotFound.NoDataOfRecord" {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var res []Record
	for _, x := range r.RecordList {
		if strings.EqualFold(x.Name, sub) {
			res = append(res, Record{ID: strconv.FormatUint(x.RecordId, 10), Name: x.Name, Type: x.Type, Value: x.Value})
		}
	}
	return res, nil
}

func (t *tencent) Create(ctx context.Context, zone, sub, typ, value, remark string) (string, error) {
	var r struct{ RecordId uint64 }
	err := t.call(ctx, "CreateRecord", map[string]any{"Domain": zone, "SubDomain": sub, "RecordType": typ, "RecordLine": "默认", "Value": value, "Remark": remark}, &r)
	return strconv.FormatUint(r.RecordId, 10), err
}

func (t *tencent) Update(ctx context.Context, zone, id, sub, typ, value, remark string) error {
	rid, _ := strconv.ParseUint(id, 10, 64)
	return t.call(ctx, "ModifyRecord", map[string]any{"Domain": zone, "RecordId": rid, "SubDomain": sub, "RecordType": typ, "RecordLine": "默认", "Value": value, "Remark": remark}, nil)
}

func (t *tencent) SetStatus(ctx context.Context, zone, id string, enabled bool) error {
	rid, _ := strconv.ParseUint(id, 10, 64)
	return t.call(ctx, "ModifyRecordStatus", map[string]any{"Domain": zone, "RecordId": rid, "Status": map[bool]string{true: "ENABLE", false: "DISABLE"}[enabled]}, nil)
}

func (t *tencent) Delete(ctx context.Context, zone, id string) error {
	rid, _ := strconv.ParseUint(id, 10, 64)
	err := t.call(ctx, "DeleteRecord", map[string]any{"Domain": zone, "RecordId": rid}, nil)
	if e, ok := err.(*apiError); ok && strings.HasPrefix(e.code, "ResourceNotFound") {
		return nil
	}
	return err
}

// ---------------- 华为云 DNS（SDK-HMAC-SHA256） ----------------

type huawei struct {
	hc     *http.Client
	base   string
	ak, sk string
}

// huaweiEscape 按华为云 API 签名要求编码路径段与查询参数。
func huaweiEscape(s string) string {
	return strings.NewReplacer("+", "%20", "*", "%2A", "%7E", "~").Replace(url.QueryEscape(s))
}

// huaweiSign 为请求加上 X-Sdk-Date 与 Authorization 头。
func huaweiSign(req *http.Request, ak, sk string, body []byte, t time.Time) {
	date := t.UTC().Format("20060102T150405Z")
	req.Header.Set("X-Sdk-Date", date)
	// 规范 URI：逐段编码，并以 / 结尾
	segs := strings.Split(req.URL.Path, "/")
	for i, s := range segs {
		segs[i] = huaweiEscape(s)
	}
	uri := strings.Join(segs, "/")
	if !strings.HasSuffix(uri, "/") {
		uri += "/"
	}
	q := req.URL.Query()
	keys := make([]string, 0, len(q))
	for k := range q {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var qs []string
	for _, k := range keys {
		vs := append([]string(nil), q[k]...)
		sort.Strings(vs)
		for _, v := range vs {
			qs = append(qs, huaweiEscape(k)+"="+huaweiEscape(v))
		}
	}
	headers := map[string]string{"content-type": req.Header.Get("Content-Type"), "host": req.URL.Host, "x-sdk-date": date}
	names := []string{"content-type", "host", "x-sdk-date"}
	var ch strings.Builder
	for _, n := range names {
		ch.WriteString(n + ":" + strings.TrimSpace(headers[n]) + "\n")
	}
	signed := strings.Join(names, ";")
	canonical := req.Method + "\n" + uri + "\n" + strings.Join(qs, "&") + "\n" + ch.String() + "\n" + signed + "\n" + sha256Hex(body)
	str := "SDK-HMAC-SHA256\n" + date + "\n" + sha256Hex([]byte(canonical))
	sig := hex.EncodeToString(hmacSHA256([]byte(sk), str))
	req.Header.Set("Authorization", "SDK-HMAC-SHA256 Access="+ak+", SignedHeaders="+signed+", Signature="+sig)
}

func (h *huawei) call(ctx context.Context, method, path string, q url.Values, in, out any) error {
	var body []byte
	if in != nil {
		body, _ = json.Marshal(in)
	}
	u := h.base + path
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	req, _ := http.NewRequestWithContext(ctx, method, u, strings.NewReader(string(body)))
	req.Header.Set("Content-Type", "application/json")
	huaweiSign(req, h.ak, h.sk, body, now())
	b, code, err := do(h.hc, req)
	if err != nil {
		return err
	}
	if code/100 != 2 {
		var e struct {
			Code    string `json:"code"`
			Message string `json:"message"`
			ErrCode string `json:"error_code"`
			ErrMsg  string `json:"error_msg"`
		}
		json.Unmarshal(b, &e)
		if e.Code == "" {
			e.Code, e.Message = e.ErrCode, e.ErrMsg
		}
		if e.Code != "" {
			return &apiError{"华为云 DNS", e.Code, e.Message}
		}
		return fmt.Errorf("华为云 DNS 返回 HTTP %d: %s", code, trimErr(string(b)))
	}
	if out != nil && len(b) > 0 {
		return json.Unmarshal(b, out)
	}
	return nil
}

type huaweiZone struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

func (h *huawei) zones(ctx context.Context, name string) ([]huaweiZone, error) {
	q := url.Values{"type": {"public"}, "limit": {"500"}}
	if name != "" {
		q.Set("name", name+".")
	}
	var r struct {
		Zones []huaweiZone `json:"zones"`
	}
	err := h.call(ctx, "GET", "/v2/zones", q, nil, &r)
	return r.Zones, err
}

func (h *huawei) zoneID(ctx context.Context, zone string) (string, error) {
	zs, err := h.zones(ctx, zone)
	if err != nil {
		return "", err
	}
	for _, z := range zs {
		if strings.EqualFold(strings.TrimSuffix(z.Name, "."), zone) {
			return z.ID, nil
		}
	}
	return "", fmt.Errorf("华为云 DNS：账号下没有域名 %s", zone)
}

func (h *huawei) Zones(ctx context.Context) ([]string, error) {
	zs, err := h.zones(ctx, "")
	var res []string
	for _, z := range zs {
		res = append(res, strings.TrimSuffix(z.Name, "."))
	}
	return res, err
}

func (h *huawei) Records(ctx context.Context, zone, sub string) ([]Record, error) {
	zid, err := h.zoneID(ctx, zone)
	if err != nil {
		return nil, err
	}
	fqdn := sub + "." + zone + "."
	var r struct {
		Recordsets []struct {
			ID      string   `json:"id"`
			Name    string   `json:"name"`
			Type    string   `json:"type"`
			Records []string `json:"records"`
		} `json:"recordsets"`
	}
	if err := h.call(ctx, "GET", "/v2/zones/"+zid+"/recordsets", url.Values{"name": {fqdn}, "limit": {"500"}}, nil, &r); err != nil {
		return nil, err
	}
	var res []Record
	for _, x := range r.Recordsets {
		if strings.EqualFold(x.Name, fqdn) {
			res = append(res, Record{ID: x.ID, Name: sub, Type: x.Type, Value: strings.Join(x.Records, ",")})
		}
	}
	return res, nil
}

func (h *huawei) Create(ctx context.Context, zone, sub, typ, value, remark string) (string, error) {
	zid, err := h.zoneID(ctx, zone)
	if err != nil {
		return "", err
	}
	var r struct {
		ID string `json:"id"`
	}
	err = h.call(ctx, "POST", "/v2/zones/"+zid+"/recordsets", nil,
		map[string]any{"name": sub + "." + zone + ".", "type": typ, "records": []string{value}, "ttl": 300, "description": remark}, &r)
	return r.ID, err
}

func (h *huawei) Update(ctx context.Context, zone, id, sub, typ, value, remark string) error {
	zid, err := h.zoneID(ctx, zone)
	if err != nil {
		return err
	}
	return h.call(ctx, "PUT", "/v2/zones/"+zid+"/recordsets/"+url.PathEscape(id), nil,
		map[string]any{"name": sub + "." + zone + ".", "type": typ, "records": []string{value}, "ttl": 300, "description": remark}, nil)
}

// SetStatus 使用华为云 v2.1 接口设置记录集状态。
func (h *huawei) SetStatus(ctx context.Context, zone, id string, enabled bool) error {
	return h.call(ctx, "PUT", "/v2.1/recordsets/"+url.PathEscape(id)+"/statuses/set", nil,
		map[string]string{"status": map[bool]string{true: "ENABLE", false: "DISABLE"}[enabled]}, nil)
}

func (h *huawei) Delete(ctx context.Context, zone, id string) error {
	zid, err := h.zoneID(ctx, zone)
	if err != nil {
		return err
	}
	err = h.call(ctx, "DELETE", "/v2/zones/"+zid+"/recordsets/"+url.PathEscape(id), nil, nil, nil)
	if e, ok := err.(*apiError); ok && e.code == "DNS.0313" { // 记录集不存在
		return nil
	}
	return err
}

// ---------------- Cloudflare（API Token） ----------------

type cloudflare struct {
	hc    *http.Client
	base  string
	token string
}

func (c *cloudflare) call(ctx context.Context, method, path string, q url.Values, in, out any) error {
	u := c.base + path
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	var body io.Reader
	if in != nil {
		body = jsonBody(in)
	}
	req, _ := http.NewRequestWithContext(ctx, method, u, body)
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Content-Type", "application/json")
	b, code, err := do(c.hc, req)
	if err != nil {
		return err
	}
	var r struct {
		Success bool `json:"success"`
		Errors  []struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"errors"`
		Result json.RawMessage `json:"result"`
	}
	if json.Unmarshal(b, &r) != nil {
		return fmt.Errorf("Cloudflare 返回 HTTP %d: %s", code, trimErr(string(b)))
	}
	if !r.Success {
		if len(r.Errors) > 0 {
			return &apiError{"Cloudflare", strconv.Itoa(r.Errors[0].Code), r.Errors[0].Message}
		}
		return fmt.Errorf("Cloudflare 返回 HTTP %d", code)
	}
	if out != nil {
		return json.Unmarshal(r.Result, out)
	}
	return nil
}

func (c *cloudflare) zoneID(ctx context.Context, zone string) (string, error) {
	var zs []struct{ ID, Name string }
	if err := c.call(ctx, "GET", "/zones", url.Values{"name": {zone}}, nil, &zs); err != nil {
		return "", err
	}
	for _, z := range zs {
		if strings.EqualFold(z.Name, zone) {
			return z.ID, nil
		}
	}
	return "", fmt.Errorf("Cloudflare：账号下没有域名 %s（或 Token 无权访问）", zone)
}

func (c *cloudflare) Zones(ctx context.Context) ([]string, error) {
	var zs []struct{ Name string }
	if err := c.call(ctx, "GET", "/zones", url.Values{"per_page": {"50"}}, nil, &zs); err != nil {
		return nil, err
	}
	var res []string
	for _, z := range zs {
		res = append(res, z.Name)
	}
	return res, nil
}

func (c *cloudflare) Records(ctx context.Context, zone, sub string) ([]Record, error) {
	zid, err := c.zoneID(ctx, zone)
	if err != nil {
		return nil, err
	}
	var rs []struct{ ID, Type, Name, Content string }
	if err := c.call(ctx, "GET", "/zones/"+zid+"/dns_records", url.Values{"name": {sub + "." + zone}, "per_page": {"100"}}, nil, &rs); err != nil {
		return nil, err
	}
	var res []Record
	for _, x := range rs {
		if strings.EqualFold(x.Name, sub+"."+zone) {
			res = append(res, Record{ID: x.ID, Name: sub, Type: x.Type, Value: x.Content})
		}
	}
	return res, nil
}

func (c *cloudflare) Create(ctx context.Context, zone, sub, typ, value, remark string) (string, error) {
	zid, err := c.zoneID(ctx, zone)
	if err != nil {
		return "", err
	}
	var r struct{ ID string }
	err = c.call(ctx, "POST", "/zones/"+zid+"/dns_records", nil,
		map[string]any{"type": typ, "name": sub + "." + zone, "content": value, "ttl": 1, "proxied": false, "comment": remark}, &r)
	return r.ID, err
}

func (c *cloudflare) Update(ctx context.Context, zone, id, sub, typ, value, remark string) error {
	zid, err := c.zoneID(ctx, zone)
	if err != nil {
		return err
	}
	return c.call(ctx, "PUT", "/zones/"+zid+"/dns_records/"+url.PathEscape(id), nil,
		map[string]any{"type": typ, "name": sub + "." + zone, "content": value, "ttl": 1, "proxied": false, "comment": remark}, nil)
}

func (c *cloudflare) Delete(ctx context.Context, zone, id string) error {
	zid, err := c.zoneID(ctx, zone)
	if err != nil {
		return err
	}
	err = c.call(ctx, "DELETE", "/zones/"+zid+"/dns_records/"+url.PathEscape(id), nil, nil, nil)
	if e, ok := err.(*apiError); ok && e.code == "81044" { // 记录不存在
		return nil
	}
	return err
}

func nonce() string {
	b := make([]byte, 16)
	rand.Read(b)
	return hex.EncodeToString(b)
}
