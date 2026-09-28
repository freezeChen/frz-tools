package e2e

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	v1 "github.com/freezeChen/frz-tools/api/v1"
)

// batchManifest 造一份**跨主机成立**的 manifest：制品按 digest 引用。
//
// 这正是批量部署对 manifest 的硬要求（迭代 5b 规格 D3）：art_xxx 是某一台机上的那一行，
// 换一台机要么找不到、要么指向别的东西；digest 是内容寻址的，在每台机上指同一份字节。
func batchManifest(app, digest string) string {
	return fmt.Sprintf(`apiVersion: ops.frz.io/v1alpha1
kind: ApplicationSpec
application: %s
runtime: go
artifact:
  digest: %s
exec:
  argv: [bin/%s]
  workingDirectory: /var/lib/%s
  runUser: %s
logs:
  directory: /var/log/%s
health:
  readiness:
    type: tcp
    target: "127.0.0.1:28080"
`, app, digest, app, app, app, app)
}

// batchReport 是批量部署 --json 输出的那部分结构。
//
// 只声明用得上的字段：这个类型的存在是为了在断言里说清楚「我在看什么」，而不是
// 把 fleet 的整个报告复制一份——复制的那份迟早会与新字段脱节。
type batchReport struct {
	Action       string `json:"action"`
	Application  string `json:"application"`
	BatchID      string `json:"batchId"`
	StoppedEarly bool   `json:"stoppedEarly"`
	Succeeded    int    `json:"succeeded"`
	Failed       int    `json:"failed"`
	Skipped      int    `json:"skipped"`
	NotRun       int    `json:"notRun"`
	Results      []struct {
		Host         string `json:"host"`
		Status       string `json:"status"`
		OperationID  string `json:"operationId"`
		ErrorCode    string `json:"errorCode"`
		ErrorMessage string `json:"errorMessage"`
		Detail       string `json:"detail"`
	} `json:"results"`
}

// assertCountsAddUp 钉住那份报告的地基：四个计数加起来必须等于目标总数，
// 而且每一台都要有一个明确的结局。
func (r batchReport) assertCountsAddUp(t *testing.T) {
	t.Helper()
	if sum := r.Succeeded + r.Failed + r.Skipped + r.NotRun; sum != len(r.Results) {
		t.Fatalf("四个计数加起来不等于总数：%d != %d（%+v）", sum, len(r.Results), r.Results)
	}
	for _, result := range r.Results {
		if result.Status == "" {
			t.Fatalf("有这么一台没有结局：%+v", result)
		}
	}
}

// targetFixture 是一个控制点加若干台目标机。
type targetFixture struct {
	pki   *testPKI
	coord *daemon
	// 每台目标机：守护进程、地址、以及它自己的 mTLS 客户端参数。
	targets map[string]*targetHost
	order   []string
	dir     string
}

type targetHost struct {
	daemon  *daemon
	address string
}

// newTargetFixture 起一个控制点与 `count` 台目标机，每台都开着 mTLS 监听。
//
// 「目标机」在这里是**同一台机器上的另一个 opsd 实例**（不同的 socket、库、目录与
// 监听端口）——跨物理主机的验证没有做，这一点写进了迭代 5 文档的未验证清单。
func newTargetFixture(t *testing.T, count int, app string) *targetFixture {
	t.Helper()
	pki := newTestPKI(t)
	fixture := &targetFixture{
		pki:     pki,
		coord:   newDaemon(t),
		targets: map[string]*targetHost{},
		dir:     t.TempDir(),
	}
	fixture.coord.start(t)

	for i := 0; i < count; i++ {
		name := fmt.Sprintf("web-%d", i+1)
		d, addr := startRemoteDaemon(t, pki, `    - cn: opsctl-central
      scope: write
`)
		fixture.targets[name] = &targetHost{daemon: d, address: addr}
		fixture.order = append(fixture.order, name)

		// 控制点登记这台主机（地址就是它的 mTLS 监听地址）。
		if _, _, err := runOpsctl(t, fixture.coord.socket, "host", "create", name, "--address", addr); err != nil {
			t.Fatalf("登记主机 %s: %v", name, err)
		}
	}
	return fixture
}

