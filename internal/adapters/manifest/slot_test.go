package manifest

import (
	"strings"
	"testing"

	v1 "github.com/freezeChen/frz-tools/api/v1"
	"github.com/freezeChen/frz-tools/internal/domain"
)

// 迭代 4a：蓝绿形态的 manifest 解码与校验。
//
// 这一组用例护两件事：①**新增字段是兼容的**——没有 slots/nginx 的 manifest
// （迭代 3 写的那些）解码路径与行为一字不变；②蓝绿的每一条边界都被钉住，
// 尤其是「哪些字段与槽位互斥」——那是两套部署形态共存时最容易含糊的地方。

// blueGreenManifest 是一份合法的蓝绿 manifest。
const blueGreenManifest = `apiVersion: ops.frz.io/v1alpha1
kind: ApplicationSpec
application: orders-api
runtime: go
artifact:
  id: art_XXXX
  version: 1.2.3
  unpack:
    strategy: tar-gz
exec:
  argv: [bin/server]
  workingDirectory: /var/lib/orders-api
  runUser: orders-api
  environment:
    GOMEMLIMIT: 40MiB
  slots:
    blue:
      ports: [18081]
      readiness:
        type: tcp
        target: "127.0.0.1:18081"
      environment:
        ORDERS_HTTP_ADDR: "127.0.0.1:18081"
    green:
      ports: [18082]
      readiness:
        type: tcp
        target: "127.0.0.1:18082"
      environment:
        ORDERS_HTTP_ADDR: "127.0.0.1:18082"
health:
  startTimeoutSeconds: 60
  stopTimeoutSeconds: 30
logs:
  directory: /var/log/orders-api
nginx:
  listen: 8080
  serverName: orders.example.com
`

func TestParseBlueGreenManifest(t *testing.T) {
	spec, err := Parse([]byte(blueGreenManifest))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if !spec.BlueGreen() {
		t.Fatal("声明了 exec.slots 的应用应当被识别为蓝绿")
	}
	if slots := spec.SlotsFor(); len(slots) != 2 || slots[0] != domain.SlotBlue || slots[1] != domain.SlotGreen {
		t.Fatalf("SlotsFor 应当按固定顺序返回两个槽位，got %v", slots)
	}

	blue, ok := spec.SlotOf(domain.SlotBlue)
	if !ok {
		t.Fatal("blue 槽位应当存在")
	}
	if len(blue.Ports) != 1 || blue.Ports[0] != 18081 {
		t.Fatalf("blue 的端口不对: %v", blue.Ports)
	}
	if blue.Readiness.Type != domain.ReadinessTCP || blue.Readiness.Target != "127.0.0.1:18081" {
		t.Fatalf("blue 的就绪目标不对: %+v", blue.Readiness)
	}
	// 没写 consecutiveSuccesses 时补默认值 1——与单槽就绪同一条规则。
	if blue.Readiness.ConsecutiveSuccesses != 1 {
		t.Fatalf("槽位就绪的连续成功次数应当补成 1，got %d", blue.Readiness.ConsecutiveSuccesses)
	}

	// 槽位环境与 exec.environment **合并**，同名时槽位优先。
	env := spec.SlotEnvironment(domain.SlotGreen)
	if env["GOMEMLIMIT"] != "40MiB" {
		t.Fatalf("共享环境应当出现在槽位环境里: %v", env)
	}
	if env["ORDERS_HTTP_ADDR"] != "127.0.0.1:18082" {
		t.Fatalf("槽位自己的环境不对: %v", env)
	}
	// 合并不能改到原规格：改一次规格就会把另一个槽位的端口也改掉。
	if blue.Environment["ORDERS_HTTP_ADDR"] != "127.0.0.1:18081" {
		t.Fatalf("合并污染了槽位自己的环境: %v", blue.Environment)
	}

	// nginx 段：没写 observationSeconds / drainSeconds 时走默认值。
	if !spec.Nginx.Configured() || spec.Nginx.Listen != 8080 {
		t.Fatalf("nginx 段没解出来: %+v", spec.Nginx)
	}
	if spec.Nginx.ObservationSeconds != domain.DefaultObservationSeconds {
		t.Fatalf("观察窗口应当走默认值 %d，got %d",
			domain.DefaultObservationSeconds, spec.Nginx.ObservationSeconds)
	}
	if spec.Nginx.DrainSeconds != domain.DefaultDrainSeconds {
		t.Fatalf("排空时间应当走默认值 %d，got %d", domain.DefaultDrainSeconds, spec.Nginx.DrainSeconds)
	}
	if got := spec.Nginx.EffectiveUpstreamName(spec.Application); got != "frz_orders-api" {
		t.Fatalf("upstream 名应当按应用派生，got %q", got)
	}
}

