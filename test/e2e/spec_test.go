package e2e

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	v1 "github.com/freezeChen/frz-tools/api/v1"
)

func decodeSpec(t *testing.T, raw string) v1.ApplicationSpec {
	t.Helper()
	var spec v1.ApplicationSpec
	if err := json.Unmarshal([]byte(raw), &spec); err != nil {
		t.Fatalf("cannot decode spec from %q: %v", raw, err)
	}
	return spec
}

// billingManifest 与 internal/adapters/manifest 的冻结格式一致。artifactID 由调用方
// 填入刚上传的制品，因此 unpack 策略会按制品的 mediaType 推断。
func billingManifest(artifactID string) string {
	return fmt.Sprintf(`apiVersion: ops.frz.io/v1alpha1
kind: ApplicationSpec
application: billing-api
runtime: go
artifact:
  id: %s
exec:
  argv: [/opt/billing-api/bin/billing-api, --config, /etc/billing-api/config.yaml]
  workingDirectory: /var/lib/billing-api
  runUser: billing-api
  environment:
    GOMEMLIMIT: 40MiB
  secretEnvironment:
    DB_PASSWORD:
      kind: env
      name: billing_db_password
  ports: [8080]
health:
  readiness:
    type: tcp
    target: "127.0.0.1:8080"
    consecutiveSuccesses: 2
  startTimeoutSeconds: 90
logs:
  directory: /var/log/billing-api
`, artifactID)
}

// uploadBillingArtifact 上传一份制品并返回它的元数据。
func uploadBillingArtifact(t *testing.T, d *daemon, dir string) v1.Artifact {
	t.Helper()

	path := writeArtifactFile(t, dir, "billing-api.tar.gz", "billing-api-e2e-tarball")
	stdout, _, err := runOpsctl(t, d.socket, "artifact", "put", path, "--media-type", "application/gzip", "--json")
	if err != nil {
		t.Fatalf("put artifact: %v", err)
	}
	return decodeArtifact(t, stdout)
}

