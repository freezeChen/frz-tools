package main

import (
	"fmt"
	"os"
	"path/filepath"

	v1 "github.com/freezeChen/frz-tools/api/v1"
	"github.com/freezeChen/frz-tools/internal/domain"

	"github.com/spf13/cobra"
)

// 模板正文与 remote 段的两个变体。默认模板 = 正文 + 注释形态的 remote 段；
// `--remote` 模板 = 正文 + 取消注释的 remote 段。
//
// 模板刻意**内嵌在代码里**而不是读仓库里的文件：装好的机器上未必有源码树，
// opsctl 必须自带它（迭代 6 规格 D2）。它因此与 opsd.example.yaml 是两份文本，
// 会漂移——对策是一条单元测试断言两者的键集合一致（init_test.go），
// 漂移让测试红，而不是让用户先发现。
const initTemplateBody = `# opsd 配置骨架，由 opsctl init 生成。
# 改完之后先校验：opsctl config validate --file <本文件路径>
apiVersion: ops.frz.io/v1alpha1
kind: OpsdConfig

socket:
  path: /run/opsd/opsd.sock
  mode: "0660"

database:
  path: /var/lib/opsd/opsd.db

runtime:
  workDirectory: /var/lib/opsd/work
  logDirectory: /var/log/opsd
  workers: 2
  shutdownGraceSeconds: 10

execution:
  defaultTimeoutSeconds: 300
  maxOutputBytes: 1048576
  allowedPaths:
    - /usr/bin
    - /usr/local/bin
  # 按 key 脱敏的环境变量；SecretRef 解析出的值还会额外按值脱敏。
  sensitiveEnvKeys:
    - TOKEN
    - PASSWORD
    - SECRET

# 制品存储。root 不配置即禁用制品相关端点（返回 CONFIG_INVALID 而非启动失败）。
artifactStore:
  root: /var/lib/opsd/artifacts
  # blob 文件与目录模式，由 opsd 在真实文件系统上强制。
  fileMode: "0640"
  dirMode: "0750"
  maxUploadBytes: 2147483648
  quotaBytes: 21474836480

# 备份存储。root 不配置即禁用备份端点（返回 CONFIG_INVALID 而非启动失败）。
# **root 必须与 artifactStore.root 分开**：制品 GC 把「digest 不在 artifacts 表里」的
# blob 一律当孤儿删除，共用同一个根会让 artifact gc 删掉全部备份。
backupStore:
  root: /var/lib/opsd/backups
  fileMode: "0640"
  dirMode: "0750"
  # 备份与制品**独立计数**的配额：备份天然比制品大得多，混在一个配额里
  # 会让「传制品」因为「备份占满」而失败。
  quotaBytes: 21474836480

# 允许 file 类凭据引用的目录。只有解析符号链接后落在这些目录内、
# 且不对属组或其他用户开放的文件才会被读取。
secrets:
  allowedFileDirectories:
    - /etc/opsd/secrets

sudo:
  allowedCommands: []

# Nginx 参数（蓝绿发布）。只在**声明了 exec.slots 的蓝绿应用**上用得到；
# 整段省略时按 confDir=/etc/nginx、二进制走 PATH 的默认值走。
nginx:
  confDir: /etc/nginx
  binary: nginx
  mainConfig: /etc/nginx/nginx.conf

`

// initRemoteCommented 是默认模板里注释形态的 remote 段，与 opsd.example.yaml 的
// 注释段同款：不配这一段就完全不听 TCP。
const initRemoteCommented = `# 远程监听：不配这一段就**完全不听 TCP**，形态与只有 socket 时逐字节一致。
# 配上之后 opsd 会在这个地址上跑 mTLS：双向证书，且只有 clients 名单里的 CN 能进来。
#
# 证书由运维自己签发（本工具只消费、不签发、不轮换）。
# 签发时 SAN 必须包含客户端用来连它的那个名字（地址或域名），否则客户端会（正确地）拒绝。
#
# remote:
#   listen: "0.0.0.0:9443"
#   certFile: /etc/opsd/pki/server.crt
#   keyFile: /etc/opsd/pki/server.key      # 模式必须不宽于 0600，属主必须是 opsd 的运行用户
#   clientCAFile: /etc/opsd/pki/ca.crt     # 签发客户端证书的 CA
#   clients:
#     # 不在名单里的 CN 一律拒绝（白名单，不是黑名单）。
#     - cn: opsctl-central
#       scope: write                       # read 只能打只读端点；write 是全部
`

