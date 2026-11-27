package views

import (
	"context"
	"time"

	"github.com/KangminNa/naru/internal/contract"
	"github.com/KangminNa/naru/internal/model"
)

// attachmentLock은 그 컨테이너를 그 네트워크에서 떼지 못하는 이유다 — 컨테이너가 잠겼으면 그 이유, 기본 네트워크면 default.
func attachmentLock(r model.ContainerRow, network string) string {
	switch {
	case r.Locked != "":
		return r.Locked
	case model.IsDefaultNetwork(network):
		return model.LockedDefault
	}
	return ""
}

// ── 여러 컨테이너 지우기 ───────────────────

type removalBoard struct{ b containerBoard }

func NewRemovalBoard(p BoardParts) contract.RemovalBoard { return removalBoard{containerBoard{p}} }

// Plan은 고른 것마다 어떻게 될지다 (이름순, 모르는 이름은 뺀다).
func (rb removalBoard) Plan(ctx context.Context, names []string) ([]model.RemovalRow, error) {
	rows, err := rb.b.rows(ctx)
	if err != nil {
		return nil, err
	}
	want := map[string]bool{}
	for _, n := range names {
		want[n] = true
	}
	var out []model.RemovalRow
	for _, r := range rows {
		if !want[r.Name] {
			continue
		}
		row := model.RemovalRow{Row: r.ContainerRow, Skip: r.Locked}
		if row.Skip == "" && (r.Running() || r.State == "restarting") {
			row.Skip = "running"
		}
		out = append(out, row)
	}
	sortRemoval(out)
	return out, nil
}

// Remove는 지운다. 보호된 것은 건너뛰고, 돌고 있는 것은 StopRunning일 때만 멈춘 뒤 지운다 (강제 없음).
func (rb removalBoard) Remove(ctx context.Context, names []string, o model.RemoveOptions) (model.RemovalResult, error) {
	plan, err := rb.Plan(ctx, names)
	if err != nil {
		return model.RemovalResult{}, err
	}
	var res model.RemovalResult
	for _, p := range plan {
		name := p.Row.Name
		switch p.Skip {
		case "":
		case "running":
			if !o.StopRunning {
				res.Skipped = append(res.Skipped, name)
				continue
			}
			if err := rb.b.p.Switch.TurnOff(ctx, name); err != nil {
				res.Failed = append(res.Failed, name)
				continue
			}
		default:
			res.Skipped = append(res.Skipped, name)
			continue
		}
		if err := rb.b.p.Sweeper.RemoveStopped(ctx, name, o.AnonVolumes); err != nil {
			res.Failed = append(res.Failed, name)
			continue
		}
		res.Removed = append(res.Removed, name)
	}
	return res, nil
}

func sortRemoval(rows []model.RemovalRow) {
	for i := 1; i < len(rows); i++ {
		for j := i; j > 0 && rows[j].Row.Name < rows[j-1].Row.Name; j-- {
			rows[j], rows[j-1] = rows[j-1], rows[j]
		}
	}
}

// ── 네트워크 ──────────────────────────────

// NetworkParts는 네트워크 화면이 쓰는 것들이다.
type NetworkParts struct {
	Board  BoardParts // 컨테이너가 잠겼는지 (Naru 자신·웹서버·서비스) — 컨테이너 화면과 같은 판단
	Store  contract.NetworkStore
	Joiner contract.NetworkJoiner
}

type networkBoard struct {
	b containerBoard
	p NetworkParts
}

func NewNetworkBoard(p NetworkParts) contract.NetworkBoard {
	return networkBoard{containerBoard{p.Board}, p}
}

// List는 네트워크들이다 (이름순). 기본 네트워크와 앱 네트워크는 잠겨 있다.
func (nb networkBoard) List(ctx context.Context) ([]model.NetworkRow, error) {
	lctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	rows, err := nb.p.Store.Networks(lctx)
	if err != nil {
		return nil, err
	}
	for i := range rows {
		rows[i].Locked = nb.networkLock(rows[i].Name)
	}
	return rows, nil
}

func (nb networkBoard) networkLock(name string) string {
	switch {
	case model.IsDefaultNetwork(name):
		return model.LockedDefault
	case name == nb.p.Board.AppNetwork:
		return model.LockedApp
	}
	return ""
}

func (nb networkBoard) Create(ctx context.Context, name model.NetworkName, internal bool) error {
	return nb.p.Store.CreateNetwork(ctx, name, internal)
}

// Remove는 쓰지 않는 네트워크를 지운다. 기본·앱 네트워크는 model.ErrProtected, 붙은 것이 있으면 Docker가 거절한다(model.ErrInUse).
func (nb networkBoard) Remove(ctx context.Context, name string) error {
	if nb.networkLock(name) != "" {
		return model.ErrProtected
	}
	return nb.p.Store.RemoveNetwork(ctx, name)
}

// Attach는 컨테이너를 네트워크에 붙인다 (별칭은 선택). 잠긴 컨테이너는 model.ErrProtected.
func (nb networkBoard) Attach(ctx context.Context, container string, network model.NetworkName, alias string) error {
	r, err := nb.b.find(ctx, container)
	if err != nil {
		return err
	}
	if r.Locked != "" {
		return model.ErrProtected
	}
	alias, err = model.ParseNetworkAlias(alias)
	if err != nil {
		return err
	}
	var aliases []string
	if alias != "" {
		aliases = []string{alias}
	}
	return nb.p.Joiner.JoinAs(ctx, network.String(), container, aliases)
}

// Detach는 컨테이너를 네트워크에서 뗀다. 잠긴 컨테이너와 기본 네트워크는 model.ErrProtected.
func (nb networkBoard) Detach(ctx context.Context, container, network string) error {
	r, err := nb.b.find(ctx, container)
	if err != nil {
		return err
	}
	if attachmentLock(r.ContainerRow, network) != "" {
		return model.ErrProtected
	}
	return nb.p.Joiner.Leave(ctx, network, container)
}
