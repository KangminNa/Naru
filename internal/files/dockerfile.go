package files

import (
	"bufio"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/KangminNa/naru/internal/model"
)

// DockerfileReader는 Dockerfile을 읽는다 — 단계(FROM … AS 이름)마다 EXPOSE한 TCP 포트.
type DockerfileReader struct{}

// stage는 Dockerfile의 단계 하나다.
type stage struct {
	name  string // 소문자, 이름이 없으면 빈 값
	from  string // FROM 뒤의 이미지 — 앞 단계의 이름이면 그 단계를 이어받는다
	ports []model.Port
}

func readStages(folder string) ([]stage, bool) {
	f, err := os.Open(filepath.Join(folder, "Dockerfile"))
	if err != nil {
		return nil, false
	}
	defer f.Close()
	var stages []stage
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) < 2 {
			continue
		}
		switch {
		case strings.EqualFold(fields[0], "FROM"):
			var rest []string
			for _, x := range fields[1:] {
				if !strings.HasPrefix(x, "--") { // --platform=… 같은 것은 건너뛴다
					rest = append(rest, x)
				}
			}
			st := stage{}
			if len(rest) > 0 {
				st.from = strings.ToLower(rest[0])
			}
			if len(rest) >= 3 && strings.EqualFold(rest[1], "AS") {
				st.name = strings.ToLower(rest[2])
			}
			stages = append(stages, st)
		case strings.EqualFold(fields[0], "EXPOSE"):
			if len(stages) == 0 {
				stages = append(stages, stage{})
			}
			cur := &stages[len(stages)-1]
			for _, field := range fields[1:] {
				num, proto, _ := strings.Cut(field, "/")
				if proto != "" && !strings.EqualFold(proto, "tcp") {
					continue
				}
				if n, err := strconv.Atoi(num); err == nil && n > 0 && n < 65536 {
					cur.ports = append(cur.ports, model.Port(n))
				}
			}
		}
	}
	return stages, true
}

// Ports는 빌드할 단계(비면 맨 마지막)가 연 포트다 — 그 단계가 이어받은 단계(FROM 단계이름)의 EXPOSE도 따라간다.
// 그런 단계가 없으면 포트도 없다 (빌드하기 전에 Stages로 따로 알린다).
func (DockerfileReader) Ports(folder, target string) ([]model.Port, bool) {
	stages, found := readStages(folder)
	if !found {
		return nil, false
	}
	byName := map[string]int{}
	for i, s := range stages {
		if s.name != "" {
			byName[s.name] = i
		}
	}
	i := len(stages) - 1
	if target != "" {
		j, ok := byName[strings.ToLower(target)]
		if !ok {
			return nil, true
		}
		i = j
	}
	var ports []model.Port
	for seen := 0; i >= 0 && seen <= len(stages); seen++ {
		for _, p := range stages[i].ports {
			if !containsPort(ports, p) {
				ports = append(ports, p)
			}
		}
		j, ok := byName[stages[i].from]
		if !ok || j >= i {
			break
		}
		i = j
	}
	return ports, true
}

// Stages는 이름이 있는 단계들이다 — 적힌 순서대로, 소문자로.
func (DockerfileReader) Stages(folder string) []string {
	stages, _ := readStages(folder)
	var out []string
	for _, s := range stages {
		if s.name != "" {
			out = append(out, s.name)
		}
	}
	return out
}

func containsPort(ps []model.Port, p model.Port) bool {
	for _, x := range ps {
		if x == p {
			return true
		}
	}
	return false
}
