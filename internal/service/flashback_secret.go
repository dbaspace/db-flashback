package service

import (
	"context"
	"strings"

	secrypto "db-flashback/internal/crypto"
	"db-flashback/pkg/utils/log"

	"go.uber.org/zap"
)

func flashbackMigrateSecrets(ctx context.Context) {
	if !secrypto.HasKey() {
		log.Warn("flashback data key missing; instance user/password and cloud secrets stay plaintext until flashback.data_key is generated or FLASHBACK_DATA_KEY is set")
		return
	}
	nInst, err := flashbackMigrateInstanceSecrets(ctx)
	if err != nil {
		log.Warn("flashback migrate instance secrets", zap.Error(err))
	} else if nInst > 0 {
		log.Info("flashback encrypted instance secrets", zap.Int("count", nInst))
	}
	nArg, err := flashbackMigrateArgSecrets(ctx)
	if err != nil {
		log.Warn("flashback migrate cloud secrets", zap.Error(err))
	} else if nArg > 0 {
		log.Info("flashback encrypted cloud secrets", zap.Int("count", nArg))
	}
}

func flashbackMigrateInstanceSecrets(ctx context.Context) (int, error) {
	rows, err := flashbackStore.ListInstancesRaw(ctx)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, r := range rows {
		needUser := strings.TrimSpace(r.User) != "" && !secrypto.IsSealed(r.User)
		needPass := strings.TrimSpace(r.Password) != "" && !secrypto.IsSealed(r.Password)
		if !needUser && !needPass {
			continue
		}
		user, err := secrypto.Open(r.User)
		if err != nil {
			return n, err
		}
		pass, err := secrypto.Open(r.Password)
		if err != nil {
			return n, err
		}
		r.User, r.Password = user, pass
		if err := flashbackStore.UpsertInstance(ctx, r); err != nil {
			return n, err
		}
		n++
	}
	return n, nil
}

func flashbackMigrateArgSecrets(ctx context.Context) (int, error) {
	rows, err := flashbackStore.ListArgs(ctx)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, r := range rows {
		if !flashbackArgIsSecret(r.Key) || strings.TrimSpace(r.Value) == "" || secrypto.IsSealed(r.Value) {
			continue
		}
		sealed, err := secrypto.MustSeal(r.Value)
		if err != nil {
			return n, err
		}
		if err := flashbackStore.UpsertArg(ctx, r.Key, sealed, r.Description); err != nil {
			return n, err
		}
		n++
	}
	return n, nil
}
