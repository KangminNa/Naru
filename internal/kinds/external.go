package kinds

import "github.com/KangminNa/naru/internal/model"

// ExternalDestination은 적어 둔 주소다 (https://로 시작하면 TLS로 보낸다).
// "이 서버"(localhost)는 저장할 때 이미 host.docker.internal로 바뀌어 있다 — 웹서버 컨테이너에서 본 호스트다.
type ExternalDestination struct{}

func (ExternalDestination) Find(s model.Service) model.Destination {
	return model.Destination{Address: s.External}
}

// ExternalStatus는 언제나 "연결만 함"이다 — Naru가 띄우는 것이 아니다.
type ExternalStatus struct{}

func (ExternalStatus) Read(model.Service, model.ContainerStates) model.ServiceStatus {
	return model.ServiceStatus{Key: "routed", Tone: "muted"}
}
