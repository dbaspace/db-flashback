package service

// 离线 catalog 引导参考 PDU-PostgreSQLDataUnloader info.c
// https://github.com/wublabdubdub/PDU-PostgreSQLDataUnloader
// Licensed under Apache License 2.0

import (
	"encoding/binary"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

const (
	flashbackRelmapperMagic = 0x592717
	flashbackMaxRelMappings = 64

	flashbackOIDDatabase   = 1262
	flashbackOIDNamespace  = 2615
	flashbackOIDClass      = 1259
	flashbackOIDAttribute  = 1249
	flashbackOIDType       = 1247
	flashbackOIDConstraint = 2606
)

type flashbackRelMap map[uint32]uint32

func flashbackLoadRelMap(path string) flashbackRelMap {
	out := flashbackRelMap{}
	raw, err := os.ReadFile(path)
	if err != nil || len(raw) < 8 {
		return out
	}
	magic := int32(binary.LittleEndian.Uint32(raw[0:4]))
	n := int32(binary.LittleEndian.Uint32(raw[4:8]))
	if magic != flashbackRelmapperMagic || n < 0 || n > flashbackMaxRelMappings {
		return out
	}
	need := 8 + int(n)*8
	if len(raw) < need {
		return out
	}
	for i := 0; i < int(n); i++ {
		off := 8 + i*8
		oid := binary.LittleEndian.Uint32(raw[off : off+4])
		node := binary.LittleEndian.Uint32(raw[off+4 : off+8])
		if oid != 0 && node != 0 {
			out[oid] = node
		}
	}
	return out
}

func flashbackResolveMappedNode(maps ...flashbackRelMap) func(oid, stored uint32) uint32 {
	return func(oid, stored uint32) uint32 {
		if stored != 0 {
			return stored
		}
		for _, m := range maps {
			if n := m[oid]; n != 0 {
				return n
			}
		}
		return oid
	}
}

func flashbackCatalogDatabaseRel(major int) *flashbackRelation {
	cols := []flashbackColumn{
		flashbackCol("oid", "oid", 4, "i"),
		flashbackCol("datname", "name", 64, "c"),
		flashbackCol("datdba", "oid", 4, "i"),
		flashbackCol("encoding", "int4", 4, "i"),
	}
	if major >= 15 {
		cols = append(cols, flashbackCol("datlocprovider", "char", 1, "c"))
	}
	cols = append(cols,
		flashbackCol("datistemplate", "bool", 1, "c"),
		flashbackCol("datallowconn", "bool", 1, "c"),
	)
	if major >= 16 {
		cols = append(cols, flashbackCol("dathasloginevt", "bool", 1, "c"))
	}
	return flashbackBuildRel(cols)
}

func flashbackCatalogNamespaceRel() *flashbackRelation {
	return flashbackBuildRel([]flashbackColumn{
		flashbackCol("oid", "oid", 4, "i"),
		flashbackCol("nspname", "name", 64, "c"),
		flashbackCol("nspowner", "oid", 4, "i"),
	})
}

func flashbackCatalogClassRel() *flashbackRelation {
	return flashbackBuildRel([]flashbackColumn{
		flashbackCol("oid", "oid", 4, "i"),
		flashbackCol("relname", "name", 64, "c"),
		flashbackCol("relnamespace", "oid", 4, "i"),
		flashbackCol("reltype", "oid", 4, "i"),
		flashbackCol("reloftype", "oid", 4, "i"),
		flashbackCol("relowner", "oid", 4, "i"),
		flashbackCol("relam", "oid", 4, "i"),
		flashbackCol("relfilenode", "oid", 4, "i"),
		flashbackCol("reltablespace", "oid", 4, "i"),
		flashbackCol("relpages", "int4", 4, "i"),
		flashbackCol("reltuples", "float4", 4, "i"),
		flashbackCol("relallvisible", "int4", 4, "i"),
		flashbackCol("reltoastrelid", "oid", 4, "i"),
		flashbackCol("relhasindex", "bool", 1, "c"),
		flashbackCol("relisshared", "bool", 1, "c"),
		flashbackCol("relpersistence", "char", 1, "c"),
		flashbackCol("relkind", "char", 1, "c"),
		flashbackCol("relnatts", "int2", 2, "s"),
	})
}

func flashbackCatalogAttributeRel(major int) *flashbackRelation {
	cols := []flashbackColumn{
		flashbackCol("attrelid", "oid", 4, "i"),
		flashbackCol("attname", "name", 64, "c"),
		flashbackCol("atttypid", "oid", 4, "i"),
		flashbackCol("attstattarget", "int4", 4, "i"),
		flashbackCol("attlen", "int2", 2, "s"),
		flashbackCol("attnum", "int2", 2, "s"),
		flashbackCol("attndims", "int4", 4, "i"),
		flashbackCol("attcacheoff", "int4", 4, "i"),
		flashbackCol("atttypmod", "int4", 4, "i"),
		flashbackCol("attbyval", "bool", 1, "c"),
		flashbackCol("attalign", "char", 1, "c"),
		flashbackCol("attstorage", "char", 1, "c"),
	}
	if major >= 14 {
		cols = append(cols, flashbackCol("attcompression", "char", 1, "c"))
	}
	cols = append(cols,
		flashbackCol("attnotnull", "bool", 1, "c"),
		flashbackCol("atthasdef", "bool", 1, "c"),
		flashbackCol("atthasmissing", "bool", 1, "c"),
		flashbackCol("attidentity", "char", 1, "c"),
		flashbackCol("attgenerated", "char", 1, "c"),
		flashbackCol("attisdropped", "bool", 1, "c"),
	)
	return flashbackBuildRel(cols)
}

func flashbackCatalogTypeRel() *flashbackRelation {
	return flashbackBuildRel([]flashbackColumn{
		flashbackCol("oid", "oid", 4, "i"),
		flashbackCol("typname", "name", 64, "c"),
		flashbackCol("typnamespace", "oid", 4, "i"),
		flashbackCol("typowner", "oid", 4, "i"),
		flashbackCol("typlen", "int2", 2, "s"),
		flashbackCol("typbyval", "bool", 1, "c"),
		flashbackCol("typalign", "char", 1, "c"),
		flashbackCol("typstorage", "char", 1, "c"),
		flashbackCol("typtype", "char", 1, "c"),
		flashbackCol("typcategory", "char", 1, "c"),
		flashbackCol("typispreferred", "bool", 1, "c"),
		flashbackCol("typisdefined", "bool", 1, "c"),
		flashbackCol("typdelim", "char", 1, "c"),
		flashbackCol("typrelid", "oid", 4, "i"),
		flashbackCol("typsubscript", "oid", 4, "i"),
		flashbackCol("typelem", "oid", 4, "i"),
		flashbackCol("typarray", "oid", 4, "i"),
	})
}

func flashbackScanCatalogRel(path string, rel *flashbackRelation) []map[string]string {
	rows, err := flashbackScanHeapFile(path, rel, true)
	if err != nil {
		return nil
	}
	var out []map[string]string
	for _, t := range rows {
		if len(t.Values) > 0 {
			out = append(out, t.Values)
		}
	}
	return out
}

type flashbackOfflineCatalog struct {
	PGData    string
	Major     int
	Version   string
	DBOID     uint32
	DBName    string
	GlobalMap flashbackRelMap
	DBMap     flashbackRelMap
	resolve   func(oid, stored uint32) uint32
	ns        map[uint32]string
	types     map[uint32]flashbackColumn
	classes   []map[string]string
}

func flashbackOpenOfflinePGDATA(pgdata string) (*flashbackOfflineCatalog, error) {
	pgdata = filepath.Clean(strings.TrimSpace(pgdata))
	major, ver, err := flashbackReadPGVersion(pgdata)
	if err != nil {
		return nil, fmt.Errorf("读 PG_VERSION: %w", err)
	}
	if _, err := os.Stat(filepath.Join(pgdata, "global", "pg_control")); err != nil {
		return nil, fmt.Errorf("缺少 global/pg_control: %w", err)
	}
	gmap := flashbackLoadRelMap(filepath.Join(pgdata, "global", "pg_filenode.map"))
	return &flashbackOfflineCatalog{
		PGData: pgdata, Major: major, Version: ver, GlobalMap: gmap,
		resolve: flashbackResolveMappedNode(gmap),
		ns:      map[uint32]string{},
		types:   map[uint32]flashbackColumn{},
	}, nil
}

func (c *flashbackOfflineCatalog) mappedPath(dboid, oid, stored uint32) string {
	return flashbackHeapRelationPath(c.PGData, dboid, c.resolve(oid, stored))
}

func (c *flashbackOfflineCatalog) listDatabases() ([]dtoishDB, error) {
	node := c.resolve(flashbackOIDDatabase, 0)
	path := flashbackHeapRelationPath(c.PGData, 0, node)
	rows := flashbackScanCatalogRel(path, flashbackCatalogDatabaseRel(c.Major))
	var out []dtoishDB
	for _, r := range rows {
		name := strings.TrimSpace(r["datname"])
		oid := flashbackParseU32Map(r, "oid")
		if name == "" || oid == 0 {
			continue
		}
		out = append(out, dtoishDB{Name: name, OID: oid})
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("未能从 pg_database 解出任何库（文件 %s）", path)
	}
	return out, nil
}

type dtoishDB struct {
	Name string
	OID  uint32
}

func (c *flashbackOfflineCatalog) useDatabase(name string) error {
	name = strings.TrimSpace(name)
	dbs, err := c.listDatabases()
	if err != nil {
		return err
	}
	for _, db := range dbs {
		if db.Name == name {
			c.DBOID = db.OID
			c.DBName = db.Name
			c.DBMap = flashbackLoadRelMap(filepath.Join(c.PGData, "base", fmt.Sprintf("%d", db.OID), "pg_filenode.map"))
			c.resolve = flashbackResolveMappedNode(c.DBMap, c.GlobalMap)
			return c.loadCatalog()
		}
	}
	return fmt.Errorf("离线 PGDATA 中没有库 %s", name)
}

func (c *flashbackOfflineCatalog) loadCatalog() error {
	nsPath := c.mappedPath(c.DBOID, flashbackOIDNamespace, 0)
	for _, r := range flashbackScanCatalogRel(nsPath, flashbackCatalogNamespaceRel()) {
		oid := flashbackParseU32Map(r, "oid")
		n := strings.TrimSpace(r["nspname"])
		if oid != 0 && n != "" {
			c.ns[oid] = n
		}
	}
	typePath := c.mappedPath(c.DBOID, flashbackOIDType, 0)
	for _, r := range flashbackScanCatalogRel(typePath, flashbackCatalogTypeRel()) {
		oid := flashbackParseU32Map(r, "oid")
		if oid == 0 {
			continue
		}
		c.types[oid] = flashbackColumn{
			Name: strings.TrimSpace(r["typname"]), TypeName: strings.TrimSpace(r["typname"]),
			TypeOID: oid, Typlen: flashbackParseI32Map(r, "typlen"),
			Typalign: strings.TrimSpace(r["typalign"]), TypType: strings.TrimSpace(r["typtype"]),
			Typelem: flashbackParseU32Map(r, "typelem"), BaseName: strings.TrimSpace(r["typname"]),
		}
	}
	classPath := c.mappedPath(c.DBOID, flashbackOIDClass, 0)
	c.classes = flashbackScanCatalogRel(classPath, flashbackCatalogClassRel())
	if len(c.ns) == 0 || len(c.classes) == 0 {
		return fmt.Errorf("离线 catalog 不完整：namespace=%d class=%d type=%d", len(c.ns), len(c.classes), len(c.types))
	}
	return nil
}

func (c *flashbackOfflineCatalog) listUserTables() [][2]string {
	var out [][2]string
	for _, r := range c.classes {
		kind := strings.TrimSpace(r["relkind"])
		if kind != "r" && kind != "p" {
			continue
		}
		schema := c.ns[flashbackParseU32Map(r, "relnamespace")]
		if schema == "" || schema == "pg_catalog" || schema == "information_schema" || strings.HasPrefix(schema, "pg_toast") {
			continue
		}
		name := strings.TrimSpace(r["relname"])
		if name == "" || strings.HasPrefix(name, "pg_") {
			continue
		}
		out = append(out, [2]string{schema, name})
	}
	return out
}

func (c *flashbackOfflineCatalog) loadDictionary(tables []string) (*flashbackDictionary, error) {
	attrPath := c.mappedPath(c.DBOID, flashbackOIDAttribute, 0)
	attrs := flashbackScanCatalogRel(attrPath, flashbackCatalogAttributeRel(c.Major))
	byRel := map[uint32][]map[string]string{}
	for _, a := range attrs {
		relid := flashbackParseU32Map(a, "attrelid")
		if flashbackParseI32Map(a, "attnum") <= 0 {
			continue
		}
		byRel[relid] = append(byRel[relid], a)
	}
	all := flashbackTablesIsAll(tables)
	wanted := flashbackPDUWantedKeys(tables)
	dict := &flashbackDictionary{
		DBOID: c.DBOID, DBName: c.DBName,
		ByRelNode: map[uint32]*flashbackRelation{},
		Wanted:    map[string]*flashbackRelation{},
		Toast:     newFlashbackToastCache(),
	}
	for _, r := range c.classes {
		kind := strings.TrimSpace(r["relkind"])
		if kind != "r" && kind != "p" {
			continue
		}
		schema := c.ns[flashbackParseU32Map(r, "relnamespace")]
		name := strings.TrimSpace(r["relname"])
		qual := strings.ToLower(schema + "." + name)
		if !all && !flashbackPDUWantedHit(wanted, schema, name) {
			continue
		}
		if schema == "pg_catalog" || schema == "information_schema" {
			continue
		}
		oid := flashbackParseU32Map(r, "oid")
		relnode := c.resolve(oid, flashbackParseU32Map(r, "relfilenode"))
		toastOID := flashbackParseU32Map(r, "reltoastrelid")
		rel := &flashbackRelation{
			Schema: schema, Name: name, OID: oid, RelNode: relnode,
			ToastOID: toastOID, DBOID: c.DBOID, colByNum: map[int]flashbackColumn{},
			toast: dict.Toast,
		}
		if toastOID != 0 {
			for _, tr := range c.classes {
				if flashbackParseU32Map(tr, "oid") == toastOID {
					rel.ToastNode = c.resolve(toastOID, flashbackParseU32Map(tr, "relfilenode"))
					break
				}
			}
		}
		for _, a := range byRel[oid] {
			attnum := flashbackParseI32Map(a, "attnum")
			col := flashbackColumn{
				Name: strings.TrimSpace(a["attname"]), AttNum: attnum,
				TypeOID:  flashbackParseU32Map(a, "atttypid"),
				Typlen:   flashbackParseI32Map(a, "attlen"),
				Typalign: strings.TrimSpace(a["attalign"]),
				NotNull:  strings.TrimSpace(a["attnotnull"]) == "t",
				Dropped:  strings.TrimSpace(a["attisdropped"]) == "t",
			}
			if typ, ok := c.types[col.TypeOID]; ok {
				col.TypeName = typ.TypeName
				col.TypType = typ.TypType
				col.Typelem = typ.Typelem
				col.BaseName = typ.TypeName
				if typ.Typlen != 0 {
					col.Typlen = typ.Typlen
				}
				if typ.Typalign != "" {
					col.Typalign = typ.Typalign
				}
			}
			if col.TypeName == "" {
				col.TypeName = flashbackTypeNameByOID(col.TypeOID)
			}
			rel.Columns = append(rel.Columns, col)
			rel.colByNum[attnum] = col
		}
		rel.Columns = flashbackDedupeSortColumns(rel.Columns)
		rel.colByNum = map[int]flashbackColumn{}
		for _, col := range rel.Columns {
			rel.colByNum[col.AttNum] = col
		}
		if len(rel.Columns) == 0 {
			rel.Missing = true
		}
		dict.Wanted[qual] = rel
		if rel.RelNode != 0 {
			dict.ByRelNode[rel.RelNode] = rel
		}
	}
	if len(dict.Wanted) == 0 {
		return dict, flashbackPDUDictMissError(c, tables, all)
	}
	return dict, nil
}

func flashbackDedupeSortColumns(cols []flashbackColumn) []flashbackColumn {
	best := map[int]flashbackColumn{}
	for _, c := range cols {
		if c.AttNum <= 0 {
			continue
		}
		if c.TypeName == "" {
			c.TypeName = flashbackTypeNameByOID(c.TypeOID)
		}
		old, ok := best[c.AttNum]
		if !ok || (old.Dropped && !c.Dropped) || (old.TypeOID == 0 && c.TypeOID != 0) {
			best[c.AttNum] = c
		}
	}
	out := make([]flashbackColumn, 0, len(best))
	for _, c := range best {
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].AttNum < out[j].AttNum })
	return out
}

