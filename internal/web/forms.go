package web

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/KangminNa/naru/internal/model"
)

// serviceForm은 서비스 추가·설정 폼에서 받은 글자 그대로다. 틀렸을 때 다시 그리는 데 쓴다.
type serviceForm struct {
	Kind       string
	Name       string
	Source     string
	Branch     string
	BuildPath  string
	Folder     string
	Upstream   string
	Port       string
	Domain     string
	Token      string
	ClearToken bool
	Env        string
	Volumes    string
	AutoDeploy bool
}

func readServiceForm(r *http.Request) serviceForm {
	return serviceForm{
		Kind:       r.FormValue("kind"),
		Name:       strings.TrimSpace(r.FormValue("name")),
		Source:     strings.TrimSpace(r.FormValue("source")),
		Branch:     strings.TrimSpace(r.FormValue("branch")),
		BuildPath:  strings.TrimSpace(r.FormValue("build_path")),
		Folder:     strings.TrimSpace(r.FormValue("folder")),
		Upstream:   strings.TrimSpace(r.FormValue("upstream")),
		Port:       strings.TrimSpace(r.FormValue("port")),
		Domain:     strings.TrimSpace(r.FormValue("domain")),
		Token:      strings.TrimSpace(r.FormValue("token")),
		ClearToken: r.FormValue("clear_token") == "1",
		Env:        strings.ReplaceAll(r.FormValue("env"), "\r\n", "\n"),
		Volumes:    strings.ReplaceAll(r.FormValue("volumes"), "\r\n", "\n"),
		AutoDeploy: r.FormValue("auto_deploy") == "1",
	}
}

// formFrom은 저장된 값으로 설정 폼을 채운다.
func formFrom(f model.ServiceForm) serviceForm {
	port := ""
	if f.Port != 0 {
		port = strconv.Itoa(int(f.Port))
	}
	return serviceForm{
		Source: f.Source, Branch: f.Branch, BuildPath: f.BuildPath, Folder: f.Folder, Upstream: f.External,
		Port: port, AutoDeploy: f.AutoDeploy, Env: f.EnvText, Volumes: f.Volumes,
	}
}

// input은 글자들을 값 객체로 바꾼다. 모양이 틀린 칸이 있으면 그 InputError를 돌려준다.
// 어느 칸이 이 종류에 필요한지는 화면이 아니라 종류 담당자(InputChecker)가 본다 —
// "source"는 저장소 주소로도, 이미지 이름으로도 읽어 두고 종류가 고른다.
// 환경 변수는 .env처럼 적어도 받는다 — 저장할 모양으로 고친 줄은 notes로 돌려준다.
func (f serviceForm) input(kind model.KindName) (model.ServiceInput, []model.EnvNote, error) {
	in := model.ServiceInput{Kind: kind, Token: f.Token, ClearToken: f.ClearToken, AutoDeploy: f.AutoDeploy}
	var err error
	if f.Name != "" {
		if in.Name, err = model.ParseServiceName(f.Name); err != nil {
			return in, nil, err
		}
	}
	// 둘 다 아니면 둘 다 비어 있게 된다 — 그러면 종류 담당자가 자기에게 필요한 칸(저장소 주소·이미지 이름)이 틀렸다고 알려준다.
	in.Repo, _ = model.ParseRepoURL(f.Source)
	in.Image, _ = model.ParseImageRef(f.Source)
	if in.Branch, err = model.ParseBranch(f.Branch); err != nil {
		return in, nil, err
	}
	if f.Upstream != "" {
		if in.External, err = model.ParseExternalAddress(f.Upstream); err != nil {
			return in, nil, model.InputError{Field: "upstream", Code: "upstream"}
		}
	}
	if in.BuildPath, err = model.ParseFolderPath(f.BuildPath); err != nil {
		return in, nil, err
	}
	if in.Folder, err = model.ParseFolderPath(f.Folder); err != nil {
		return in, nil, err
	}
	if in.Port, err = model.ParsePort(f.Port); err != nil {
		return in, nil, err
	}
	var notes []model.EnvNote
	if in.Env, notes, err = model.ParseEnvVars(f.Env); err != nil {
		return in, nil, err
	}
	if in.Volumes, err = model.ParseVolumes(f.Volumes); err != nil {
		return in, nil, err
	}
	if in.Domain, err = model.ParseOptionalDomainName(f.Domain); err != nil {
		return in, nil, err
	}
	return in, notes, nil
}

// errLineOf는 여러 줄 칸에서 틀린 줄 번호다 (없으면 0).
func errLineOf(err error) int {
	if ie, ok := model.AsInputError(err); ok {
		return ie.Line
	}
	return 0
}

// envFix는 환경 변수를 저장하면서 고친 종류 하나와 그 줄 수다 — 화면 알림용.
type envFix struct {
	Kind  string
	Count int
}

// envFixOrder는 알림에 보일 순서이자, 주소로 받을 수 있는 종류의 전부다.
var envFixOrder = []model.EnvFix{model.EnvFixQuotes, model.EnvFixExport, model.EnvFixComment, model.EnvFixSpaces, model.EnvFixDuplicate}

// envFixQuery는 고친 종류마다의 줄 수를 주소에 실을 글로 만든다 ("quotes-2.export-1").
// 이름과 값은 싣지 않는다 — 주소는 기록에 남는다.
func envFixQuery(notes []model.EnvNote) string {
	count := map[model.EnvFix]map[int]bool{}
	for _, n := range notes {
		if count[n.Did] == nil {
			count[n.Did] = map[int]bool{}
		}
		count[n.Did][n.Line] = true
	}
	var parts []string
	for _, k := range envFixOrder {
		if c := len(count[k]); c > 0 {
			parts = append(parts, fmt.Sprintf("%s-%d", k, c))
		}
	}
	return strings.Join(parts, ".")
}

// envFixesOf는 주소의 envfix를 알림으로 되돌린다. 모르는 종류나 이상한 수는 버린다.
func envFixesOf(r *http.Request) []envFix {
	var out []envFix
	for _, part := range strings.Split(r.URL.Query().Get("envfix"), ".") {
		kind, num, ok := strings.Cut(part, "-")
		n, err := strconv.Atoi(num)
		if !ok || err != nil || n < 1 || n > 9999 {
			continue
		}
		for _, k := range envFixOrder {
			if string(k) == kind {
				out = append(out, envFix{Kind: kind, Count: n})
			}
		}
	}
	return out
}

// withEnvFix는 고친 것이 있으면 주소에 envfix를 붙인다.
func withEnvFix(path string, notes []model.EnvNote) string {
	q := envFixQuery(notes)
	if q == "" {
		return path
	}
	sep := "?"
	if strings.Contains(path, "?") {
		sep = "&"
	}
	return path + sep + "envfix=" + q
}

// errKeyOf는 실패를 화면 문구 키로 바꾼다. 모르는 실패는 "처리하지 못했어요".
func errKeyOf(err error) string {
	if ie, ok := model.AsInputError(err); ok {
		return "err." + ie.Code
	}
	return "err.internal"
}
