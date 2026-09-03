package service

import (
	"fmt"
	"sort"
	"strings"
)

func flashbackSynthesizeDDL(cat *flashbackCatalog, xid uint32, muts []flashbackCatalogMut) []flashbackChange {
	if len(muts) == 0 {
		return nil
	}
	var created, dropped []uint32
	renames := map[uint32][2]string{} // oid -> {old,new} name
	nsRenames := map[uint32][2]string{}
	addedCols := map[uint32][]flashbackCatalogAttr{}
	droppedCols := map[uint32][]flashbackCatalogAttr{}
	classOld := map[uint32]map[string]string{}
	classNew := map[uint32]map[string]string{}

	for _, m := range muts {
		switch m.rel {
		case flashbackCatalogClassName:
			oid := flashbackParseU32(pickRow(m.op, m.old, m.new)["oid"])
			if oid == 0 {
				continue
			}
			if m.old != nil {
				classOld[oid] = m.old
			}
			if m.new != nil {
				classNew[oid] = m.new
			}
			kind := strings.TrimSpace(pickRow(m.op, m.old, m.new)["relkind"])
			if !flashbackIsUserRelkind(kind) {
				continue
			}
			switch m.op {
			case "INSERT":
				created = append(created, oid)
			case "DELETE":
				dropped = append(dropped, oid)
			case "UPDATE":
				oldN := strings.TrimSpace(m.old["relname"])
				newN := strings.TrimSpace(m.new["relname"])
				if oldN != "" && newN != "" && oldN != newN &&
					!flashbackIsCatalogRelName(oldN) && !flashbackIsCatalogRelName(newN) {
					renames[oid] = [2]string{oldN, newN}
				}
			}
		case flashbackCatalogNSName:
			if m.op == "UPDATE" {
				oid := flashbackParseU32(m.new["oid"])
				oldN, newN := strings.TrimSpace(m.old["nspname"]), strings.TrimSpace(m.new["nspname"])
				if oid != 0 && oldN != "" && newN != "" && oldN != newN {
					nsRenames[oid] = [2]string{oldN, newN}
				}
			}
		case flashbackCatalogAttrName:
			row := pickRow(m.op, m.old, m.new)
			relid := flashbackParseU32(row["attrelid"])
			attnum := flashbackParseInt(row["attnum"])
			if relid == 0 || attnum <= 0 {
				continue
			}
			if flashbackParseBool(row["attisdropped"]) && m.op != "UPDATE" && m.op != "INSERT" {
				continue
			}
			attr := flashbackAttrFromRow(cat, row)
			switch m.op {
			case "INSERT":
				if !attr.Dropped {
					addedCols[relid] = append(addedCols[relid], attr)
				}
			case "DELETE":
				droppedCols[relid] = append(droppedCols[relid], flashbackAttrFromRow(cat, m.old))
			case "UPDATE":
				was := flashbackParseBool(m.old["attisdropped"])
				now := flashbackParseBool(m.new["attisdropped"])
				oldN, newN := strings.TrimSpace(m.old["attname"]), strings.TrimSpace(m.new["attname"])
				if !was && now {
					droppedCols[relid] = append(droppedCols[relid], flashbackAttrFromRow(cat, m.old))
				} else if oldN != "" && newN != "" && oldN != newN && !strings.Contains(newN, "pg.dropped") && !strings.Contains(oldN, "pg.dropped") {
					// column rename handled below as ALTER
					addedCols[relid] = append(addedCols[relid], flashbackCatalogAttr{RelID: relid, Name: oldN + "\x00" + newN, AttNum: -1})
				}
			}
		}
	}

	createdSet := map[uint32]struct{}{}
	droppedSet := map[uint32]struct{}{}
	for _, oid := range created {
		createdSet[oid] = struct{}{}
	}
	for _, oid := range dropped {
		droppedSet[oid] = struct{}{}
	}

	var out []flashbackChange
	for _, oid := range uniqueU32(created) {
		row := classNew[oid]
		schema, table := flashbackCatalogQual(cat, row, oid)
		if table == "" || flashbackIsCatalogRelName(table) {
			continue
		}
		stmt, risk := flashbackFormatCreateTable(cat, schema, table, oid, addedCols[oid], row)
		out = append(out, flashbackChange{
			XID: xid, Schema: schema, Table: table, Op: "CREATE", RelNode: flashbackParseU32(row["relfilenode"]),
			DDLRedo: stmt, DDLUndo: fmt.Sprintf("DROP TABLE IF EXISTS %s;", flashbackQualified(schema, table)), DDLRisk: risk,
		})
	}
	for _, oid := range uniqueU32(dropped) {
		row := classOld[oid]
		schema, table := flashbackCatalogQual(cat, row, oid)
		if table == "" && flashbackParseU32(row["oid"]) != 0 {
			out = append(out, flashbackChange{
				XID: xid, Op: "DROP", Schema: "pg_catalog", Table: fmt.Sprintf("oid_%d", flashbackParseU32(row["oid"])),
				DDLRedo: fmt.Sprintf("-- DROP TABLE oid=%d (缺名称)", flashbackParseU32(row["oid"])),
				DDLRisk: "缺旧行图像，无法还原 CREATE TABLE",
			})
			continue
		}
		if table == "" || flashbackIsCatalogRelName(table) {
			continue
		}
		create, risk := flashbackFormatCreateTable(cat, schema, table, oid, droppedCols[oid], row)
		if create == "" || !strings.Contains(create, "(") {
			risk = joinRisk(risk, "缺旧行图像，无法还原完整 CREATE TABLE")
			out = append(out, flashbackChange{
				XID: xid, Schema: schema, Table: table, Op: "DROP",
				DDLRedo: fmt.Sprintf("DROP TABLE IF EXISTS %s;", flashbackQualified(schema, table)),
				DDLRisk: risk,
			})
			continue
		}
		out = append(out, flashbackChange{
			XID: xid, Schema: schema, Table: table, Op: "DROP",
			DDLRedo: fmt.Sprintf("DROP TABLE IF EXISTS %s;", flashbackQualified(schema, table)),
			DDLUndo: create, DDLRisk: risk,
		})
	}
	for oid, pair := range renames {
		if _, ok := createdSet[oid]; ok {
			continue
		}
		if _, ok := droppedSet[oid]; ok {
			continue
		}
		schema := flashbackCatalogSchema(cat, classNew[oid], oid)
		out = append(out, flashbackChange{
			XID: xid, Schema: schema, Table: pair[1], Op: "ALTER",
			DDLRedo: fmt.Sprintf("ALTER TABLE %s RENAME TO %s;", flashbackQualified(schema, pair[0]), flashbackQuoteIdent(pair[1])),
			DDLUndo: fmt.Sprintf("ALTER TABLE %s RENAME TO %s;", flashbackQualified(schema, pair[1]), flashbackQuoteIdent(pair[0])),
		})
	}
	for oid, cols := range addedCols {
		if _, ok := createdSet[oid]; ok {
			continue
		}
		schema, table := flashbackCatalogQual(cat, classNew[oid], oid)
		if table == "" {
			schema, table = flashbackCatalogQual(cat, nil, oid)
		}
		if table == "" || flashbackIsCatalogRelName(table) {
			continue
		}
		for _, col := range cols {
			if col.AttNum < 0 && strings.Contains(col.Name, "\x00") {
				parts := strings.SplitN(col.Name, "\x00", 2)
				out = append(out, flashbackChange{
					XID: xid, Schema: schema, Table: table, Op: "ALTER",
					DDLRedo: fmt.Sprintf("ALTER TABLE %s RENAME COLUMN %s TO %s;", flashbackQualified(schema, table), flashbackQuoteIdent(parts[0]), flashbackQuoteIdent(parts[1])),
					DDLUndo: fmt.Sprintf("ALTER TABLE %s RENAME COLUMN %s TO %s;", flashbackQualified(schema, table), flashbackQuoteIdent(parts[1]), flashbackQuoteIdent(parts[0])),
				})
				continue
			}
			if col.Dropped || col.Name == "" || strings.Contains(col.Name, "pg.dropped") {
				continue
			}
			def := flashbackQuoteIdent(col.Name) + " " + flashbackDDLTypeName(col.TypeName)
			if col.NotNull {
				def += " NOT NULL"
			}
			out = append(out, flashbackChange{
				XID: xid, Schema: schema, Table: table, Op: "ALTER",
				DDLRedo: fmt.Sprintf("ALTER TABLE %s ADD COLUMN %s;", flashbackQualified(schema, table), def),
				DDLUndo: fmt.Sprintf("ALTER TABLE %s DROP COLUMN IF EXISTS %s;", flashbackQualified(schema, table), flashbackQuoteIdent(col.Name)),
			})
		}
	}
	for oid, cols := range droppedCols {
		if _, ok := droppedSet[oid]; ok {
			continue
		}
		schema, table := flashbackCatalogQual(cat, classNew[oid], oid)
		if table == "" {
			schema, table = flashbackCatalogQual(cat, classOld[oid], oid)
		}
		if table == "" {
			schema, table = flashbackCatalogQual(cat, nil, oid)
		}
		if table == "" || flashbackIsCatalogRelName(table) {
			continue
		}
		for _, col := range cols {
			if col.Name == "" || strings.Contains(col.Name, "pg.dropped") {
				continue
			}
			def := flashbackQuoteIdent(col.Name) + " " + flashbackDDLTypeName(col.TypeName)
			if col.NotNull {
				def += " NOT NULL"
			}
			undo := ""
			risk := ""
			if col.TypeName == "" {
				risk = "缺旧列类型，无法还原 ADD COLUMN"
			} else {
				undo = fmt.Sprintf("ALTER TABLE %s ADD COLUMN %s;", flashbackQualified(schema, table), def)
			}
			out = append(out, flashbackChange{
				XID: xid, Schema: schema, Table: table, Op: "ALTER",
				DDLRedo: fmt.Sprintf("ALTER TABLE %s DROP COLUMN IF EXISTS %s;", flashbackQualified(schema, table), flashbackQuoteIdent(col.Name)),
				DDLUndo: undo, DDLRisk: risk,
			})
		}
	}
	for _, pair := range nsRenames {
		oldN, newN := pair[0], pair[1]
		if flashbackIsSystemNamespace(oldN) || flashbackIsSystemNamespace(newN) {
			continue
		}
		out = append(out, flashbackChange{
			XID: xid, Schema: newN, Table: "", Op: "ALTER",
			DDLRedo: fmt.Sprintf("ALTER SCHEMA %s RENAME TO %s;", flashbackQuoteIdent(oldN), flashbackQuoteIdent(newN)),
			DDLUndo: fmt.Sprintf("ALTER SCHEMA %s RENAME TO %s;", flashbackQuoteIdent(newN), flashbackQuoteIdent(oldN)),
		})
	}
	return out
}

