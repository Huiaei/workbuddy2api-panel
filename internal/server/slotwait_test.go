package server

import (
	"context"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
)

// TestSlotWaitForRealmDetectsInFlightCap 池里「号健康但名额占满」必须被
// SlotWaitForRealm 识别出来，调用方据此等一个名额而不是直接 503（issue #117）。
func TestSlotWaitForRealmDetectsInFlightCap(t *testing.T) {
	p := pool.New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.SetMaxInFlight(1)

	if p.SlotWaitForRealm("m1", "") {
		t.Fatal("名额未占满时不应报告需要等名额")
	}
	if !p.Acquire("u1") {
		t.Fatal("Acquire 应成功")
	}
	if !p.SlotWaitForRealm("m1", "") {
		t.Fatal("名额占满且号健康时应报告需要等名额")
	}
	p.Release("u1")
	if p.SlotWaitForRealm("m1", "") {
		t.Fatal("名额释放后不应再报告需要等名额")
	}
}

// TestSlotWaitForRealmIgnoresDisabledAndCooling 号被停用/冷却时不构成
// 「只差名额」——等下去不会变好，不该让请求干耗。
func TestSlotWaitForRealmIgnoresDisabledAndCooling(t *testing.T) {
	p := pool.New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.SetMaxInFlight(1)
	if !p.Acquire("u1") {
		t.Fatal("Acquire 应成功")
	}
	p.Disable("u1", "test")
	if p.SlotWaitForRealm("m1", "") {
		t.Fatal("已停用的号不应报告需要等名额")
	}
}

// TestWaitForSlotReturnsWhenSlotFreed 名额被释放后 waitForSlot 应尽快返回 true。
func TestWaitForSlotReturnsWhenSlotFreed(t *testing.T) {
	p := pool.New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.SetMaxInFlight(1)
	if !p.Acquire("u1") {
		t.Fatal("Acquire 应成功")
	}
	go func() {
		time.Sleep(120 * time.Millisecond)
		p.Release("u1")
	}()
	start := time.Now()
	if !waitForSlot(context.Background(), p, "m1", "") {
		t.Fatal("名额释放后 waitForSlot 应返回 true")
	}
	if d := time.Since(start); d > 3*time.Second {
		t.Fatalf("waitForSlot 等待过久: %v", d)
	}
}

// TestWaitForSlotRespectsContextCancel 客户端断连时 waitForSlot 应立即返回 false，
// 不把请求挂在死等上。
func TestWaitForSlotRespectsContextCancel(t *testing.T) {
	p := pool.New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.SetMaxInFlight(1)
	if !p.Acquire("u1") {
		t.Fatal("Acquire 应成功")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 80*time.Millisecond)
	defer cancel()
	start := time.Now()
	if waitForSlot(ctx, p, "m1", "") {
		t.Fatal("ctx 取消后 waitForSlot 应返回 false")
	}
	if d := time.Since(start); d > 3*time.Second {
		t.Fatalf("ctx 取消后未及时返回: %v", d)
	}
}

// TestWaitForSlotIsBounded 上限必须生效：名额一直被占时 waitForSlot 到点返回，
// 不会把请求挂死。
func TestWaitForSlotIsBounded(t *testing.T) {
	old := slotWaitCap
	slotWaitCap = 200 * time.Millisecond
	defer func() { slotWaitCap = old }()

	p := pool.New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.SetMaxInFlight(1)
	if !p.Acquire("u1") {
		t.Fatal("Acquire 应成功")
	}
	start := time.Now()
	waitForSlot(context.Background(), p, "m1", "")
	if d := time.Since(start); d > 3*time.Second {
		t.Fatalf("waitForSlot 超过上限: %v", d)
	}
}
