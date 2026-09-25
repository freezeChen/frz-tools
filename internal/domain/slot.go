package domain

import (
	"fmt"
	"net"
	"net/url"
	"path"
	"regexp"
	"strings"

	v1 "github.com/freezeChen/frz-tools/api/v1"
)

// 迭代 4：Nginx 蓝绿发布。
//
// 这个文件放的是「两个 slot」这件事在领域里的全部表达：枚举、每个 slot 的规格、
// 派生出来的路径与名字、以及把它们钉住的校验。**它不含任何 IO**——切流、Nginx 与
// 对账在适配器与应用层，这里只回答「一个 slot 长什么样、叫什么名字、住在哪」。

// Slot 是蓝绿的槽位。取值的两个名字是刻意的：**blue / green 是运维的工作语言**，
// 而 slot-a / slot-b 之类的说法到了值班电话里总要再解释一遍。
type Slot string

const (
	SlotBlue  Slot = "blue"
	SlotGreen Slot = "green"
)

// Slots 是两个槽位，**顺序固定**（blue 在前）。
//
// 顺序固定是为了让「遍历 slot」这件事的结果稳定：日志、`app slot list` 的输出、
// 生成配置时的顺序都不该随 map 的遍历顺序漂移（Go 的 map 遍历本来就是随机的，
// 那会让同样的输入产生不同的输出，测试与排查都变难）。
var Slots = [2]Slot{SlotBlue, SlotGreen}

// Valid 报告 s 是不是认识的槽位。
func (s Slot) Valid() bool {
	return s == SlotBlue || s == SlotGreen
}

// Opposite 返回另一个槽位。蓝绿只有两侧，因此「非当前」就是「另一个」。
func (s Slot) Opposite() Slot {
	if s == SlotBlue {
		return SlotGreen
	}
	return SlotBlue
}

// ParseSlot 把外部输入（manifest 的键、命令行参数）变成 Slot。
// 大小写不敏感、两侧空白忽略，其余一律拒绝——**不做「猜一个最接近的」**：
// 把 `Blue` 猜成 `blue` 是可以的（人写的东西），把 `blu` 猜成 `blue` 不行。
func ParseSlot(raw string) (Slot, error) {
	slot := Slot(strings.ToLower(strings.TrimSpace(raw)))
	if !slot.Valid() {
		return "", NewError(v1.CodeManifestInvalid,
			"slot 只能是 %s 或 %s（got %q）", SlotBlue, SlotGreen, raw)
	}
	return slot, nil
}

// SpecSlot 是一个槽位自己的规格。**只放「两个槽位可能不同」的东西**：
// 端口、就绪目标、以及把这个槽位的端口告诉应用的环境变量。
//
// 为什么端口要按槽位写、而不是「基准端口 + 偏移」：偏移量是魔数，而且应用自己的配置
// 文件里也得知道这个偏移。写全两遍是多两行，但每一行都是运维本来就要决定的事。
type SpecSlot struct {
	// Ports 是这个槽位的应用监听端口（供 Nginx upstream 与断言使用）。
	//
	// 它**不**自动进入环境变量：应用怎么知道自己的端口由运维决定，
	// 见 SpecSlot.Environment——工具不发明 FRZ_SLOT_PORT 之类的魔法名字。
	Ports []int
	// Readiness 是这个槽位的就绪目标。端口不同，探活目标当然不同。
	Readiness SpecReadiness
	// Environment 是**只对这个槽位生效**的环境变量，与 exec.environment 合并，
	// 同名时以槽位的为准（局部覆盖全局）。
	Environment map[string]string
}

// SpecNginx 是蓝绿应用的对外入口声明。
//
// Listen 为 0 表示**没有声明 nginx 段**（0 不是合法端口，因此这个取值没有歧义）
// ——蓝绿应用的 Nginx 段是必需的，但「必需」由校验去说，而不是靠零值假装。
type SpecNginx struct {
	// Listen 是 Nginx 监听的对外端口。它与两个槽位的端口**不得相交**：
	// 撞上就是两个进程抢同一个端口。
	Listen int
	// ServerName 是 server_name（可选）。
	ServerName string
	// UpstreamName 是 upstream 的名字（可选，默认按应用名派生，见 UpstreamName）。
	UpstreamName string
	// ObservationSeconds 是切流后的观察窗口（0 = 不观察）。
	ObservationSeconds int
	// DrainSeconds 是切流后等旧槽位排空的时间（0 = 不等）。
	DrainSeconds int
}