func flashbackIsSystemNamespace(name string) bool {
	n := strings.ToLower(strings.TrimSpace(name))
	return n == "pg_catalog" || n == "information_schema" || strings.HasPrefix(n, "pg_toast") || strings.HasPrefix(n, "pg_temp")
}

func pickRow(op string, old, new map[string]string) map[string]string {
	if op == "DELETE" {
		return old
	}
	if len(new) > 0 {
		return new
	}
	return old
}

func flashbackAttrFromRow(cat *flashbackCatalog, row map[string]string) flashbackCatalogAttr {
	a := flashbackCatalogAttr{
		RelID:   flashbackParseU32(row["attrelid"]),
		AttNum:  flashbackParseInt(row["attnum"]),
		Name:    strings.TrimSpace(row["attname"]),
		TypeOID: flashbackParseU32(row["atttypid"]),
		Typlen:  flashbackParseInt(row["attlen"]),
		Align:   strings.TrimSpace(row["attalign"]),
		NotNull: flashbackParseBool(row["attnotnull"]),
		Dropped: flashbackParseBool(row["attisdropped"]),
	}
	if t, ok := cat.types[a.TypeOID]; ok {
		a.TypeName = t.TypeName
		if a.Typlen == 0 {
			a.Typlen = t.Typlen
		}
		if a.Align == "" {
			a.Align = t.Align
		}
	}
	return a
}

