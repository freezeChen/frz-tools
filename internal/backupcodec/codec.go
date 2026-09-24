// Package backupcodec 是备份的**传输编码**：把逻辑备份流压成、必要时加密成要落盘的
// 字节，并能反过来还原。
//
// 它在应用层，不下放给 BackupAdapter：把加密交给每个适配器，等于让每种数据库各实现
// 一套，任何一处写错都是静默的数据泄露或静默的不可恢复（迭代 2 规格 D1）。
//
// 编码顺序是「先压缩再加密」——反过来毫无意义，密文不可压。
//
// 流格式（自描述，读侧不需要外部告诉它这份备份加没加密）：
//
//	header: "FRZBC001" | flags(1 字节：bit0 = 已加密)
//	之后：
//	  未加密 → 压缩后的字节原样跟到底
//	  已加密 → 若干个分块 [nonce(12) | u32be(密文长度) | 密文]
//
// 每个分块用独立的随机 nonce，并把**分块序号作为附加认证数据**：这样重排或删除中间
// 分块都会被 GCM 拒绝，而不是悄悄解出一段错位的内容。
//
// 已加密的流以一个**零长度明文的分块**收尾。它不是多余的：没有它的话，在分块边界上
// 被截断的流看起来就是一次正常的 EOF，而「悄悄少了一段数据」正是备份最坏的一种失败。
package backupcodec

import (
	"compress/gzip"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"

	v1 "github.com/freezeChen/frz-tools/api/v1"
	"github.com/freezeChen/frz-tools/internal/domain"
)

const (
	// ChunkSize 是加密分块的明文大小。64 KiB 在内存占用与分块开销之间取平衡。
	ChunkSize = 64 * 1024
	// magic 是流头标识。它让「拿未加密的流去解密」这类误用立刻失败，
	// 而不是解出一堆垃圾再说。
	magic = "FRZBC001"
	// nonceSize / tagSize 是 AES-GCM 的固定开销。
	nonceSize = 12
	tagSize   = 16
	// keyIDBytes 是密钥标识的字节数。
	keyIDBytes = 8

	flagEncrypted = 1 << 0
)

// 本包的错误都是内部一致性问题，不是调用方的输入问题：加密流读不开意味着
// 「密钥不对」或「内容被改过」，两者都不该被当成普通校验失败静默放过。
var (
	ErrNotEncrypted      = errors.New("backup stream is not encrypted")
	ErrEncryptedMismatch = errors.New("backup stream is encrypted but no key was provided")
	ErrTruncated         = errors.New("backup stream is truncated")
	ErrBadMagic          = errors.New("backup stream has an unknown header")
)

// KeyID 返回密钥材料的标识：密钥摘要的前 8 字节，十六进制。
//
// 它是**标识**而不是密钥——用来回答「这个备份是哪把钥匙加的」，不足以还原密钥。
// 将来轮换密钥时，这是唯一能说明「哪些备份还能用新钥匙打开」的依据。
func KeyID(material []byte) string {
	sum := sha256.Sum256(material)
	return hex.EncodeToString(sum[:keyIDBytes])
}

// deriveKey 把任意长度的密钥材料转成 AES-256 的 32 字节密钥。
//
// 用 sha256 而不是直接截断/填充：截断会丢掉熵，填充会引入可预测的字节。
func deriveKey(material []byte) []byte {
	sum := sha256.Sum256(material)
	return sum[:]
}

// NewWriter 返回一个把明文写成编码字节的 Writer。
// compression 为 none 时不压缩；key 为 nil 时不加密。
func NewWriter(dst io.Writer, compression domain.Compression, key []byte) (io.WriteCloser, error) {
	switch compression {
	case domain.CompressionNone, domain.CompressionGzip:
	default:
		return nil, domain.NewError(v1.CodeManifestInvalid, "不支持的压缩方式 %q", compression)
	}

	flags := byte(0)
	if key != nil {
		flags |= flagEncrypted
	}
	if _, err := dst.Write([]byte(magic)); err != nil {
		return nil, domain.NewError(v1.CodeInternal, "写入备份流头失败: %v", err)
	}
	if _, err := dst.Write([]byte{flags}); err != nil {
		return nil, domain.NewError(v1.CodeInternal, "写入备份流头失败: %v", err)
	}

	sink := dst
	var framer *frameWriter
	if key != nil {
		aead, err := newAEAD(key)
		if err != nil {
			return nil, err
		}
		// 必须预分配容量：Write 里会做 buf[len(buf):ChunkSize] 这样的切片，
		// cap 为 0 的 nil 切片上会直接 panic。
		framer = &frameWriter{dst: dst, aead: aead, buf: make([]byte, 0, ChunkSize)}
		sink = framer
	}

	if compression == domain.CompressionGzip {
		zw := gzip.NewWriter(sink)
		return &encoder{dst: dst, gzip: zw, framer: framer}, nil
	}
	return &encoder{dst: dst, framer: framer}, nil
}

