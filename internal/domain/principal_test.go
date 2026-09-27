package domain

import (
	"context"
	"testing"
)

// 「谁做的」只由这一处决定，因此这里逐条把两种来路的行为钉住：
// 本机沿用自报值（迭代 4 的行为），远程一律用证书身份。
func TestClaimedActor(t *testing.T) {
	for _, tc := range []struct {
		name      string
		ctx       context.Context
		createdBy string
		wantActor string
		wantClaim string
	}{
		{
			name:      "没有身份（例如调度器自建）",
			ctx:       context.Background(),
			createdBy: "scheduler",
			wantActor: "scheduler",
		},
		{
			name:      "本机：能连上 socket 就等于本地运维，自报值可信",
			ctx:       WithPrincipal(context.Background(), LocalPrincipal()),
			createdBy: "alice",
			wantActor: "alice",
		},
		{
			name:      "远程：用证书 CN，自报值另记一条线索",
			ctx:       WithPrincipal(context.Background(), remote("opsctl-central")),
			createdBy: "alice",
			wantActor: "remote:opsctl-central",
			wantClaim: "alice",
		},
		{
			name:      "远程：自报值与认证值一致时不重复记",
			ctx:       WithPrincipal(context.Background(), remote("opsctl-central")),
			createdBy: "remote:opsctl-central",
			wantActor: "remote:opsctl-central",
		},
		{
			name:      "远程：没自报时只有认证身份",
			ctx:       WithPrincipal(context.Background(), remote("opsctl-central")),
			createdBy: "  ",
			wantActor: "remote:opsctl-central",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			actor, claimed := ClaimedActor(tc.ctx, tc.createdBy)
			if actor != tc.wantActor {
				t.Fatalf("actor: want %q, got %q", tc.wantActor, actor)
			}
			if claimed != tc.wantClaim {
				t.Fatalf("claimedBy: want %q, got %q", tc.wantClaim, claimed)
			}
		})
	}
}

func remote(cn string) Principal {
	return Principal{Kind: PrincipalKindRemote, Name: cn, Scope: ScopeWrite}
}

// 空白名单表示「这台机上的全部应用」，而不是「一个都不能碰」——两者搞反会让
// 一份没写 applications 的配置变成谁都进不来，而报出来的是「没被授权」。
func TestAllowsApplication(t *testing.T) {
	open := Principal{}
	if !open.AllowsApplication("anything") {
		t.Fatal("空白名单应当表示全部应用")
	}

	scoped := Principal{Applications: []string{"orders-api"}}
	if !scoped.AllowsApplication("orders-api") {
		t.Fatal("名单里的应用应当放行")
	}
	if scoped.AllowsApplication("billing-api") {
		t.Fatal("名单外的应用应当拒绝")
	}
	// 应用名是标识符，不做大小写不敏感的近似匹配：授权判断上的近似等于没有判断。
	if scoped.AllowsApplication("ORDERS-API") {
		t.Fatal("大小写不同就是不同的应用名")
	}
}

// 写包含读，读不包含写。这条矩阵是档位判断的全部内容。
func TestAccessScopeAllows(t *testing.T) {
	cases := []struct {
		have AccessScope
		want AccessScope
		ok   bool
	}{
		{ScopeWrite, ScopeWrite, true},
		{ScopeWrite, ScopeRead, true},
		{ScopeRead, ScopeRead, true},
		{ScopeRead, ScopeWrite, false},
		{"", ScopeRead, false},
		{"admin", ScopeRead, false},
	}
	for _, tc := range cases {
		if got := tc.have.Allows(tc.want); got != tc.ok {
			t.Fatalf("%q.Allows(%q): want %v, got %v", tc.have, tc.want, tc.ok, got)
		}
	}
}

// Validate 只认两个取值：配置里写错档位必须当场被挡，而不是退化成一个更宽的档位。
func TestAccessScopeValid(t *testing.T) {
	for _, valid := range []AccessScope{ScopeRead, ScopeWrite} {
		if !valid.Valid() {
			t.Fatalf("%q 应当是合法档位", valid)
		}
	}
	for _, invalid := range []AccessScope{"", "admin", "READ"} {
		if invalid.Valid() {
			t.Fatalf("%q 不该是合法档位", invalid)
		}
	}
}

func TestPrincipalFromDistinguishesAbsentFromEmpty(t *testing.T) {
	if _, ok := PrincipalFrom(context.Background()); ok {
		t.Fatal("没有身份时 ok 必须是 false：它要与「认证成空身份」区分开")
	}
	ctx := WithPrincipal(context.Background(), Principal{Kind: PrincipalKindRemote})
	principal, ok := PrincipalFrom(ctx)
	if !ok {
		t.Fatal("放进去的身份应当取得到")
	}
	if !principal.IsRemote() {
		t.Fatal("取出来的身份应当仍然是远程")
	}
}
