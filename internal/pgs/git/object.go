package git

import "fmt"

// ObjectType git 对象类型
type ObjectType string

const (
	ObjBlob   ObjectType = "blob"
	ObjTree   ObjectType = "tree"
	ObjCommit ObjectType = "commit"
	ObjTag    ObjectType = "tag"
)

// RawObject 底层对象抽象，对应 on-disk loose object
type RawObject struct {
	Type    ObjectType
	Size    int    // content 字节数
	Content []byte // 原始未压缩内容
}

// NewRawObject 构造并设置 Size
func NewRawObject(t ObjectType, content []byte) *RawObject {
	return &RawObject{Type: t, Size: len(content), Content: content}
}

// Oid 计算本对象的 oid
func (o *RawObject) Oid() Oid {
	return ComputeOid(string(o.Type), o.Content)
}

// Header 返回 loose object 的 header 字节："<type> <size>\0"
func (o *RawObject) Header() []byte {
	return []byte(fmt.Sprintf("%s %d\x00", o.Type, o.Size))
}
