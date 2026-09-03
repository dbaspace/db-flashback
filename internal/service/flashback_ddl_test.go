package service

import (
	"strings"
	"testing"
)

func testCatalog() *flashbackCatalog {
	return &flashbackCatalog{
		namespaces:     map[uint32]string{2200: "public"},
		types:          map[uint32]flashbackCatalogAttr{23: {TypeOID: 23, TypeName: "int4", Typlen: 4, Align: "i"}},
		classes:        map[uint32]*flashbackCatalogClass{},
		attrs:          map[uint32]map[int]*flashbackCatalogAttr{},
		graveyardClass: map[uint32]*flashbackCatalogClass{},
		graveyardAttrs: map[uint32]map[int]*flashbackCatalogAttr{},
	}
}

func TestFlashbackSynthesizeCreateDrop(t *testing.T) {
	cat := testCatalog()
	muts := []flashbackCatalogMut{
		{rel: flashbackCatalogClassName, op: "INSERT", new: map[string]string{
			"oid": "50000", "relname": "t1", "relnamespace": "2200", "relkind": "r", "relfilenode": "50000",
		}},
		{rel: flashbackCatalogAttrName, op: "INSERT", new: map[string]string{
			"attrelid": "50000", "attnum": "1", "attname": "id", "atttypid": "23", "attlen": "4", "attalign": "i", "attisdropped": "f",
		}},
	}
	ch := flashbackSynthesizeDDL(cat, 11, muts)
	if len(ch) != 1 || ch[0].Op != "CREATE" {
		t.Fatalf("create: %+v", ch)
	}
	if ch[0].DDLUndo == "" || !flashbackUndoHas([]string{ch[0].DDLUndo}, "DROP TABLE") {
		t.Fatalf("create undo: %s", ch[0].DDLUndo)
	}
	if ch[0].DDLRedo == "" || !flashbackUndoHas([]string{ch[0].DDLRedo}, "CREATE TABLE") {
		t.Fatalf("create redo: %s", ch[0].DDLRedo)
	}

	cat.graveyardClass[50000] = &flashbackCatalogClass{OID: 50000, Namespace: 2200, Name: "t1", Kind: "r"}
	cat.graveyardAttrs[50000] = map[int]*flashbackCatalogAttr{
		1: {RelID: 50000, AttNum: 1, Name: "id", TypeOID: 23, TypeName: "int4"},
	}
	drop := flashbackSynthesizeDDL(cat, 12, []flashbackCatalogMut{
		{rel: flashbackCatalogClassName, op: "DELETE", old: map[string]string{
			"oid": "50000", "relname": "t1", "relnamespace": "2200", "relkind": "r",
		}},
		{rel: flashbackCatalogAttrName, op: "DELETE", old: map[string]string{
			"attrelid": "50000", "attnum": "1", "attname": "id", "atttypid": "23",
		}},
	})
	if len(drop) != 1 || drop[0].Op != "DROP" {
		t.Fatalf("drop: %+v", drop)
	}
	if !flashbackUndoHas([]string{drop[0].DDLUndo}, "CREATE TABLE") {
		t.Fatalf("drop undo should recreate: %s", drop[0].DDLUndo)
	}
}

func TestFlashbackSynthesizeDropMissingImage(t *testing.T) {
	cat := testCatalog()
	ch := flashbackSynthesizeDDL(cat, 1, []flashbackCatalogMut{
		{rel: flashbackCatalogClassName, op: "DELETE", old: map[string]string{"oid": "9"}},
	})
	if len(ch) != 1 || ch[0].Op != "DROP" {
		t.Fatalf("%+v", ch)
	}
	if ch[0].DDLUndo != "" {
		t.Fatalf("must not invent CREATE: %s", ch[0].DDLUndo)
	}
	if !flashbackUndoHas([]string{ch[0].DDLRisk}, "缺旧行图像") {
		t.Fatalf("risk=%s", ch[0].DDLRisk)
	}
}

func TestFlashbackSynthesizeRenameAddDropColumn(t *testing.T) {
	cat := testCatalog()
	cat.classes[1] = &flashbackCatalogClass{OID: 1, Namespace: 2200, Name: "t2", Kind: "r"}
	ch := flashbackSynthesizeDDL(cat, 3, []flashbackCatalogMut{
		{rel: flashbackCatalogClassName, op: "UPDATE",
			old: map[string]string{"oid": "1", "relname": "t_old", "relnamespace": "2200", "relkind": "r"},
			new: map[string]string{"oid": "1", "relname": "t_new", "relnamespace": "2200", "relkind": "r"},
		},
		{rel: flashbackCatalogAttrName, op: "INSERT", new: map[string]string{
			"attrelid": "1", "attnum": "2", "attname": "extra", "atttypid": "23", "attisdropped": "f",
		}},
		{rel: flashbackCatalogAttrName, op: "UPDATE",
			old: map[string]string{"attrelid": "1", "attnum": "3", "attname": "gone", "atttypid": "23", "attisdropped": "f"},
			new: map[string]string{"attrelid": "1", "attnum": "3", "attname": "........pg.dropped.3........", "attisdropped": "t"},
		},
	})
	var ops []string
	for _, c := range ch {
		ops = append(ops, c.Op+" "+c.DDLRedo)
		if c.Op != "ALTER" {
			t.Fatalf("want ALTER: %+v", c)
		}
	}
	if !flashbackUndoHas(ops, "RENAME TO") || !flashbackUndoHas(ops, "ADD COLUMN") || !flashbackUndoHas(ops, "DROP COLUMN") {
		t.Fatalf("ops=%v", ops)
	}
}

