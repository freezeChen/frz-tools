// frz-probe 是 Linux 容器验证里扮演「被 opsd 托管的应用」的探针。
//
// 它的职责是把「进程实际收到了什么」写成可断言的事实，而**不打印凭据值**：
// 报告里只有长度与 sha256，凭据明文永远不出现在文件、日志或断言输出中。
//
// --allow-write / --deny-write 可以重复出现，一次启动探多条路径。
//
// 它同时扮演一个真实的长驻服务：先写报告、再监听就绪端口。这个顺序是必需的——
// 就绪探测通过就意味着报告已经落盘，断言不必与启动时序赛跑。
package main

import (
	"crypto/sha256"
	"encoding/hex"
	"flag"
	"fmt"
	"net"
	"os"
	"strings"
)

func main() {
	reportPath := flag.String("report", "", "报告文件路径（必须落在 unit 的可写路径内）")
	listenAddr := flag.String("listen", "", "就绪监听地址 host:port")
	hashEnv := flag.String("hash-env", "", "对该环境变量的值取长度与 sha256")
	hashFileEnv := flag.String("hash-file-env", "", "把该环境变量的值当作路径，读取文件内容后取长度与 sha256")
	// 这两条**可重复**：一次启动里探多个路径是常态（例如「strict 档下 /var 也不许写，
	// legacy 档下 /usr 不许写」要靠两条不同的路径分别证明）。Go 的 flag 默认是「后一个
	// 覆盖前一个」，用 flag.String 的话第二条会静默吃掉第一条——断言于是少测一条，
	// 而且看起来是绿的。2026-09-24 在 CentOS 7 上真的踩到了。
	var allowWrite, denyWrite stringList
	flag.Var(&allowWrite, "allow-write", "期望可写的路径（unit 已声明）；可重复")
	flag.Var(&denyWrite, "deny-write", "期望不可写的路径（unit 未声明，ProtectSystem 应拦截）；可重复")
	// httpVersion 非空时，每个连接回一句 HTTP 200 + "version=<值>" 再关闭。
	// 蓝绿验证靠它回答「现在服务的是哪一版」——就绪探测只连接、不读内容，
	// 因此这一项对既有的 TCP 就绪断言没有影响。
	httpVersion := flag.String("http-version", "", "在该端口上应答 HTTP，正文为 version=<值>")
	flag.Parse()

	// 监听地址与版本可以**从环境变量取**（flag 为空时）。
	//
	// 这不是语法糖：蓝绿的两个槽位共用同一份 argv，而它们必须听不同的端口——端口只能
	// 从「每个槽位自己的环境」里来。这也正是规格 D6 里那条契约的实样：「应用怎么知道
	// 自己的端口」由运维通过槽位环境告诉它，工具不发明魔法变量名。探针作为「被托管的
	// 应用」，示范的就是这个配合。
	if *listenAddr == "" {
		*listenAddr = os.Getenv("FRZ_PROBE_LISTEN")
	}
	if *httpVersion == "" {
		*httpVersion = os.Getenv("FRZ_PROBE_VERSION")
	}
	if *reportPath == "" || *listenAddr == "" {
		fmt.Fprintln(os.Stderr, "必须提供 --report 与 --listen（或 FRZ_PROBE_LISTEN 环境变量）")
		os.Exit(2)
	}

	var lines []string

	if *hashEnv != "" {
		value, ok := os.LookupEnv(*hashEnv)
		if !ok {
			lines = append(lines, fmt.Sprintf("env:%s missing", *hashEnv))
		} else {
			lines = append(lines, fmt.Sprintf("env:%s len=%d sha256=%s", *hashEnv, len(value), digest(value)))
		}
	}

	if *hashFileEnv != "" {
		path, ok := os.LookupEnv(*hashFileEnv)
		switch {
		case !ok:
			lines = append(lines, fmt.Sprintf("file-env:%s missing", *hashFileEnv))
		default:
			absolute := "no"
			if strings.HasPrefix(path, "/") {
				absolute = "yes"
			}
			// kind=file 的环境变量里应当是路径，由应用自己按路径读取：
			// 这一次读取就是以运行用户身份完成的，是真正的跨用户可读性证据。
			content, err := os.ReadFile(path)
			if err != nil {
				lines = append(lines, fmt.Sprintf("file-env:%s read-error=yes path-absolute=%s", *hashFileEnv, absolute))
			} else {
				lines = append(lines, fmt.Sprintf("file-env:%s len=%d sha256=%s path-absolute=%s",
					*hashFileEnv, len(content), digest(string(content)), absolute))
			}
		}
	}

	for _, path := range allowWrite {
		lines = append(lines, fmt.Sprintf("allow-write %s writable=%s", path, writable(path)))
	}
	for _, path := range denyWrite {
		lines = append(lines, fmt.Sprintf("deny-write %s writable=%s", path, writable(path)))
	}

	if err := os.WriteFile(*reportPath, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		// 报告写不出来说明可写路径的判断本身就是错的，直接让 unit 失败，
		// 而不是留一个「看起来在跑」的进程让断言去猜。
		fmt.Fprintf(os.Stderr, "写报告失败：%v\n", err)
		os.Exit(1)
	}

	listener, err := net.Listen("tcp", *listenAddr)
	if err != nil {
		fmt.Fprintf(os.Stderr, "监听 %s 失败：%v\n", *listenAddr, err)
		os.Exit(1)
	}
	for {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		if *httpVersion != "" {
			body := "version=" + *httpVersion + "\n"
			_, _ = fmt.Fprintf(conn, "HTTP/1.1 200 OK\r\nContent-Type: text/plain\r\nContent-Length: %d\r\nConnection: close\r\n\r\n%s",
				len(body), body)
		}
		// 就绪探测只做 TCP 连接，连接内容无关紧要；立刻关掉避免占满队列。
		_ = conn.Close()
	}
}

// writable 尝试真正落盘一个字节，返回 yes/no。
func writable(path string) string {
	if err := os.WriteFile(path, []byte("probe\n"), 0o600); err != nil {
		return "no"
	}
	return "yes"
}

// stringList 是「可重复的字符串旗标」。
type stringList []string

func (l *stringList) String() string { return strings.Join(*l, ",") }

func (l *stringList) Set(value string) error {
	*l = append(*l, value)
	return nil
}

func digest(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}
