package service

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"
)

func flashbackCheckpointTime(ctx context.Context, db *sql.DB) time.Time {
	if db == nil {
		return time.Time{}
	}
	var t time.Time
	if err := db.QueryRowContext(ctx, `SELECT checkpoint_time FROM pg_control_checkpoint()`).Scan(&t); err == nil {
		return t
	}
	return time.Time{}
}

func flashbackStreamCloudWAL(
	ctx context.Context,
	provider flashbackCloudWALProvider,
	spec flashbackCloudWALSpec,
	pkgs []flashbackCloudWALObject,
	walDir, dlDir string,
	dict *flashbackDictionary,
	opts flashbackParseOpts,
	maxBytes, bytesPerSec int64,
	retries int,
	onFetch func(done, total int),
	onPkg func(i int, obj flashbackCloudWALObject, files int, written int64),
	emit func(flashbackChange) bool,
) (flashbackParseStats, int64, error) {
	var st flashbackParseStats
	if dict != nil {
		st.WantedDB = dict.DBOID
		for _, rel := range dict.Wanted {
			if rel != nil && rel.RelNode != 0 {
				st.WantedRel = rel.RelNode
				break
			}
		}
	}
	if retries <= 0 {
		retries = 1
	}
	p := &flashbackWALParser{
		dict: dict, dboid: 0, fpw: flashbackNewFPWCache(opts.MaxFPWPages), st: &st,
		maxChanges: opts.MaxChanges, timeFrom: opts.TimeFrom, timeTo: opts.TimeTo,
		txn: flashbackNewTxnBuf(),
	}
	if dict != nil {
		p.dboid = dict.DBOID
	}
	if err := os.MkdirAll(walDir, 0o700); err != nil {
		return st, 0, err
	}
	if err := os.MkdirAll(dlDir, 0o700); err != nil {
		return st, 0, err
	}
	seen := map[string]struct{}{}
	var written int64
	for i, obj := range pkgs {
		if p.maxChanges > 0 && p.emitted >= p.maxChanges {
			st.ChangeTrunc = true
			break
		}
		if p.pastEnd {
			break
		}
		var files []flashbackWALFile
		var err error
		for attempt := 1; attempt <= retries; attempt++ {
			files, err = flashbackDownloadAndUnpackCloudPkg(ctx, provider, spec, obj, walDir, dlDir, bytesPerSec)
			if err == nil {
				break
			}
			if attempt == retries {
				return st, written, fmt.Errorf("增量包 %s: %w", firstNonEmpty(obj.Name, obj.ID), err)
			}
		}
		if onFetch != nil {
			onFetch(i+1, len(pkgs))
		}
		sort.Slice(files, func(a, b int) bool { return files[a].Name < files[b].Name })
		for _, f := range files {
			if _, ok := seen[f.Name]; ok {
				_ = os.Remove(filepath.Join(walDir, f.Name))
				continue
			}
			seen[f.Name] = struct{}{}
			if maxBytes > 0 && written+f.Size > maxBytes {
				return st, written, fmt.Errorf("解压后 WAL 超过上限 %d bytes，请缩小时间窗", maxBytes)
			}
			path := filepath.Join(walDir, f.Name)
			n := f.Size
			if fi, e := os.Stat(path); e == nil {
				n = fi.Size()
			}
			ch, ferr := p.feedFile(path)
			if opts.DeleteAfter {
				_ = os.Remove(path)
			}
			if ferr != nil {
				return st, written, fmt.Errorf("%s: %w", f.Name, ferr)
			}
			written += n
			for _, c := range ch {
				if emit != nil && !emit(c) {
					st.ChangeTrunc = true
					flashbackFinishParser(p, opts, emit, &st)
					return st, written, nil
				}
			}
		}
		if onPkg != nil {
			onPkg(i+1, obj, len(files), written)
		}
	}
	flashbackFinishParser(p, opts, emit, &st)
	return st, written, nil
}

func flashbackDownloadAndUnpackCloudPkg(
	ctx context.Context,
	provider flashbackCloudWALProvider,
	spec flashbackCloudWALSpec,
	obj flashbackCloudWALObject,
	walDir, dlDir string,
	bytesPerSec int64,
) ([]flashbackWALFile, error) {
	url, err := provider.DownloadURL(ctx, spec, obj)
	if err != nil {
		return nil, err
	}
	dest := filepath.Join(dlDir, flashbackCloudDownloadDestName(obj, url))
	if _, err := flashbackDownloadHTTP(ctx, url, dest, bytesPerSec); err != nil {
		return nil, err
	}
	defer os.Remove(dest)
	files, err := flashbackUnpackCloudWAL(dest, walDir)
	if err != nil {
		return nil, err
	}
	return files, nil
}

func flashbackListTencentLogBackups(ctx context.Context, instanceID, region string, from, to time.Time) ([]flashbackCloudWALObject, error) {
	p, err := flashbackNewTencentWALProvider(ctx, region)
	if err != nil {
		return nil, err
	}
	return p.ListByTime(ctx, flashbackCloudWALSpec{InstanceID: instanceID, From: from, To: to})
}
