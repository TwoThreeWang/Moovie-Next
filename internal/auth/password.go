package auth

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
)

// PasswordKeyring 只有客户端已经加密的内容，服务端不接收主密钥或订阅明文。
type PasswordKeyring struct {
	Salt       []byte `json:"salt"`
	Blob       []byte `json:"blob"`
	Iterations int    `json:"iterations"`
	Base       int64  `json:"base"`
}

func (k PasswordKeyring) Valid() bool {
	return len(k.Salt) == 16 && len(k.Blob) >= 29 && len(k.Blob) <= 4096 && k.Iterations >= 100000 && k.Iterations <= 5000000 && k.Base >= 0
}

var ErrPasswordConflict = errors.New("账号或同步配置已变化")
var ErrKeyringRequired = errors.New("修改密码需要同时更新加密配置")

// SetPasswordAndKeyring 将密码、登录版本和客户端重新加密的配置一起提交；任何失败全部回滚。
// nil 仅兼容尚未建立钥匙圈的旧表单；已有同步数据时必须提供新密文。
func (s *Store) SetPasswordAndKeyring(ctx context.Context, id int64, expectedEpoch int, hash string, k *PasswordKeyring) (int, int64, error) {
	beginner, ok := s.db.(interface {
		Begin(context.Context) (pgx.Tx, error)
	})
	if !ok {
		return 0, 0, errors.New("数据库不支持事务")
	}
	tx, err := beginner.Begin(ctx)
	if err != nil {
		return 0, 0, err
	}
	defer tx.Rollback(context.WithoutCancel(ctx))
	var epoch int
	if err = tx.QueryRow(ctx, `SELECT session_epoch FROM users WHERE id=$1 FOR NO KEY UPDATE`, id).Scan(&epoch); err != nil {
		return 0, 0, err
	}
	if epoch != expectedEpoch {
		return 0, 0, ErrPasswordConflict
	}
	var version int64
	if k == nil {
		var exists bool
		if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM keyrings WHERE user_id=$1)`, id).Scan(&exists); err != nil {
			return 0, 0, err
		}
		if exists {
			return 0, 0, ErrKeyringRequired
		}
	} else {
		if !k.Valid() {
			return 0, 0, errors.New("钥匙圈格式不正确")
		}
		if k.Base == 0 {
			err = tx.QueryRow(ctx, `INSERT INTO keyrings (user_id,salt,iterations,blob) VALUES ($1,$2,$3,$4)
ON CONFLICT(user_id) DO NOTHING RETURNING version`, id, k.Salt, k.Iterations, k.Blob).Scan(&version)
		} else {
			err = tx.QueryRow(ctx, `UPDATE keyrings SET salt=$2,iterations=$3,blob=$4,version=version+1,updated_at=now()
WHERE user_id=$1 AND version=$5 RETURNING version`, id, k.Salt, k.Iterations, k.Blob, k.Base).Scan(&version)
		}
		if errors.Is(err, pgx.ErrNoRows) {
			return 0, 0, ErrPasswordConflict
		}
		if err != nil {
			return 0, 0, err
		}
	}
	err = tx.QueryRow(ctx, `UPDATE users SET password_hash=$2,session_epoch=session_epoch+1 WHERE id=$1 RETURNING session_epoch`, id, hash).Scan(&epoch)
	if err != nil {
		return 0, 0, err
	}
	if err = tx.Commit(ctx); err != nil {
		return 0, 0, err
	}
	return epoch, version, nil
}