// 一条真实链路：真实 opsd + 真实 opsctl，从提交 manifest 到读回。
func TestSpecPutShowRoundTripThroughCLI(t *testing.T) {
	d := newDaemon(t)
	d.start(t)

	if _, _, err := runOpsctl(t, d.socket, "app", "create", "billing-api"); err != nil {
		t.Fatalf("create application: %v", err)
	}
	dir := t.TempDir()
	artifact := uploadBillingArtifact(t, d, dir)

	manifestPath := writeArtifactFile(t, dir, "billing-api.yaml", billingManifest(artifact.ID))
	if _, _, err := runOpsctl(t, d.socket, "spec", "put", "--app", "billing-api", "--file", manifestPath); err != nil {
		t.Fatalf("put spec: %v", err)
	}

	stdout, _, err := runOpsctl(t, d.socket, "spec", "show", "--app", "billing-api", "--json")
	if err != nil {
		t.Fatalf("show spec: %v", err)
	}
	spec := decodeSpec(t, stdout)

	if spec.APIVersion != v1.APIVersion || spec.Kind != "ApplicationSpec" {
		t.Fatalf("信封字段不符：%+v", spec)
	}
	if spec.Application != "billing-api" || spec.Runtime != "go" {
		t.Fatalf("application/runtime 与提交内容不一致：%+v", spec)
	}
	if spec.Artifact.ID != artifact.ID {
		t.Fatalf("artifact 与提交内容不一致：%s != %s", spec.Artifact.ID, artifact.ID)
	}
	// 未显式声明 unpack，按 mediaType（application/gzip）推断为 tar-gz。
	if spec.Artifact.Unpack.Strategy != "tar-gz" {
		t.Fatalf("unpack.strategy want tar-gz, got %q", spec.Artifact.Unpack.Strategy)
	}
	if len(spec.Exec.Argv) != 3 || spec.Exec.Argv[0] != "/opt/billing-api/bin/billing-api" {
		t.Fatalf("argv 与提交内容不一致：%+v", spec.Exec.Argv)
	}
	if spec.Exec.RunUser != "billing-api" || spec.Exec.WorkingDirectory != "/var/lib/billing-api" {
		t.Fatalf("exec 段与提交内容不一致：%+v", spec.Exec)
	}
	if spec.Exec.Environment["GOMEMLIMIT"] != "40MiB" {
		t.Fatalf("environment 与提交内容不一致：%+v", spec.Exec.Environment)
	}
	// 凭据只保留引用与交付方式；值从不经过 API。
	if ref := spec.Exec.SecretEnvironment["DB_PASSWORD"]; ref.Kind != "env" || ref.Name != "billing_db_password" {
		t.Fatalf("secretEnvironment 与提交内容不一致：%+v", spec.Exec.SecretEnvironment)
	}
	if spec.Health.Readiness.Type != "tcp" || spec.Health.Readiness.Target != "127.0.0.1:8080" {
		t.Fatalf("readiness 与提交内容不一致：%+v", spec.Health.Readiness)
	}
	if spec.Health.Readiness.ConsecutiveSuccesses != 2 || spec.Health.StartTimeoutSeconds != 90 {
		t.Fatalf("健康检查参数与提交内容不一致：%+v", spec.Health)
	}
	// 未声明的字段由默认值补齐：这是「同一份 manifest 在任何入口都得到同一结果」的一部分。
	if spec.Health.StopTimeoutSeconds != 30 {
		t.Fatalf("stopTimeoutSeconds 默认值应为 30，got %d", spec.Health.StopTimeoutSeconds)
	}
	if spec.Systemd.UnitName != "billing-api.service" || spec.Systemd.RestartPolicy != "on-failure" {
		t.Fatalf("systemd 默认值不符：%+v", spec.Systemd)
	}
	if spec.Logs.Directory != "/var/log/billing-api" {
		t.Fatalf("logs.directory 与提交内容不一致：%q", spec.Logs.Directory)
	}

	// 人类可读输出同样要指向同一个应用与 unit。
	human, _, err := runOpsctl(t, d.socket, "spec", "show", "--app", "billing-api")
	if err != nil {
		t.Fatalf("show spec (human): %v", err)
	}
	if !strings.Contains(human, "application: billing-api") || !strings.Contains(human, "unit:        billing-api.service") {
		t.Fatalf("人类可读输出不完整：%s", human)
	}
}

