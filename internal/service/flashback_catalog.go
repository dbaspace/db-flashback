package service

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

const (
	flashbackCatalogClassName = "pg_class"
	flashbackCatalogAttrName  = "pg_attribute"
	flashbackCatalogNSName    = "pg_namespace"
	flashbackCatalogTypeName  = "pg_type"
)

type flashbackCatalogClass struct {
	OID       uint32
	Namespace uint32
	RelNode   uint32
	Name      string
	Kind      string
}

type flashbackCatalogAttr struct {
	RelID    uint32
	AttNum   int
	Name     string
	TypeOID  uint32
	Typlen   int
	Align    string
	NotNull  bool
	Dropped  bool
	TypeName string
	Default  string
}

type flashbackCatalogMut struct {
	xid     uint32
	rel     string // pg_class / pg_attribute / pg_namespace / pg_type
	op      string
	old     map[string]string
	new     map[string]string
	relNode uint32
}

type flashbackCatalog struct {
	classRel *flashbackRelation
	attrRel  *flashbackRelation
	nsRel    *flashbackRelation
	typeRel  *flashbackRelation
	byNode   map[uint32]*flashbackRelation // catalog decoder by relfilenode

	namespaces     map[uint32]string
	types          map[uint32]flashbackCatalogAttr // oid -> typname/typlen/align (reuse attr)
	classes        map[uint32]*flashbackCatalogClass
	attrs          map[uint32]map[int]*flashbackCatalogAttr
	nodeToOID      map[uint32]uint32
	graveyardClass map[uint32]*flashbackCatalogClass
	graveyardAttrs map[uint32]map[int]*flashbackCatalogAttr
	pks            map[uint32][]string
	defaults       map[uint32]map[int]string

	pending map[uint32][]flashbackCatalogMut
}

func flashbackAttachCatalog(ctx context.Context, db *sql.DB, dict *flashbackDictionary) error {
	if dict == nil {
		return fmt.Errorf("nil dictionary")
	}
	cat := &flashbackCatalog{
		byNode:         map[uint32]*flashbackRelation{},
		namespaces:     map[uint32]string{},
		types:          map[uint32]flashbackCatalogAttr{},
		classes:        map[uint32]*flashbackCatalogClass{},
		attrs:          map[uint32]map[int]*flashbackCatalogAttr{},
		nodeToOID:      map[uint32]uint32{},
		graveyardClass: map[uint32]*flashbackCatalogClass{},
		graveyardAttrs: map[uint32]map[int]*flashbackCatalogAttr{},
		pks:            map[uint32][]string{},
		defaults:       map[uint32]map[int]string{},
		pending:        map[uint32][]flashbackCatalogMut{},
	}
	for _, name := range []string{flashbackCatalogClassName, flashbackCatalogAttrName, flashbackCatalogNSName, flashbackCatalogTypeName} {
		rel, err := flashbackLoadRelation(ctx, db, "pg_catalog", name)
		if err != nil {
			return fmt.Errorf("catalog %s: %w", name, err)
		}
		rel.Schema = "pg_catalog"
		cat.byNode[rel.RelNode] = rel
		if rel.OID != 0 && rel.OID != rel.RelNode {
			cat.byNode[rel.OID] = rel
		}
		switch name {
		case flashbackCatalogClassName:
			cat.classRel = rel
		case flashbackCatalogAttrName:
			cat.attrRel = rel
		case flashbackCatalogNSName:
			cat.nsRel = rel
		case flashbackCatalogTypeName:
			cat.typeRel = rel
		}
	}
	if err := cat.loadSnapshot(ctx, db, dict); err != nil {
		return err
	}
	dict.Catalog = cat
	return nil
}

