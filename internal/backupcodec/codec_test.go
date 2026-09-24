package backupcodec

import (
	"bytes"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/freezeChen/frz-tools/internal/domain"
)

var testKey = []byte("a-test-key-material")

// encode 用给定的压缩与密钥把 plaintext 编成字节。
func encode(t *testing.T, plaintext []byte, compression domain.Compression, key []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	writer, err := NewWriter(&buf, compression, key)
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}
	if _, err := writer.Write(plaintext); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	return buf.Bytes()
}

// decode 把编码字节还原成明文。
func decode(t *testing.T, encoded []byte, compression domain.Compression, key []byte) ([]byte, error) {
	t.Helper()
	reader, err := Decode(bytes.NewReader(encoded), compression, key)
	if err != nil {
		return nil, err
	}
	return io.ReadAll(reader)
}

func TestRoundTrip(t *testing.T) {
	// 超过一个分块、以及恰好一个分块、空内容，三种长度都过一遍：
	// 分块边界是这类流最容易写错的地方。
	payloads := map[string][]byte{
		"空内容":  {},
		"一小段":  []byte("hello 备份"),
		"恰好一块": bytes.Repeat([]byte("x"), ChunkSize),
		"跨多块":  bytes.Repeat([]byte("0123456789abcdef"), ChunkSize),
	}

	combinations := []struct {
		name        string
		compression domain.Compression
		key         []byte
	}{
		{"不压缩不加密", domain.CompressionNone, nil},
		{"只压缩", domain.CompressionGzip, nil},
		{"只加密", domain.CompressionNone, testKey},
		{"压缩并加密", domain.CompressionGzip, testKey},
	}

	for _, combo := range combinations {
		for name, payload := range payloads {
			t.Run(combo.name+"/"+name, func(t *testing.T) {
				encoded := encode(t, payload, combo.compression, combo.key)
				got, err := decode(t, encoded, combo.compression, combo.key)
				if err != nil {
					t.Fatalf("decode: %v", err)
				}
				if !bytes.Equal(got, payload) {
					t.Fatalf("往返不一致：原始 %d 字节，还原 %d 字节", len(payload), len(got))
				}
			})
		}
	}
}

func TestCompressionActuallyCompresses(t *testing.T) {
	payload := bytes.Repeat([]byte("compressible "), ChunkSize)
	plain := encode(t, payload, domain.CompressionNone, nil)
	compressed := encode(t, payload, domain.CompressionGzip, nil)
	if len(compressed) >= len(plain) {
		t.Fatalf("压缩后不该更大：%d vs %d", len(compressed), len(plain))
	}
}

// 加密必须真的改变字节：否则「默认加密」只是一句口号。
func TestEncryptionActuallyEncrypts(t *testing.T) {
	payload := []byte("secret-ish content")
	plain := encode(t, payload, domain.CompressionNone, nil)
	encrypted := encode(t, payload, domain.CompressionNone, testKey)

	if bytes.Contains(encrypted, payload) {
		t.Fatal("密文里出现了明文")
	}
	if bytes.Equal(plain, encrypted) {
		t.Fatal("加密后的字节不该与未加密的相同")
	}
}

func TestWrongKeyFails(t *testing.T) {
	encoded := encode(t, []byte("payload"), domain.CompressionGzip, testKey)
	if _, err := decode(t, encoded, domain.CompressionGzip, []byte("another-key")); err == nil {
		t.Fatal("换一把密钥必须解不开")
	}
}

// 没有密钥时**绝不能**降级成「当作明文处理」：那会让一份加密备份被静默地
// 当成损坏或不完整，而调用方以为只是读了个空。
func TestEncryptedStreamWithoutKeyFails(t *testing.T) {
	encoded := encode(t, []byte("payload"), domain.CompressionNone, testKey)
	_, err := decode(t, encoded, domain.CompressionNone, nil)
	if !errors.Is(err, ErrEncryptedMismatch) {
		t.Fatalf("want ErrEncryptedMismatch, got %v", err)
	}
}

