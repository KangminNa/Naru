// Package model은 객체들이 주고받는 데이터다. 행동은 "자기 값이 올바르다"를 지키는 것뿐이다.
// 값 객체는 Parse…를 지나야만 만들어진다 — 그래서 잘못된 값이 다른 객체에 닿을 수 없다.
package model

import (
	"net"
	"net/mail"
	"net/url"
	"path"
	"regexp"
	"strconv"
	"strings"
)

// ── 서비스 이름 ─────────────────────────────

// ServiceName은 영문 소문자·숫자·- 31자 이내다. 컨테이너 이름에도 쓰인다.
type ServiceName struct{ v string }

var serviceNamePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,30}$`)

func ParseServiceName(s string) (ServiceName, error) {
	s = strings.ToLower(strings.TrimSpace(s))
	if !serviceNamePattern.MatchString(s) {
		return ServiceName{}, InputError{Field: "name", Code: "name"}
	}
	return ServiceName{s}, nil
}

func (n ServiceName) String() string { return n.v }
func (n ServiceName) IsZero() bool   { return n.v == "" }

var nonNameChars = regexp.MustCompile(`[^a-z0-9-]+`)

// SuggestServiceName은 아무 글자에서 이름으로 쓸 수 있는 모양을 만든다. 쓸 게 없으면 "service".
func SuggestServiceName(from string) ServiceName {
	name := strings.Trim(nonNameChars.ReplaceAllString(strings.ToLower(from), "-"), "-")
	if len(name) > 31 {
		name = strings.Trim(name[:31], "-")
	}
	if name == "" {
		name = "service"
	}
	return ServiceName{name}
}

// WithSuffix는 "blog" → "blog-2". 31자를 넘지 않게 앞을 자른다.
func (n ServiceName) WithSuffix(i int) ServiceName {
	suffix := "-" + strconv.Itoa(i)
	base := n.v
	if len(base)+len(suffix) > 31 {
		base = strings.TrimRight(base[:31-len(suffix)], "-")
	}
	return ServiceName{base + suffix}
}

// ── 도메인 ─────────────────────────────────

// DomainName은 공개 DNS 이름 모양이다. 붙여 넣은 https://…/ 와 대문자는 정리된다.
type DomainName struct{ v string }

var domainPattern = regexp.MustCompile(`^([a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?\.)+[a-z]{2,63}$`)

func ParseDomainName(s string) (DomainName, error) {
	d := strings.ToLower(strings.TrimSpace(s))
	d = strings.TrimPrefix(d, "https://")
	d = strings.TrimPrefix(d, "http://")
	d, _, _ = strings.Cut(d, "/")
	d = strings.TrimSuffix(d, ".")
	if len(d) > 253 || !domainPattern.MatchString(d) {
		return DomainName{}, InputError{Field: "domain", Code: "domain"}
	}
	return DomainName{d}, nil
}

// ParseOptionalDomainName은 빈 칸이면 빈 값, 아니면 검사한다.
func ParseOptionalDomainName(s string) (DomainName, error) {
	if strings.TrimSpace(s) == "" {
		return DomainName{}, nil
	}
	return ParseDomainName(s)
}

func (d DomainName) String() string { return d.v }
func (d DomainName) IsZero() bool   { return d.v == "" }

// FirstLabel은 "blog.example.com" → "blog".
func (d DomainName) FirstLabel() string {
	first, _, _ := strings.Cut(d.v, ".")
	return first
}

// ── 저장소 · 브랜치 · 이미지 ─────────────────

// RepoURL은 git이 옵션이나 로컬 파일로 오해할 수 없는 http(s) 주소다.
type RepoURL struct{ v string }

func ParseRepoURL(s string) (RepoURL, error) {
	s = strings.TrimSpace(s)
	bad := InputError{Field: "repo", Code: "repo"}
	if strings.ContainsAny(s, " \t\n") || strings.HasPrefix(s, "-") {
		return RepoURL{}, bad
	}
	u, err := url.Parse(s)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" || u.User != nil {
		return RepoURL{}, bad
	}
	return RepoURL{s}, nil
}

func (r RepoURL) String() string { return r.v }
func (r RepoURL) IsZero() bool   { return r.v == "" }

// Name은 "https://github.com/me/Blog.git" → "Blog".
func (r RepoURL) Name() string {
	return path.Base(strings.TrimSuffix(strings.TrimSuffix(r.v, "/"), ".git"))
}

// Branch는 -로 시작하지 않고 ..이 없는 브랜치 이름이다.
type Branch struct{ v string }

var branchPattern = regexp.MustCompile(`^[A-Za-z0-9._/-]{1,200}$`)

func ParseBranch(s string) (Branch, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return Branch{}, nil
	}
	if !branchPattern.MatchString(s) || strings.HasPrefix(s, "-") || strings.Contains(s, "..") {
		return Branch{}, InputError{Field: "branch", Code: "branch"}
	}
	return Branch{s}, nil
}

// DefaultBranch는 저장소 종류가 브랜치를 비워 두면 쓰는 값이다.
func DefaultBranch() Branch { return Branch{"main"} }

func (b Branch) String() string { return b.v }
func (b Branch) IsZero() bool   { return b.v == "" }

// ImageRef는 Docker 이미지 이름 모양이다.
type ImageRef struct{ v string }

var imagePattern = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._/:@-]{0,254}$`)