func (c *flashbackCatalog) loadSnapshot(ctx context.Context, db *sql.DB, dict *flashbackDictionary) error {
	ns, err := db.QueryContext(ctx, `SELECT oid, nspname FROM pg_namespace`)
	if err != nil {
		return err
	}
	for ns.Next() {
		var oid uint32
		var name string
		if err := ns.Scan(&oid, &name); err != nil {
			ns.Close()
			return err
		}
		c.namespaces[oid] = name
	}
	ns.Close()
	if err := ns.Err(); err != nil {
		return err
	}

	ts, err := db.QueryContext(ctx, `SELECT oid, typname, typlen, typalign::text FROM pg_type`)
	if err != nil {
		return err
	}
	for ts.Next() {
		var a flashbackCatalogAttr
		if err := ts.Scan(&a.TypeOID, &a.TypeName, &a.Typlen, &a.Align); err != nil {
			ts.Close()
			return err
		}
		c.types[a.TypeOID] = a
	}
	ts.Close()
	if err := ts.Err(); err != nil {
		return err
	}

	for _, rel := range dict.Wanted {
		if rel == nil || rel.Missing || rel.OID == 0 {
			continue
		}
		cl := &flashbackCatalogClass{OID: rel.OID, RelNode: rel.RelNode, Name: rel.Name, Kind: "r"}
		for nsp, name := range c.namespaces {
			if strings.EqualFold(name, rel.Schema) {
				cl.Namespace = nsp
				break
			}
		}
		c.classes[rel.OID] = cl
		if rel.RelNode != 0 {
			c.nodeToOID[rel.RelNode] = rel.OID
		}
		am := map[int]*flashbackCatalogAttr{}
		defs := map[int]string{}
		for _, col := range rel.Columns {
			am[col.AttNum] = &flashbackCatalogAttr{
				RelID: rel.OID, AttNum: col.AttNum, Name: col.Name,
				TypeOID: col.TypeOID, Typlen: col.Typlen, Align: col.Typalign,
				NotNull: col.NotNull, Dropped: col.Dropped, TypeName: col.TypeName,
				Default: col.Default,
			}
			if strings.TrimSpace(col.Default) != "" {
				defs[col.AttNum] = col.Default
			}
		}
		c.attrs[rel.OID] = am
		if len(rel.PKCols) > 0 {
			c.pks[rel.OID] = append([]string(nil), rel.PKCols...)
		}
		if len(defs) > 0 {
			c.defaults[rel.OID] = defs
		}
	}
	return nil
}

func (c *flashbackCatalog) enrichOld(rel, op string, old, new map[string]string) map[string]string {
	if op != "UPDATE" && op != "DELETE" {
		return old
	}
	row := old
	if len(row) == 0 {
		row = new
	}
	out := flashbackCopyStrMap(old)
	switch rel {
	case flashbackCatalogClassName:
		oid := flashbackParseU32(out["oid"])
		if oid == 0 {
			oid = flashbackParseU32(row["oid"])
		}
		if oid == 0 {
			oid = flashbackParseU32(new["oid"])
		}
		cl := c.classes[oid]
		if cl == nil {
			return out
		}
		if strings.TrimSpace(out["oid"]) == "" {
			out["oid"] = strconv.FormatUint(uint64(oid), 10)
		}
		if strings.TrimSpace(out["relname"]) == "" && cl.Name != "" {
			out["relname"] = cl.Name
		}
		if strings.TrimSpace(out["relnamespace"]) == "" && cl.Namespace != 0 {
			out["relnamespace"] = strconv.FormatUint(uint64(cl.Namespace), 10)
		}
		if strings.TrimSpace(out["relkind"]) == "" && cl.Kind != "" {
			out["relkind"] = cl.Kind
		}
		if strings.TrimSpace(out["relfilenode"]) == "" && cl.RelNode != 0 {
			out["relfilenode"] = strconv.FormatUint(uint64(cl.RelNode), 10)
		}
	case flashbackCatalogAttrName:
		relid := flashbackParseU32(out["attrelid"])
		if relid == 0 {
			relid = flashbackParseU32(row["attrelid"])
		}
		if relid == 0 {
			relid = flashbackParseU32(new["attrelid"])
		}
		attnum := flashbackParseInt(out["attnum"])
		if attnum == 0 {
			attnum = flashbackParseInt(row["attnum"])
		}
		if attnum == 0 {
			attnum = flashbackParseInt(new["attnum"])
		}
		a := c.attrs[relid][attnum]
		if a == nil {
			return out
		}
		if strings.TrimSpace(out["attrelid"]) == "" {
			out["attrelid"] = strconv.FormatUint(uint64(relid), 10)
		}
		if strings.TrimSpace(out["attnum"]) == "" {
			out["attnum"] = strconv.Itoa(attnum)
		}
		if strings.TrimSpace(out["attname"]) == "" && a.Name != "" {
			out["attname"] = a.Name
		}
		if strings.TrimSpace(out["atttypid"]) == "" && a.TypeOID != 0 {
			out["atttypid"] = strconv.FormatUint(uint64(a.TypeOID), 10)
		}
		if strings.TrimSpace(out["attlen"]) == "" && a.Typlen != 0 {
			out["attlen"] = strconv.Itoa(a.Typlen)
		}
		if strings.TrimSpace(out["attalign"]) == "" && a.Align != "" {
			out["attalign"] = a.Align
		}
	}
	return out
}