// targetsArgs 是「控制点 + 打目标机用的证书」这一组公共参数。
func (f *targetFixture) args(extra ...string) []string {
	base := []string{
		"--socket", f.coord.socket,
		"--client-cert", f.pki.clientCert,
		"--client-key", f.pki.clientKey,
		"--ca-cert", f.pki.ca.certPath,
	}
	return append(base, extra...)
}

func (f *targetFixture) hostsFlag() string {
	return strings.Join(f.order, ",")
}

// prepareTarget 在一台目标机上注册应用并推好制品，返回制品的摘要。
//
// 「按 digest 引用」要成立，前提是每台机上都真的有这份字节——这正是准备阶段要确认的事。
func (f *targetFixture) prepareTarget(t *testing.T, name, app string, content []byte) string {
	t.Helper()
	target := f.targets[name]

	remote := []string{
		"--remote", target.address,
		"--client-cert", f.pki.clientCert,
		"--client-key", f.pki.clientKey,
		"--ca-cert", f.pki.ca.certPath,
	}

	if _, _, err := runOpsctlBinary(t, append(remote, "app", "create", app)...); err != nil {
		t.Fatalf("%s 上创建应用: %v", name, err)
	}

	artifactPath := filepath.Join(f.dir, app+".bin")
	if err := os.WriteFile(artifactPath, content, 0o644); err != nil {
		t.Fatalf("写制品文件: %v", err)
	}
	stdout, _, err := runOpsctlBinary(t, append(remote, "artifact", "put", artifactPath, "--json")...)
	if err != nil {
		t.Fatalf("%s 上推制品: %v", name, err)
	}
	var artifact v1.Artifact
	if err := json.Unmarshal([]byte(stdout), &artifact); err != nil {
		t.Fatalf("decode artifact from %q: %v", stdout, err)
	}
	return artifact.Digest
}

func writeBatchManifest(t *testing.T, dir, name, content string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("写 manifest: %v", err)
	}
	return path
}

// 目标登记的最小闭环，走 CLI。
func TestTargetRegistryThroughCLI(t *testing.T) {
	f := newTargetFixture(t, 2, "orders-api")

	stdout, _, err := runOpsctl(t, f.coord.socket, "app", "target", "set",
		"--app", "orders-api", "--hosts", "web-1,web-2")
	if err != nil {
		t.Fatalf("target set: %v", err)
	}
	// 替换语义：打印的必须是**替换之后**的完整列表——一次少写一台就是真的少了一台。
	for _, name := range []string{"web-1", "web-2"} {
		if !strings.Contains(stdout, name) {
			t.Fatalf("登记结果里应当有 %s：%s", name, stdout)
		}
	}

	// 写错一个主机名当场被拒，而且注册表一字未改。
	_, code, err := runOpsctl(t, f.coord.socket, "app", "target", "set",
		"--app", "orders-api", "--hosts", "web-1,typo-host")
	if code != 2 {
		t.Fatalf("登记不存在的主机 want exit 2 (HOST_NOT_FOUND), got %d (%v)", code, err)
	}
	// 报错必须指向**那个主机名**。只断言退出码会让「其实是因为别的理由失败」也算过——
	// 这条断言第一次写出来时就撞在旗标互斥上，退的也是 2。
	if err == nil || !strings.Contains(err.Error(), "typo-host") {
		t.Fatalf("报错应当指出是哪个主机名不在册：%v", err)
	}
	after, _, err := runOpsctl(t, f.coord.socket, "app", "target", "list", "--app", "orders-api", "--json")
	if err != nil {
		t.Fatalf("target list: %v", err)
	}
	// `app target list --app X --json` 打的是那**一个**应用的部署目标（与 app inspect
	// 打一个应用同形），因此直接解成 ApplicationTargets。
	var targets v1.ApplicationTargets
	if err := json.Unmarshal([]byte(after), &targets); err != nil {
		t.Fatalf("decode targets from %q: %v", after, err)
	}
	if len(targets.Hosts) != 2 {
		t.Fatalf("失败的写入动了注册表：%+v", targets)
	}

	// 跨主机汇总：一次列出全部应用的部署目标。
	if _, _, err := runOpsctl(t, f.coord.socket, "app", "target", "set",
		"--app", "billing-api", "--hosts", "web-2"); err != nil {
		t.Fatalf("target set: %v", err)
	}
	all, _, err := runOpsctl(t, f.coord.socket, "app", "target", "list", "--json")
	if err != nil {
		t.Fatalf("target list: %v", err)
	}
	var list v1.TargetsListResponse
	if err := json.Unmarshal([]byte(all), &list); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(list.Items) != 2 {
		t.Fatalf("应当有两个应用登记了目标，得到 %d", len(list.Items))
	}
}