func ParseImageRef(s string) (ImageRef, error) {
	s = strings.TrimSpace(s)
	if !imagePattern.MatchString(s) || strings.Contains(s, "..") {
		return ImageRef{}, InputError{Field: "image", Code: "image"}
	}
	return ImageRef{s}, nil
}

func (r ImageRef) String() string { return r.v }
func (r ImageRef) IsZero() bool   { return r.v == "" }

// Name은 "ghcr.io/me/api:latest" → "api".
func (r ImageRef) Name() string {
	s, _, _ := strings.Cut(r.v, "@")
	base := path.Base(s)
	if i := strings.LastIndex(base, ":"); i > 0 {
		base = base[:i]
	}
	return base
}

// ── 외부 주소 ──────────────────────────────

// ExternalAddress는 "호스트:포트"다 (https://로 시작하면 TLS로 붙는다).
// localhost는 웹서버 컨테이너 안에서 "이 서버"를 가리키는 host.docker.internal로 바뀐다.
type ExternalAddress struct{ v string }

var hostPattern = regexp.MustCompile(`^[a-zA-Z0-9]([a-zA-Z0-9.-]{0,251}[a-zA-Z0-9])?$`)

func ParseExternalAddress(s string) (ExternalAddress, error) {
	bad := InputError{Field: "external", Code: "external"}
	raw := strings.TrimSpace(s)
	scheme := ""
	if rest, ok := strings.CutPrefix(raw, "https://"); ok {
		scheme, raw = "https://", rest
	} else {
		raw = strings.TrimPrefix(raw, "http://")
	}
	raw = strings.TrimSuffix(raw, "/")
	host, portStr, err := net.SplitHostPort(raw)
	if err != nil {
		return ExternalAddress{}, bad
	}
	if p, err := strconv.Atoi(portStr); err != nil || p < 1 || p > 65535 {
		return ExternalAddress{}, bad
	}
	if host == "localhost" || host == "127.0.0.1" || host == "::1" {
		host = "host.docker.internal"
	}
	if net.ParseIP(host) == nil && !hostPattern.MatchString(host) {
		return ExternalAddress{}, bad
	}
	return ExternalAddress{scheme + net.JoinHostPort(host, portStr)}, nil
}

func (a ExternalAddress) String() string { return a.v }
func (a ExternalAddress) IsZero() bool   { return a.v == "" }

// Host는 이름 짓기에 쓰는 호스트의 첫 부분이다.
func (a ExternalAddress) Host() string {
	host, _, _ := net.SplitHostPort(strings.TrimPrefix(a.v, "https://"))
	first, _, _ := strings.Cut(host, ".")
	return first
}

// ── 폴더 경로 ──────────────────────────────

// FolderPath는 저장소 안을 벗어나지 않는 상대 경로다 ("..은 없다"). 빈 값은 저장소 맨 위.
type FolderPath struct{ v string }

func ParseFolderPath(s string) (FolderPath, error) {
	rel := strings.Trim(strings.ReplaceAll(strings.TrimSpace(s), "\\", "/"), "/")
	for _, seg := range strings.Split(rel, "/") {
		if seg == ".." {
			return FolderPath{}, InputError{Field: "folder", Code: "path"}
		}
	}
	return FolderPath{path.Clean("/" + rel)[1:]}, nil
}

func (p FolderPath) String() string { return p.v }

// ── 포트 · 이메일 · 계정 이름 ─────────────────

// Port는 1~65535. 0은 "모름"이다.
type Port int

func ParsePort(s string) (Port, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, nil
	}
	n, err := strconv.Atoi(s)
	if err != nil || n < 1 || n > 65535 {
		return 0, InputError{Field: "port", Code: "port"}
	}
	return Port(n), nil
}

// Email은 인증서 연락처다.
type Email struct{ v string }