// 在分块边界上截断是最隐蔽的一种损坏：读起来就是一次正常的 EOF。
// 收尾分块就是为了让它可被发现。
func TestTruncationAtChunkBoundaryFails(t *testing.T) {
	payload := bytes.Repeat([]byte("y"), ChunkSize*2)
	encoded := encode(t, payload, domain.CompressionNone, testKey)

	// 手工构造「去掉最后一个完整分块（含收尾块）」的流：只有把它切在
	// 收尾分块之前，才是在测「边界截断」而不是「半个分块」。
	truncated := encoded[:len(encoded)-(nonceSize+4+tagSize)]
	if _, err := decode(t, truncated, domain.CompressionNone, testKey); !errors.Is(err, ErrTruncated) {
		t.Fatalf("want ErrTruncated, got %v", err)
	}
}

func TestTruncationMidChunkFails(t *testing.T) {
	payload := bytes.Repeat([]byte("z"), ChunkSize)
	encoded := encode(t, payload, domain.CompressionNone, testKey)
	if _, err := decode(t, encoded[:len(encoded)/2], domain.CompressionNone, testKey); err == nil {
		t.Fatal("半个分块必须失败")
	}
}

func TestTamperedCiphertextFails(t *testing.T) {
	encoded := encode(t, []byte("payload-that-is-long-enough"), domain.CompressionNone, testKey)
	// 改掉密文中间的一个字节。GCM 必须发现它，而不是解出一段错位的内容。
	encoded[len(encoded)-1] ^= 0xff
	if _, err := decode(t, encoded, domain.CompressionNone, testKey); err == nil {
		t.Fatal("被改过的密文必须失败")
	}
}

// 分块序号进了附加认证数据，因此交换两个分块必须失败——
// 否则攻击者可以在不解开内容的前提下重排数据。
func TestChunkReorderingFails(t *testing.T) {
	// 未压缩时每个分块的内容可以直接对调位置。
	payload := bytes.Repeat([]byte("a"), ChunkSize)
	payload = append(payload, bytes.Repeat([]byte("b"), ChunkSize)...)
	encoded := encode(t, payload, domain.CompressionNone, testKey)

	frameSize := nonceSize + 4 + ChunkSize + tagSize
	first := encoded[len(encoded)-2*frameSize : len(encoded)-frameSize]
	second := encoded[len(encoded)-frameSize:]
	swapped := append([]byte{}, encoded[:len(encoded)-2*frameSize]...)
	swapped = append(swapped, second...)
	swapped = append(swapped, first...)

	if _, err := decode(t, swapped, domain.CompressionNone, testKey); err == nil {
		t.Fatal("分块被重排必须失败")
	}
}

func TestBadMagicFails(t *testing.T) {
	if _, err := NewReader(strings.NewReader("not-a-backup-stream")); !errors.Is(err, ErrBadMagic) {
		t.Fatalf("want ErrBadMagic, got %v", err)
	}
}

func TestEmptyStreamFails(t *testing.T) {
	if _, err := NewReader(bytes.NewReader(nil)); !errors.Is(err, ErrTruncated) {
		t.Fatalf("want ErrTruncated, got %v", err)
	}
}

// 密钥标识必须稳定、且不是密钥本身：它是「用哪把钥匙」的标签，不是钥匙。
func TestKeyID(t *testing.T) {
	first := KeyID(testKey)
	if first != KeyID(testKey) {
		t.Fatal("同一份密钥材料的标识必须稳定")
	}
	if first == KeyID([]byte("another-key")) {
		t.Fatal("不同密钥必须有不同的标识")
	}
	if strings.Contains(first, string(testKey)) {
		t.Fatal("标识里不该出现密钥本身")
	}
}

// 收尾分块之后不该再有数据：被追加过的流要能被发现。
func TestTrailingDataAfterTerminatorFails(t *testing.T) {
	encoded := encode(t, []byte("payload"), domain.CompressionNone, testKey)
	appended := append(append([]byte{}, encoded...), []byte("extra-bytes-after-terminator")...)
	if _, err := decode(t, appended, domain.CompressionNone, testKey); err == nil {
		t.Fatal("收尾分块之后的数据必须被发现")
	}
}

// 不支持的压缩方式要在构造 Writer 时就拒绝，而不是写出一段没人能解的流。
func TestNewWriterRejectsUnknownCompression(t *testing.T) {
	var buf bytes.Buffer
	if _, err := NewWriter(&buf, domain.Compression("zstd"), nil); err == nil {
		t.Fatal("未知压缩方式必须被拒绝")
	}
}
