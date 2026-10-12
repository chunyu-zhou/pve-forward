package server

import (
	"net/http"
	"strings"
	"sync"
	"time"

	"pveforward/internal/secret"
	"pveforward/internal/sshx"
	"pveforward/internal/store"
)

// ---------------- 虚拟机 ----------------

type vmOut struct {
	*store.VM
	Owner    string `json:"owner"`
	NodeName string `json:"nodeName"`
	Rules    int    `json:"rules"`
}

func (s *Server) listVMs(w http.ResponseWriter, r *http.Request) {
	vms, err := s.db.VMs(queryInt(r, "node"))
	if err != nil {
		fail(w, err)
		return
	}
	users := map[int64]string{}
	if us, err := s.db.Users(); err == nil {
		for _, u := range us {
			users[u.ID] = u.Username
		}
	}
	nodes := map[int64]*store.Node{}
	if ns, err := s.db.Nodes(); err == nil {
		for _, n := range ns {
			nodes[n.ID] = n
		}
	}
	refs := map[int64]int{}
	rows, err := s.db.Query(`SELECT vm_id, COUNT(*) FROM rules GROUP BY vm_id`)
	if err == nil {
		for rows.Next() {
			var id int64
			var c int
			rows.Scan(&id, &c)
			refs[id] = c
		}
		rows.Close()
	}
	res := []*vmOut{}
	for _, v := range vms {
		o := &vmOut{VM: v, Rules: refs[v.ID]}
		if v.UserID != nil {
			o.Owner = users[*v.UserID]
		}
		if n := nodes[v.NodeID]; n != nil {
			o.NodeName = n.Name
		}
		res = append(res, o)
	}
	writeJSON(w, res)
}