func TestSpecPutRejectsInvalidManifestThroughCLI(t *testing.T) {
	d := newDaemon(t)
	d.start(t)

	if _, _, err := runOpsctl(t, d.socket, "app", "create", "billing-api"); err != nil {
		t.Fatalf("create application: %v", err)
	}
	dir := t.TempDir()

	// 相对 argv[0] 用 .. 逃出 release 目录：MANIFEST_INVALID → 退出码 17。
	// （相对 argv 本身是合法的（迭代 3 规格 D4），逃出才是错误。）
	invalid := `apiVersion: ops.frz.io/v1alpha1
kind: ApplicationSpec
application: billing-api
runtime: go
artifact:
  id: art_whatever
exec:
  argv: [bin/../../etc/shadow]
  workingDirectory: /var/lib/billing-api
  runUser: billing-api
logs:
  directory: /var/log/billing-api
health:
  readiness:
    type: tcp
    target: "127.0.0.1:8080"
`
	invalidPath := writeArtifactFile(t, dir, "invalid.yaml", invalid)
	if _, code, err := runOpsctl(t, d.socket, "spec", "put", "--app", "billing-api", "--file", invalidPath); code != 17 {
		t.Fatalf("want exit 17 (MANIFEST_INVALID), got %d: %v", code, err)
	}

	// 未知字段同样必须被严格解码拦住。
	unknown := billingManifest("art_whatever") + "  unexpected: 1\n"
	unknownPath := writeArtifactFile(t, dir, "unknown.yaml", unknown)
	if _, code, err := runOpsctl(t, d.socket, "spec", "put", "--app", "billing-api", "--file", unknownPath); code != 17 {
		t.Fatalf("want exit 17 (MANIFEST_INVALID), got %d: %v", code, err)
	}

	// 非法提交不得留下任何副作用。
	if _, code, err := runOpsctl(t, d.socket, "spec", "show", "--app", "billing-api"); code != 2 {
		t.Fatalf("非法 manifest 不应落库，want exit 2 (SPEC_NOT_FOUND), got %d: %v", code, err)
	}

	// unpack 策略与制品 mediaType 冲突：MANIFEST_CONFLICT → 退出码 18。
	artifact := uploadBillingArtifact(t, d, dir)
	conflicting := fmt.Sprintf(`apiVersion: ops.frz.io/v1alpha1
kind: ApplicationSpec
application: billing-api
runtime: go
artifact:
  id: %s
  unpack:
    strategy: zip
exec:
  argv: [/opt/billing-api/bin/billing-api]
  workingDirectory: /var/lib/billing-api
  runUser: billing-api
health:
  readiness:
    type: tcp
    target: "127.0.0.1:8080"
logs:
  directory: /var/log/billing-api
`, artifact.ID)
	conflictPath := writeArtifactFile(t, dir, "conflict.yaml", conflicting)
	if _, code, err := runOpsctl(t, d.socket, "spec", "put", "--app", "billing-api", "--file", conflictPath); code != 18 {
		t.Fatalf("want exit 18 (MANIFEST_CONFLICT), got %d: %v", code, err)
	}

	// 应用不存在时是 APPLICATION_NOT_FOUND → 退出码 2。
	manifestPath := writeArtifactFile(t, dir, "billing-api.yaml", billingManifest(artifact.ID))
	if _, code, err := runOpsctl(t, d.socket, "spec", "put", "--app", "no-such-app", "--file", manifestPath); code != 2 {
		t.Fatalf("want exit 2 (APPLICATION_NOT_FOUND), got %d: %v", code, err)
	}
}

func TestArtifactDownloadThroughCLI(t *testing.T) {
	d := newDaemon(t)
	d.start(t)

	content := "artifact-download-e2e-payload"
	dir := t.TempDir()
	uploadedPath := writeArtifactFile(t, dir, "app.tar.gz", content)
	stdout, _, err := runOpsctl(t, d.socket, "artifact", "put", uploadedPath, "--media-type", "application/gzip", "--json")
	if err != nil {
		t.Fatalf("put: %v", err)
	}
	artifact := decodeArtifact(t, stdout)

	// 用 ID 与摘要两种引用各下载一次，落盘字节必须与上传内容逐字节一致。
	for _, ref := range []string{artifact.ID, artifact.Digest} {
		dest := filepath.Join(dir, "downloaded-"+strings.TrimPrefix(ref, "sha256:")+".tar.gz")
		if _, _, err := runOpsctl(t, d.socket, "artifact", "download", ref, "--output", dest); err != nil {
			t.Fatalf("download %s: %v", ref, err)
		}
		downloaded, err := os.ReadFile(dest)
		if err != nil {
			t.Fatalf("read downloaded file: %v", err)
		}
		if string(downloaded) != content {
			t.Fatalf("下载内容与上传内容不一致：%q != %q", string(downloaded), content)
		}
		// 下载下来的字节自身必须能算出记录的摘要。
		if digest := digestOfBytes(downloaded); digest != artifact.Digest {
			t.Fatalf("下载内容的摘要 %s 与记录 %s 不一致", digest, artifact.Digest)
		}
	}

	// 改坏存储里的 blob：摘要校验必须让下载失败（ARTIFACT_CHECKSUM_MISMATCH → 退出码 5），
	// 而不是把损坏的内容当成功写到目标路径。
	blob := blobPathOf(t, d.artifactsDir, artifact.Digest)
	if err := os.WriteFile(blob, []byte("tampered-artifact-bytes-x"), 0o640); err != nil {
		t.Fatalf("tamper with blob: %v", err)
	}
	brokenDest := filepath.Join(dir, "broken.tar.gz")
	if _, code, err := runOpsctl(t, d.socket, "artifact", "download", artifact.ID, "--output", brokenDest); code != 5 {
		t.Fatalf("want exit 5 (ARTIFACT_CHECKSUM_MISMATCH), got %d: %v", code, err)
	}
	if _, err := os.Stat(brokenDest); !os.IsNotExist(err) {
		t.Fatalf("失败的下载不应留下目标文件，stat err=%v", err)
	}
}