func TestFlashbackUndoRedoDDL(t *testing.T) {
	ch := flashbackChange{Op: "CREATE", Schema: "public", Table: "t",
		DDLRedo: `CREATE TABLE "public"."t" (id int);`,
		DDLUndo: `DROP TABLE IF EXISTS "public"."t";`,
	}
	u, _ := flashbackUndoSQL(ch)
	r, _ := flashbackRedoSQL(ch)
	if u != ch.DDLUndo || r != ch.DDLRedo {
		t.Fatalf("undo=%s redo=%s", u, r)
	}
}

func TestFlashbackSynthesizeIgnoresToastRename(t *testing.T) {
	cat := testCatalog()
	cat.classes[9] = &flashbackCatalogClass{OID: 9, Namespace: 2200, Name: "t_user", Kind: "r"}
	ch := flashbackSynthesizeDDL(cat, 4, []flashbackCatalogMut{
		{rel: flashbackCatalogClassName, op: "UPDATE",
			old: map[string]string{"oid": "9", "relname": "t_user", "relkind": "r"},
			new: map[string]string{"oid": "9", "relname": "pg_toast_2609", "relkind": "t"},
		},
	})
	for _, c := range ch {
		if strings.Contains(c.DDLRedo+c.DDLUndo, "pg_toast") {
			t.Fatalf("toast leaked: %+v", c)
		}
	}
}

func TestFlashbackFormatCreateTablePKAndDefault(t *testing.T) {
	cat := testCatalog()
	cat.pks = map[uint32][]string{1: {"id"}}
	cat.defaults = map[uint32]map[int]string{1: {1: "nextval('t_id_seq')"}}
	cat.attrs[1] = map[int]*flashbackCatalogAttr{
		1: {RelID: 1, AttNum: 1, Name: "id", TypeName: "int4", NotNull: true, Default: "nextval('t_id_seq')"},
	}
	stmt, risk := flashbackFormatCreateTable(cat, "public", "t", 1, nil, nil)
	if !strings.Contains(stmt, "PRIMARY KEY") || !strings.Contains(stmt, "DEFAULT nextval") {
		t.Fatalf("stmt=%s", stmt)
	}
	if strings.Contains(risk, "未还原主键/索引/默认值") {
		t.Fatalf("risk should note leftover only: %s", risk)
	}
}

func TestFlashbackSynthesizeSchemaRename(t *testing.T) {
	cat := testCatalog()
	ch := flashbackSynthesizeDDL(cat, 9, []flashbackCatalogMut{
		{rel: flashbackCatalogNSName, op: "UPDATE",
			old: map[string]string{"oid": "2200", "nspname": "app"},
			new: map[string]string{"oid": "2200", "nspname": "app_v2"},
		},
	})
	if len(ch) != 1 || !strings.Contains(ch[0].DDLRedo, "ALTER SCHEMA") || !strings.Contains(ch[0].DDLUndo, "app_v2") {
		t.Fatalf("%+v", ch)
	}
}

func TestFlashbackWantDDL(t *testing.T) {
	if !flashbackWantDDL("") || !flashbackWantDDL("ddl") || flashbackWantDDL("insert") {
		t.Fatal("want ddl flags")
	}
}

func TestFlashbackMatchChangeDDL(t *testing.T) {
	dict := &flashbackDictionary{Wanted: map[string]*flashbackRelation{
		"public.t_old": {Schema: "public", Name: "t_old", Missing: true},
	}}
	ch := flashbackChange{Op: "ALTER", Schema: "public", Table: "t_new",
		DDLRedo: `ALTER TABLE "public"."t_old" RENAME TO "t_new";`,
		DDLUndo: `ALTER TABLE "public"."t_new" RENAME TO "t_old";`,
	}
	if !dict.matchChange(ch) {
		t.Fatal("quoted RENAME should match wanted public.t_old")
	}
	if dict.matchChange(flashbackChange{Op: "INSERT", Schema: "public", Table: "other"}) {
		t.Fatal("unrelated DML must not match")
	}
}
