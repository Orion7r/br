// Copyright 2020 PingCAP, Inc. Licensed under Apache-2.0.

package utils

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/pingcap/errors"
	"github.com/pingcap/log"
	pd "github.com/tikv/pd/client"
	"github.com/tikv/pd/pkg/tsoutil"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"

	berrors "github.com/Orion7r/pr/pkg/errors"
)

const (
	brServiceSafePointIDFormat      = "br-%s"
	preUpdateServiceSafePointFactor = 3
	checkGCSafePointGapTime         = 5 * time.Second
	// DefaultBRGCSafePointTTL means PD keep safePoint limit at least 5min.
	DefaultBRGCSafePointTTL = 5 * 60
)

// BRServiceSafePoint is metadata of service safe point from a BR 'instance'.
type BRServiceSafePoint struct {
	ID       string
	TTL      int64
	BackupTS uint64
}

// MarshalLogObject implements zapcore.ObjectMarshaler.
func (sp BRServiceSafePoint) MarshalLogObject(encoder zapcore.ObjectEncoder) error {
	encoder.AddString("ID", sp.ID)
	ttlDuration := time.Duration(sp.TTL) * time.Second
	encoder.AddString("TTL", ttlDuration.String())
	backupTime, _ := tsoutil.ParseTS(sp.BackupTS)
	encoder.AddString("BackupTime", backupTime.String())
	encoder.AddUint64("BackupTS", sp.BackupTS)
	return nil
}

// getGCSafePoint returns the current gc safe point.
// TODO: Some cluster may not enable distributed GC.
func getGCSafePoint(ctx context.Context, pdClient pd.Client) (uint64, error) {
	safePoint, err := pdClient.UpdateGCSafePoint(ctx, 0)
	if err != nil {
		return 0, errors.Trace(err)
	}
	return safePoint, nil
}

// MakeSafePointID makes a unique safe point ID, for reduce name conflict.
func MakeSafePointID() string {
	return fmt.Sprintf(brServiceSafePointIDFormat, uuid.New())
}

// CheckGCSafePoint checks whether the ts is older than GC safepoint.
// Note: It ignores errors other than exceed GC safepoint.
func CheckGCSafePoint(ctx context.Context, pdClient pd.Client, ts uint64) error {
	// TODO: use PDClient.GetGCSafePoint instead once PD client exports it.
	safePoint, err := getGCSafePoint(ctx, pdClient)
	if err != nil {
		log.Warn("fail to get GC safe point", zap.Error(err))
		return nil
	}
	if ts <= safePoint {
		return errors.Annotatef(berrors.ErrBackupGCSafepointExceeded, "GC safepoint %d exceed TS %d", safePoint, ts)
	}
	return nil
}

// UpdateServiceSafePoint register BackupTS to PD, to lock down BackupTS as safePoint with TTL seconds.
func UpdateServiceSafePoint(ctx context.Context, pdClient pd.Client, sp BRServiceSafePoint) error {
	log.Debug("update PD safePoint limit with TTL",
		zap.Object("safePoint", sp))

	lastSafePoint, err := pdClient.UpdateServiceGCSafePoint(ctx,
		sp.ID, sp.TTL, sp.BackupTS-1)
	if lastSafePoint > sp.BackupTS-1 {
		log.Warn("service GC safe point lost, we may fail to back up if GC lifetime isn't long enough",
			zap.Uint64("lastSafePoint", lastSafePoint),
			zap.Object("safePoint", sp),
		)
	}
	return errors.Trace(err)
}

// StartServiceSafePointKeeper will run UpdateServiceSafePoint periodicity
// hence keeping service safepoint won't lose.
func StartServiceSafePointKeeper(
	ctx context.Context,
	pdClient pd.Client,
	sp BRServiceSafePoint,
) {
	// It would be OK since TTL won't be zero, so gapTime should > `0.
	updateGapTime := time.Duration(sp.TTL) * time.Second / preUpdateServiceSafePointFactor
	update := func(ctx context.Context) {
		if err := UpdateServiceSafePoint(ctx, pdClient, sp); err != nil {
			log.Warn("failed to update service safe point, backup may fail if gc triggered",
				zap.Error(err),
			)
		}
	}
	check := func(ctx context.Context) {
		if err := CheckGCSafePoint(ctx, pdClient, sp.BackupTS); err != nil {
			log.Panic("cannot pass gc safe point check, aborting",
				zap.Error(err),
				zap.Object("safePoint", sp),
			)
		}
	}
	updateTick := time.NewTicker(updateGapTime)
	checkTick := time.NewTicker(checkGCSafePointGapTime)
	update(ctx)
	go func() {
		defer updateTick.Stop()
		defer checkTick.Stop()
		for {
			select {
			case <-ctx.Done():
				log.Debug("service safe point keeper exited")
				return
			case <-updateTick.C:
				update(ctx)
			case <-checkTick.C:
				check(ctx)
			}
		}
	}()
}
