package ent

// DomainInstance 本地最小视图，替代 Hub Ent 实体，供闪回连库与预检查使用。
type DomainInstance struct {
	ID         string
	MainIP     string
	DomainName string
	InstanceID string
	DbType     string
	Port       int
}