// 准备阶段没过就**一台都不动**：这是批量发布最重要的一条——它把「静默少发一台然后
// 报成功」这种失败形态挡在动手之前。
func TestBatchDeployRefusesToStartWhenATargetIsUnreachable(t *testing.T) {
	f := newTargetFixture(t, 1, "orders-api")
	digest := f.prepareTarget(t, "web-1", "orders-api", []byte("orders-v1"))

	// 再登记一台连不上的（拿一个空闲端口，没人听）。
	deadAddr := freePort(t)
	if _, _, err := runOpsctl(t, f.coord.socket, "host", "create", "web-dead", "--address", deadAddr); err != nil {
		t.Fatalf("登记主机: %v", err)
	}

	manifest := writeBatchManifest(t, f.dir, "batch.yaml",
		batchManifest("orders-api", digest))

	before := countOperations(t, f.targets["web-1"].daemon.database, v1.KindAppDeploy)

	stdout, code, err := runOpsctlBinary(t, f.args("app", "deploy",
		"--app", "orders-api", "--file", manifest, "--hosts", "web-1,web-dead")...)
	if code != 36 {
		t.Fatalf("want exit 36 (BATCH_PREFLIGHT_FAILED), got %d：%s %v", code, stdout, err)
	}
	// 汇总必须把每一台都算进去——一台机器既不在成功里、也不在失败里是不允许的。
	if !strings.Contains(stdout, "共 2 台") {
		t.Fatalf("汇总行不对：%s", stdout)
	}

	// 可达的那台也**一个操作都没有**：这就是「一台都没动」。
	if after := countOperations(t, f.targets["web-1"].daemon.database, v1.KindAppDeploy); after != before {
		t.Fatalf("准备阶段没过时不该提交任何部署：之前 %d 条，之后 %d 条", before, after)
	}
}

