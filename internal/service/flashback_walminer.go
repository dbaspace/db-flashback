package service

import (
	"fmt"
	"strings"

	"db-flashback/internal/service/dto"
)

const flashbackBulkInsertWarnRows = 1000

// flashbackAddWalMinerLimits 把 WalMiner 使用限制落到预检查（不因此失败）。
func flashbackAddWalMinerLimits(items *[]dto.FlashbackCheckItem, dict *flashbackDictionary, sqlType string) {
	if flashbackWantDDL(sqlType) {
		flashbackAddCheck(items, "scope_dml", "DML 与有限 DDL", flashbackCheckWarning,
			"可解析用户表 INSERT/UPDATE/DELETE，以及能从目录元组反推的 CREATE/DROP TABLE、RENAME、ADD/DROP COLUMN。TRUNCATE / VACUUM FULL / CLUSTER / 改表空间 / 改列类型 rewrite 后的数据、DROP 后整表行还原、库级/函数/触发器不做。")
		flashbackAddCheck(items, "ddl_catalog_image", "DDL 目录旧图像", flashbackCheckWarning,
			"DROP TABLE 还原 CREATE 需要 catalog 旧行图像（系统表主键 + full_page_writes 或 CONTAINS_OLD_TUPLE）。无旧图像时只能发现 DROP，不能编造建表语句。")
	} else {
		flashbackAddCheck(items, "scope_dml", "仅 DML", flashbackCheckWarning,
			"本次 sql_type 未包含 ddl，只解析 INSERT/UPDATE/DELETE。CREATE/ALTER/DROP/TRUNCATE 等不会生成 SQL。")
	}
	flashbackAddCheck(items, "dict_snapshot", "数据字典快照", flashbackCheckWarning,
		"任务创建时冻结 schema 到 dict.json，解析只用该快照（可用 dict_task_id 加载更早任务）。时间窗之后改过列类型且发生 rewrite 时，旧 DML 仍可能无法解码。")
	flashbackAddCheck(items, "timeline", "时间线", flashbackCheckPassed,
		"仅拉取与当前数据字典一致的 timeline 上的 WAL。")
	flashbackAddCheck(items, "bulk_copy", "大宗 COPY", flashbackCheckWarning,
		"不建议对同一事务内大宗 COPY / 大批量 INSERT 做闪回：解析慢且占内存，超过上限会截断。")
	flashbackAddCheck(items, "undo_match", "undo 匹配", flashbackCheckWarning,
		"有主键时按列值定位。无唯一键时 undo/redo 附加变更当时的 ctid；VACUUM 后 ctid 可能失效，不能当成对单条 tuple 的永久定位。")

	var rewritten, dropped, noUnique []string
	if dict != nil {
		for _, rel := range dict.Wanted {
			if rel == nil {
				continue
			}
			q := rel.Schema + "." + rel.Name
			if rel.OID != 0 && rel.RelNode != 0 && rel.OID != rel.RelNode {
				rewritten = append(rewritten, fmt.Sprintf("%s (oid=%d relfilenode=%d)", q, rel.OID, rel.RelNode))
			}
			for _, c := range rel.Columns {
				if c.Dropped {
					dropped = append(dropped, q+"."+c.Name)
				}
			}
			if len(rel.PKCols) == 0 && rel.ReplIdent != "f" && rel.ReplIdent != "i" {
				noUnique = append(noUnique, q)
			}
		}
	}
	if len(rewritten) > 0 {
		flashbackAddCheck(items, "rewrite", "表 rewrite", flashbackCheckWarning,
			"下列表 relfilenode 与 oid 不同，可能做过 VACUUM FULL / CLUSTER / TRUNCATE / 改表空间 / 改列类型。DDL 之前的 DML 通常无法再解析："+strings.Join(rewritten, "；"))
	} else {
		flashbackAddCheck(items, "rewrite", "表 rewrite", flashbackCheckWarning,
			"若时间窗内对该表做过 DROP/TRUNCATE、改表空间、改列类型或 VACUUM FULL，DDL 之前的该表 DML 不会再被解析。")
	}
	if len(dropped) > 0 {
		flashbackAddCheck(items, "dropped_cols", "已删除列", flashbackCheckWarning,
			"已 DROP 的列按 WalMiner 方式还原为 encode(..., 'hex')："+strings.Join(dropped, ", "))
	}
	if len(noUnique) > 0 {
		flashbackAddCheck(items, "no_unique", "无唯一键", flashbackCheckWarning,
			"下列表无主键且非 FULL/INDEX identity，undo 可能匹配多行："+strings.Join(noUnique, ", "))
	}
}
