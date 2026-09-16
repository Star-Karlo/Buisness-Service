package models

import (
	"time"

	"github.com/google/uuid"
)

// LedgerAccount is one line of a company's chart of accounts.
type LedgerAccount struct {
	ID        uuid.UUID `gorm:"type:uuid;primaryKey;default:gen_random_uuid()" json:"id"`
	CompanyID uuid.UUID `gorm:"type:uuid;not null" json:"companyId"`
	Kode      string    `gorm:"not null" json:"kode"`
	Nama      string    `gorm:"not null" json:"nama"`
	Tipe      string    `gorm:"not null" json:"tipe"`
	// IsSystem marks the accounts the automatic postings route through.
	// They cannot be renamed or deleted: the console's derivations name
	// them by kode.
	IsSystem  bool      `gorm:"not null;default:false" json:"isSystem"`
	CreatedAt time.Time `json:"createdAt"`
	UpdatedAt time.Time `json:"updatedAt"`
}

func (LedgerAccount) TableName() string { return "ledger_accounts" }

// LedgerAccountTypes are the five account classes the reports group by.
var LedgerAccountTypes = []string{"aset", "kewajiban", "modal", "pendapatan", "beban"}

// SystemLedgerAccounts are the accounts every company starts with. The
// kodes are the ones the console's automatic postings use.
var SystemLedgerAccounts = []LedgerAccount{
	{Kode: "1-1000", Nama: "Kas/Bank", Tipe: "aset"},
	{Kode: "1-1100", Nama: "Piutang Usaha", Tipe: "aset"},
	{Kode: "2-1000", Nama: "Hutang Usaha", Tipe: "kewajiban"},
	{Kode: "3-1000", Nama: "Modal Pemilik", Tipe: "modal"},
	{Kode: "4-1000", Nama: "Pendapatan Jasa Angkutan", Tipe: "pendapatan"},
	{Kode: "4-9000", Nama: "Pendapatan Lain-lain", Tipe: "pendapatan"},
	{Kode: "5-1000", Nama: "Beban Uang Sangu", Tipe: "beban"},
	{Kode: "5-2000", Nama: "Beban Administrasi Bank", Tipe: "beban"},
	{Kode: "5-9000", Nama: "Beban Operasional Lain", Tipe: "beban"}, //nolint:misspell // Indonesian
}

// LedgerManualEntry is a journal line a person typed. The automatic lines
// are derived by the console from orders and invoices and never stored.
type LedgerManualEntry struct {
	ID              uuid.UUID  `gorm:"type:uuid;primaryKey;default:gen_random_uuid()" json:"id"`
	CompanyID       uuid.UUID  `gorm:"type:uuid;not null" json:"companyId"`
	Tanggal         time.Time  `gorm:"type:date;not null" json:"tanggal"`
	Keterangan      string     `gorm:"not null;default:''" json:"keterangan"`
	AkunDebitKode   string     `gorm:"column:akun_debit_kode;not null" json:"akunDebitKode"`
	AkunKreditKode  string     `gorm:"column:akun_kredit_kode;not null" json:"akunKreditKode"`
	Jumlah          Money      `gorm:"type:numeric(18,2);not null" json:"jumlah"`
	CreatedByUserID *uuid.UUID `gorm:"column:created_by_user_id;type:uuid" json:"createdByUserId,omitempty"`
	CreatedAt       time.Time  `json:"createdAt"`
	UpdatedAt       time.Time  `json:"updatedAt"`
}

func (LedgerManualEntry) TableName() string { return "ledger_manual_entries" }