// 加了 --allow-partial 才继续，且被跳过的那台要说清原因。
func TestBatchDeployAllowPartialSkipsAndContinues(t *testing.T) {
	f := newTargetFixture(t, 1, "orders-api")
	digest := f.prepareTarget(t, "web-1", "orders-api", []byte("orders-v1"))

	deadAddr := freePort(t)
	if _, _, err := runOpsctl(t, f.coord.socket, "host", "create", "web-dead", "--address", deadAddr); err != nil {
		t.Fatalf("登记主机: %v", err)
	}
	manifest := writeBatchManifest(t, f.dir, "batch.yaml", batchManifest("orders-api", digest))

	stdout, _, _ := runOpsctlBinary(t, f.args("app", "deploy", "--app", "orders-api",
		"--file", manifest, "--hosts", "web-1,web-dead", "--allow-partial", "--json")...)
	// 允许部分继续时，批次本身的结局取决于**可达的那台**跑成没跑成：非 Linux 上没有
	// runtime 适配器，因此它必然失败（退出码 37）；Linux 上则可能成功（退出码 0）。
	// 两种都对，这里断言的是**结构**而不是平台的脾气。
	var report batchReport
	if err := json.Unmarshal([]byte(stdout), &report); err != nil {
		t.Fatalf("decode report from %q: %v", stdout, err)
	}
	report.assertCountsAddUp(t)

	if len(report.Results) != 2 {
		t.Fatalf("报告里应当有 2 台，得到 %d", len(report.Results))
	}
	byHost := map[string]string{}
	detail := map[string]string{}
	for _, result := range report.Results {
		byHost[result.Host] = result.Status
		detail[result.Host] = result.Detail
	}
	if byHost["web-dead"] != "skipped" {
		t.Fatalf("不可达的那台应当是 skipped，得到 %q", byHost["web-dead"])
	}
	// 跳过的那台必须说明为什么——不然「跳过」等于没说。
	if strings.TrimSpace(detail["web-dead"]) == "" {
		t.Fatalf("跳过的那台要给出原因：%+v", report.Results)
	}
	if byHost["web-1"] == "skipped" || byHost["web-1"] == "" {
		t.Fatalf("可达的那台应当真的被尝试过，得到 %q", byHost["web-1"])
	}
}

// `--hosts @应用名` 用登记好的部署目标展开（而不是要求每次手打主机名）。
func TestBatchDeployExpandsRegisteredTargets(t *testing.T) {
	f := newTargetFixture(t, 2, "orders-api")
	digest := f.prepareTarget(t, "web-1", "orders-api", []byte("orders-v1"))
	f.prepareTarget(t, "web-2", "orders-api", []byte("orders-v1"))

	if _, _, err := runOpsctl(t, f.coord.socket, "app", "target", "set",
		"--app", "orders-api", "--hosts", "web-1,web-2"); err != nil {
		t.Fatalf("target set: %v", err)
	}
	manifest := writeBatchManifest(t, f.dir, "batch.yaml", batchManifest("orders-api", digest))

	stdout, _, _ := runOpsctlBinary(t, f.args("app", "deploy", "--app", "orders-api",
		"--file", manifest, "--hosts", "@orders-api", "--json")...)

	var report batchReport
	if err := json.Unmarshal([]byte(stdout), &report); err != nil {
		t.Fatalf("decode report from %q: %v", stdout, err)
	}
	report.assertCountsAddUp(t)
	if len(report.Results) != 2 {
		t.Fatalf("注册表里有两台，报告里就该有两台，得到 %d", len(report.Results))
	}
	hosts := []string{report.Results[0].Host, report.Results[1].Host}
	if hosts[0] != "web-1" || hosts[1] != "web-2" {
		t.Fatalf("顺序应当与登记时一致：%v", hosts)
	}
}

// manifest 用 artifact.id 时，`--hosts` 在**提交任何东西之前**被拒。
func TestBatchDeployRejectsArtifactIDReference(t *testing.T) {
	f := newTargetFixture(t, 1, "orders-api")
	f.prepareTarget(t, "web-1", "orders-api", []byte("orders-v1"))

	manifest := writeBatchManifest(t, f.dir, "by-id.yaml", `apiVersion: ops.frz.io/v1alpha1
kind: ApplicationSpec
application: orders-api
runtime: go
artifact:
  id: art_something
exec:
  argv: [bin/orders-api]
  workingDirectory: /var/lib/orders-api
  runUser: orders-api
logs:
  directory: /var/log/orders-api
health:
  readiness:
    type: tcp
    target: "127.0.0.1:28080"
`)

	before := countOperations(t, f.targets["web-1"].daemon.database, v1.KindAppDeploy)
	_, code, err := runOpsctlBinary(t, f.args("app", "deploy",
		"--app", "orders-api", "--file", manifest, "--hosts", "web-1")...)
	if code != 2 {
		t.Fatalf("want exit 2 (INVALID_REQUEST), got %d (%v)", code, err)
	}
	if err == nil || !strings.Contains(err.Error(), "digest") {
		t.Fatalf("报错要说清该怎么办（改成 digest）：%v", err)
	}
	if after := countOperations(t, f.targets["web-1"].daemon.database, v1.KindAppDeploy); after != before {
		t.Fatalf("被拒的批量部署不该留下任何操作：之前 %d，之后 %d", before, after)
	}
}

