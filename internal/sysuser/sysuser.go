// Package sysuser 把「运行用户」这个名字解析成 uid/gid。
//
// 单独成包是因为有两个适配器要用它：runtime/systemd 装 unit 时要 chown 目录与凭据，
// release/local 建 release 目录时要 chown 解出来的文件。**同一件事只实现一份**——
// 「这个文件到底属于谁」在两处各算一遍，迟早会有一处先漂移，而它的表现是权限错误，
// 最难查的一类故障。
package sysuser

import (
	"os/user"
	"strconv"

	v1 "github.com/freezeChen/frz-tools/api/v1"
	"github.com/freezeChen/frz-tools/internal/domain"
)

// Resolver 把 runUser 解析成 uid/gid。
//
// 做成可注入的，是因为测试进程通常不是 root，无法把文件 chown 给别的用户；
// 测试注入当前进程的 uid/gid，从而仍能断言「chown 真的被调用过、模式真的落实了」。
type Resolver func(name string) (uid, gid int, err error)

// OSLookup 是生产实现：查系统用户数据库——useradd 写进去的就是同一份数据。
func OSLookup(name string) (int, int, error) {
	account, err := user.Lookup(name)
	if err != nil {
		return 0, 0, domain.NewError(v1.CodeInternal, "解析用户 %q 失败: %v", name, err)
	}
	uid, err := strconv.Atoi(account.Uid)
	if err != nil {
		return 0, 0, domain.NewError(v1.CodeInternal, "用户 %q 的 uid 不是数字: %q", name, account.Uid)
	}
	gid, err := strconv.Atoi(account.Gid)
	if err != nil {
		return 0, 0, domain.NewError(v1.CodeInternal, "用户 %q 的 gid 不是数字: %q", name, account.Gid)
	}
	return uid, gid, nil
}