func (c *flashbackCatalog) discardXID(xid uint32) {
	if c == nil {
		return
	}
	delete(c.pending, xid)
}

func flashbackCopyStrMap(in map[string]string) map[string]string {
	out := map[string]string{}
	for k, v := range in {
		out[k] = v
	}
	return out
}

func (c *flashbackCatalog) decoder(relNode uint32) *flashbackRelation {
	if c == nil {
		return nil
	}
	return c.byNode[relNode]
}

func (c *flashbackCatalog) apply(xid uint32, rel *flashbackRelation, op string, old, new map[string]string, relNode uint32) {
	if c == nil || rel == nil {
		return
	}
	name := rel.Name
	old = c.enrichOld(name, op, old, new)
	c.pending[xid] = append(c.pending[xid], flashbackCatalogMut{xid: xid, rel: name, op: op, old: old, new: new, relNode: relNode})
	switch name {
	case flashbackCatalogNSName:
		row := new
		if op == "DELETE" {
			row = old
		}
		if oid := flashbackParseU32(row["oid"]); oid != 0 {
			if op == "DELETE" {
				delete(c.namespaces, oid)
			} else if n := strings.TrimSpace(row["nspname"]); n != "" {
				c.namespaces[oid] = n
			}
		}
	case flashbackCatalogTypeName:
		row := new
		if op == "DELETE" {
			row = old
		}
		if oid := flashbackParseU32(row["oid"]); oid != 0 {
			if op == "DELETE" {
				delete(c.types, oid)
			} else {
				c.types[oid] = flashbackCatalogAttr{
					TypeOID:  oid,
					TypeName: strings.TrimSpace(row["typname"]),
					Typlen:   flashbackParseInt(row["typlen"]),
					Align:    strings.TrimSpace(row["typalign"]),
				}
			}
		}
	case flashbackCatalogClassName:
		c.applyClass(op, old, new)
	case flashbackCatalogAttrName:
		c.applyAttr(op, old, new)
	}
}

func (c *flashbackCatalog) applyClass(op string, old, new map[string]string) {
	row := new
	if op == "DELETE" {
		row = old
	}
	oid := flashbackParseU32(row["oid"])
	if oid == 0 {
		return
	}
	if op == "DELETE" {
		if cl := c.classes[oid]; cl != nil {
			cp := *cl
			c.graveyardClass[oid] = &cp
			if cl.RelNode != 0 {
				delete(c.nodeToOID, cl.RelNode)
			}
		}
		if am := c.attrs[oid]; am != nil {
			c.graveyardAttrs[oid] = am
		}
		delete(c.classes, oid)
		delete(c.attrs, oid)
		return
	}
	if kind := strings.TrimSpace(row["relkind"]); kind != "" && !flashbackIsUserRelkind(kind) {
		return
	}
	if flashbackIsCatalogRelName(strings.TrimSpace(row["relname"])) {
		return
	}
	cl := c.classes[oid]
	if cl == nil {
		cl = &flashbackCatalogClass{OID: oid}
		c.classes[oid] = cl
	}
	if v := strings.TrimSpace(row["relname"]); v != "" {
		cl.Name = v
	}
	if v := flashbackParseU32(row["relnamespace"]); v != 0 {
		cl.Namespace = v
	}
	if v := strings.TrimSpace(row["relkind"]); v != "" {
		cl.Kind = v
	}
	node := flashbackParseU32(row["relfilenode"])
	if node == 0 {
		node = oid
	}
	if cl.RelNode != 0 && cl.RelNode != node {
		delete(c.nodeToOID, cl.RelNode)
	}
	cl.RelNode = node
	c.nodeToOID[node] = oid
}

