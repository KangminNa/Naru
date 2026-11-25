package model

import (
	"strconv"
	"strings"
	"time"
)

// ── 로그 (M6-2) ─────────────────────────────

type LogSource string

const (
	LogApp     LogSource = "app"     // 앱이 찍은 것 (컨테이너 출력)
	LogRequest LogSource = "request" // 웹서버가 받은 요청
)

// LogLine은 로그 한 줄이다. 요청이면 Method·Path·Status·Took이 채워진다.
type LogLine struct {
	At     time.Time
	Source LogSource
	Stream string // 앱: "stdout" · "stderr"
	Text   string // 앱: 찍은 글자 그대로
	Method string
	Path   string // 쿼리 문자열 포함 — 내 서버의 로그다
	Status int
	Took   time.Duration
}

// LogFilter는 로그 화면에서 무엇을 볼지다 — 전체 · 앱 · 요청.
type LogFilter string

const (
	LogsAll      LogFilter = "all"
	LogsApp      LogFilter = "app"
	LogsRequests LogFilter = "request"
)

// ParseLogFilter는 모르는 값이면 전체다.
func ParseLogFilter(s string) LogFilter {
	switch LogFilter(s) {
	case LogsApp, LogsRequests:
		return LogFilter(s)
	}
	return LogsAll
}

// LogsView는 서비스 로그 화면이다. 줄은 시간순(오래된 것부터).
type LogsView struct {
	Service      Service
	Filter       LogFilter
	Query        string            // 찾은 글자
	Deployment   DeploymentID      // 보관된 지난 배포를 보고 있으면 그 번호 (0이면 지금 것)
	Live         DeploymentID      // 지금 도는 배포
	Archives     []LogArchiveEntry // 보관된 지난 배포 — 최근 것부터
	Lines        []LogLine
	HasApp       bool   // 앱 출력이 있는 종류인가 (컨테이너 서비스)
	HasRequests  bool   // 주소가 있어 요청 기록이 있을 수 있는가
	AppError     string // 앱 출력을 읽지 못한 이유 (그대로 보여준다)
	RequestError string // 요청 기록을 읽지 못한 이유
}

// LogQuery는 로그 화면에서 고른 것이다 — 무엇을, 어느 배포(0이면 지금 것), 찾을 글자, 받기인지.
type LogQuery struct {
	Show       LogFilter
	Deployment DeploymentID
	Text       string
	Download   bool // 받기 — 화면보다 많은 줄을 준다
}

const logQueryMax = 200

// ParseLogQuery는 주소의 값들을 받는다. 모르는 값은 기본으로, 찾을 글자는 앞뒤 공백을 빼고 200자까지.
func ParseLogQuery(show, deployment, text string) LogQuery {
	q := LogQuery{Show: ParseLogFilter(show), Text: strings.TrimSpace(text)}
	if n, err := strconv.ParseInt(deployment, 10, 64); err == nil && n > 0 {
		q.Deployment = DeploymentID(n)
	}
	if r := []rune(q.Text); len(r) > logQueryMax {
		q.Text = string(r[:logQueryMax])
	}
	return q
}

// Matches는 찾을 글자가 그 줄에 있는가 (대소문자 무시). 글자가 없으면 모두 맞는다.
// 앱 출력은 글자, 요청은 메서드·경로·상태 코드에서 찾는다.
func (q LogQuery) Matches(l LogLine) bool {
	if q.Text == "" {
		return true
	}
	hay := l.Text
	if l.Source == LogRequest {
		hay = l.Method + " " + l.Path + " " + strconv.Itoa(l.Status)
	}
	return strings.Contains(strings.ToLower(hay), strings.ToLower(q.Text))
}

// LogArchiveEntry는 보관된 앱 출력 하나다 — 배포를 바꿀 때 지운 옛 컨테이너의 것.
type LogArchiveEntry struct {
	Deployment DeploymentID
	At         time.Time // 남긴 때
	Bytes      int64     // 압축한 크기
}