// 本地制品文件与 manifest 里的摘要对不上时，必须在**任何一次上传之前**被拦下——
// 否则五台机都会装上错的制品，而每台机上的部署都会成功。
func TestBatchDeployVerifiesLocalArtifactDigest(t *testing.T) {
	f := newTargetFixture(t, 1, "orders-api")
	digest := f.prepareTarget(t, "web-1", "orders-api", []byte("orders-v1"))
	manifest := writeBatchManifest(t, f.dir, "batch.yaml", batchManifest("orders-api", digest))

	// 另一份内容：摘要对不上。
	mismatched := filepath.Join(f.dir, "wrong.bin")
	if err := os.WriteFile(mismatched, []byte("完全不是那份制品"), 0o644); err != nil {
		t.Fatalf("写文件: %v", err)
	}

	_, code, err := runOpsctlBinary(t, f.args("app", "deploy", "--app", "orders-api",
		"--file", manifest, "--hosts", "web-1", "--artifact", mismatched)...)
	if code != 5 {
		t.Fatalf("want exit 5 (ARTIFACT_CHECKSUM_MISMATCH), got %d (%v)", code, err)
	}
	if err == nil || !strings.Contains(err.Error(), "摘要") {
		t.Fatalf("报错要说清是摘要对不上：%v", err)
	}
}

// 批次号就是每台机上的幂等键：同名重跑**不重做**已经完成（或已经失败）的机器。
//
// 这个断言需要「目标机上真的建出了操作」，而那只在装配了 runtime 适配器的平台上成立
// （非 Linux 上 deploy 在**创建期**就被 RUNTIME_UNSUPPORTED 拒掉，一条操作都不会有）。
// 因此这里按平台分开表述，并把「为什么跳过」写清楚——一条被跳过的断言只有在理由
// 具体时才不算装饰。
func TestBatchRerunDoesNotRedoFinishedHosts(t *testing.T) {
	f := newTargetFixture(t, 1, "orders-api")
	digest := f.prepareTarget(t, "web-1", "orders-api", []byte("orders-v1"))
	manifest := writeBatchManifest(t, f.dir, "batch.yaml", batchManifest("orders-api", digest))

	args := func() []string {
		return f.args("app", "deploy", "--app", "orders-api", "--file", manifest,
			"--hosts", "web-1", "--batch", "batch_fixed_for_rerun", "--json")
	}

	first, _, _ := runOpsctlBinary(t, args()...)
	var firstReport batchReport
	if err := json.Unmarshal([]byte(first), &firstReport); err != nil {
		t.Fatalf("decode report from %q: %v", first, err)
	}
	firstReport.assertCountsAddUp(t)

	database := f.targets["web-1"].daemon.database
	afterFirst := countOperations(t, database, v1.KindAppDeploy)
	if afterFirst == 0 {
		t.Skipf("这台机上没有 runtime 适配器（非 Linux），部署在创建期就被拒：" +
			"「批次号即幂等键」由 internal/fleet 的单元测试覆盖")
	}

	// 记录下来第一次的键，第二次必须完全一样——否则「继续」会变成「重做」。
	key := f.lastIdempotencyKey(t)
	if key != "batch_fixed_for_rerun:deploy:orders-api" {
		t.Fatalf("幂等键的拼法不对：%q", key)
	}

	second, _, _ := runOpsctlBinary(t, args()...)
	var secondReport batchReport
	if err := json.Unmarshal([]byte(second), &secondReport); err != nil {
		t.Fatalf("decode report from %q: %v", second, err)
	}
	secondReport.assertCountsAddUp(t)

	if afterSecond := countOperations(t, database, v1.KindAppDeploy); afterSecond != afterFirst {
		t.Fatalf("同一个批次号重跑不该产生新操作：之前 %d 条，之后 %d 条", afterFirst, afterSecond)
	}
	// 两个批次的结论必须一致（含失败——失败的机器**不会**被自动重试）。
	if firstReport.Failed != secondReport.Failed || firstReport.Succeeded != secondReport.Succeeded {
		t.Fatalf("同名重跑改变了结论：%+v → %+v", firstReport, secondReport)
	}
}