func (c *flashbackCatalog) applyAttr(op string, old, new map[string]string) {
	row := new
	if op == "DELETE" {
		row = old
	}
	relid := flashbackParseU32(row["attrelid"])
	attnum := flashbackParseInt(row["attnum"])
	if relid == 0 || attnum <= 0 {
		return
	}
	if c.attrs[relid] == nil {
		c.attrs[relid] = map[int]*flashbackCatalogAttr{}
	}
	if op == "DELETE" {
		delete(c.attrs[relid], attnum)
		return
	}
	a := c.attrs[relid][attnum]
	if a == nil {
		a = &flashbackCatalogAttr{RelID: relid, AttNum: attnum}
		c.attrs[relid][attnum] = a
	}
	if v := strings.TrimSpace(row["attname"]); v != "" {
		a.Name = v
	}
	if v := flashbackParseU32(row["atttypid"]); v != 0 {
		a.TypeOID = v
		if t, ok := c.types[v]; ok {
			a.TypeName = t.TypeName
			if a.Typlen == 0 {
				a.Typlen = t.Typlen
			}
			if a.Align == "" {
				a.Align = t.Align
			}
		}
	}
	if v := row["attlen"]; v != "" {
		a.Typlen = flashbackParseInt(v)
	}
	if v := strings.TrimSpace(row["attalign"]); v != "" {
		a.Align = v
	}
	a.NotNull = flashbackParseBool(row["attnotnull"])
	a.Dropped = flashbackParseBool(row["attisdropped"])
}

func (c *flashbackCatalog) userRelation(relNode uint32) *flashbackRelation {
	if c == nil || relNode == 0 {
		return nil
	}
	oid := c.nodeToOID[relNode]
	if oid == 0 {
		return nil
	}
	cl := c.classes[oid]
	if cl == nil || (cl.Kind != "r" && cl.Kind != "p" && cl.Kind != "") {
		return nil
	}
	rel := &flashbackRelation{
		Schema: c.namespaces[cl.Namespace], Name: cl.Name,
		OID: cl.OID, RelNode: cl.RelNode, colByNum: map[int]flashbackColumn{},
	}
	if rel.Schema == "" {
		rel.Schema = "public"
	}
	var nums []int
	for n := range c.attrs[oid] {
		nums = append(nums, n)
	}
	sort.Ints(nums)
	for _, n := range nums {
		a := c.attrs[oid][n]
		if a == nil {
			continue
		}
		col := flashbackColumn{
			Name: a.Name, AttNum: a.AttNum, TypeName: a.TypeName, TypeOID: a.TypeOID,
			Typlen: a.Typlen, Typalign: a.Align, NotNull: a.NotNull, Dropped: a.Dropped,
		}
		if col.TypeName == "" {
			if t, ok := c.types[a.TypeOID]; ok {
				col.TypeName = t.TypeName
				if col.Typlen == 0 {
					col.Typlen = t.Typlen
				}
				if col.Typalign == "" {
					col.Typalign = t.Align
				}
			}
		}
		rel.Columns = append(rel.Columns, col)
		rel.colByNum[col.AttNum] = col
	}
	if len(rel.Columns) == 0 {
		return nil
	}
	return rel
}

func (c *flashbackCatalog) flushXID(xid uint32) []flashbackChange {
	if c == nil {
		return nil
	}
	muts := c.pending[xid]
	delete(c.pending, xid)
	return flashbackSynthesizeDDL(c, xid, muts)
}

func (c *flashbackCatalog) flushAll() []flashbackChange {
	if c == nil || len(c.pending) == 0 {
		return nil
	}
	var xids []uint32
	for x := range c.pending {
		xids = append(xids, x)
	}
	sort.Slice(xids, func(i, j int) bool { return xids[i] < xids[j] })
	var out []flashbackChange
	for _, x := range xids {
		out = append(out, c.flushXID(x)...)
	}
	return out
}

func flashbackParseU32(s string) uint32 {
	s = strings.TrimSpace(s)
	if s == "" || s == `\N` {
		return 0
	}
	n, _ := strconv.ParseUint(s, 10, 32)
	return uint32(n)
}

func flashbackParseInt(s string) int {
	s = strings.TrimSpace(s)
	if s == "" || s == `\N` {
		return 0
	}
	n, _ := strconv.Atoi(s)
	return n
}

func flashbackParseBool(s string) bool {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "t", "true", "1":
		return true
	default:
		return false
	}
}
