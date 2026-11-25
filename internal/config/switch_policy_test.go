package config

import "testing"

// 换号策略：每一类触发的默认值必须等于「改动前行为」，不能悄悄改变语义。

func TestSwitchPolicyDefaults(t *testing.T) {
	var p SwitchPolicy

	if got := p.ReserveAmountValue(); got != 10 {
		t.Errorf("余额保底默认应为 10（v1.46.0 从 20 下调），得到 %v", got)
	}
	if p.ReserveStrictOn() {
		t.Error("严格模式默认应为关闭")
	}
	if !p.ProtectCacheOn() {
		t.Error("缓存保护默认应开启")
	}
	if got := p.CacheKeepRateValue(); got != 0.5 {
		t.Errorf("缓存保护门槛默认应为 0.5，得到 %v", got)
	}
	if !p.CooldownOnErrorOn() {
		t.Error("出错冷却默认应开启")
	}
	if !p.LoadBalanceOn() {
		t.Error("负载均衡默认应开启")
	}
	if got := p.MaxAttemptsValue(); got != 3 {
		t.Errorf("最大尝试账号数默认应为 3，得到 %d", got)
	}
}

// 显式设成 0 的保底值回落到默认，避免「设 0 = 完全不保底」的歧义。
func TestReserveAmountZeroFallsBack(t *testing.T) {
	v := 0.0
	p := SwitchPolicy{ReserveAmount: &v}
	if got := p.ReserveAmountValue(); got != 10 {
		t.Errorf("0 应回落到默认 10，得到 %v", got)
	}
}

// 自定义值生效。
func TestSwitchPolicyCustom(t *testing.T) {
	amt := 200.0
	strict := true
	protect := false
	rate := 0.8
	cool := false
	load := false
	att := 5

	p := SwitchPolicy{
		ReserveAmount:   &amt,
		ReserveStrict:   &strict,
		ProtectCache:    &protect,
		CacheKeepRate:   &rate,
		CooldownOnError: &cool,
		LoadBalance:     &load,
		MaxAttempts:     &att,
	}
	if p.ReserveAmountValue() != 200 || !p.ReserveStrictOn() || p.ProtectCacheOn() ||
		p.CacheKeepRateValue() != 0.8 || p.CooldownOnErrorOn() || p.LoadBalanceOn() ||
		p.MaxAttemptsValue() != 5 {
		t.Errorf("自定义值未全部生效: %+v", p)
	}
}

// 越界值要被夹住，防止把账号锁死或无限重试。
func TestSwitchPolicyBounds(t *testing.T) {
	bad := 1.5
	p := SwitchPolicy{CacheKeepRate: &bad}
	if got := p.CacheKeepRateValue(); got != 0.5 {
		t.Errorf(">1 的命中率门槛应回落默认，得到 %v", got)
	}

	neg := -0.1
	p2 := SwitchPolicy{CacheKeepRate: &neg}
	if got := p2.CacheKeepRateValue(); got != 0.5 {
		t.Errorf("负数门槛应回落默认，得到 %v", got)
	}

	tooMany := 100
	p3 := SwitchPolicy{MaxAttempts: &tooMany}
	if got := p3.MaxAttemptsValue(); got != 10 {
		t.Errorf("尝试数应封顶 10，得到 %d", got)
	}

	zero := 0
	p4 := SwitchPolicy{MaxAttempts: &zero}
	if got := p4.MaxAttemptsValue(); got != 3 {
		t.Errorf("0 应回落默认 3，得到 %d", got)
	}
}

// 旧字段（creditReserveAmount / creditReserveStrict）仍要生效，不能因换结构丢配置。
func TestSwitchPolicyOfLegacyCompat(t *testing.T) {
	legacyAmt := 88.0
	legacyStrict := true
	s := &Server{
		CreditReserveAmount: &legacyAmt,
		CreditReserveStrict: &legacyStrict,
	}
	p := s.SwitchPolicyOf()
	if p.ReserveAmountValue() != 88 {
		t.Errorf("旧字段余额保底应被继承，得到 %v", p.ReserveAmountValue())
	}
	if !p.ReserveStrictOn() {
		t.Error("旧字段严格模式应被继承")
	}
}

// 新字段优先于旧字段。
func TestSwitchPolicyPrefersNew(t *testing.T) {
	legacyAmt := 88.0
	newAmt := 30.0
	s := &Server{
		CreditReserveAmount: &legacyAmt,
		SwitchPolicy:        SwitchPolicy{ReserveAmount: &newAmt},
	}
	if got := s.SwitchPolicyOf().ReserveAmountValue(); got != 30 {
		t.Errorf("新字段应优先，得到 %v", got)
	}
}
