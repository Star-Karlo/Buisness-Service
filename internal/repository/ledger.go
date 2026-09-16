package repository

import (
	"context"
	"errors"

	"github.com/google/uuid"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/karlo/business-service/internal/models"
)

type LedgerRepository struct{ db *gorm.DB }

func NewLedgerRepository(db *gorm.DB) *LedgerRepository {
	return &LedgerRepository{db: db}
}

// EnsureSystemAccounts inserts the accounts every company starts with, once.
// ON CONFLICT DO NOTHING, so a company that renamed nothing and a company
// that exists since before this table get the same nine rows exactly once.
func (r *LedgerRepository) EnsureSystemAccounts(ctx context.Context, companyID uuid.UUID) error {
	rows := make([]models.LedgerAccount, 0, len(models.SystemLedgerAccounts))
	for _, a := range models.SystemLedgerAccounts {
		a.CompanyID = companyID
		a.IsSystem = true
		rows = append(rows, a)
	}
	return r.db.WithContext(ctx).
		Clauses(clause.OnConflict{Columns: []clause.Column{{Name: "company_id"}, {Name: "kode"}}, DoNothing: true}).
		Create(&rows).Error
}

func (r *LedgerRepository) ListAccounts(ctx context.Context, companyID uuid.UUID) ([]models.LedgerAccount, error) {
	var rows []models.LedgerAccount
	err := r.db.WithContext(ctx).Where("company_id = ?", companyID).Order("kode ASC").Find(&rows).Error
	return rows, err
}

func (r *LedgerRepository) FindAccount(ctx context.Context, companyID, id uuid.UUID) (*models.LedgerAccount, error) {
	var row models.LedgerAccount
	err := r.db.WithContext(ctx).Where("company_id = ? AND id = ?", companyID, id).First(&row).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, ErrNotFound
	}
	return &row, err
}

func (r *LedgerRepository) CreateAccount(ctx context.Context, a *models.LedgerAccount) error {
	err := r.db.WithContext(ctx).Create(a).Error
	if err != nil && isUniqueViolation(err) {
		return ErrConflict
	}
	return err
}

func (r *LedgerRepository) UpdateAccount(ctx context.Context, a *models.LedgerAccount) error {
	err := r.db.WithContext(ctx).Save(a).Error
	if err != nil && isUniqueViolation(err) {
		return ErrConflict
	}
	return err
}

func (r *LedgerRepository) DeleteAccount(ctx context.Context, companyID, id uuid.UUID) error {
	res := r.db.WithContext(ctx).Where("company_id = ? AND id = ? AND is_system = FALSE", companyID, id).Delete(&models.LedgerAccount{})
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		return ErrNotFound
	}
	return nil
}

func (r *LedgerRepository) ListEntries(ctx context.Context, companyID uuid.UUID) ([]models.LedgerManualEntry, error) {
	var rows []models.LedgerManualEntry
	err := r.db.WithContext(ctx).Where("company_id = ?", companyID).Order("tanggal DESC, created_at DESC").Find(&rows).Error
	return rows, err
}

func (r *LedgerRepository) FindEntry(ctx context.Context, companyID, id uuid.UUID) (*models.LedgerManualEntry, error) {
	var row models.LedgerManualEntry
	err := r.db.WithContext(ctx).Where("company_id = ? AND id = ?", companyID, id).First(&row).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, ErrNotFound
	}
	return &row, err
}

func (r *LedgerRepository) CreateEntry(ctx context.Context, e *models.LedgerManualEntry) error {
	return r.db.WithContext(ctx).Create(e).Error
}

func (r *LedgerRepository) UpdateEntry(ctx context.Context, e *models.LedgerManualEntry) error {
	return r.db.WithContext(ctx).Save(e).Error
}

func (r *LedgerRepository) DeleteEntry(ctx context.Context, companyID, id uuid.UUID) error {
	res := r.db.WithContext(ctx).Where("company_id = ? AND id = ?", companyID, id).Delete(&models.LedgerManualEntry{})
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		return ErrNotFound
	}
	return nil
}
