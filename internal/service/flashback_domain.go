package service

import (
	"context"
	"fmt"
	"strings"

	"go.uber.org/zap"

	"db-flashback/internal/service/dto"
	"db-flashback/internal/storage/databases/ent"
	"db-flashback/internal/storage/flashback"
	"db-flashback/pkg/utils/log"
)

const (
	flashbackDomainWarnNotHub     = "未能解析 Hub domain_instances.id，请勿用任务 id 作为实例 id"
	flashbackDomainWarnEqualsTask = "解析到的实例 id 与任务 id 相同，已清空 instance_id"
)

// flashbackHubDomain 解析结果用的最小域实例视图（可注入，便于单测）。
type flashbackHubDomain struct {
	ID            string
	MDMInstanceID string
}

type flashbackHubDomainLookup struct {
	GetByID        func(id string) *flashbackHubDomain
	ListByHostPort func(host string, port int) []*flashbackHubDomain
	ListByMDM      func(mdmID string) []*flashbackHubDomain
}

type flashbackResolvedDomain struct {
	InstanceID    string
	MDMInstanceID string
	Warning       string
	Changed       bool
}

func flashbackSanitizeTaskIDs(taskID, candidate string) string {
	taskID = strings.TrimSpace(taskID)
	candidate = strings.TrimSpace(candidate)
	if candidate == "" || candidate == taskID {
		return ""
	}
	return candidate
}

func flashbackJoinWarning(existing, extra string) string {
	existing = strings.TrimSpace(existing)
	extra = strings.TrimSpace(extra)
	if extra == "" {
		return existing
	}
	if existing == "" {
		return extra
	}
	if strings.Contains(existing, extra) {
		return existing
	}
	return existing + "；" + extra
}