// 观察窗口与排空时间的默认值。
//
// 观察窗口默认不为 0：切流之后什么都不看就宣布成功，等于把「新版本是不是真的在服务」
// 这件事交给运气。30 秒是一个折中——足够发现「起来了但立刻退出」这类问题，
// 又不至于让每次发布都多等一分钟（它可以按应用配置，测试里会调到 1–2 秒）。
const (
	DefaultObservationSeconds = 30
	DefaultDrainSeconds       = 5
	// MaxObservationSeconds / MaxDrainSeconds 是上界：观察窗口是**在 Operation 里**
	// 串行等待的，一个几小时的窗口会把 worker 占住。
	MaxObservationSeconds = 3600
	MaxDrainSeconds       = 300
)

// Configured 报告是否声明了 nginx 段。
func (n SpecNginx) Configured() bool { return n.Listen > 0 }

// ==== 派生出来的路径与名字 ====
//
// 与 1c 起就有的约定（ReleaseDir / EnvFilePath / UnitPath）同一条纪律：
// **派生规则只能有一处**。蓝绿应用若手写了 unit 名，就会出现「工具按槽位生成、
// 运维按自己的名字写」两套名字，而 enable/stop/status 全都要用那个名字。

// SlotDir 返回某个槽位的工作目录：`/opt/opsd/apps/<application>/slots/<slot>`。
func SlotDir(application string, slot Slot) string {
	return path.Join("/opt/opsd/apps", application, "slots", string(slot))
}

// SlotCurrentDir 是槽位自己的 current 指针：指向该槽位当前运行的 release 目录。
//
// 它与单槽应用的 `<releases 根>/current` **不是同一个东西**：单槽只有一个指针，
// 蓝绿每个槽位一个。两个槽位同时运行不同的 release，正是蓝绿的全部意义。
func SlotCurrentDir(application string, slot Slot) string {
	return path.Join(SlotDir(application, slot), "current")
}

// SlotCurrentLinkTarget 是槽位 current 指针**写进符号链接的目标**（相对路径）。
//
// 相对而不是绝对：整个 releases 根可以被整体搬走或换前缀（测试里的 RootPath 就是
// 这么做的），绝对路径会在那时失效——与单槽 current 用相对目标同一个理由。
// 从 `<app>/slots/<slot>/current` 上跳两级正好回到 `<app>`，再进 releases。
func SlotCurrentLinkTarget(releaseID string) string {
	return path.Join("..", "..", "releases", releaseID)
}

// SlotEnvFilePath 是槽位自己的非敏感环境文件：`/etc/opsd/apps/<app>.<slot>.env`。
//
// 每个槽位一份（而不是共用 `<app>.env`）是必需的：**槽位之间的环境本来就是不同的**
// ——至少端口不同。共用一份文件就无法表达「blue 听 18081、green 听 18082」。
func SlotEnvFilePath(application string, slot Slot) string {
	return path.Join("/etc/opsd/apps", application+"."+string(slot)+".env")
}

// SlotSecretsEnvFilePath 是槽位自己的敏感环境文件。凭据内容两个槽位通常相同，
// 但文件仍按槽位分开：unit 的 EnvironmentFile= 是逐 unit 写的，让两个 unit 指向
// 同一个文件会引入「谁在改写它」的共享状态，而按槽位分开没有任何代价。
func SlotSecretsEnvFilePath(application string, slot Slot) string {
	return path.Join("/etc/opsd/apps", application+"."+string(slot)+".secrets.env")
}

// SlotSecretFileVarValue 是 kind=file 的凭据在槽位环境里承载的值。
func SlotSecretFileVarValue(application string, slot Slot, name string) string {
	return path.Join(SecretsDir(application), string(slot), name)
}

// SlotSecretsDir 是某个槽位存放 kind=file 凭据副本的目录。
func SlotSecretsDir(application string, slot Slot) string {
	return path.Join(SecretsDir(application), string(slot))
}

// SlotUnitName 是槽位对应的 unit 名：`<application>-<slot>.service`。
func SlotUnitName(application string, slot Slot) string {
	return fmt.Sprintf("%s-%s.service", application, slot)
}

// UpstreamName 是 Nginx 里 upstream 的名字。默认 `frz_<application>`：
// `frz_` 前缀把「我们生成的」与用户自己的 upstream 分开，下划线则避开连字符
// 在某些位置（例如变量名、正则）上的歧义。
func UpstreamName(application string) string {
	return "frz_" + application
}

// ManagedNginxDir 是受管配置目录：`<confDir>/frz-managed`。
//
// 单独一个子目录是刻意的：**工具只写自己的目录，不改写用户的主配置**
// （路线图第 7 节的原文）。运维需要在主配置里 include 这个目录，而
// 「有没有被真的加载」由 `nginx -T` 检查（见 4b）。
func ManagedNginxDir(confDir string) string {
	return path.Join(confDir, "frz-managed")
}

// ManagedNginxFile 是某个应用的受管配置文件。
func ManagedNginxFile(confDir, application string) string {
	return path.Join(ManagedNginxDir(confDir), application+".conf")
}

// ==== 校验 ====

