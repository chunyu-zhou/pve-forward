package store

import (
	"database/sql"
	"errors"
	"strings"
	"time"
)

var ErrTOTPChanged = errors.New("两步验证状态已变化或二维码已过期，请重新操作")

// 两步验证（TOTP）：每个用户（含管理员）各自绑定。密钥加密保存（由调用方用主密钥加解密）。

// migrate2FA 为旧数据库的用户表增加两步验证列。
func (d *DB) migrate2FA() error {
	for _, col := range []string{
		"totp_secret_enc BLOB",                    // 已启用的密钥
		"totp_pending_enc BLOB",                   // 绑定过程中生成、尚未验证的密钥
		"totp_last INTEGER NOT NULL DEFAULT 0",    // 上次使用的时间步（拒绝重复使用同一验证码）
		"totp_enabled INTEGER NOT NULL DEFAULT 0", // 是否已启用
		"totp_pending_expires INTEGER NOT NULL DEFAULT 0",
	} {
		name := col[:strings.IndexByte(col, ' ')]
		var has int
		if err := d.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('users') WHERE name=?`, name).Scan(&has); err != nil {
			return err
		}
		if has == 0 {
			if _, err := d.Exec(`ALTER TABLE users ADD COLUMN ` + col); err != nil {
				return err
			}
		}
	}
	return nil
}

func totpUpdated(r sql.Result, err error) error {
	if err != nil {
		return err
	}
	n, err := r.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return ErrTOTPChanged
	}
	return nil
}

// TOTP 返回用户已启用的密钥（密文）与上次使用的时间步；未启用时 enabled 为 false。
func (d *DB) TOTP(uid int64) (secretEnc []byte, last int64, enabled bool, err error) {
	err = d.QueryRow(`SELECT totp_secret_enc, totp_last, totp_enabled FROM users WHERE id=?`, uid).Scan(&secretEnc, &last, &enabled)
	return
}

// TOTPPending 返回绑定过程中的密钥（密文）。
func (d *DB) TOTPPending(uid int64) ([]byte, error) {
	var b []byte
	err := d.QueryRow(`SELECT totp_pending_enc FROM users WHERE id=? AND totp_enabled=0 AND totp_pending_expires>?`, uid, time.Now().Unix()).Scan(&b)
	return b, err
}

func (d *DB) SetTOTPPending(uid int64, enc []byte) error {
	return totpUpdated(d.Exec(`UPDATE users SET totp_pending_enc=?, totp_pending_expires=? WHERE id=? AND totp_enabled=0`, enc, time.Now().Add(10*time.Minute).Unix(), uid))
}

// EnableTOTP 启用绑定过程中的密钥，并记录本次验证使用的时间步。
func (d *DB) EnableTOTP(uid int64, enc []byte, step int64, keepSession string) error {
	tx, err := d.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := totpUpdated(tx.Exec(`UPDATE users SET totp_secret_enc=totp_pending_enc, totp_pending_enc=NULL, totp_pending_expires=0, totp_last=?, totp_enabled=1 WHERE id=? AND totp_enabled=0 AND totp_pending_enc=? AND totp_pending_expires>?`, step, uid, enc, time.Now().Unix())); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM sessions WHERE user_id=? AND token_hash<>?`, uid, keepSession); err != nil {
		return err
	}
	return tx.Commit()
}

// DisableTOTP 解除两步验证。
func (d *DB) DisableTOTP(uid int64) error {
	return d.disableTOTP(uid, nil, 0, "")
}

// DisableVerifiedTOTP 仅解除刚验证过的绑定，保留当前会话。
func (d *DB) DisableVerifiedTOTP(uid int64, enc []byte, step int64, keepSession string) error {
	return d.disableTOTP(uid, enc, step, keepSession)
}

func (d *DB) disableTOTP(uid int64, enc []byte, step int64, keepSession string) error {
	tx, err := d.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	query := `UPDATE users SET totp_secret_enc=NULL, totp_pending_enc=NULL, totp_pending_expires=0, totp_last=0, totp_enabled=0 WHERE id=?`
	args := []any{uid}
	if enc != nil {
		query += ` AND totp_enabled=1 AND totp_secret_enc=? AND totp_last<?`
		args = append(args, enc, step)
	}
	if err := totpUpdated(tx.Exec(query, args...)); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM sessions WHERE user_id=? AND token_hash<>?`, uid, keepSession); err != nil {
		return err
	}
	return tx.Commit()
}

// UseTOTPStep 记录成功使用的时间步；只有大于上次的才会更新（并发登录时同一验证码只生效一次）。
func (d *DB) UseTOTPStep(uid int64, enc []byte, step int64) bool {
	r, err := d.Exec(`UPDATE users SET totp_last=? WHERE id=? AND totp_enabled=1 AND totp_secret_enc=? AND totp_last<?`, step, uid, enc, step)
	if err != nil {
		return false
	}
	n, _ := r.RowsAffected()
	return n == 1
}