func flashbackUniqueHubDomain(cands []*flashbackHubDomain) *flashbackHubDomain {
	var first *flashbackHubDomain
	seen := map[string]struct{}{}
	for _, c := range cands {
		if c == nil {
			continue
		}
		id := strings.TrimSpace(c.ID)
		if id == "" {
			continue
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		if first == nil {
			cp := *c
			first = &cp
		}
	}
	if len(seen) != 1 {
		return nil
	}
	return first
}

func flashbackHubDomainFromEnt(dom *ent.DomainInstance) *flashbackHubDomain {
	if dom == nil {
		return nil
	}
	return &flashbackHubDomain{ID: strings.TrimSpace(dom.ID), MDMInstanceID: strings.TrimSpace(dom.InstanceID)}
}

func flashbackUniqueEnt(cands []*ent.DomainInstance) *ent.DomainInstance {
	var first *ent.DomainInstance
	seen := map[string]struct{}{}
	for _, d := range cands {
		if d == nil {
			continue
		}
		id := strings.TrimSpace(d.ID)
		if id == "" {
			continue
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		if first == nil {
			first = d
		}
	}
	if len(seen) != 1 {
		return nil
	}
	return first
}

func flashbackQueryDomainEntByHostPort(_ context.Context, host string, port int) []*ent.DomainInstance {
	inst, err := lookupConfiguredInstanceByHostPort(host, port)
	if err != nil {
		return nil
	}
	return []*ent.DomainInstance{instanceToDomain(inst)}
}

func flashbackQueryDomainEntByMDM(_ context.Context, mdmID string) []*ent.DomainInstance {
	mdmID = strings.TrimSpace(mdmID)
	if mdmID == "" {
		return nil
	}
	if inst, err := lookupConfiguredInstance(mdmID); err == nil {
		return []*ent.DomainInstance{instanceToDomain(inst)}
	}
	cfg := runtimeConfig()
	if cfg == nil {
		return nil
	}
	var out []*ent.DomainInstance
	for _, inst := range cfg.Instances {
		if strings.TrimSpace(inst.CloudInstanceID) == mdmID {
			out = append(out, instanceToDomain(inst))
		}
	}
	return out
}

func flashbackLookupDomainInstance(ctx context.Context, instanceID, host string, port int) (*ent.DomainInstance, error) {
	if inst, err := lookupConfiguredInstance(instanceID); err == nil {
		return instanceToDomain(inst), nil
	}
	if d := flashbackUniqueEnt(flashbackQueryDomainEntByHostPort(ctx, host, port)); d != nil {
		return d, nil
	}
	if d := flashbackUniqueEnt(flashbackQueryDomainEntByMDM(ctx, instanceID)); d != nil {
		return d, nil
	}
	return nil, fmt.Errorf("instance not found")
}

// flashbackBindTaskHubDomain 把任务绑到真实 Hub domain_instances.id，且保证 ≠ 任务 id。
func flashbackBindTaskHubDomain(row *flashback.TaskRow, hubID, mdmID string) error {
	if row == nil {
		return fmt.Errorf("task row is nil")
	}
	hubID = strings.TrimSpace(hubID)
	if hubID == "" {
		return fmt.Errorf("Hub domain_instances.id 为空")
	}
	if strings.TrimSpace(row.ID) == "" {
		row.ID = flashback.NewID()
	}
	if hubID == strings.TrimSpace(row.ID) {
		row.ID = flashback.NewID()
		if hubID == strings.TrimSpace(row.ID) {
			return fmt.Errorf("无法分配与实例 id 不同的任务 id")
		}
	}
	row.InstanceID = hubID
	row.MDMInstanceID = strings.TrimSpace(mdmID)
	return nil
}

func flashbackAssignTaskHubDomain(ctx context.Context, row *flashback.TaskRow, reqInstanceID, host string, port int) error {
	dom, err := flashbackLookupDomainInstance(ctx, reqInstanceID, host, port)
	if err != nil {
		return err
	}
	return flashbackBindTaskHubDomain(row, dom.ID, dom.InstanceID)
}

func flashbackResolveHubDomainWith(row *flashback.TaskRow, lookup flashbackHubDomainLookup) flashbackResolvedDomain {
	out := flashbackResolvedDomain{}
	if row == nil {
		return out
	}
	taskID := strings.TrimSpace(row.ID)
	stored := strings.TrimSpace(row.InstanceID)
	storedMDM := strings.TrimSpace(row.MDMInstanceID)
	host := strings.TrimSpace(row.Host)
	port := row.Port

	var found *flashbackHubDomain
	if stored != "" && stored != taskID && lookup.GetByID != nil {
		found = lookup.GetByID(stored)
	}
	if found == nil && host != "" && port > 0 && lookup.ListByHostPort != nil {
		found = flashbackUniqueHubDomain(lookup.ListByHostPort(host, port))
	}
	if found == nil && storedMDM != "" && storedMDM != taskID && lookup.ListByMDM != nil {
		found = flashbackUniqueHubDomain(lookup.ListByMDM(storedMDM))
	}
	if found == nil && stored != "" && stored != taskID && lookup.ListByMDM != nil {
		found = flashbackUniqueHubDomain(lookup.ListByMDM(stored))
	}

	if found != nil {
		hubID := strings.TrimSpace(found.ID)
		mdmID := flashbackSanitizeTaskIDs(taskID, found.MDMInstanceID)
		if hubID == "" || hubID == taskID {
			out.MDMInstanceID = mdmID
			out.Warning = flashbackDomainWarnEqualsTask
			out.Changed = stored != "" || storedMDM != mdmID
			return out
		}
		out.InstanceID = hubID
		out.MDMInstanceID = mdmID
		out.Changed = out.InstanceID != stored || out.MDMInstanceID != storedMDM
		return out
	}

	if stored == "" || stored == taskID {
		out.MDMInstanceID = flashbackSanitizeTaskIDs(taskID, storedMDM)
		out.Warning = flashbackDomainWarnNotHub
		out.Changed = stored == taskID
		return out
	}
	out.InstanceID = stored
	out.MDMInstanceID = flashbackSanitizeTaskIDs(taskID, storedMDM)
	out.Warning = flashbackDomainWarnNotHub
	return out
}

func flashbackLiveHubDomainLookup(ctx context.Context) flashbackHubDomainLookup {
	return flashbackHubDomainLookup{
		GetByID: func(id string) *flashbackHubDomain {
			inst, err := lookupConfiguredInstance(id)
			if err != nil {
				return nil
			}
			return flashbackHubDomainFromEnt(instanceToDomain(inst))
		},
		ListByHostPort: func(host string, port int) []*flashbackHubDomain {
			ents := flashbackQueryDomainEntByHostPort(ctx, host, port)
			out := make([]*flashbackHubDomain, 0, len(ents))
			for _, d := range ents {
				if v := flashbackHubDomainFromEnt(d); v != nil {
					out = append(out, v)
				}
			}
			return out
		},
		ListByMDM: func(mdmID string) []*flashbackHubDomain {
			ents := flashbackQueryDomainEntByMDM(ctx, mdmID)
			out := make([]*flashbackHubDomain, 0, len(ents))
			for _, d := range ents {
				if v := flashbackHubDomainFromEnt(d); v != nil {
					out = append(out, v)
				}
			}
			return out
		},
	}
}

func flashbackResolveHubDomain(ctx context.Context, row *flashback.TaskRow) flashbackResolvedDomain {
	return flashbackResolveHubDomainWith(row, flashbackLiveHubDomainLookup(ctx))
}

func (s *FlashbackImpl) persistResolvedDomain(ctx context.Context, row *flashback.TaskRow, res flashbackResolvedDomain) {
	if !res.Changed || row == nil || strings.TrimSpace(row.ID) == "" {
		return
	}
	if res.InstanceID == "" && flashbackSanitizeTaskIDs(row.ID, row.InstanceID) != "" {
		return
	}
	if err := s.store.UpdateInstanceIDs(ctx, row.ID, res.InstanceID, res.MDMInstanceID); err != nil {
		log.Warn("flashback persist resolved domain", zap.Error(err), zap.String("task", row.ID))
		return
	}
	row.InstanceID = res.InstanceID
	row.MDMInstanceID = res.MDMInstanceID
}

func flashbackTaskFromResolved(r *flashback.TaskRow, res flashbackResolvedDomain) *dto.FlashbackTask {
	task := flashbackTaskFromRow(r)
	if task == nil {
		return nil
	}
	hubID := flashbackSanitizeTaskIDs(r.ID, res.InstanceID)
	mdmID := flashbackSanitizeTaskIDs(r.ID, res.MDMInstanceID)
	task.InstanceID = hubID
	task.DomainInstanceID = hubID
	task.MDMInstanceID = mdmID
	if res.Warning != "" {
		task.Warning = flashbackJoinWarning(task.Warning, res.Warning)
	}
	return task
}

func (s *FlashbackImpl) taskDTO(ctx context.Context, row *flashback.TaskRow) *dto.FlashbackTask {
	res := flashbackResolveHubDomain(ctx, row)
	s.persistResolvedDomain(ctx, row, res)
	return flashbackTaskFromResolved(row, res)
}
