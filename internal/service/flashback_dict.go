package service

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
)

type flashbackColumn struct {
	Name         string
	AttNum       int
	TypeName     string
	TypeOID      uint32
	Typlen       int
	Typalign     string
	TypType      string // b base, d domain, e enum
	Typelem      uint32
	BaseName     string
	BaseOID      uint32
	ElemName     string
	ElemTyplen   int
	ElemTypalign string
	EnumLabels   map[uint32]string
	NotNull      bool
	IsPK         bool
	Dropped      bool
	Default      string
}

type flashbackRelation struct {
	Schema    string
	Name      string
	OID       uint32
	RelNode   uint32
	ToastNode uint32
	ToastOID  uint32
	DBOID     uint32
	ReplIdent string // d/n/f/i
	PKCols    []string
	Columns   []flashbackColumn
	colByNum  map[int]flashbackColumn
	Missing   bool
	toast     *flashbackToastCache
}

type flashbackDictionary struct {
	DBOID     uint32
	DBName    string
	ByRelNode map[uint32]*flashbackRelation
	Wanted    map[string]*flashbackRelation // schema.table lower
	Catalog   *flashbackCatalog
	Toast     *flashbackToastCache
}

func flashbackListPGUserTables(ctx context.Context, db *sql.DB) ([]string, error) {
	rows, err := db.QueryContext(ctx, `
SELECT n.nspname, c.relname
FROM pg_class c
JOIN pg_namespace n ON n.oid = c.relnamespace
WHERE c.relkind IN ('r', 'p')
  AND n.nspname NOT IN ('pg_catalog', 'information_schema', 'pg_toast')
  AND n.nspname NOT LIKE 'pg_temp%'
  AND n.nspname NOT LIKE 'pg_toast_temp%'
ORDER BY 1, 2`)
	if err != nil {
		return nil, fmt.Errorf("list user tables: %w", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var schema, table string
		if err := rows.Scan(&schema, &table); err != nil {
			return nil, err
		}
		out = append(out, schema+"."+table)
	}
	return out, rows.Err()
}

func flashbackLoadDictionary(ctx context.Context, db *sql.DB, dbName string, tables []string) (*flashbackDictionary, error) {
	var dboid uint32
	if err := db.QueryRowContext(ctx, `SELECT oid FROM pg_database WHERE datname = current_database()`).Scan(&dboid); err != nil {
		return nil, fmt.Errorf("pg_database oid: %w", err)
	}
	names := flashbackNormalizeTableNames(tables)
	if len(names) == 0 {
		listed, err := flashbackListPGUserTables(ctx, db)
		if err != nil {
			return nil, err
		}
		if len(listed) == 0 {
			return nil, fmt.Errorf("库下没有可闪回的表")
		}
		names = listed
	}
	dict := &flashbackDictionary{
		DBOID:     dboid,
		DBName:    dbName,
		ByRelNode: map[uint32]*flashbackRelation{},
		Wanted:    map[string]*flashbackRelation{},
	}
	for _, raw := range names {
		schema, table, err := flashbackParseTableName(raw)
		if err != nil {
			return nil, err
		}
		rel, err := flashbackLoadRelation(ctx, db, schema, table)
		if err != nil {
			if !strings.Contains(err.Error(), "不存在") {
				return nil, err
			}
			rel = &flashbackRelation{Schema: schema, Name: table, Missing: true, colByNum: map[int]flashbackColumn{}}
		}
		rel.DBOID = dboid
		key := strings.ToLower(rel.Schema + "." + rel.Name)
		dict.Wanted[key] = rel
		if rel.RelNode != 0 {
			dict.ByRelNode[rel.RelNode] = rel
		}
		if rel.OID != 0 && !rel.Missing {
			dict.ByRelNode[rel.OID] = rel
		}
	}
	flashbackBindDictionary(dict)
	return dict, nil
}

func flashbackBindDictionary(dict *flashbackDictionary) {
	if dict == nil {
		return
	}
	if dict.Toast == nil {
		dict.Toast = newFlashbackToastCache()
	}
	if dict.ByRelNode == nil {
		dict.ByRelNode = map[uint32]*flashbackRelation{}
	}
	if dict.Wanted == nil {
		dict.Wanted = map[string]*flashbackRelation{}
	}
	for _, rel := range dict.Wanted {
		if rel == nil {
			continue
		}
		rel.toast = dict.Toast
		if rel.colByNum == nil {
			rel.colByNum = map[int]flashbackColumn{}
			for _, c := range rel.Columns {
				rel.colByNum[c.AttNum] = c
			}
		}
		if rel.RelNode != 0 {
			dict.ByRelNode[rel.RelNode] = rel
		}
		if rel.OID != 0 && !rel.Missing {
			dict.ByRelNode[rel.OID] = rel
		}
	}
}

func (d *flashbackDictionary) toastOwner(relNode uint32) *flashbackRelation {
	if d == nil || relNode == 0 {
		return nil
	}
	for _, rel := range d.Wanted {
		if rel != nil && !rel.Missing && (rel.ToastNode == relNode || rel.ToastOID == relNode) {
			return rel
		}
	}
	return nil
}

func firstRelNode(dict *flashbackDictionary) uint32 {
	if dict == nil {
		return 0
	}
	for _, rel := range dict.Wanted {
		if rel != nil && rel.RelNode != 0 {
			return rel.RelNode
		}
	}
	return 0
}

func firstOID(dict *flashbackDictionary) uint32 {
	if dict == nil {
		return 0
	}
	for _, rel := range dict.Wanted {
		if rel != nil && rel.OID != 0 {
			return rel.OID
		}
	}
	return 0
}

func flashbackLoadRelation(ctx context.Context, db *sql.DB, schema, table string) (*flashbackRelation, error) {
	rel := &flashbackRelation{Schema: schema, Name: table, colByNum: map[int]flashbackColumn{}}
	err := db.QueryRowContext(ctx, `
SELECT c.oid,
       COALESCE(pg_relation_filenode(c.oid), NULLIF(c.relfilenode, 0), c.oid),
       c.relreplident::text,
       COALESCE(pg_relation_filenode(NULLIF(c.reltoastrelid, 0)), 0),
       COALESCE(c.reltoastrelid, 0)
FROM pg_class c
JOIN pg_namespace n ON n.oid = c.relnamespace
WHERE n.nspname = $1 AND c.relname = $2 AND c.relkind IN ('r','p')`, schema, table).Scan(&rel.OID, &rel.RelNode, &rel.ReplIdent, &rel.ToastNode, &rel.ToastOID)
	if err == sql.ErrNoRows {
		return nil, fmt.Errorf("表 %s.%s 不存在", schema, table)
	}
	if err != nil {
		return nil, fmt.Errorf("lookup %s.%s: %w", schema, table, err)
	}
	rows, err := db.QueryContext(ctx, `
SELECT a.attnum, a.attname, a.attnotnull, a.attisdropped,
       COALESCE(t.typname, 'dropped'), COALESCE(t.oid, 0),
       CASE WHEN a.attisdropped THEN a.attlen ELSE t.typlen END,
       CASE WHEN a.attisdropped THEN a.attalign::text ELSE t.typalign::text END,
       COALESCE(t.typtype::text, 'b'), COALESCE(t.typelem, 0),
       COALESCE(bt.typname, COALESCE(t.typname, 'dropped')), COALESCE(bt.oid, t.oid, 0),
       COALESCE(et.typname, ''), COALESCE(et.typlen, 0), COALESCE(et.typalign, ''),
       EXISTS (
         SELECT 1 FROM pg_index i
         WHERE i.indrelid = a.attrelid AND i.indisprimary
           AND a.attnum = ANY(i.indkey)
       ) AS is_pk,
       COALESCE(pg_get_expr(ad.adbin, ad.adrelid), '')
FROM pg_attribute a
LEFT JOIN pg_type t ON t.oid = NULLIF(a.atttypid, 0)
LEFT JOIN pg_type bt ON t.typtype = 'd' AND bt.oid = t.typbasetype
LEFT JOIN pg_type et ON et.oid = t.typelem
LEFT JOIN pg_attrdef ad ON ad.adrelid = a.attrelid AND ad.adnum = a.attnum
WHERE a.attrelid = $1 AND a.attnum > 0
ORDER BY a.attnum`, rel.OID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var enumOIDs []uint32
	for rows.Next() {
		var c flashbackColumn
		var elemTyplen int
		if err := rows.Scan(&c.AttNum, &c.Name, &c.NotNull, &c.Dropped, &c.TypeName, &c.TypeOID, &c.Typlen, &c.Typalign, &c.TypType, &c.Typelem,
			&c.BaseName, &c.BaseOID, &c.ElemName, &elemTyplen, &c.ElemTypalign, &c.IsPK, &c.Default); err != nil {
			return nil, err
		}
		c.ElemTyplen = elemTyplen
		if c.Dropped {
			rel.Columns = append(rel.Columns, c)
			rel.colByNum[c.AttNum] = c
			continue
		}
		if c.TypType == "e" {
			enumOIDs = append(enumOIDs, c.TypeOID)
		}
		rel.Columns = append(rel.Columns, c)
		rel.colByNum[c.AttNum] = c
		if c.IsPK {
			rel.PKCols = append(rel.PKCols, c.Name)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(rel.Columns) == 0 {
		return nil, fmt.Errorf("表 %s.%s 无可用列", schema, table)
	}
	if err := flashbackLoadEnumLabels(ctx, db, rel, enumOIDs); err != nil {
		return nil, err
	}
	return rel, nil
}

func flashbackLoadEnumLabels(ctx context.Context, db *sql.DB, rel *flashbackRelation, oids []uint32) error {
	if len(oids) == 0 {
		return nil
	}
	labels := map[uint32]map[uint32]string{}
	for _, typ := range oids {
		rows, err := db.QueryContext(ctx, `SELECT oid, enumlabel FROM pg_enum WHERE enumtypid = $1`, typ)
		if err != nil {
			return err
		}
		for rows.Next() {
			var oid uint32
			var lab string
			if err := rows.Scan(&oid, &lab); err != nil {
				rows.Close()
				return err
			}
			if labels[typ] == nil {
				labels[typ] = map[uint32]string{}
			}
			labels[typ][oid] = lab
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
	}
	for i := range rel.Columns {
		if m := labels[rel.Columns[i].TypeOID]; m != nil {
			rel.Columns[i].EnumLabels = m
			rel.colByNum[rel.Columns[i].AttNum] = rel.Columns[i]
		}
	}
	return nil
}

func flashbackRelationTypeSummary(rel *flashbackRelation) (supported, unsupported []string) {
	if rel == nil {
		return nil, nil
	}
	for _, c := range rel.Columns {
		if c.Dropped {
			continue
		}
		st, hint := flashbackTypeSupported(c)
		item := c.Name + " " + c.TypeName
		if hint != "" && hint != c.TypeName {
			item += "(" + hint + ")"
		}
		if st == "unsupported" {
			unsupported = append(unsupported, item)
		} else {
			supported = append(supported, item)
		}
	}
	return supported, unsupported
}

func (d *flashbackDictionary) match(schema, table string) *flashbackRelation {
	if d == nil {
		return nil
	}
	return d.Wanted[strings.ToLower(schema+"."+table)]
}

func (d *flashbackDictionary) matchChange(ch flashbackChange) bool {
	if d == nil {
		return false
	}
	if d.match(ch.Schema, ch.Table) != nil {
		return true
	}
	switch strings.ToUpper(ch.Op) {
	case "CREATE", "DROP", "ALTER", "TRUNCATE":
		blob := strings.ToLower(ch.DDLRedo + " " + ch.DDLUndo + " " + ch.Schema + "." + ch.Table)
		blob = strings.ReplaceAll(blob, `"`, "")
		if strings.Contains(blob, "alter schema") {
			return true
		}
		for key := range d.Wanted {
			if key != "" && strings.Contains(blob, key) {
				return true
			}
		}
	}
	return false
}

func flashbackWantDDL(sqlType string) bool {
	f := flashbackNormalizeSQLTypes(sqlType)
	if len(f) == 0 {
		return true
	}
	_, ok := f["DDL"]
	return ok
}

func flashbackListCloudRoles(ctx context.Context, db *sql.DB) ([]string, error) {
	rows, err := db.QueryContext(ctx, `
SELECT rolname FROM pg_roles
WHERE rolname IN ('rds_superuser','pg_tencentdb_superuser','rdsadmin')`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var names []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			return nil, err
		}
		names = append(names, n)
	}
	return names, rows.Err()
}
