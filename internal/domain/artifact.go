package domain

import (
	"crypto/sha256"
	"encoding/hex"
	"hash"
	"io"
	"strings"
	"time"

	v1 "github.com/freezeChen/frz-tools/api/v1"
)

const digestAlgorithm = "sha256"

// Digest 是内容寻址标识，格式固定为 "sha256:<64 位小写十六进制>"。
type Digest string

func (d Digest) String() string { return string(d) }

func (d Digest) Validate() error {
	algorithm, hexPart, found := strings.Cut(string(d), ":")
	if !found || algorithm != digestAlgorithm {
		return NewError(v1.CodeInvalidRequest, "digest must look like %s:<hex>, got %q", digestAlgorithm, d)
	}
	if len(hexPart) != sha256.Size*2 {
		return NewError(v1.CodeInvalidRequest, "digest %q has the wrong length", d)
	}
	for _, r := range hexPart {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return NewError(v1.CodeInvalidRequest, "digest %q must be lowercase hexadecimal", d)
		}
	}
	return nil
}

func NewDigestFromHash(sum []byte) Digest {
	return Digest(digestAlgorithm + ":" + hex.EncodeToString(sum))
}

func ParseDigest(value string) (Digest, error) {
	digest := Digest(value)
	if err := digest.Validate(); err != nil {
		return "", err
	}
	return digest, nil
}

// Hasher 在流式写入的同时计算摘要和字节数，避免为了取摘要而回读整个文件。
type Hasher struct {
	hash hash.Hash
	size int64
}

func NewHasher() *Hasher {
	return &Hasher{hash: sha256.New()}
}

func (h *Hasher) Write(p []byte) (int, error) {
	n, err := h.hash.Write(p)
	h.size += int64(n)
	return n, err
}

func (h *Hasher) Digest() Digest { return NewDigestFromHash(h.hash.Sum(nil)) }

func (h *Hasher) Size() int64 { return h.size }

// DigestOf 计算整个 reader 的摘要与长度，仅用于小对象和测试。
func DigestOf(r io.Reader) (Digest, int64, error) {
	hasher := NewHasher()
	if _, err := io.Copy(hasher, r); err != nil {
		return "", 0, err
	}
	return hasher.Digest(), hasher.Size(), nil
}

// Artifact 是不可变制品。它的内容由 Digest 唯一确定，因此不存在覆盖语义，
// 只有新增或软删除。
type Artifact struct {
	ID        string
	Digest    Digest
	Size      int64
	MediaType string
	Name      string
	CreatedAt time.Time
	CreatedBy string
	DeletedAt *time.Time
}

func (a *Artifact) Validate() error {
	if err := a.Digest.Validate(); err != nil {
		return err
	}
	if a.Size < 0 {
		return NewError(v1.CodeInvalidRequest, "artifact size must not be negative")
	}
	if a.ID == "" {
		return NewError(v1.CodeInternal, "artifact id must be set before validation")
	}
	return nil
}

func (a *Artifact) Deleted() bool { return a.DeletedAt != nil }
