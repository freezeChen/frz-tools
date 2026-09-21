package idgen

import (
	"crypto/rand"
	"encoding/base32"
)

var encoding = base32.StdEncoding.WithPadding(base32.NoPadding)

func New(prefix string) string {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		panic("idgen: entropy source failed: " + err.Error())
	}
	return prefix + "_" + encoding.EncodeToString(buf)
}

func NewOperationID() string { return New("op") }
