package pool

import (
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// 本文件钉住**交叉审计第二轮**在「积分燃尽熔断（burn guard）」这条线上抓到的
// 两个可观测性/可运维性缺陷：
//
//   - D10：熔断号在面板两处被计为「健康 / 可服务」——运维看面板以为还有 1 个
//     健康号，实际那号根本选不出来，排障方向完全错。
//   - D11：面板「解冻」（Revive）与「删号重加」（Remove）都不清熔断状态 ⇒
//     运维点了恢复，号依然选不出来，且没有任何提示说明还差 cooldown 到期。

// TestBurnBlockedAccountIsNotCountedAsHealthy 熔断号不得被计入 healthy。
//
// 熔断（burn guard）是**选号层面**的排除：burnBlocked 在各选号路径上都生效，
// 但账号本身 healthy（可登入、可签到）。聚合口径若把它算成 healthy，面板
// 总览就会显示「还有 1 个健康号」，而实际上号选不出来。
func TestBurnBlockedAccountIsNotCountedAsHealthy(t *testing.T) {
	p := New("")
	a := &auth.Auth{UID: "u1"}
	p.Add(a)
	p.SetBurnGuard(50, time.Minute, time.Minute, time.Hour)

	if !p.NoteBurn(a.UID, 60) {
		t.Fatal("累计 60 > 阈值 50 应触发熔断")
	}
	if p.Pick() != nil {
		t.Fatal("熔断期内账号不应被选中")
	}

	total, healthy, cooling, disabled, paused, inFlightFull := p.CountsDetailedWithPaused()
	if total != 1 {
		t.Fatalf("total=%d want 1", total)
	}
	if healthy != 0 {
		t.Fatalf("熔断号不得计为 healthy（healthy=%d）", healthy)
	}
	if cooling != 1 {
		t.Fatalf("熔断号应计为 cooling（cooling=%d）", cooling)
	}
	if disabled != 0 || paused != 0 {
		t.Fatalf("熔断 ≠ 禁用/暂停（disabled=%d paused=%d）", disabled, paused)
	}
	_ = inFlightFull
}

// TestBurnBlockedAccountIsNotCountedAsServableForModel 模型维度的可服务数
// 也不得把熔断号算进去（面板「模型锁池」表）。
func TestBurnBlockedAccountIsNotCountedAsServableForModel(t *testing.T) {
	p := New("")
	burned := &auth.Auth{UID: "u1"}
	auth.BackfillRealmFor(burned, "global")
	other := &auth.Auth{UID: "u2"}
	auth.BackfillRealmFor(other, "global")
	p.Add(burned)
	p.Add(other)
	p.SetBurnGuard(50, time.Minute, time.Minute, time.Hour)

	if !p.NoteBurn(burned.UID, 60) {
		t.Fatal("累计 60 > 阈值 50 应触发熔断")
	}
	// u2 被该模型挡住 ⇒ 生成一行 glm-5.2 的锁池记录（Total=2, Locked=1）。
	p.BlockModelBackoff(other.UID, "glm-5.2", "11102 model [glm-5.2] service info not found")

	rows := p.ModelLockView()
	var found bool
	for _, row := range rows {
		if row.Model != "glm-5.2" {
			continue
		}
		found = true
		// u1 熔断（选号层面不可用）+ u2 被该模型挡住 ⇒ 实际可服务 0 个。
		if row.Servable != 0 {
			t.Fatalf("熔断号不得计入 Servable（servable=%d，row=%+v）", row.Servable, row)
		}
	}
	if !found {
		t.Fatalf("未找到 glm-5.2 的模型锁池行: %+v", rows)
	}
}

// TestReviveClearsBurnGuard 「解冻」必须一并解除积分燃尽熔断。
//
// 面板「恢复账号」的语义是「运维口径无条件恢复」（internal/panel/panel.go
// 的 accountRevive 注释原文如此）。不清熔断的话，运维点了恢复，账号**依然
// 选不出来**，而且没有任何提示说明还差 cooldown 到期 —— 运维会以为按钮坏了。
func TestReviveClearsBurnGuard(t *testing.T) {
	p := New("")
	a := &auth.Auth{UID: "u1"}
	p.Add(a)
	p.SetBurnGuard(50, time.Minute, time.Minute, time.Hour)

	if !p.NoteBurn(a.UID, 60) {
		t.Fatal("累计 60 > 阈值 50 应触发熔断")
	}
	if p.Pick() != nil {
		t.Fatal("熔断期内账号不应被选中")
	}

	if !p.Revive("u1") {
		t.Fatal("Revive 应返回 true")
	}
	if p.Pick() == nil {
		t.Fatal("解冻后账号应重新可选（熔断状态必须一并清除）")
	}
	st := p.statusOf("u1", p.byUID["u1"])
	if st.Cooling {
		t.Fatalf("解冻后不应仍处于冷却：%+v", st)
	}
}

// TestRemoveClearsBurnGuard 删号后重新导入同一账号，不该还挂着旧熔断。
//
// 否则「删了重加」仍选不出来，与面板「恢复账号」的运维直觉相反。
func TestRemoveClearsBurnGuard(t *testing.T) {
	p := New("")
	a := &auth.Auth{UID: "u1"}
	p.Add(a)
	p.SetBurnGuard(50, time.Minute, time.Minute, time.Hour)

	if !p.NoteBurn(a.UID, 60) {
		t.Fatal("累计 60 > 阈值 50 应触发熔断")
	}

	if got := p.Remove("u1"); got == nil {
		t.Fatal("Remove 应返回账号")
	}
	// 重新导入同一账号（模拟「删号重加」）。
	p.Add(&auth.Auth{UID: "u1"})
	if p.Pick() == nil {
		t.Fatal("删号重加后账号应可选（旧熔断必须被清除）")
	}
}