// validateSlots 校验蓝绿形态的槽位声明。全部返回 MANIFEST_INVALID。
func (e *SpecExec) validateSlots() error {
	if len(e.Slots) == 0 {
		return nil
	}
	// 蓝绿与单槽是两种形态，端口声明只能有一种写法：同时给必然有一处是错的，
	// 而「哪一处是错的」不该由工具去猜。
	if len(e.Ports) > 0 {
		return NewError(v1.CodeManifestInvalid,
			"exec.ports 与 exec.slots 互斥：声明了槽位就以每个槽位自己的 ports 为准（单槽形态请去掉 exec.slots）")
	}
	for _, slot := range Slots {
		if _, ok := e.Slots[slot]; !ok {
			return NewError(v1.CodeManifestInvalid,
				"exec.slots 必须同时给出 %s 与 %s（只给一个不是蓝绿）", SlotBlue, SlotGreen)
		}
	}
	for name := range e.Slots {
		if !name.Valid() {
			return NewError(v1.CodeManifestInvalid,
				"exec.slots 的键只能是 %s 或 %s（got %q）", SlotBlue, SlotGreen, name)
		}
	}

	seen := map[int]Slot{}
	for _, slot := range Slots {
		spec := e.Slots[slot]
		if len(spec.Ports) == 0 {
			return NewError(v1.CodeManifestInvalid, "exec.slots.%s.ports 不能为空", slot)
		}
		for _, port := range spec.Ports {
			if port < 1 || port > 65535 {
				return NewError(v1.CodeManifestInvalid, "exec.slots.%s.ports 中的端口越界: %d", slot, port)
			}
			// 两个槽位抢同一个端口，切流必然失败（新槽位根本起不来）。
			if other, taken := seen[port]; taken {
				return NewError(v1.CodeManifestInvalid,
					"exec.slots 里 %s 与 %s 都用了端口 %d：两个槽位会抢同一个端口", other, slot, port)
			}
			seen[port] = slot
		}
		if err := validateReadiness(spec.Readiness); err != nil {
			return NewError(v1.CodeManifestInvalid, "exec.slots.%s.readiness: %s", slot, MessageOf(err))
		}
		// 槽位的环境变量与 exec.environment 走同一套规则（名字合法、不与敏感变量撞键）。
		for name := range spec.Environment {
			if !validEnvName(name) {
				return NewError(v1.CodeManifestInvalid,
					"exec.slots.%s.environment 的键 %q 不是合法的环境变量名", slot, name)
			}
			if _, clash := e.SecretEnvironment[name]; clash {
				return NewError(v1.CodeManifestConflict,
					"环境变量 %q 同时出现在 exec.slots.%s.environment 与 secretEnvironment 中", name, slot)
			}
		}
	}
	return nil
}

// SlotsFor 返回该规格的槽位（按 Slots 的固定顺序），单槽形态返回 nil。
//
// 「是不是蓝绿」只由这一个方法回答：到处写 `len(spec.Exec.Slots) > 0` 迟早会有一处
// 忘了判断，而那种遗漏的表现是「蓝绿应用被当成单槽部署」——发布出去了，但没有切流。
func (s *ApplicationSpec) SlotsFor() []Slot {
	if len(s.Exec.Slots) == 0 {
		return nil
	}
	return Slots[:]
}

// BlueGreen 报告这是不是一个蓝绿应用。
func (s *ApplicationSpec) BlueGreen() bool { return len(s.Exec.Slots) > 0 }

// SlotOf 返回某个槽位的规格。不是蓝绿应用、或槽位不存在时返回 false。
func (s *ApplicationSpec) SlotOf(slot Slot) (SpecSlot, bool) {
	spec, ok := s.Exec.Slots[slot]
	return spec, ok
}

// SlotReadiness 返回某个槽位的就绪规格。
func (s *ApplicationSpec) SlotReadiness(slot Slot) SpecReadiness {
	return s.Exec.Slots[slot].Readiness
}

// SlotPorts 返回某个槽位声明的端口（副本，避免调用方改到规格里）。
func (s *ApplicationSpec) SlotPorts(slot Slot) []int {
	ports := s.Exec.Slots[slot].Ports
	return append([]int(nil), ports...)
}

// SlotUnitName 返回该应用某个槽位的 unit 名。
func (s *ApplicationSpec) SlotUnitName(slot Slot) string {
	return SlotUnitName(s.Application, slot)
}

// SlotEnvironment 返回某个槽位**合并之后**的环境变量：exec.environment 打底，
// 槽位自己的覆盖同名键。
//
// 合并只在这里做一次：unit 渲染、环境文件写入、断言三处都要用它，
// 各算一份迟早会出现「部署时用的是 A，断言看的是 B」。
func (s *ApplicationSpec) SlotEnvironment(slot Slot) map[string]string {
	merged := make(map[string]string, len(s.Exec.Environment)+len(s.Exec.Slots[slot].Environment))
	for name, value := range s.Exec.Environment {
		merged[name] = value
	}
	for name, value := range s.Exec.Slots[slot].Environment {
		merged[name] = value
	}
	if len(merged) == 0 {
		return nil
	}
	return merged
}

