package model

import "time"

// ── 서버 모니터링 (2단계) ─────────────────────

// MetricPoint는 서버 또는 서비스 하나의 한 때다. 기록할 때는 그 순간의 값, 화면에서는 칸 하나의 평균·최고.
type MetricPoint struct {
	At       time.Time
	CPU      float64 // % — 서버 전체를 100%로 본다
	CPUMax   float64
	MemUsed  uint64
	MemMax   uint64
	MemTotal uint64
	// 서버만
	DiskUsed, DiskTotal uint64
	NetIn, NetOut       uint64 // 기록: 웹서버 컨테이너가 받고 보낸 누적 바이트 · 화면: 그 칸 동안 받고 보낸 양
}

// MetricRange는 서버 화면의 기간이다.
type MetricRange string

const (
	RangeHour  MetricRange = "1h"
	RangeDay   MetricRange = "24h"
	RangeWeek  MetricRange = "7d"
	RangeMonth MetricRange = "30d"
)

// AllRanges는 화면에 보일 순서다.
func AllRanges() []MetricRange { return []MetricRange{RangeHour, RangeDay, RangeWeek, RangeMonth} }

// ParseMetricRange는 모르는 값이면 24시간이다.
func ParseMetricRange(s string) MetricRange {
	for _, r := range AllRanges() {
		if string(r) == s {
			return r
		}
	}
	return RangeDay
}

// Span은 그 기간의 길이, Step은 그래프 칸 하나의 길이다 (칸은 60~96개).
func (r MetricRange) Span() time.Duration {
	switch r {
	case RangeHour:
		return time.Hour
	case RangeWeek:
		return 7 * 24 * time.Hour
	case RangeMonth:
		return 30 * 24 * time.Hour
	}
	return 24 * time.Hour
}

func (r MetricRange) Step() time.Duration {
	switch r {
	case RangeHour:
		return time.Minute
	case RangeWeek:
		return 2 * time.Hour
	case RangeMonth:
		return 8 * time.Hour
	}
	return 15 * time.Minute
}

// ServiceMetrics는 서비스 하나의 칸마다 값과 그 기간에 다시 시작한 횟수다.
type ServiceMetrics struct {
	Service  ServiceID
	Name     string
	Points   []MetricPoint
	Restarts int
	OOM      int // 그중 메모리 부족
}

// MonitorView는 서버 화면이다. 칸은 오래된 것부터 — 기록이 없는 칸은 빠진다.
type MonitorView struct {
	Range    MetricRange
	From, To time.Time
	Step     time.Duration
	Server   []MetricPoint
	Services []ServiceMetrics
	Events   []AuditEntry // 그 기간에 Naru가 본 일 — 최근 것부터
}

// AuditEntry는 기록 한 줄이다 — 관리자가 한 일 또는 Naru가 본 일. 비밀 값은 담지 않는다.
type AuditEntry struct {
	At      time.Time
	System  bool      // Naru가 본 일 (멈춤·배포·디스크 …) — 아니면 관리자가 한 일
	Who     string    // 관리자 이름 (로그인 실패면 적은 이름)
	IP      string    // 관리자가 접속한 곳
	Action  string    // "login" · "service.deploy" · "service.down" … — 화면 문구 키
	Target  string    // 서비스 이름 등
	Service ServiceID // 서비스에 한 일이면
	OK      bool
	Detail  string // 덧붙임 (멈춘 이유 등)
}