// NewReader 返回一个把编码字节还原成明文的 Reader。
// key 为 nil 表示调用方没有密钥；此时若流是加密的，第一次读取就会失败。
func NewReader(src io.Reader) (io.Reader, error) {
	header := make([]byte, len(magic)+1)
	if _, err := io.ReadFull(src, header); err != nil {
		if errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, io.EOF) {
			return nil, fmt.Errorf("%w: 流头不完整", ErrTruncated)
		}
		return nil, err
	}
	if string(header[:len(magic)]) != magic {
		return nil, fmt.Errorf("%w: %q", ErrBadMagic, string(header[:len(magic)]))
	}

	if header[len(magic)]&flagEncrypted == 0 {
		// 未加密：剩下的就是压缩字节（或裸字节），交给上层按 compression 解。
		return &decoderSource{src: src, encrypted: false}, nil
	}
	return &decoderSource{src: src, encrypted: true}, nil
}

// Decode 在 NewReader 的基础上，按这份备份声明的压缩方式与密钥把明文解出来。
// key 为 nil 而流是加密的时返回 ErrEncryptedMismatch——**绝不**降级成「当作明文处理」。
func Decode(src io.Reader, compression domain.Compression, key []byte) (io.Reader, error) {
	source, err := NewReader(src)
	if err != nil {
		return nil, err
	}
	source, err = decryptIfNeeded(source, key)
	if err != nil {
		return nil, err
	}
	if compression == domain.CompressionGzip {
		zr, err := gzip.NewReader(source)
		if err != nil {
			return nil, domain.NewError(v1.CodeInternal, "备份流的压缩段无法解析: %v", err)
		}
		return zr, nil
	}
	return source, nil
}

func decryptIfNeeded(src io.Reader, key []byte) (io.Reader, error) {
	source, ok := src.(*decoderSource)
	if !ok {
		return src, nil
	}
	if !source.encrypted {
		return source, nil
	}
	if key == nil {
		return nil, ErrEncryptedMismatch
	}
	aead, err := newAEAD(key)
	if err != nil {
		return nil, err
	}
	return &frameReader{src: source.src, aead: aead}, nil
}