func ParseEmail(s string) (Email, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return Email{}, nil
	}
	a, err := mail.ParseAddress(s)
	if err != nil || a.Address != s || !strings.Contains(s, ".") {
		return Email{}, InputError{Field: "email", Code: "email"}
	}
	return Email{s}, nil
}

func (e Email) String() string { return e.v }
func (e Email) IsZero() bool   { return e.v == "" }

// Username은 영문·숫자·.-_ 2~32자다.
type Username struct{ v string }

var usernamePattern = regexp.MustCompile(`^[a-zA-Z0-9._-]{2,32}$`)

func ParseUsername(s string) (Username, error) {
	s = strings.TrimSpace(s)
	if !usernamePattern.MatchString(s) {
		return Username{}, InputError{Field: "username", Code: "username"}
	}
	return Username{s}, nil
}

func (u Username) String() string { return u.v }

// ── 환경 변수 · 볼륨 ─────────────────────────

// EnvVars는 "KEY=value" 줄들이다. 빈 줄과 # 주석은 괜찮다.
type EnvVars struct{ text string }

var envKeyPattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// EnvFix는 환경 변수를 저장하면서 고친 종류다.
type EnvFix string

const (
	EnvFixExport    EnvFix = "export"    // 앞의 export를 뺐다
	EnvFixQuotes    EnvFix = "quotes"    // 값을 감싼 따옴표를 벗겼다
	EnvFixComment   EnvFix = "comment"   // 줄 끝 주석을 뺐다
	EnvFixSpaces    EnvFix = "spaces"    // 이름·값 앞뒤 공백을 뺐다
	EnvFixDuplicate EnvFix = "duplicate" // 같은 이름이 앞에 또 있다 (나중 것이 이긴다)
)

// EnvNote는 저장하면서 고친 줄 하나다 — 화면이 "이렇게 바꿨어요"로 보여 준다. 값은 담지 않는다.
type EnvNote struct {
	Line int
	Key  string
	Did  EnvFix
}

// ParseEnvVars는 화면에 적은 글(.env를 붙여 넣어도 된다)을 저장할 모양으로 바꾼다.
// 저장되는 줄은 늘 "KEY=값"(= 뒤 그대로)이라 컨테이너에 넘기는 규칙(List)과 이미 저장된 값의 뜻은 바뀌지 않는다.
// .env처럼 적은 것 — export, 따옴표, 줄 끝 주석, 앞뒤 공백 — 은 풀어서 저장하고 notes로 알린다.
// 여러 줄 값과 닫히지 않은 따옴표는 받지 않는다 (몇 번째 줄인지 InputError.Line으로).
func ParseEnvVars(text string) (EnvVars, []EnvNote, error) {
	lines := strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n")
	out := make([]string, 0, len(lines))
	var notes []EnvNote
	seen := map[string]bool{}
	for i, line := range lines {
		n := i + 1
		t := strings.TrimSpace(line)
		if t == "" || strings.HasPrefix(t, "#") {
			out = append(out, line)
			continue
		}
		var did []EnvFix
		if rest, ok := cutExport(t); ok {
			t, did = rest, append(did, EnvFixExport)
		}
		k, raw, ok := strings.Cut(t, "=")
		key := strings.TrimSpace(k)
		if !ok || !envKeyPattern.MatchString(key) {
			return EnvVars{}, nil, InputError{Field: "env", Code: "env", Line: n}
		}
		value, fixes, code := envValue(raw, lines[i+1:])
		if code != "" {
			return EnvVars{}, nil, InputError{Field: "env", Code: code, Line: n}
		}
		if key != k && !contains(fixes, EnvFixSpaces) {
			fixes = append(fixes, EnvFixSpaces)
		}
		did = append(did, fixes...)
		if seen[key] {
			did = append(did, EnvFixDuplicate)
		}
		seen[key] = true
		for _, d := range did {
			notes = append(notes, EnvNote{Line: n, Key: key, Did: d})
		}
		out = append(out, key+"="+value)
	}
	return EnvVars{strings.Join(out, "\n")}, notes, nil
}

func cutExport(t string) (string, bool) {
	for _, p := range []string{"export ", "export\t"} {
		if rest, ok := strings.CutPrefix(t, p); ok {
			return strings.TrimLeft(rest, " \t"), true
		}
	}
	return t, false
}