// `observationSeconds: 0` 是**显式不观察**，不能被默认值改写。
//
// 这一条是 wire 用指针的唯一理由：domain 的 int 分不出「没写」与「写了 0」，
// 在 domain 里补默认值会让「关掉观察」永远做不到——而这个开关恰恰是排查时最想用的那个。
func TestParseBlueGreenExplicitZeroObservation(t *testing.T) {
	manifest := strings.Replace(blueGreenManifest,
		"  serverName: orders.example.com", "  serverName: orders.example.com\n  observationSeconds: 0\n  drainSeconds: 0", 1)
	spec, err := Parse([]byte(manifest))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if spec.Nginx.ObservationSeconds != 0 {
		t.Fatalf("显式写 0 必须保持 0（不观察），got %d", spec.Nginx.ObservationSeconds)
	}
	if spec.Nginx.DrainSeconds != 0 {
		t.Fatalf("显式写 0 必须保持 0（不等排空），got %d", spec.Nginx.DrainSeconds)
	}
}

func TestParseRejectsInvalidBlueGreen(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(string) string
		// wantConflict 为真时期望 MANIFEST_CONFLICT（撞键类），否则 MANIFEST_INVALID。
		wantConflict bool
	}{
		{"slots 与 ports 同时给", func(s string) string {
			return strings.Replace(s, "  slots:", "  ports: [18081]\n  slots:", 1)
		}, false},
		{"slots 与 unitName 同时给", func(s string) string {
			return strings.Replace(s, "  runUser: orders-api",
				"  runUser: orders-api\nsystemd:\n  unitName: orders-api.service", 1)
		}, false},
		{"slots 与 health.readiness 同时给", func(s string) string {
			return strings.Replace(s, "health:\n  startTimeoutSeconds: 60",
				"health:\n  readiness:\n    type: tcp\n    target: \"127.0.0.1:18081\"\n  startTimeoutSeconds: 60", 1)
		}, false},
		{"缺一个槽位", func(s string) string {
			// 整块删掉 green：只删一行环境变量的话 manifest 依然合法，测的就不是这件事了。
			return strings.Replace(s, `    green:
      ports: [18082]
      readiness:
        type: tcp
        target: "127.0.0.1:18082"
      environment:
        ORDERS_HTTP_ADDR: "127.0.0.1:18082"
`, "", 1)
		}, false},
		{"槽位名拼错", func(s string) string {
			return strings.Replace(s, "    green:", "    gren:", 1)
		}, false},
		{"两个槽位端口相交", func(s string) string {
			return strings.Replace(s, "      ports: [18082]", "      ports: [18081]", 1)
		}, false},
		{"槽位端口越界", func(s string) string {
			return strings.Replace(s, "      ports: [18082]", "      ports: [70000]", 1)
		}, false},
		{"槽位端口为空", func(s string) string {
			return strings.Replace(s, "      ports: [18082]", "      ports: []", 1)
		}, false},
		{"槽位缺就绪目标", func(s string) string {
			return strings.Replace(s, `        target: "127.0.0.1:18082"`, "        target: 127.0.0.1", 1)
		}, false},
		{"蓝绿应用没声明 nginx 段", func(s string) string {
			return strings.Replace(s, "nginx:\n  listen: 8080\n  serverName: orders.example.com\n", "", 1)
		}, false},
		{"nginx.listen 与槽位端口相撞", func(s string) string {
			return strings.Replace(s, "  listen: 8080", "  listen: 18081", 1)
		}, false},
		{"nginx.serverName 含非法字符", func(s string) string {
			return strings.Replace(s, "  serverName: orders.example.com",
				`  serverName: "orders.example.com; }"`, 1)
		}, false},
		{"nginx.upstreamName 含斜杠", func(s string) string {
			return strings.Replace(s, "  listen: 8080", "  listen: 8080\n  upstreamName: a/b", 1)
		}, false},
		{"观察窗口越界", func(s string) string {
			return strings.Replace(s, "  listen: 8080", "  listen: 8080\n  observationSeconds: 99999", 1)
		}, false},
		{"槽位环境与敏感变量撞键", func(s string) string {
			return strings.Replace(s, "exec:\n  argv: [bin/server]",
				"exec:\n  argv: [bin/server]\n  secretEnvironment:\n    ORDERS_HTTP_ADDR:\n      kind: env\n      name: x", 1)
		}, true},
		{"槽位环境名非法", func(s string) string {
			return strings.Replace(s, `        ORDERS_HTTP_ADDR: "127.0.0.1:18082"`,
				`        "2BAD-NAME": "x"`, 1)
		}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mutated := tc.mutate(blueGreenManifest)
			if mutated == blueGreenManifest {
				t.Fatal("这条用例的定点替换没生效——它测的是别的东西")
			}
			_, err := Parse([]byte(mutated))
			if err == nil {
				t.Fatal("应当被拒")
			}
			want := v1.CodeManifestInvalid
			if tc.wantConflict {
				want = v1.CodeManifestConflict
			}
			if code := domain.CodeOf(err); code != want {
				t.Fatalf("want %s, got %s：%v", want, code, err)
			}
		})
	}
}