// validateNginx 校验 nginx 段。Listen 为 0 表示没声明，直接通过。
func (n SpecNginx) validate(spec *ApplicationSpec) error {
	if !n.Configured() {
		return nil
	}
	if n.Listen < 1 || n.Listen > 65535 {
		return NewError(v1.CodeManifestInvalid, "nginx.listen 端口越界: %d", n.Listen)
	}
	// 对外端口撞上某个槽位的端口：两个进程抢同一个监听，而其中一个是 Nginx 自己。
	for _, slot := range Slots {
		if port, ok := spec.Exec.Slots[slot]; ok {
			for _, p := range port.Ports {
				if p == n.Listen {
					return NewError(v1.CodeManifestInvalid,
						"nginx.listen 的端口 %d 与 exec.slots.%s 的端口相同：对外端口与槽位端口不能撞", n.Listen, slot)
				}
			}
		}
	}
	if n.UpstreamName != "" && !upstreamNamePattern.MatchString(n.UpstreamName) {
		return NewError(v1.CodeManifestInvalid,
			"nginx.upstreamName 只允许字母、数字、下划线与连字符，且必须以字母或数字开头（got %q）", n.UpstreamName)
	}
	if n.ServerName != "" && !serverNamePattern.MatchString(n.ServerName) {
		return NewError(v1.CodeManifestInvalid,
			"nginx.serverName 含不能原样写进 Nginx 配置的字符（got %q）", n.ServerName)
	}
	if n.ObservationSeconds < 0 || n.ObservationSeconds > MaxObservationSeconds {
		return NewError(v1.CodeManifestInvalid,
			"nginx.observationSeconds 取值范围是 0–%d（got %d）", MaxObservationSeconds, n.ObservationSeconds)
	}
	if n.DrainSeconds < 0 || n.DrainSeconds > MaxDrainSeconds {
		return NewError(v1.CodeManifestInvalid,
			"nginx.drainSeconds 取值范围是 0–%d（got %d）", MaxDrainSeconds, n.DrainSeconds)
	}
	return nil
}

// EffectiveUpstreamName 返回实际使用的 upstream 名（写了就用写的，没写按应用派生）。
func (n SpecNginx) EffectiveUpstreamName(application string) string {
	if n.UpstreamName != "" {
		return n.UpstreamName
	}
	return UpstreamName(application)
}

// upstreamNamePattern / serverNamePattern 是**安全边界**，不是格式偏好：
// 这两个值会被原样写进 Nginx 配置。上游名里出现空白或分号等于让 manifest 作者
// 写任意 Nginx 指令；server_name 里出现换行同理。与 unit 渲染里的
// checkRenderablePath 是同一条理由（那边防的是 unit 语法，这边防的是 Nginx 语法）。
var (
	upstreamNamePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,63}$`)
	serverNamePattern   = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._*:-]{0,255}$`)
)

// validateReadiness 是就绪规格的校验，**单槽与槽位共用同一份**。
//
// 抽出来是因为它原来长在 SpecHealth.validate 里：蓝绿的每个槽位也要探活，
// 而「复制一份校验规则」正是本仓库反复拒绝的事（两份规则必然漂移，
// 漂移的表现是「单槽能过、蓝绿过不了」这种解释不清的差异）。
func validateReadiness(r SpecReadiness) error {
	switch r.Type {
	case ReadinessTCP:
		if _, _, err := net.SplitHostPort(r.Target); err != nil {
			return NewError(v1.CodeManifestInvalid,
				"readiness.type=tcp 时 target 必须是 host:port（got %q）", r.Target)
		}
	case ReadinessHTTP:
		parsed, err := url.Parse(r.Target)
		if err != nil || parsed.Scheme == "" || parsed.Host == "" {
			return NewError(v1.CodeManifestInvalid,
				"readiness.type=http 时 target 必须是完整 URL（got %q）", r.Target)
		}
		if parsed.Scheme != "http" && parsed.Scheme != "https" {
			return NewError(v1.CodeManifestInvalid,
				"readiness.target 的协议只支持 http/https（got %q）", parsed.Scheme)
		}
	case "":
		return NewError(v1.CodeManifestInvalid, "readiness.type 不能为空")
	default:
		return NewError(v1.CodeManifestInvalid, "readiness.type 取值非法: %q", r.Type)
	}
	if r.ConsecutiveSuccesses < 0 {
		return NewError(v1.CodeManifestInvalid, "readiness.consecutiveSuccesses 不能为负数")
	}
	return nil
}
