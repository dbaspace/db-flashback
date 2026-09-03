package mdmmodel

// DBTypeEnum 与 Hub / MDM 取值对齐，保持闪回判定逻辑不变。
type DBTypeEnum string

const (
	MySQLDBTypeEnum      DBTypeEnum = "mysql"
	PostgreSQLDBTypeEnum DBTypeEnum = "postgresql"
)

type Region struct {
	Name   string
	Region string
}

type Zone struct {
	Name string
	Zone string
}

// ResourceDbsInfo 本地最小模型，字段覆盖闪回云 WAL / 厂商判定用到的 MDM 形状。
type ResourceDbsInfo struct {
	Address           string
	Name              string
	Version           string
	Tags              map[string]interface{}
	DbType            DBTypeEnum
	CredentialAccount string
	Region            *Region
	RegionID          string
	Zone              *Zone
}