// envValue는 = 뒤의 글을 값으로 푼다. code가 비어 있지 않으면 받지 않는 이유다 (InputError.Code).
// next는 뒤따르는 줄들이다 — 닫히지 않은 따옴표가 다음 줄에서 닫히면 여러 줄 값으로 본다.
func envValue(raw string, next []string) (value string, fixes []EnvFix, code string) {
	s := strings.TrimLeft(raw, " \t")
	if s != raw && strings.TrimSpace(s) != "" {
		fixes = append(fixes, EnvFixSpaces)
	}
	if s == "" {
		return "", fixes, ""
	}
	if q := s[0]; q == '"' || q == '\'' {
		var b strings.Builder
		closed := -1
		for j := 1; j < len(s); j++ {
			c := s[j]
			if q == '"' && c == '\\' && j+1 < len(s) {
				switch s[j+1] {
				case '"', '\\':
					b.WriteByte(s[j+1])
					j++
					continue
				case 'n':
					return "", nil, "envmultiline"
				}
			}
			if c == q {
				closed = j
				break
			}
			b.WriteByte(c)
		}
		if closed < 0 {
			for _, l := range next {
				if strings.ContainsRune(l, rune(q)) {
					return "", nil, "envmultiline"
				}
			}
			return "", nil, "envquote"
		}
		fixes = append(fixes, EnvFixQuotes)
		switch after := strings.TrimSpace(s[closed+1:]); {
		case after == "":
		case strings.HasPrefix(after, "#"):
			fixes = append(fixes, EnvFixComment)
		default:
			return "", nil, "envquote"
		}
		return b.String(), fixes, ""
	}
	// 따옴표가 없으면 공백 뒤의 #부터는 주석이다 (http://x/#frag 처럼 붙은 #은 값)
	for j := 1; j < len(s); j++ {
		if s[j] == '#' && (s[j-1] == ' ' || s[j-1] == '\t') {
			s = s[:j]
			fixes = append(fixes, EnvFixComment)
			break
		}
	}
	if trimmed := strings.TrimRight(s, " \t"); trimmed != s {
		s = trimmed
		if !contains(fixes, EnvFixComment) && !contains(fixes, EnvFixSpaces) {
			fixes = append(fixes, EnvFixSpaces)
		}
	}
	return s, fixes, ""
}

func contains(fixes []EnvFix, f EnvFix) bool {
	for _, x := range fixes {
		if x == f {
			return true
		}
	}
	return false
}

// StoredEnvVars는 이미 저장된 글을 그대로 쓴다 — 검사하지 않는다. 잘못된 줄은 List가 건너뛴다.
// v1에서 옮겨 온 값처럼 한 줄이 틀려도 나머지를 잃지 않게. 저장소(store)만 쓴다.
func StoredEnvVars(text string) EnvVars { return EnvVars{text} }

func (e EnvVars) Text() string { return e.text }

// List는 컨테이너에 넘길 "KEY=value" 목록이다.
func (e EnvVars) List() []string {
	var out []string
	for _, line := range strings.Split(e.text, "\n") {
		t := strings.TrimSpace(line)
		if t == "" || strings.HasPrefix(t, "#") {
			continue
		}
		k, _, ok := strings.Cut(line, "=")
		if ok && envKeyPattern.MatchString(strings.TrimSpace(k)) {
			out = append(out, strings.TrimSpace(k)+"="+line[len(k)+1:])
		}
	}
	return out
}

// Volumes는 "/서버경로:/컨테이너경로[:ro]" 줄들이다.
type Volumes struct{ text string }

func ParseVolumes(text string) (Volumes, error) {
	text = strings.ReplaceAll(text, "\r\n", "\n")
	for _, line := range strings.Split(text, "\n") {
		if t := strings.TrimSpace(line); t != "" && !validVolume(t) {
			return Volumes{}, InputError{Field: "volumes", Code: "volumes"}
		}
	}
	return Volumes{text}, nil
}

func validVolume(v string) bool {
	parts := strings.Split(v, ":")
	if len(parts) < 2 || len(parts) > 3 {
		return false
	}
	for _, p := range parts[:2] {
		if !strings.HasPrefix(p, "/") || strings.Contains(p, "..") {
			return false
		}
	}
	return len(parts) == 2 || parts[2] == "ro" || parts[2] == "rw"
}

// StoredVolumes는 이미 저장된 글을 그대로 쓴다. 잘못된 줄은 List가 건너뛴다.
func StoredVolumes(text string) Volumes { return Volumes{text} }

func (v Volumes) Text() string { return v.text }

func (v Volumes) List() []string {
	var out []string
	for _, line := range strings.Split(v.text, "\n") {
		if t := strings.TrimSpace(line); t != "" && validVolume(t) {
			out = append(out, t)
		}
	}
	return out
}
