package main

import (
	"testing"
	"time"
)

// 限流只冷却出问题的那个模型，账号对其他模型仍可用。
//
// 上游的文案本身就提示「您也可以切换其他模型继续使用」，所以冷却整个账号会连带
// 停掉本来正常的模型。
func TestThrottleParksOnlyThatModel(t *testing.T) {
	pool := newCredentialPool()
	settings := defaultGatewaySettings()
	settings.RateCooldownMillis = 300_000 // 5 分钟

	pool.failureForModel("codebuddy", "u1", "deepseek-v4.1-flash", failureRate,
		"您的使用量已超出频率限制，将在 2026-09-27 20:03:46 UTC+8 重置，您也可以切换其他模型继续使用。",
		settings, false)

	lane := pool.lanes[laneKey("codebuddy", "u1")]
	if lane == nil {
		t.Fatal("车道不存在")
	}

	// 该模型被停
	if _, cooled := lane.modelCooled("deepseek-v4.1-flash", nowFixture()); !cooled {
		t.Error("deepseek-v4.1-flash 应处于冷却")
	}
	// 其他模型不受影响 —— 这是修复的核心
	for _, other := range []string{"deepseek-v4-pro", "kimi-k3", "glm-5.3"} {
		if _, cooled := lane.modelCooled(other, nowFixture()); cooled {
			t.Errorf("%s 不应被冷却；限流是模型级的", other)
		}
	}
	// 账号级冷却不应被触发
	if !lane.CooldownUntil.IsZero() && nowFixture().Before(lane.CooldownUntil) {
		t.Error("账号级冷却不应因模型限流而触发")
	}
}

// 配额耗尽与认证失败是账号级的，仍应冷却整个账号。
func TestAccountLevelFailuresStillBenchTheAccount(t *testing.T) {
	for _, kind := range []failureKind{failureQuota, failureAuth} {
		pool := newCredentialPool()
		settings := defaultGatewaySettings()
		pool.failureForModel("codebuddy", "u1", "deepseek-v4.1-flash", kind, "余额不足", settings, false)
		lane := pool.lanes[laneKey("codebuddy", "u1")]
		if lane == nil {
			t.Fatal("车道不存在")
		}
		if lane.CooldownUntil.IsZero() || !nowFixture().Before(lane.CooldownUntil) {
			t.Errorf("kind=%v 应触发账号级冷却", kind)
		}
		if len(lane.ModelCooldowns) != 0 {
			t.Errorf("kind=%v 不应只做模型级冷却", kind)
		}
	}
}

// 上游给出重置时间时，冷却应落在那个时刻（而不是固定的 5 分钟）。
func TestCooldownHonoursUpstreamResetHint(t *testing.T) {
	pool := newCredentialPool()
	settings := defaultGatewaySettings()
	settings.RateCooldownMillis = 60_000 // 若解析失败会得到 1 分钟

	// 目标时刻：距现在 2 小时（明显长于配置的 5 分钟，短于配额冷却上限），
	// 并按上游的写法用 UTC+8 表达，这样能区分「采用提示」与「退回配置」。
	target := time.Now().Add(2 * time.Hour).Truncate(time.Second).UTC()
	hintText := target.Add(8*time.Hour).Format("2006-01-02 15:04:05") + " UTC+8"
	reason := "您的使用量已超出频率限制，将在 " + hintText + " 重置，您也可以切换其他模型继续使用。"
	pool.failureForModel("codebuddy", "u1", "m1", failureRate, reason, settings, false)

	lane := pool.lanes[laneKey("codebuddy", "u1")]
	until := lane.ModelCooldowns["m1"]
	if until.IsZero() {
		t.Fatal("未记录冷却到期时间")
	}
	if diff := until.Sub(target); diff > time.Second || diff < -time.Second {
		t.Errorf("到期时间 = %v，want %v（提示按 UTC+8 解读）", until, target)
	}
	if remaining := time.Until(until); remaining < 90*time.Minute {
		t.Errorf("剩余 %v，应接近提示的 2 小时而非配置的 5 分钟", remaining)
	}
}

// 提示时间已过去（时钟偏差）时退回配置时长，不能据此认定「未限流」。
func TestPastResetHintFallsBackToConfiguredCooldown(t *testing.T) {
	pool := newCredentialPool()
	settings := defaultGatewaySettings()
	settings.RateCooldownMillis = 300_000

	reason := "您的使用量已超出频率限制，将在 2000-01-01 00:00:00 UTC+8 重置"
	pool.failureForModel("codebuddy", "u1", "m1", failureRate, reason, settings, false)

	lane := pool.lanes[laneKey("codebuddy", "u1")]
	until := lane.ModelCooldowns["m1"]
	if until.IsZero() {
		t.Fatal("应仍记录冷却（提示已过期不等于未限流）")
	}
	// 过去的提示不能被采用，否则冷却立刻失效；应退回配置的 5 分钟。
	if remaining := time.Until(until); remaining <= 4*time.Minute || remaining > 5*time.Minute {
		t.Errorf("剩余 %v，应退回配置的 5 分钟量级", remaining)
	}
}

// 时间解析的多种写法。
func TestParseUpstreamResetTime(t *testing.T) {
	cases := []struct {
		message string
		wantOK  bool
		year    int
	}{
		{"将在 2026-09-27 20:03:46 UTC+8 重置", true, 2026},
		{"将在 2026-09-27 20:03:46 UTC+08:00 重置", true, 2026},
		{"将于 2026-09-27 20:03:46 重置", true, 2026},
		{"resets at 2026-09-27 20:03:46 UTC+8", true, 2026},
		{"resets at 2026-09-27 20:03:46", true, 2026},
		{"您的使用量已超出频率限制，将在 2026-09-27 20:03:46 UTC+8 重置，您也可以切换其他模型继续使用。", true, 2026},
		{"频率限制，稍后再试", false, 0},
		{"", false, 0},
	}
	for _, c := range cases {
		got, ok := parseUpstreamResetTime(c.message, timeLocal())
		if ok != c.wantOK {
			t.Errorf("%q -> ok=%v, want %v", c.message, ok, c.wantOK)
			continue
		}
		if ok && got.Year() != c.year {
			t.Errorf("%q -> year %d, want %d", c.message, got.Year(), c.year)
		}
	}
}

func nowFixture() time.Time { return time.Now() }

func timeLocal() *time.Location { return time.Local }

func timeUntil(t time.Time) time.Duration { return time.Until(t) }