func flashbackTypeNameByOID(oid uint32) string {
	switch oid {
	case 16:
		return "bool"
	case 17:
		return "bytea"
	case 18:
		return "char"
	case 19:
		return "name"
	case 20:
		return "int8"
	case 21:
		return "int2"
	case 23:
		return "int4"
	case 25:
		return "text"
	case 26:
		return "oid"
	case 700:
		return "float4"
	case 701:
		return "float8"
	case 1042:
		return "bpchar"
	case 1043:
		return "varchar"
	case 1082:
		return "date"
	case 1114:
		return "timestamp"
	case 1184:
		return "timestamptz"
	case 1700:
		return "numeric"
	default:
		return ""
	}
}

func flashbackPDUWantedKeys(tables []string) map[string]struct{} {
	wanted := map[string]struct{}{}
	for _, raw := range flashbackNormalizeTableNames(tables) {
		key := strings.ToLower(raw)
		if !strings.Contains(key, ".") {
			wanted[key] = struct{}{}
			wanted["public."+key] = struct{}{}
			continue
		}
		if schema, table, err := flashbackParseTableName(key); err == nil {
			wanted[strings.ToLower(schema+"."+table)] = struct{}{}
			continue
		}
		wanted[key] = struct{}{}
	}
	return wanted
}