func TestHostAndEnvironmentCommandsThroughCLI(t *testing.T) {
	d := newDaemon(t)
	d.start(t)

	// 本机 Host 由 opsd 启动时自举，地址为空表示本机。
	stdout, _, err := runOpsctl(t, d.socket, "host", "list", "--json")
	if err != nil {
		t.Fatalf("host list: %v", err)
	}
	var hosts v1.HostListResponse
	if err := json.Unmarshal([]byte(stdout), &hosts); err != nil {
		t.Fatalf("decode host list from %q: %v", stdout, err)
	}
	if len(hosts.Items) != 1 || hosts.Items[0].Name != "local" || hosts.Items[0].Address != "" {
		t.Fatalf("want the bootstrapped local host, got %+v", hosts.Items)
	}

	if _, _, err := runOpsctl(t, d.socket, "host", "create", "web-01", "--address", "10.0.0.11", "--label", "role=web"); err != nil {
		t.Fatalf("host create: %v", err)
	}
	inspect, _, err := runOpsctl(t, d.socket, "host", "inspect", "web-01", "--json")
	if err != nil {
		t.Fatalf("host inspect: %v", err)
	}
	// 单对象命令与既有的 app/artifact inspect 一致：--json 直接输出对象本身，
	// 而列表命令输出带 apiVersion 的信封。
	var host v1.Host
	if err := json.Unmarshal([]byte(inspect), &host); err != nil {
		t.Fatalf("decode host from %q: %v", inspect, err)
	}
	if host.Address != "10.0.0.11" || host.Labels["role"] != "web" {
		t.Fatalf("unexpected host: %+v", host)
	}
	if _, code, err := runOpsctl(t, d.socket, "host", "inspect", "no-such-host"); code != 2 {
		t.Fatalf("want exit 2 (HOST_NOT_FOUND), got %d: %v", code, err)
	}

	if _, _, err := runOpsctl(t, d.socket, "env", "create", "production", "--label", "tier=prod"); err != nil {
		t.Fatalf("env create: %v", err)
	}
	envList, _, err := runOpsctl(t, d.socket, "env", "list", "--json")
	if err != nil {
		t.Fatalf("env list: %v", err)
	}
	var environments v1.EnvironmentListResponse
	if err := json.Unmarshal([]byte(envList), &environments); err != nil {
		t.Fatalf("decode environment list from %q: %v", envList, err)
	}
	if len(environments.Items) != 1 || environments.Items[0].Name != "production" {
		t.Fatalf("unexpected environments: %+v", environments.Items)
	}
	if _, code, err := runOpsctl(t, d.socket, "env", "inspect", "no-such-env"); code != 2 {
		t.Fatalf("want exit 2 (ENVIRONMENT_NOT_FOUND), got %d: %v", code, err)
	}
}

// blobPathOf 复刻 blob 存储的路径约定：<root>/blobs/sha256/ab/cd/<hex>。
func blobPathOf(t *testing.T, root, digest string) string {
	t.Helper()

	hexPart := strings.TrimPrefix(digest, "sha256:")
	if len(hexPart) < 4 {
		t.Fatalf("unexpected digest %q", digest)
	}
	return filepath.Join(root, "blobs", "sha256", hexPart[0:2], hexPart[2:4], hexPart)
}

func digestOfBytes(content []byte) string {
	sum := sha256.Sum256(content)
	return "sha256:" + hex.EncodeToString(sum[:])
}