func flashbackCatalogQual(cat *flashbackCatalog, row map[string]string, oid uint32) (schema, table string) {
	if row != nil {
		table = strings.TrimSpace(row["relname"])
		if nsp := flashbackParseU32(row["relnamespace"]); nsp != 0 {
			schema = cat.namespaces[nsp]
		}
	}
	cl := cat.classes[oid]
	if cl == nil && cat.graveyardClass != nil {
		cl = cat.graveyardClass[oid]
	}
	if cl != nil {
		if table == "" {
			table = cl.Name
		}
		if schema == "" {
			schema = cat.namespaces[cl.Namespace]
		}
	}
	if schema == "" {
		schema = "public"
	}
	return schema, table
}

func flashbackCatalogSchema(cat *flashbackCatalog, row map[string]string, oid uint32) string {
	s, _ := flashbackCatalogQual(cat, row, oid)
	return s
}

func flashbackFormatCreateTable(cat *flashbackCatalog, schema, table string, oid uint32, extra []flashbackCatalogAttr, row map[string]string) (string, string) {
	cols := map[int]flashbackCatalogAttr{}
	src := cat.attrs[oid]
	if len(src) == 0 && cat.graveyardAttrs != nil {
		src = cat.graveyardAttrs[oid]
	}
	for n, a := range src {
		if a != nil && !a.Dropped && a.Name != "" && !strings.Contains(a.Name, "pg.dropped") {
			cols[n] = *a
		}
	}
	for _, a := range extra {
		if a.AttNum > 0 && !a.Dropped && a.Name != "" {
			cols[a.AttNum] = a
		}
	}
	if len(cols) == 0 {
		return "", "无列定义"
	}
	var nums []int
	for n := range cols {
		nums = append(nums, n)
	}
	sort.Ints(nums)
	var parts []string
	for _, n := range nums {
		a := cols[n]
		typ := flashbackDDLTypeName(a.TypeName)
		if typ == "" {
			typ = "text"
		}
		p := flashbackQuoteIdent(a.Name) + " " + typ
		if a.NotNull {
			p += " NOT NULL"
		}
		if def := flashbackCatalogDefault(cat, oid, n, a); def != "" {
			p += " DEFAULT " + def
		}
		parts = append(parts, p)
	}
	if pks := flashbackCatalogPKs(cat, oid); len(pks) > 0 {
		var q []string
		for _, n := range pks {
			q = append(q, flashbackQuoteIdent(n))
		}
		parts = append(parts, "PRIMARY KEY ("+strings.Join(q, ", ")+")")
	}
	risk := "未还原二级索引/检查约束/外键"
	if flashbackCatalogPKs(cat, oid) == nil && flashbackCatalogDefault(cat, oid, 0, flashbackCatalogAttr{}) == "" {
		hasDef := false
		for _, a := range cols {
			if flashbackCatalogDefault(cat, oid, a.AttNum, a) != "" {
				hasDef = true
				break
			}
		}
		if !hasDef {
			risk = "未还原主键/索引/默认值/约束"
		}
	}
	_ = row
	return fmt.Sprintf("CREATE TABLE %s (%s);", flashbackQualified(schema, table), strings.Join(parts, ", ")), risk
}