// nginx 段只服务蓝绿：没声明 slots 却写 nginx，是把两套形态各写了一半。
func TestParseRejectsNginxWithoutSlots(t *testing.T) {
	manifest := strings.Replace(validManifest, "systemd:",
		"nginx:\n  listen: 8080\nsystemd:", 1)
	_, err := Parse([]byte(manifest))
	if code := domain.CodeOf(err); code != v1.CodeManifestInvalid {
		t.Fatalf("want MANIFEST_INVALID, got %s：%v", code, err)
	}
}

// 兼容性回归：迭代 3 写的 manifest（没有 slots / nginx）解码路径一字不变。
//
// 这条断言的价值不在「能解出来」，而在**新增字段没有把老 manifest 的语义带偏**：
// 单槽应用不该被识别成蓝绿，也不该突然要求 Nginx。
func TestParseSingleSlotManifestStaysSingleSlot(t *testing.T) {
	spec, err := Parse([]byte(validManifest))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if spec.BlueGreen() {
		t.Fatal("没有 exec.slots 的应用不该被识别为蓝绿")
	}
	if spec.SlotsFor() != nil {
		t.Fatalf("单槽应用的 SlotsFor 应当是 nil，got %v", spec.SlotsFor())
	}
	if spec.Nginx.Configured() {
		t.Fatalf("没写 nginx 段时不该被当成配置了 Nginx: %+v", spec.Nginx)
	}
	if len(spec.Exec.Ports) != 1 || spec.Exec.Ports[0] != 8080 {
		t.Fatalf("单槽的 exec.ports 应当照原样解出来: %v", spec.Exec.Ports)
	}
	if spec.Health.Readiness.Target != "127.0.0.1:8080" {
		t.Fatalf("单槽的就绪目标应当照原样解出来: %+v", spec.Health.Readiness)
	}
}

// 同一份蓝绿规格**连续校验两次**都必须通过。
//
// 这条回归测试钉的是一个真实的 bug：`applyDefaults` 会给规格补上 `systemd.unitName`，而
// 「蓝绿应用不得手写 unit 名」是一条互斥规则——于是第二次校验时，规格带着**上一次自己补的
// 默认值**撞上了自己的规则。生产里的路径正是「两次校验」：`PrepareDeploy` 校验一次并把
// （已被默认值改写的）规格存进库，执行部署时把规格读回来再校验一次。少了这条，每一次蓝绿
// 部署都会在执行阶段以「不得手写 systemd.unitName」失败——而那份 manifest 里根本没写过它。
func TestBlueGreenSpecValidatesRepeatedly(t *testing.T) {
	spec, err := Parse([]byte(blueGreenManifest))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	// 第一次校验已经把默认值写回规格了（这是 spec.Validate 既有的语义）。
	if err := spec.Validate(); err != nil {
		t.Fatalf("第二次校验不该失败: %v", err)
	}
	if spec.Systemd.UnitName != "" {
		t.Fatalf("蓝绿规格不该被补上 unit 名（按槽位派生），got %q", spec.Systemd.UnitName)
	}
	// 而**手写** unit 名仍然必须被拒：那是真的写错了。
	spec.Systemd.UnitName = "orders-api.service"
	if err := spec.Validate(); domain.CodeOf(err) != v1.CodeManifestInvalid {
		t.Fatalf("手写 unit 名必须被拒，got %v", err)
	}
}
