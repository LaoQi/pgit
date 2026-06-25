package git

import (
	"crypto/sha1"
	"encoding/hex"
	"fmt"
)

// Oid 是 git 对象的 SHA1 标识，40 字符小写 hex
type Oid string

// ZeroOid 全零 oid，用于 ref CAS 的「必须不存在」语义
var ZeroOid = Oid("0000000000000000000000000000000000000000")

// IsZero 判断是否全零
func (o Oid) IsZero() bool { return o == ZeroOid }

// String 返回 string 形式
func (o Oid) String() string { return string(o) }

// Valid 简单校验长度与 hex 合法性
func (o Oid) Valid() bool {
	if len(o) != 40 {
		return false
	}
	_, err := hex.DecodeString(string(o))
	return err == nil
}

// ComputeOid 计算对象 oid：sha1("<type> <size>\0<content>")
func ComputeOid(objType string, content []byte) Oid {
	h := sha1.New()
	fmt.Fprintf(h, "%s %d\x00", objType, len(content))
	h.Write(content)
	return Oid(hex.EncodeToString(h.Sum(nil)))
}
