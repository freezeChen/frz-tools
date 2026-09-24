package e2e

import (
	"database/sql"
	"testing"

	_ "modernc.org/sqlite"
)

// 本机 Host 记录由 opsd 启动时自举，属于守护进程侧的接线；用例层的幂等性由
// internal/application 的单元测试覆盖，这里证明真实二进制启动时确实执行了自举，
// 并且重启不会产生第二行。
func TestDaemonBootstrapsLocalHost(t *testing.T) {
	d := newDaemon(t)
	d.start(t)

	assertLocalHosts(t, d.database, 1)

	d.kill(t)
	d.start(t)

	assertLocalHosts(t, d.database, 1)
}

func assertLocalHosts(t *testing.T, database string, want int) {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+database)
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	defer db.Close()

	rows, err := db.Query(`SELECT name, address FROM hosts`)
	if err != nil {
		t.Fatalf("query hosts: %v", err)
	}
	defer rows.Close()

	type host struct{ name, address string }
	var hosts []host
	for rows.Next() {
		var h host
		if err := rows.Scan(&h.name, &h.address); err != nil {
			t.Fatalf("scan host: %v", err)
		}
		hosts = append(hosts, h)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate hosts: %v", err)
	}

	if len(hosts) != want {
		t.Fatalf("want %d host records, got %d: %+v", want, len(hosts), hosts)
	}
	if want == 0 {
		return
	}
	if hosts[0].name != "local" || hosts[0].address != "" {
		t.Fatalf("want the local host named %q with an empty address, got %+v", "local", hosts[0])
	}
}
