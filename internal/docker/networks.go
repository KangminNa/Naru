package docker

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"

	"github.com/KangminNa/naru/internal/model"
)

// Networks는 이 서버의 네트워크와 붙은 컨테이너다 (NetworkStore). 붙은 것은 컨테이너 목록에서 센다 — 네트워크마다 묻지 않는다.
func (k Containers) Networks(ctx context.Context) ([]model.NetworkRow, error) {
	var raw []struct {
		Name     string
		Driver   string
		Internal bool
		Labels   map[string]string
		IPAM     struct {
			Config []struct{ Subnet string }
		}
	}
	if err := k.c.call(ctx, http.MethodGet, "/networks", nil, &raw); err != nil {
		return nil, err
	}
	all, err := k.c.containers(ctx)
	if err != nil {
		return nil, err
	}
	members := map[string][]string{}
	for name, c := range all {
		for _, n := range c.Networks {
			members[n] = append(members[n], name)
		}
	}
	out := make([]model.NetworkRow, 0, len(raw))
	for _, r := range raw {
		row := model.NetworkRow{Name: r.Name, Driver: r.Driver, Internal: r.Internal, Project: r.Labels[labelComposeProject], Containers: members[r.Name]}
		if len(r.IPAM.Config) > 0 {
			row.Subnet = r.IPAM.Config[0].Subnet
		}
		sort.Strings(row.Containers)
		out = append(out, row)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// CreateNetwork는 bridge 네트워크를 만든다. 같은 이름이 있으면 model.ErrInUse.
func (k Containers) CreateNetwork(ctx context.Context, name model.NetworkName, internal bool) error {
	err := k.c.call(ctx, http.MethodPost, "/networks/create", map[string]any{"Name": name.String(), "Driver": "bridge", "Internal": internal, "CheckDuplicate": true}, nil)
	if errors.Is(err, ErrConflict) {
		return fmt.Errorf("%w: %v", model.ErrInUse, err)
	}
	return err
}

// RemoveNetwork는 네트워크를 지운다. 강제하지 않는다 — 붙은 컨테이너가 있으면 model.ErrInUse.
func (k Containers) RemoveNetwork(ctx context.Context, name string) error {
	err := k.c.call(ctx, http.MethodDelete, "/networks/"+url.PathEscape(name), nil, nil)
	switch {
	case err == nil:
		return nil
	case errors.Is(err, ErrConflict) || strings.Contains(err.Error(), "active endpoints"):
		return fmt.Errorf("%w: %v", model.ErrInUse, err)
	case errors.Is(err, ErrNotFound):
		return fmt.Errorf("%w: %v", model.ErrNotFound, err)
	}
	return err
}

// isShortID는 Docker가 별칭으로 붙이는 컨테이너 ID 앞 12자인가.
func isShortID(s string) bool {
	if len(s) != 12 {
		return false
	}
	for _, r := range s {
		if !(r >= '0' && r <= '9' || r >= 'a' && r <= 'f') {
			return false
		}
	}
	return true
}
