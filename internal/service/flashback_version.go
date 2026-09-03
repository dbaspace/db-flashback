package service

import (
	"fmt"
	"strconv"
	"strings"
	"unicode"

	"db-flashback/internal/service/dto"
)

// 版本范围依据官网社区当前维护线：
// https://www.postgresql.org/community/ （2026-08-13：18.6 / 17.11 / 16.15 / 15.19 / 14.24 / 19 Beta 3）
// WAL 页 magic、Heap DML 操作码以各 REL_*_STABLE 的 xlog_internal.h / heapam_xlog.h 为准。
const (
	flashbackMinMajor      = 12
	flashbackMaxMajor      = 19
	flashbackVerifiedMajor = 15
)

func flashbackParseServerMajor(ver string) int {
	ver = strings.TrimSpace(ver)
	end := 0
	for end < len(ver) && unicode.IsDigit(rune(ver[end])) {
		end++
	}
	if end == 0 {
		return 0
	}
	n, _ := strconv.Atoi(ver[:end])
	return n
}

func flashbackMagicMajor(magic uint16) int {
	switch s := flashbackPageMagics[magic]; s {
	case "12":
		return 12
	case "13":
		return 13
	case "14":
		return 14
	case "15", "15+":
		return 15
	case "16":
		return 16
	case "17":
		return 17
	case "18":
		return 18
	case "19":
		return 19
	default:
		return 0
	}
}

// flashbackVersionGate 预检查版本。
func flashbackVersionGate(ver string) (status, msg string) {
	major := flashbackParseServerMajor(ver)
	if major == 0 {
		return flashbackCheckWarning, ver + "：无法解析大版本，将按 WAL 页 magic 尝试解析"
	}
	impacts := flashbackVersionImpactSummary(major)
	switch {
	case major < flashbackMinMajor:
		return flashbackCheckFailed, fmt.Sprintf("PostgreSQL %s（大版本 %d）早于 %d，XLog 记录布局过旧，不支持闪回", ver, major, flashbackMinMajor)
	case major > 19:
		return flashbackCheckWarning, fmt.Sprintf("PostgreSQL %s（大版本 %d）新于已矩阵验证的 12–19。Heap DML 操作码若未改可继续解析。%s",
			ver, major, impacts)
	case major == 19:
		return flashbackCheckPassed, fmt.Sprintf("%s：19 仍为 Beta，但矩阵已跑通同一套 DML/WAL 自测（页 magic 0xD121）。%s", ver, impacts)
	case major == flashbackVerifiedMajor:
		return flashbackCheckPassed, fmt.Sprintf("%s：基线版本，Heap/XLog 解析已实测（含矩阵）。%s", ver, impacts)
	case major >= flashbackMinMajor && major <= flashbackMaxMajor:
		return flashbackCheckPassed, fmt.Sprintf("%s：15–19 矩阵覆盖单表/多表/整库闪回任务。%s", ver, impacts)
	default:
		return flashbackCheckWarning, fmt.Sprintf("%s：页 magic 已知，按 PG15 Heap 布局解码。%s", ver, impacts)
	}
}

// flashbackVersionImpactSummary 各版本对闪回的影响（官方头文件差异，不是 GUC 清单）。
func flashbackVersionImpactSummary(major int) string {
	switch {
	case major <= 0:
		return ""
	case major <= 15:
		return "VACUUM/PRUNE 的 HEAP2 记录不参与 undo；COPY 走 HEAP2 MULTI_INSERT=0x50。"
	case major == 16:
		return "RelFileNode 改名为 RelFileLocator，磁盘仍是 3×Oid。HEAP2 PRUNE 结构有调整，闪回忽略 prune/vacuum/visible。"
	case major == 17:
		return "HEAP2 的 PRUNE/FREEZE/VACUUM 合并为 PRUNE_ON_ACCESS/VACUUM_SCAN/VACUUM_CLEANUP（操作码 0x10/0x20/0x30），MULTI_INSERT 仍为 0x50。"
	case major == 18:
		return "INPLACE 记录增了失效消息字段，闪回不解析 INPLACE；DML 操作码与 17 相同。"
	case major == 19:
		return "WAL 页 magic=0xD121；Heap INSERT/UPDATE/DELETE 与 MULTI_INSERT=0x50 与 18 相同，矩阵已实测。"
	default:
		return "20+ 以 WAL 页 magic 为准；若 Heap DML 操作码未改则按 18/19 布局解析。"
	}
}

func flashbackAddVersionImpacts(items *[]dto.FlashbackCheckItem, ver string) {
	major := flashbackParseServerMajor(ver)
	lines := []string{
		"对照官网内核头文件，且 12–19 已用容器/官方二进制矩阵跑通：页头/24 字节 XLogRecord/Heap INSERT·UPDATE·DELETE 稳定。",
		"HEAP2 MULTI_INSERT 官方操作码为 0x50（不是 0x40；0x40 在 HEAP 是 HOT_UPDATE，在 HEAP2 是 VISIBLE）。",
	}
	if s := flashbackVersionImpactSummary(major); s != "" {
		lines = append(lines, s)
	}
	flashbackAddCheck(items, "wal_abi", "WAL ABI 版本", flashbackCheckPassed, strings.Join(lines, " "))
}
