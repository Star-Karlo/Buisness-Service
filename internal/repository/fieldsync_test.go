package repository

import (
	"context"
	"os"
	"testing"

	"gorm.io/driver/postgres"
	"gorm.io/gorm"

	"github.com/karlo/business-service/internal/fieldconfig"
	"github.com/karlo/business-service/internal/models"
)

// SyncCatalog is the half of the field-configuration contract that makes the
// table trustworthy: the configurator reads field_definitions, so a field
// declared only in code would be invisible there.
//
// Skipped without TEST_DATABASE_URL, so the normal test run needs no database.
func TestSyncCatalogPublishesEveryDeclaredField(t *testing.T) {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("set TEST_DATABASE_URL to run the field catalogue sync against a database")
	}

	db, err := gorm.Open(postgres.Open(dsn), &gorm.Config{})
	if err != nil {
		t.Fatalf("connect: %v", err)
	}

	repo := NewFieldConfigRepository(db)
	ctx := context.Background()

	if err := repo.SyncCatalog(ctx); err != nil {
		t.Fatalf("SyncCatalog: %v", err)
	}

	for _, entity := range fieldconfig.Entities() {
		for _, declared := range fieldconfig.Declared(entity) {
			var row models.FieldDefinition
			err := db.Where("entity = ? AND key = ?", string(entity), declared.Key).First(&row).Error
			if err != nil {
				t.Errorf("%s.%s was declared in code but is absent from the table: %v",
					entity, declared.Key, err)
				continue
			}
			if !row.IsActive {
				t.Errorf("%s.%s is declared but marked inactive", entity, declared.Key)
			}
			if row.DefaultRequirement != string(declared.Default) {
				t.Errorf("%s.%s default = %q, want %q",
					entity, declared.Key, row.DefaultRequirement, declared.Default)
			}
			// Locked in code must be non-configurable in the table, or the
			// configurator will offer a toggle that CheckOverride then refuses.
			if row.Configurable == declared.Locked {
				t.Errorf("%s.%s configurable = %v but locked = %v",
					entity, declared.Key, row.Configurable, declared.Locked)
			}
		}
	}

	// Running twice must be a no-op, not a duplicate-key failure: this runs on
	// every startup, and every restart would otherwise crash the service.
	if err := repo.SyncCatalog(ctx); err != nil {
		t.Fatalf("second SyncCatalog: %v", err)
	}
}