func flashbackCatalogPKs(cat *flashbackCatalog, oid uint32) []string {
	if cat == nil || cat.pks == nil {
		return nil
	}
	return cat.pks[oid]
}

func flashbackCatalogDefault(cat *flashbackCatalog, oid uint32, attnum int, a flashbackCatalogAttr) string {
	if strings.TrimSpace(a.Default) != "" {
		return strings.TrimSpace(a.Default)
	}
	if cat == nil || cat.defaults == nil {
		return ""
	}
	return strings.TrimSpace(cat.defaults[oid][attnum])
}

func flashbackDDLTypeName(typ string) string {
	typ = strings.TrimSpace(strings.ToLower(typ))
	if typ == "" {
		return ""
	}
	return typ
}

func flashbackIsUserRelkind(kind string) bool {
	switch strings.TrimSpace(kind) {
	case "", "r", "p":
		return true
	default:
		return false
	}
}

func flashbackIsCatalogRelName(name string) bool {
	n := strings.ToLower(strings.TrimSpace(name))
	return strings.HasPrefix(n, "pg_toast") || strings.HasPrefix(n, "pg_temp")
}

func uniqueU32(in []uint32) []uint32 {
	seen := map[uint32]struct{}{}
	var out []uint32
	for _, v := range in {
		if _, ok := seen[v]; ok {
			continue
		}
		seen[v] = struct{}{}
		out = append(out, v)
	}
	return out
}
