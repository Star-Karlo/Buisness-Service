package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"slices"
	"syscall"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/karlo/business-service/internal/archive"
	"github.com/karlo/business-service/internal/config"
	"github.com/karlo/business-service/internal/storage"
)

// runArchive is the cold-storage entry point.
//
//	server archive            archive everything past the retention window
//	server archive --dry-run  report what would be archived, touch nothing
//	server restore order <id> bring one aggregate back (order|agreement|invoice)
//
// Exit status is the report: non-zero when any aggregate failed, so the
// scheduled task shows red rather than a green run that skipped things.
func runArchive(cfg *config.Config, db *gorm.DB, args []string) error {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	store, err := storage.New(ctx, storage.Config{
		Bucket:    cfg.StorageBucket,
		Region:    cfg.StorageRegion,
		Endpoint:  cfg.StorageEndpoint,
		AccessKey: cfg.StorageAccessKey,
		SecretKey: cfg.StorageSecretKey,
	})
	if err != nil {
		return err
	}
	if !store.Configured() {
		return errors.New("archive: STORAGE_BUCKET is not set; nowhere to write")
	}

	a := archive.New(db, store, archive.Options{
		Retain: map[string]time.Duration{
			archive.EntityOrder:     cfg.ArchiveRetainOrders,
			archive.EntityAgreement: cfg.ArchiveRetainAgreements,
			archive.EntityInvoice:   cfg.ArchiveRetainInvoices,
		},
		Batch:  cfg.ArchiveBatch,
		DryRun: slices.Contains(args, "--dry-run"),
		Prefix: cfg.ArchivePrefix,
	})

	if i := slices.Index(args, "restore"); i >= 0 {
		if len(args) < i+3 {
			return errors.New("usage: server restore <order|agreement|invoice> <id>")
		}
		id, err := uuid.Parse(args[i+2])
		if err != nil {
			return fmt.Errorf("restore: %q is not a uuid", args[i+2])
		}
		err = a.Restore(ctx, args[i+1], id)
		if errors.Is(err, storage.ErrColdObject) {
			slog.Info("restore requested from Glacier; run the same command again once it has thawed (up to 48h)",
				"entity", args[i+1], "id", id)
			return nil
		}
		if err == nil {
			slog.Info("restored", "entity", args[i+1], "id", id)
		}
		return err
	}

	sum, err := a.Run(ctx)
	slog.Info("archive run finished",
		"invoices", sum.Invoices, "orders", sum.Orders, "agreements", sum.Agreements,
		"files", sum.Files, "bytes", sum.Bytes, "failed", sum.Failed, "dryRun", slices.Contains(args, "--dry-run"))
	if err != nil {
		return err
	}
	if sum.Failed > 0 {
		os.Exit(2)
	}
	return nil
}