func flashbackPDUWantedHit(wanted map[string]struct{}, schema, name string) bool {
	schema = strings.ToLower(strings.TrimSpace(schema))
	name = strings.ToLower(strings.TrimSpace(name))
	if _, ok := wanted[schema+"."+name]; ok {
		return true
	}
	if _, ok := wanted[schema]; ok {
		return true
	}
	return false
}

func flashbackPDUDictMissError(c *flashbackOfflineCatalog, tables []string, all bool) error {
	db := "unknown"
	classN := 0
	seen := "无"
	if c != nil {
		if strings.TrimSpace(c.DBName) != "" {
			db = c.DBName
		}
		classN = len(c.classes)
		seen = flashbackFormatUserTables(c.listUserTables(), 20)
	}
	scope := flashbackTableScopeName(tables)
	if all {
		return fmt.Errorf("库 %s %s扫描未找到用户表（pg_class 行 %d，已见用户表：%s）", db, scope, classN, seen)
	}
	return fmt.Errorf("库 %s 未匹配%s %s（已见用户表：%s）", db, scope, strings.Join(flashbackNormalizeTableNames(tables), ", "), seen)
}

func flashbackTableScopeName(tables []string) string {
	names := flashbackNormalizeTableNames(tables)
	switch {
	case len(names) == 0:
		return "整库"
	case len(names) == 1:
		return "单表"
	default:
		return "多表"
	}
}

func flashbackFormatUserTables(tables [][2]string, limit int) string {
	if len(tables) == 0 {
		return "无"
	}
	if limit <= 0 {
		limit = 20
	}
	n := len(tables)
	if n > limit {
		n = limit
	}
	parts := make([]string, 0, n)
	for i := 0; i < n; i++ {
		parts = append(parts, tables[i][0]+"."+tables[i][1])
	}
	s := strings.Join(parts, ", ")
	if len(tables) > limit {
		s += fmt.Sprintf(" 等共 %d 张", len(tables))
	}
	return s
}