// 批量回滚走同一套批次机制，而且 **`--to` 仍然是「回到哪个版本」、`--hosts` 才是目标**。
//
// 这条断言刻意用一个**在这几台机上都不存在的版本**：准备阶段会因此把它们标成 skipped
// （退出码 36），而这件事**在任何平台上都一样**——它不依赖「这台机上部署会不会成功」，
// 因此不会退化成「断言平台的脾气」。要验的正是那个旗标语义：如果 `--to` 被当成了目标名单，
// 报出来的会是「没有这台主机」，而不是「没有这个版本」。
func TestBatchRollbackKeepsToAsVersion(t *testing.T) {
	f := newTargetFixture(t, 2, "orders-api")
	f.prepareTarget(t, "web-1", "orders-api", []byte("orders-v1"))
	f.prepareTarget(t, "web-2", "orders-api", []byte("orders-v1"))

	// 目标来自登记的注册表（`@应用名`），顺带验批量回滚也能用登记好的目标。
	if _, _, err := runOpsctl(t, f.coord.socket, "app", "target", "set",
		"--app", "orders-api", "--hosts", "web-1,web-2"); err != nil {
		t.Fatalf("target set: %v", err)
	}

	before := countOperations(t, f.targets["web-1"].daemon.database, v1.KindAppRollback)

	stdout, code, err := runOpsctlBinary(t, f.args("app", "rollback",
		"--app", "orders-api", "--to", "v9.9.9", "--hosts", "@orders-api", "--json")...)
	if code != 36 {
		t.Fatalf("want exit 36 (BATCH_PREFLIGHT_FAILED), got %d：%s %v", code, stdout, err)
	}

	var report batchReport
	if err := json.Unmarshal([]byte(stdout), &report); err != nil {
		t.Fatalf("decode report from %q: %v", stdout, err)
	}
	report.assertCountsAddUp(t)
	if report.Action != "rollback" {
		t.Fatalf("报告里的动作应当是 rollback，得到 %q", report.Action)
	}
	if len(report.Results) != 2 {
		t.Fatalf("注册表里有两台，回滚的批次里就该有两台，得到 %d：%+v", len(report.Results), report.Results)
	}
	for _, result := range report.Results {
		if result.Status != "skipped" {
			t.Fatalf("%s 应当是 skipped（版本不存在），得到 %q", result.Host, result.Status)
		}
		// 原因必须指向**那个版本**。指向主机名就说明 `--to` 被当成了目标名单。
		if !strings.Contains(result.Detail, "v9.9.9") {
			t.Fatalf("%s 的跳过原因应当提到那个版本：%q", result.Host, result.Detail)
		}
	}

	// 准备阶段没过 → 这台机上一个回滚操作都没有。
	if after := countOperations(t, f.targets["web-1"].daemon.database, v1.KindAppRollback); after != before {
		t.Fatalf("准备阶段没过时不该提交任何回滚：之前 %d 条，之后 %d 条", before, after)
	}
}