func newAEAD(key []byte) (cipher.AEAD, error) {
	block, err := aes.NewCipher(deriveKey(key))
	if err != nil {
		return nil, domain.NewError(v1.CodeInternal, "构造加密器失败: %v", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, domain.NewError(v1.CodeInternal, "构造加密器失败: %v", err)
	}
	return aead, nil
}

// encoder 把「gzip 写入」与「分块加密」的收尾按正确顺序串起来：
// 必须先关 gzip（它要把最后一段压缩数据刷进加密层），再关加密层（写收尾分块）。
type encoder struct {
	// dst 是既没压缩也没加密时的去向。不带着它的话，那一种组合会走到 nil 的
	// framer 上——而「不压缩不加密」正是策略里 compression=none 时的正常配置。
	dst    io.Writer
	gzip   *gzip.Writer
	framer *frameWriter
}

func (e *encoder) Write(p []byte) (int, error) {
	switch {
	case e.gzip != nil:
		return e.gzip.Write(p)
	case e.framer != nil:
		return e.framer.Write(p)
	default:
		return e.dst.Write(p)
	}
}

func (e *encoder) Close() error {
	if e.gzip != nil {
		if err := e.gzip.Close(); err != nil {
			return err
		}
	}
	if e.framer != nil {
		return e.framer.Close()
	}
	return nil
}

// frameWriter 把明文按 ChunkSize 切块、逐块加密后写出。
type frameWriter struct {
	dst  io.Writer
	aead cipher.AEAD

	buf   []byte
	index int
	// closed 防止重复写收尾块：收尾块出现两次会让读侧以为数据比实际多。
	closed bool
}

func (f *frameWriter) Write(p []byte) (int, error) {
	written := 0
	for len(p) > 0 {
		if len(f.buf) == ChunkSize {
			if err := f.flush(false); err != nil {
				return written, err
			}
		}
		n := copy(f.buf[len(f.buf):ChunkSize], p)
		f.buf = f.buf[:len(f.buf)+n]
		p = p[n:]
		written += n
	}
	return written, nil
}

func (f *frameWriter) Close() error {
	if f.closed {
		return nil
	}
	f.closed = true
	return f.flush(true)
}

// flush 写出当前缓冲的分块；final 为真时再补一个零长度收尾块。
//
// 数据块只在**真的有数据**时才写：否则空内容的流会先写一个「零长度数据块」、
// 再写收尾块，而两者在字节上完全一样，读侧会把第一个当成终止标记，
// 于是「收尾块之后还有数据」——这正是这个 bug 当初的表现。
func (f *frameWriter) flush(final bool) error {
	if len(f.buf) > 0 {
		if err := f.writeChunk(f.buf); err != nil {
			return err
		}
		f.buf = f.buf[:0]
	}
	if final {
		// 零长度明文的分块作为终止标记，让「在分块边界上被截断」可被发现。
		if err := f.writeChunk(nil); err != nil {
			return err
		}
	}
	return nil
}

func (f *frameWriter) writeChunk(plaintext []byte) error {
	nonce := make([]byte, nonceSize)
	if _, err := rand.Read(nonce); err != nil {
		return domain.NewError(v1.CodeInternal, "生成加密随机数失败: %v", err)
	}
	// 分块序号进附加认证数据：重排、删除中间分块都会被拒绝。
	aad := chunkAAD(f.index)
	f.index++

	sealed := f.aead.Seal(nil, nonce, plaintext, aad)
	if _, err := f.dst.Write(nonce); err != nil {
		return err
	}
	var length [4]byte
	binary.BigEndian.PutUint32(length[:], uint32(len(sealed)))
	if _, err := f.dst.Write(length[:]); err != nil {
		return err
	}
	_, err := f.dst.Write(sealed)
	return err
}

// frameReader 逐块解密，并要求**读到零长度收尾块**才算正常结束。
type frameReader struct {
	src  io.Reader
	aead cipher.AEAD

	pending []byte
	index   int
	done    bool
}

func (f *frameReader) Read(p []byte) (int, error) {
	if len(f.pending) == 0 {
		if f.done {
			return 0, io.EOF
		}
		if err := f.nextChunk(); err != nil {
			return 0, err
		}
	}
	n := copy(p, f.pending)
	f.pending = f.pending[n:]
	return n, nil
}

func (f *frameReader) nextChunk() error {
	header := make([]byte, nonceSize+4)
	if _, err := io.ReadFull(f.src, header); err != nil {
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			// 还没读到收尾块就结束了：流在分块边界上被截断。
			return fmt.Errorf("%w: 缺少收尾分块", ErrTruncated)
		}
		return err
	}

	nonce := header[:nonceSize]
	size := binary.BigEndian.Uint32(header[nonceSize:])
	if size < tagSize {
		return fmt.Errorf("%w: 分块长度 %d 小于认证标签", ErrTruncated, size)
	}

	sealed := make([]byte, size)
	if _, err := io.ReadFull(f.src, sealed); err != nil {
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			return fmt.Errorf("%w: 分块内容不完整", ErrTruncated)
		}
		return err
	}

	aad := chunkAAD(f.index)
	f.index++

	plaintext, err := f.aead.Open(nil, nonce, sealed, aad)
	if err != nil {
		// 解密失败要么是密钥不对，要么是内容被改过。两者都不该被当成「无关紧要的
		// 校验失败」——它意味着这份备份的字节已经不是当初写下的那些。
		return domain.NewError(v1.CodeBackupVerifyFailed,
			"备份流解密失败（密钥不匹配或内容被改动）: %v", err)
	}

	if len(plaintext) == 0 {
		// 收尾块：之后必须就是 EOF，多出来的字节说明流被追加过。
		var extra [1]byte
		if n, err := f.src.Read(extra[:]); n > 0 {
			return domain.NewError(v1.CodeBackupVerifyFailed, "收尾分块之后仍有数据")
		} else if err != nil && !errors.Is(err, io.EOF) {
			return err
		}
		f.done = true
		return nil
	}
	f.pending = plaintext
	return nil
}

// decoderSource 暴露「流加没加密」，供 decryptIfNeeded 决定怎么解。
type decoderSource struct {
	src       io.Reader
	encrypted bool
}

func (d *decoderSource) Read(p []byte) (int, error) { return d.src.Read(p) }

func chunkAAD(index int) []byte {
	var aad [8]byte
	binary.BigEndian.PutUint64(aad[:], uint64(index))
	return aad[:]
}
