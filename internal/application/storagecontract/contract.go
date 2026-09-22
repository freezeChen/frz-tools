// Package storagecontract 是所有 StorageBackend 实现都必须通过的合约测试。
// 后续 S3/MinIO 适配器直接复用同一套断言，避免本地实现与远端实现语义漂移。
package storagecontract

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	v1 "frz-tools/api/v1"
	"frz-tools/internal/application"
	"frz-tools/internal/domain"
)

func Run(t *testing.T, factory func(t *testing.T) application.StorageBackend) {
	t.Helper()

	t.Run("Put 返回内容摘要且 Stat 一致", func(t *testing.T) {
		store := factory(t)
		ctx := context.Background()
		content := []byte("frz-tools artifact payload")

		stored, err := store.Put(ctx, bytes.NewReader(content), "")
		if err != nil {
			t.Fatalf("put: %v", err)
		}
		want, size, err := domain.DigestOf(bytes.NewReader(content))
		if err != nil {
			t.Fatalf("digest: %v", err)
		}
		if stored.Digest != want {
			t.Fatalf("want digest %s, got %s", want, stored.Digest)
		}
		if stored.Size != size {
			t.Fatalf("want size %d, got %d", size, stored.Size)
		}

		stat, err := store.Stat(ctx, want)
		if err != nil {
			t.Fatalf("stat: %v", err)
		}
		if stat.Digest != want || stat.Size != size {
			t.Fatalf("stat disagrees with put: %+v", stat)
		}
	})

	t.Run("Open 返回原始字节", func(t *testing.T) {
		store := factory(t)
		ctx := context.Background()
		content := []byte("binary\x00content\xff")

		stored, err := store.Put(ctx, bytes.NewReader(content), "")
		if err != nil {
			t.Fatalf("put: %v", err)
		}
		reader, err := store.Open(ctx, stored.Digest)
		if err != nil {
			t.Fatalf("open: %v", err)
		}
		defer reader.Close()

		roundTripped, err := io.ReadAll(reader)
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		if !bytes.Equal(roundTripped, content) {
			t.Fatalf("content mismatch: %q", roundTripped)
		}
	})

	t.Run("声明摘要不一致时拒绝且不落盘", func(t *testing.T) {
		store := factory(t)
		ctx := context.Background()
		content := []byte("real content")

		wrong := domain.Digest("sha256:" + strings.Repeat("ab", 32))
		if _, err := store.Put(ctx, bytes.NewReader(content), wrong); domain.CodeOf(err) != v1.CodeArtifactChecksum {
			t.Fatalf("want ARTIFACT_CHECKSUM_MISMATCH, got %v", err)
		}

		actual, _, err := domain.DigestOf(bytes.NewReader(content))
		if err != nil {
			t.Fatalf("digest: %v", err)
		}
		if _, err := store.Stat(ctx, actual); domain.CodeOf(err) != v1.CodeArtifactNotFound {
			t.Fatalf("被拒绝的上传不得留下内容，got %v", err)
		}
	})

	t.Run("声明摘要一致时接受", func(t *testing.T) {
		store := factory(t)
		ctx := context.Background()
		content := []byte("content with declared digest")

		expected, _, err := domain.DigestOf(bytes.NewReader(content))
		if err != nil {
			t.Fatalf("digest: %v", err)
		}
		stored, err := store.Put(ctx, bytes.NewReader(content), expected)
		if err != nil {
			t.Fatalf("put: %v", err)
		}
		if stored.Digest != expected {
			t.Fatalf("want %s, got %s", expected, stored.Digest)
		}
	})

	t.Run("相同内容重复上传是幂等的", func(t *testing.T) {
		store := factory(t)
		ctx := context.Background()
		content := []byte("duplicate payload")

		first, err := store.Put(ctx, bytes.NewReader(content), "")
		if err != nil {
			t.Fatalf("first put: %v", err)
		}
		second, err := store.Put(ctx, bytes.NewReader(content), "")
		if err != nil {
			t.Fatalf("second put: %v", err)
		}
		if first.Digest != second.Digest {
			t.Fatalf("same content must map to one digest: %s != %s", first.Digest, second.Digest)
		}

		entries, err := store.List(ctx)
		if err != nil {
			t.Fatalf("list: %v", err)
		}
		if len(entries) != 1 {
			t.Fatalf("重复上传不得产生第二份内容，got %d", len(entries))
		}
	})

	t.Run("缺失的摘要返回未找到", func(t *testing.T) {
		store := factory(t)
		ctx := context.Background()
		missing := domain.Digest("sha256:" + strings.Repeat("cd", 32))

		if _, err := store.Stat(ctx, missing); domain.CodeOf(err) != v1.CodeArtifactNotFound {
			t.Fatalf("stat: want ARTIFACT_NOT_FOUND, got %v", err)
		}
		if _, err := store.Open(ctx, missing); domain.CodeOf(err) != v1.CodeArtifactNotFound {
			t.Fatalf("open: want ARTIFACT_NOT_FOUND, got %v", err)
		}
		// 删除不存在的内容是幂等的，不报错。
		if err := store.Delete(ctx, missing); err != nil {
			t.Fatalf("delete missing: %v", err)
		}
	})

	t.Run("Delete 之后内容与列表都不再包含它", func(t *testing.T) {
		store := factory(t)
		ctx := context.Background()

		kept, err := store.Put(ctx, bytes.NewReader([]byte("keep me")), "")
		if err != nil {
			t.Fatalf("put kept: %v", err)
		}
		removed, err := store.Put(ctx, bytes.NewReader([]byte("remove me")), "")
		if err != nil {
			t.Fatalf("put removed: %v", err)
		}

		if err := store.Delete(ctx, removed.Digest); err != nil {
			t.Fatalf("delete: %v", err)
		}
		if _, err := store.Stat(ctx, removed.Digest); domain.CodeOf(err) != v1.CodeArtifactNotFound {
			t.Fatalf("deleted blob must be gone, got %v", err)
		}

		entries, err := store.List(ctx)
		if err != nil {
			t.Fatalf("list: %v", err)
		}
		if len(entries) != 1 || entries[0].Digest != kept.Digest {
			t.Fatalf("unexpected list contents: %+v", entries)
		}
	})

	t.Run("非法摘要被拒绝", func(t *testing.T) {
		store := factory(t)
		ctx := context.Background()

		for _, bad := range []domain.Digest{
			domain.Digest("sha256:zz"),
			domain.Digest("md5:" + strings.Repeat("ab", 16)),
			domain.Digest("sha256:" + strings.ToUpper(strings.Repeat("ab", 32))),
			domain.Digest(""),
		} {
			if _, err := store.Stat(ctx, bad); err == nil {
				t.Fatalf("digest %q must be rejected", bad)
			} else if domain.CodeOf(err) == v1.CodeArtifactNotFound {
				t.Fatalf("digest %q must be rejected as invalid, not reported missing", bad)
			}
		}
	})

	t.Run("空内容也可以存储", func(t *testing.T) {
		store := factory(t)
		ctx := context.Background()

		stored, err := store.Put(ctx, bytes.NewReader(nil), "")
		if err != nil {
			t.Fatalf("put empty: %v", err)
		}
		if stored.Size != 0 {
			t.Fatalf("want size 0, got %d", stored.Size)
		}
		reader, err := store.Open(ctx, stored.Digest)
		if err != nil {
			t.Fatalf("open: %v", err)
		}
		defer reader.Close()
		data, err := io.ReadAll(reader)
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		if len(data) != 0 {
			t.Fatalf("want empty content, got %q", data)
		}
	})

	t.Run("读取失败的中途中断不留下内容", func(t *testing.T) {
		store := factory(t)
		ctx := context.Background()

		_, err := store.Put(ctx, &failingReader{}, "")
		if err == nil {
			t.Fatal("expected an error from the failing reader")
		}
		entries, listErr := store.List(ctx)
		if listErr != nil {
			t.Fatalf("list: %v", listErr)
		}
		if len(entries) != 0 {
			t.Fatalf("中断的上传不得留下内容，got %+v", entries)
		}
	})
}

type failingReader struct{ reads int }

func (f *failingReader) Read(p []byte) (int, error) {
	f.reads++
	if f.reads == 1 {
		copy(p, "partial content")
		return len("partial content"), nil
	}
	return 0, errors.New("simulated upload failure")
}
