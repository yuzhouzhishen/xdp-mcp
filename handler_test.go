package main

import (
	"testing"

	pb "xdp-mcp/proto/xdp/v1"
)

func TestDisplayBrightnessLevelsForProductFamily(t *testing.T) {
	tests := []struct {
		name          string
		productFamily string
		highIntensity uint32
		ok            bool
	}{
		{name: "CP02", productFamily: "cp02", highIntensity: 100, ok: true},
		{name: "CP02s", productFamily: "cp02s", highIntensity: 30, ok: true},
		{name: "unknown", productFamily: "pa768s", ok: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			levels, ok := displayBrightnessLevelsForProductFamily(tt.productFamily)
			if ok != tt.ok {
				t.Fatalf("ok=%v, want %v", ok, tt.ok)
			}
			if !ok {
				return
			}

			level, ok := displayBrightnessLevelByName(levels, "高")
			if !ok {
				t.Fatal("missing high brightness level")
			}
			if level.Intensity != tt.highIntensity {
				t.Fatalf("high intensity=%d, want %d", level.Intensity, tt.highIntensity)
			}
		})
	}
}

func TestDisplayBrightnessLevelByIntensity(t *testing.T) {
	tests := []struct {
		name          string
		productFamily string
		intensity     uint32
		level         string
	}{
		{name: "CP02S off", productFamily: "cp02s", intensity: 0, level: "关"},
		{name: "CP02S low", productFamily: "cp02s", intensity: 5, level: "低"},
		{name: "CP02S medium", productFamily: "cp02s", intensity: 15, level: "中"},
		{name: "CP02S high", productFamily: "cp02s", intensity: 30, level: "高"},
		{name: "CP02S legacy high", productFamily: "cp02s", intensity: 100, level: "高"},
		{name: "CP02 high", productFamily: "cp02", intensity: 100, level: "高"},
		{name: "CP02 legacy high", productFamily: "cp02", intensity: 128, level: "高"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			levels, ok := displayBrightnessLevelsForProductFamily(tt.productFamily)
			if !ok {
				t.Fatalf("missing %s brightness levels", tt.productFamily)
			}

			level := displayBrightnessLevelByIntensity(levels, tt.intensity)
			if level.Name != tt.level {
				t.Fatalf("level=%s, want %s", level.Name, tt.level)
			}
		})
	}
}

func TestCP02SDisplayModeOptions(t *testing.T) {
	tests := []struct {
		name     string
		duration uint32
		ok       bool
	}{
		{name: "待机动画优先", duration: 30, ok: true},
		{name: "功率显示优先", duration: 0, ok: true},
		{name: "invalid", ok: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mode, ok := cp02sDisplayModeByName(tt.name)
			if ok != tt.ok {
				t.Fatalf("ok=%v, want %v", ok, tt.ok)
			}
			if !ok {
				return
			}
			if mode.Duration != tt.duration {
				t.Fatalf("duration=%d, want %d", mode.Duration, tt.duration)
			}
			if got := cp02sDisplayModeByDuration(tt.duration); got.Name != tt.name {
				t.Fatalf("mode=%s, want %s", got.Name, tt.name)
			}
		})
	}
}

func TestCP02SIdleDisplayOptions(t *testing.T) {
	tests := []struct {
		name string
		ok   bool
	}{
		{name: "流星", ok: true},
		{name: "落花", ok: true},
		{name: "康威的生命游戏", ok: true},
		{name: "时间", ok: true},
		{name: "invalid", ok: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			idleDisplay, ok := cp02sIdleDisplayByName(tt.name)
			if ok != tt.ok {
				t.Fatalf("ok=%v, want %v", ok, tt.ok)
			}
			if !ok {
				return
			}
			if got := cp02sIdleDisplayByAnimation(idleDisplay.Animation); got.Name != tt.name {
				t.Fatalf("idle display=%s, want %s", got.Name, tt.name)
			}
		})
	}
}

func TestBuildSetUSBAChargingStrategy(t *testing.T) {
	req := BuildSetChargingStrategy(ChargingStrategyUSBACharging)

	payload, ok := req.Payload.(*pb.CommandRequest_SetChargingStrategy)
	if !ok {
		t.Fatalf("payload type=%T, want set_charging_strategy", req.Payload)
	}

	if payload.SetChargingStrategy.GetChargingStrategy() != ChargingStrategyUSBACharging {
		t.Fatalf("strategy=%d, want %d", payload.SetChargingStrategy.GetChargingStrategy(), ChargingStrategyUSBACharging)
	}
}

func TestChargingStrategyDisplayName(t *testing.T) {
	if got := chargingStrategyDisplayName(ChargingStrategyUSBACharging); got != "USBA_CHARGING" {
		t.Fatalf("display name=%s, want USBA_CHARGING", got)
	}
}