// initRemoteActive 是 `--remote` 变体里取消注释的 remote 段：占位的地址与证书路径
// 加一个示例 client 条目。占位文件不存在，因此 opsd 启动必然失败——而且配置校验
// 会**逐条**指出缺的是哪个文件（迭代 6 规格 D2），把「开远程」从照着注释逐行改
// 变成「填三个值」。
const initRemoteActive = `# 远程监听：占位的证书路径必须先换成真实文件，否则 opsd 启动必然失败，
# 失败消息会逐条指出缺哪个文件。证书由运维自己签发，SAN 必须包含客户端
# 用来连它的那个名字。
remote:
  listen: "0.0.0.0:9443"
  certFile: /etc/opsd/pki/server.crt
  keyFile: /etc/opsd/pki/server.key      # 模式必须不宽于 0600，属主必须是 opsd 的运行用户
  clientCAFile: /etc/opsd/pki/ca.crt     # 签发客户端证书的 CA
  clients:
    # 不在名单里的 CN 一律拒绝（白名单，不是黑名单）。
    - cn: opsctl-central
      scope: write                       # read 只能打只读端点；write 是全部
`

// newInitCommand 生成 opsd 配置骨架（迭代 6 规格 D2）。
//
// 它是**纯本地**动作：不连 daemon、不探测环境、不问任何问题——可脚本化、可测试，
// 而配置项的全部解释恰恰是模板注释最擅长的。文件以 0600 落盘：它是要替代
// /etc/opsd/config.yaml 的文件，从出生起就不该对其他人可读。
func newInitCommand(opts *rootOptions) *cobra.Command {
	var (
		force  bool
		remote bool
	)

	cmd := &cobra.Command{
		Use:   "init <路径>",
		Short: "生成一份 opsd 配置骨架到指定路径，不连接 opsd",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			target := args[0]

			// 覆盖保护：已存在就拒绝，--force 才覆盖。误把别人调好的配置冲掉
			// 是不可逆的，所以默认不做。
			if _, err := os.Stat(target); err == nil && !force {
				return domain.NewError(v1.CodeInvalidRequest,
					"%s 已存在，拒绝覆盖；要覆盖请加 --force", target)
			}
			// 父目录必须已经存在：代为创建会把路径里的笔误（例如 /etx/opsd）
			// 变成一个真的目录树，错位比报错更难发现。
			if dir := filepath.Dir(target); dir != "" {
				if _, err := os.Stat(dir); err != nil {
					return domain.NewError(v1.CodeInvalidRequest,
						"父目录 %s 不存在，请先创建它", dir)
				}
			}

			body := initTemplateBody + initRemoteCommented
			if remote {
				body = initTemplateBody + initRemoteActive
			}
			if err := os.WriteFile(target, []byte(body), 0o600); err != nil {
				return domain.NewError(v1.CodeInvalidRequest, "无法写入 %s: %v", target, err)
			}

			fmt.Printf("已生成 opsd 配置骨架：%s\n", target)
			fmt.Printf("下一步：opsctl config validate --file %s\n", target)
			if remote {
				fmt.Println("remote 段已取消注释：占位的证书路径必须先换成真实文件，否则 opsd 启动必然失败。")
			}
			return nil
		},
	}

	cmd.Flags().BoolVar(&force, "force", false, "目标文件已存在时覆盖它")
	cmd.Flags().BoolVar(&remote, "remote", false, "remote 段生成取消注释的版本（占位地址与证书路径，需自行填真实值）")
	return cmd
}
