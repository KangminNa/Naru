package docker

import (
	"context"
	"net/http"
	"sort"
	"time"

	"github.com/KangminNa/naru/internal/model"
)

// diskUsage는 docker system df다 — 이미지·볼륨과 크기, 쓰는 컨테이너 수. 볼륨 크기를 세느라 몇 초 걸린다.
func (c *Client) diskUsage(ctx context.Context) (model.DockerDisk, error) {
	var raw struct {
		Images []struct {
			ID         string `json:"Id"`
			RepoTags   []string
			Size       int64
			Created    int64
			Containers int
		}
		Volumes []struct {
			Name      string
			Labels    map[string]string
			UsageData *struct {
				Size     int64
				RefCount int
			}
		}
	}
	if err := c.callLong(ctx, http.MethodGet, "/system/df", &raw); err != nil {
		return model.DockerDisk{}, err
	}
	var d model.DockerDisk
	for _, i := range raw.Images {
		img := model.DiskImage{ID: model.ImageID(i.ID), Size: uint64(max(i.Size, 0)), Created: time.Unix(i.Created, 0), UsedBy: max(i.Containers, 0)}
		for _, t := range i.RepoTags {
			if t != "<none>:<none>" {
				img.Tags = append(img.Tags, t)
			}
		}
		d.Images = append(d.Images, img)
	}
	for _, v := range raw.Volumes {
		vol := model.DiskVolume{Name: v.Name, Project: v.Labels[labelComposeProject]}
		if v.UsageData != nil {
			vol.Size, vol.UsedBy = uint64(max(v.UsageData.Size, 0)), max(v.UsageData.RefCount, 0)
		}
		d.Volumes = append(d.Volumes, vol)
	}
	sort.Slice(d.Images, func(i, j int) bool { return d.Images[i].Size > d.Images[j].Size })
	sort.Slice(d.Volumes, func(i, j int) bool { return d.Volumes[i].Size > d.Volumes[j].Size })
	return d, nil
}

// callLong은 기본 제한시간(10초)보다 오래 걸릴 수 있는 읽기다 — 호출한 쪽의 ctx로 끊는다.
func (c *Client) callLong(ctx context.Context, method, path string, out any) error {
	long := &Client{http: &http.Client{Transport: c.http.Transport}}
	return long.call(ctx, method, path, nil, out)
}
