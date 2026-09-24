// frz-probe 的 Java 版：真机验证里扮演「被 opsd 托管的 JVM 应用」（迭代 3c）。
//
// 它存在的理由：JVM 这一档能给出 Go 探针给不出的证据——
//
//   · `-jar app.jar` 能按**工作目录**解析到制品，因此这一条直接证明了 systemd 的
//     WorkingDirectory= 真的交到了 JVM 手里（Java 的 workingDirectory 推荐写成 release 的
//     current，就是为了这个）；
//   · `Runtime.maxMemory()` 说明 JVM 真的按 `-Xmx` 设了堆上限——而 argv 里那个
//     `-Xmx256m` 若能不被改写地到达 JVM，也就说明 D4 的「只解析 argv[0]」在真机上成立；
//   · 报告写进**自己的工作目录**，因此「工作目录对运行用户可写」也是一条断言。
//
// 它只打印非敏感事实：运行用户、工作目录、端口、Java 版本、堆上限、一个非敏感标记
// 环境变量。**凭据值不打印**，与 Go 探针同一条纪律。
//
// 编译与打包在主机上用真 JDK 完成（test/host/verify.sh），因此这个文件不是制品的一部分。

import java.io.IOException;
import java.net.InetSocketAddress;
import java.net.ServerSocket;
import java.nio.charset.StandardCharsets;
import java.nio.file.Files;
import java.nio.file.Path;
import java.nio.file.Paths;

public final class JavaProbe {
    public static void main(String[] args) throws IOException {
        String listen = null;
        for (int i = 0; i + 1 < args.length; i++) {
            if (args[i].equals("--listen")) {
                listen = args[i + 1];
            }
        }
        if (listen == null) {
            System.err.println("必须提供 --listen host:port");
            System.exit(2);
        }
        int port = Integer.parseInt(listen.substring(listen.lastIndexOf(':') + 1));

        String marker = System.getenv("PROBE_MARKER");
        if (marker == null) {
            marker = "<missing>";
        }

        // 报告落在**自己的工作目录**里（= release 的 current）。报告写完再监听：
        // 与 Go 探针同一个顺序——就绪探测通过就意味着报告已经落盘，断言不必与启动时序赛跑。
        Path report = Paths.get(System.getProperty("user.dir"), "java-probe-report.txt");
        String body = String.join("\n",
                "user=" + System.getProperty("user.name"),
                "dir=" + System.getProperty("user.dir"),
                "port=" + port,
                "java=" + System.getProperty("java.version"),
                "maxMemory=" + Runtime.getRuntime().maxMemory(),
                "marker=" + marker) + "\n";
        Files.write(report, body.getBytes(StandardCharsets.UTF_8));
        System.out.print(body);

        ServerSocket server = new ServerSocket();
        server.setReuseAddress(true);
        server.bind(new InetSocketAddress("127.0.0.1", port));
        while (true) {
            try {
                // 就绪探测只做 TCP 连接，连接内容无关紧要；立刻关掉避免占满队列。
                server.accept().close();
            } catch (IOException ignored) {
                // 单个连接出错不该让服务退出：它是长驻进程，systemd 会一直拿它当活的。
            }
        }
    }
}
