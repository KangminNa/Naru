package deploy

import (
	"context"

	"github.com/KangminNa/naru/internal/contract"
	"github.com/KangminNa/naru/internal/model"
)

// ControlParts는 멈추기·켜기·지우기가 쓰는 것들이다.
type ControlParts struct {
	Services contract.ServiceReader
	Store    contract.ServiceStore
	Live     contract.LiveStateStore
	Lock     contract.DeployLock
	Switch   contract.ContainerSwitch
	Remover  contract.ContainerRemover
	Watcher  contract.ContainerWatcher
	Images   contract.ImageCleaner
	Files    contract.SiteFiles
	Past     contract.DeployHistoryReader
	Events   contract.EventPublisher
	Logs     contract.LogArchive // 보관한 앱 출력 — 서비스를 지우면 함께 (없으면 건너뛴다)
	Kinds    contract.KindLookup // 여러 컨테이너로 도는 종류(Compose)는 그 종류가 통째로 멈추고·켜고·지운다
}

// runtime은 그 서비스의 종류가 통째로 다루는가 (Compose). 아니면 nil — 컨테이너 하나로 다룬다.
func (c serviceControl) runtime(s model.Service) contract.ServiceRuntime {
	if c.p.Kinds == nil {
		return nil
	}
	tools, ok := c.p.Kinds.Find(s.Kind)
	if !ok {
		return nil
	}
	return tools.Runtime
}

type serviceControl struct{ p ControlParts }

func NewServiceControl(p ControlParts) contract.ServiceControl { return serviceControl{p} }

// Stop은 컨테이너를 멈춘다. 직접 멈춘 것으로 표시해 두어 장애로 보이지 않게 한다.
func (c serviceControl) Stop(ctx context.Context, id model.ServiceID) error {
	s, err := c.p.Services.Get(ctx, id)
	if err != nil {
		return err
	}
	rt := c.runtime(s)
	name := s.Live.CurrentContainer()
	if name == "" && rt == nil {
		return model.ErrNothingToDeploy
	}
	s.Live.Stopped = true
	if err := c.p.Live.Save(ctx, id, s.Live); err != nil {
		return err
	}
	if rt != nil {
		return rt.TurnOff(ctx, s)
	}
	return c.p.Switch.TurnOff(ctx, name)
}

func (c serviceControl) Start(ctx context.Context, id model.ServiceID) error {
	s, err := c.p.Services.Get(ctx, id)
	if err != nil {
		return err
	}
	rt := c.runtime(s)
	name := s.Live.CurrentContainer()
	if name == "" && rt == nil {
		return model.ErrNothingToDeploy
	}
	if rt != nil {
		if err := rt.TurnOn(ctx, s); err != nil {
			return err
		}
	} else if err := c.p.Switch.TurnOn(ctx, name); err != nil {
		return err
	}
	s.Live.Stopped = false
	return c.p.Live.Save(ctx, id, s.Live)
}

// Remove는 서비스와 그 컨테이너·빌드 이미지·정적 파일을 모두 지운다. 다른 곳의 데이터(볼륨)는 건드리지 않는다.
func (c serviceControl) Remove(ctx context.Context, id model.ServiceID) error {
	if c.p.Lock.IsLocked(id) {
		return model.ErrBusy
	}
	s, err := c.p.Services.Get(ctx, id)
	if err != nil {
		return err
	}
	if rt := c.runtime(s); rt != nil {
		if err := rt.Remove(ctx, s); err != nil {
			return err
		}
	}
	names := map[string]bool{}
	if n := s.Live.CurrentContainer(); n != "" && c.runtime(s) == nil {
		names[n] = true
	}
	if more, err := c.p.Watcher.BelongingTo(ctx, id); err == nil {
		for _, n := range more {
			names[n] = true
		}
	}
	for n := range names {
		c.p.Remover.Remove(ctx, n)
	}
	if s.Live.CurrentContainer() != "" {
		if ids, err := c.p.Past.Succeeded(ctx, id); err == nil {
			for _, d := range ids {
				c.p.Images.Remove(ctx, model.ImageTag(s.Name, d))
			}
		}
	}
	c.p.Files.RemoveSite(s.Name) // 정적 사이트가 아니면 폴더가 없다
	if c.p.Logs != nil {
		c.p.Logs.RemoveAll(s.Name)
	}
	if err := c.p.Store.Delete(ctx, id); err != nil {
		return err
	}
	c.p.Events.Publish(model.ServiceDeleted{Service: id})
	c.p.Events.Publish(model.SiteMapChanged{Reason: "service deleted"})
	return nil
}