func (s *Server) updateVM(w http.ResponseWriter, r *http.Request) {
	v, err := s.db.VM(pathID(r))
	if err != nil {
		fail(w, err)
		return
	}
	var req struct {
		Action string `json:"action"` // assign | share | unshare | reclaim
		UserID int64  `json:"userId"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	tx, err := s.db.Begin()
	if err != nil {
		fail(w, err)
		return
	}
	defer tx.Rollback()
	switch req.Action {
	case "assign":
		u, err := s.db.User(req.UserID)
		if err != nil || u.Role == "admin" {
			fail(w, bad("用户不存在"))
			return
		}
		err = s.assignVM(tx, v, &u.ID, false)
		if err != nil {
			fail(w, err)
			return
		}
	case "share":
		if err := s.assignVM(tx, v, nil, true); err != nil {
			fail(w, err)
			return
		}
	case "unshare", "reclaim":
		tx.Exec(`UPDATE vms SET user_id=NULL, shared=0 WHERE id=?`, v.ID)
	default:
		jsonErr(w, http.StatusBadRequest, "未知操作")
		return
	}
	if err := tx.Commit(); err != nil {
		fail(w, err)
		return
	}
	s.mgr.Push(v.NodeID)
	nv, _ := s.db.VM(v.ID)
	writeJSON(w, nv)
}

// ---------------- 凭证 ----------------

type credOut struct {
	*store.Credential
	Nodes []string `json:"nodes"`
}

func (s *Server) listCreds(w http.ResponseWriter, r *http.Request) {
	cs, err := s.db.Credentials()
	if err != nil {
		fail(w, err)
		return
	}
	nodes, _ := s.db.Nodes()
	res := []*credOut{}
	for _, c := range cs {
		o := &credOut{Credential: c, Nodes: []string{}}
		for _, n := range nodes {
			if n.Mode == "ssh" && n.SSHAuth == "cred" && n.CredID != nil && *n.CredID == c.ID {
				o.Nodes = append(o.Nodes, n.Name)
			}
		}
		res = append(res, o)
	}
	writeJSON(w, res)
}

// 控制端生成的密钥对在保存前暂存于内存，私钥不经过浏览器
var genKeys = struct {
	sync.Mutex
	m map[string]genKey
}{m: map[string]genKey{}}

type genKey struct {
	priv string
	exp  time.Time
}

func (s *Server) generateKey(w http.ResponseWriter, r *http.Request) {
	priv, pub, err := sshx.GenerateKey("pve-forward")
	if err != nil {
		fail(w, err)
		return
	}
	id := secret.RandomToken(16)
	genKeys.Lock()
	for k, v := range genKeys.m {
		if time.Now().After(v.exp) {
			delete(genKeys.m, k)
		}
	}
	genKeys.m[id] = genKey{priv, time.Now().Add(time.Hour)}
	genKeys.Unlock()
	_, fp, _ := sshx.KeyInfo(priv, "")
	writeJSON(w, map[string]string{"keyId": id, "publicKey": pub, "fingerprint": fp,
		"command": "mkdir -p /root/.ssh && chmod 700 /root/.ssh && echo '" + pub + "' >> /root/.ssh/authorized_keys && chmod 600 /root/.ssh/authorized_keys"})
}

func (s *Server) saveCred(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Name       string `json:"name"`
		Type       string `json:"type"`
		Username   string `json:"username"`
		Password   string `json:"password"`
		PrivateKey string `json:"privateKey"`
		Passphrase string `json:"passphrase"`
		KeyID      string `json:"keyId"` // 控制端生成的密钥
		Remark     string `json:"remark"`
	}
	if !readJSON(w, r, &in) {
		return
	}
	c := &store.Credential{}
	if id := pathID(r); id > 0 {
		var err error
		if c, err = s.db.Credential(id); err != nil {
			fail(w, err)
			return
		}
	}
	in.Name = strings.TrimSpace(in.Name)
	if in.Name == "" {
		jsonErr(w, http.StatusBadRequest, "请输入凭证名称")
		return
	}
	if in.Type != "password" && in.Type != "key" {
		jsonErr(w, http.StatusBadRequest, "类型无效")
		return
	}
	if in.Username = strings.TrimSpace(in.Username); in.Username == "" {
		in.Username = "root"
	}
	typeChanged := c.ID > 0 && c.Type != in.Type
	c.Name, c.Username, c.Remark = in.Name, in.Username, in.Remark
	c.Type = in.Type
	switch {
	case in.Type == "key" && in.KeyID != "":
		genKeys.Lock()
		g, ok := genKeys.m[in.KeyID]
		delete(genKeys.m, in.KeyID)
		genKeys.Unlock()
		if !ok {
			jsonErr(w, http.StatusBadRequest, "生成的密钥已过期，请重新生成")
			return
		}
		in.PrivateKey, in.Passphrase = g.priv, ""
		fallthrough
	case in.Type == "key" && in.PrivateKey != "":
		algo, fp, err := sshx.KeyInfo(in.PrivateKey, in.Passphrase)
		if err != nil {
			jsonErr(w, http.StatusBadRequest, err.Error())
			return
		}
		c.KeyAlgo, c.KeyFP = algo, fp
		c.SecretEnc, c.PassphraseEnc = s.box.Seal(in.PrivateKey), s.box.Seal(in.Passphrase)
	case in.Type == "password" && in.Password != "":
		c.KeyAlgo, c.KeyFP = "", ""
		c.SecretEnc, c.PassphraseEnc = s.box.Seal(in.Password), nil
	case c.ID == 0 || typeChanged:
		jsonErr(w, http.StatusBadRequest, map[string]string{"key": "请提供私钥", "password": "请输入密码"}[in.Type])
		return
	}
	if err := s.db.SaveCredential(c); err != nil {
		fail(w, err)
		return
	}
	// 引用该凭证的 SSH 节点使用新凭证重新连接
	if nodes, err := s.db.Nodes(); err == nil {
		for _, n := range nodes {
			if n.Mode == "ssh" && n.CredID != nil && *n.CredID == c.ID {
				s.mgr.AddNode(n.ID)
			}
		}
	}
	writeJSON(w, c)
}

func (s *Server) deleteCred(w http.ResponseWriter, r *http.Request) {
	id := pathID(r)
	var cnt int
	s.db.QueryRow(`SELECT COUNT(*) FROM nodes WHERE cred_id=? AND ssh_auth='cred'`, id).Scan(&cnt)
	if cnt > 0 {
		jsonErr(w, http.StatusBadRequest, "该凭证正被节点使用，无法删除")
		return
	}
	s.db.Exec(`UPDATE nodes SET cred_id=NULL WHERE cred_id=?`, id)
	s.db.Exec(`DELETE FROM credentials WHERE id=?`, id)
	writeJSON(w, map[string]bool{"ok": true})
}
