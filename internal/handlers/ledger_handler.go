package handlers

import (
	"slices"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/shopspring/decimal"

	"github.com/karlo/business-service/internal/models"
	"github.com/karlo/business-service/internal/platform/response"
	"github.com/karlo/business-service/internal/repository"
)

// LedgerHandler serves the console's Finance pages: the chart of accounts
// and the journal lines a person typed. There is no service layer because
// there are no rules beyond "system accounts are read-only" and "a line
// debits one account and credits another".
type LedgerHandler struct {
	ledger *repository.LedgerRepository
}

func NewLedgerHandler(l *repository.LedgerRepository) *LedgerHandler {
	return &LedgerHandler{ledger: l}
}

type accountRequest struct {
	Kode string `json:"kode"`
	Nama string `json:"nama"`
	Tipe string `json:"tipe"`
}

func (r accountRequest) validate() string {
	if strings.TrimSpace(r.Kode) == "" || strings.TrimSpace(r.Nama) == "" {
		return "kode and nama are required"
	}
	if !slices.Contains(models.LedgerAccountTypes, r.Tipe) {
		return "tipe must be one of " + strings.Join(models.LedgerAccountTypes, ", ")
	}
	return ""
}

// ListAccounts lists the chart of accounts, seeding the system rows first.
//
// @Summary  Chart of accounts
// @Tags     Ledger
// @Security BearerAuth
// @Success  200 {array} models.LedgerAccount
// @Router   /ledger/accounts [get]
func (h *LedgerHandler) ListAccounts(c *gin.Context) {
	actor, ok := callerActor(c)
	if !ok {
		return
	}
	if err := h.ledger.EnsureSystemAccounts(c.Request.Context(), actor.CompanyID); err != nil {
		writeError(c, err)
		return
	}
	rows, err := h.ledger.ListAccounts(c.Request.Context(), actor.CompanyID)
	if err != nil {
		writeError(c, err)
		return
	}
	response.OK(c, rows)
}

// CreateAccount adds an account.
//
// @Summary  Add an account
// @Tags     Ledger
// @Security BearerAuth
// @Param    body body accountRequest true "Account"
// @Success  201 {object} models.LedgerAccount
// @Router   /ledger/accounts [post]
func (h *LedgerHandler) CreateAccount(c *gin.Context) {
	actor, ok := callerActor(c)
	if !ok {
		return
	}
	var req accountRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, err.Error())
		return
	}
	if msg := req.validate(); msg != "" {
		response.BadRequest(c, msg)
		return
	}
	row := &models.LedgerAccount{
		CompanyID: actor.CompanyID,
		Kode:      strings.TrimSpace(req.Kode),
		Nama:      strings.TrimSpace(req.Nama),
		Tipe:      req.Tipe,
	}
	if err := h.ledger.CreateAccount(c.Request.Context(), row); err != nil {
		if err == repository.ErrConflict {
			response.Conflict(c, "Kode akun sudah dipakai")
			return
		}
		writeError(c, err)
		return
	}
	response.Created(c, row)
}

// UpdateAccount renames or reclassifies an account; system accounts stay.
//
// @Summary  Edit an account
// @Tags     Ledger
// @Security BearerAuth
// @Param    id path string true "Account ID"
// @Param    body body accountRequest true "Account"
// @Success  200 {object} models.LedgerAccount
// @Router   /ledger/accounts/{id} [put]
func (h *LedgerHandler) UpdateAccount(c *gin.Context) {
	actor, ok := callerActor(c)
	if !ok {
		return
	}
	id, ok := pathUUID(c)
	if !ok {
		return
	}
	var req accountRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, err.Error())
		return
	}
	if msg := req.validate(); msg != "" {
		response.BadRequest(c, msg)
		return
	}
	row, err := h.ledger.FindAccount(c.Request.Context(), actor.CompanyID, id)
	if err != nil {
		writeError(c, err)
		return
	}
	if row.IsSystem {
		response.Conflict(c, "Akun sistem tidak dapat diubah")
		return
	}
	row.Kode = strings.TrimSpace(req.Kode)
	row.Nama = strings.TrimSpace(req.Nama)
	row.Tipe = req.Tipe
	if err := h.ledger.UpdateAccount(c.Request.Context(), row); err != nil {
		if err == repository.ErrConflict {
			response.Conflict(c, "Kode akun sudah dipakai")
			return
		}
		writeError(c, err)
		return
	}
	response.OK(c, row)
}

// DeleteAccount removes a non-system account.
//
// @Summary  Delete an account
// @Tags     Ledger
// @Security BearerAuth
// @Param    id path string true "Account ID"
// @Success  200
// @Router   /ledger/accounts/{id} [delete]
func (h *LedgerHandler) DeleteAccount(c *gin.Context) {
	actor, ok := callerActor(c)
	if !ok {
		return
	}
	id, ok := pathUUID(c)
	if !ok {
		return
	}
	row, err := h.ledger.FindAccount(c.Request.Context(), actor.CompanyID, id)
	if err != nil {
		writeError(c, err)
		return
	}
	if row.IsSystem {
		response.Conflict(c, "Akun sistem tidak dapat dihapus")
		return
	}
	if err := h.ledger.DeleteAccount(c.Request.Context(), actor.CompanyID, id); err != nil {
		writeError(c, err)
		return
	}
	response.OK(c, gin.H{"deleted": true})
}

type entryRequest struct {
	Tanggal        string          `json:"tanggal" binding:"required"`
	Keterangan     string          `json:"keterangan"`
	AkunDebitKode  string          `json:"akunDebitKode" binding:"required"`
	AkunKreditKode string          `json:"akunKreditKode" binding:"required"`
	Jumlah         decimal.Decimal `json:"jumlah"`
}

func (r entryRequest) parse() (time.Time, string) {
	t, err := time.Parse("2006-01-02", r.Tanggal)
	if err != nil {
		return time.Time{}, "tanggal must be YYYY-MM-DD"
	}
	if r.AkunDebitKode == r.AkunKreditKode {
		return time.Time{}, "akun debit dan kredit harus berbeda"
	}
	if !r.Jumlah.IsPositive() {
		return time.Time{}, "jumlah must be greater than zero"
	}
	return t, ""
}

// ListEntries lists the manual journal lines, newest first.
//
// @Summary  Manual journal entries
// @Tags     Ledger
// @Security BearerAuth
// @Success  200 {array} models.LedgerManualEntry
// @Router   /ledger/entries [get]
func (h *LedgerHandler) ListEntries(c *gin.Context) {
	actor, ok := callerActor(c)
	if !ok {
		return
	}
	rows, err := h.ledger.ListEntries(c.Request.Context(), actor.CompanyID)
	if err != nil {
		writeError(c, err)
		return
	}
	response.OK(c, rows)
}

// CreateEntry records a manual journal line.
//
// @Summary  Add a manual entry
// @Tags     Ledger
// @Security BearerAuth
// @Param    body body entryRequest true "Entry"
// @Success  201 {object} models.LedgerManualEntry
// @Router   /ledger/entries [post]
func (h *LedgerHandler) CreateEntry(c *gin.Context) {
	actor, ok := callerActor(c)
	if !ok {
		return
	}
	var req entryRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, err.Error())
		return
	}
	tanggal, msg := req.parse()
	if msg != "" {
		response.BadRequest(c, msg)
		return
	}
	userID := actor.UserID
	row := &models.LedgerManualEntry{
		CompanyID:       actor.CompanyID,
		Tanggal:         tanggal,
		Keterangan:      strings.TrimSpace(req.Keterangan),
		AkunDebitKode:   req.AkunDebitKode,
		AkunKreditKode:  req.AkunKreditKode,
		Jumlah:          req.Jumlah,
		CreatedByUserID: &userID,
	}
	if err := h.ledger.CreateEntry(c.Request.Context(), row); err != nil {
		writeError(c, err)
		return
	}
	response.Created(c, row)
}

// UpdateEntry edits a manual journal line.
//
// @Summary  Edit a manual entry
// @Tags     Ledger
// @Security BearerAuth
// @Param    id path string true "Entry ID"
// @Param    body body entryRequest true "Entry"
// @Success  200 {object} models.LedgerManualEntry
// @Router   /ledger/entries/{id} [put]
func (h *LedgerHandler) UpdateEntry(c *gin.Context) {
	actor, ok := callerActor(c)
	if !ok {
		return
	}
	id, ok := pathUUID(c)
	if !ok {
		return
	}
	var req entryRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, err.Error())
		return
	}
	tanggal, msg := req.parse()
	if msg != "" {
		response.BadRequest(c, msg)
		return
	}
	row, err := h.ledger.FindEntry(c.Request.Context(), actor.CompanyID, id)
	if err != nil {
		writeError(c, err)
		return
	}
	row.Tanggal = tanggal
	row.Keterangan = strings.TrimSpace(req.Keterangan)
	row.AkunDebitKode = req.AkunDebitKode
	row.AkunKreditKode = req.AkunKreditKode
	row.Jumlah = req.Jumlah
	if err := h.ledger.UpdateEntry(c.Request.Context(), row); err != nil {
		writeError(c, err)
		return
	}
	response.OK(c, row)
}

// DeleteEntry removes a manual journal line.
//
// @Summary  Delete a manual entry
// @Tags     Ledger
// @Security BearerAuth
// @Param    id path string true "Entry ID"
// @Success  200
// @Router   /ledger/entries/{id} [delete]
func (h *LedgerHandler) DeleteEntry(c *gin.Context) {
	actor, ok := callerActor(c)
	if !ok {
		return
	}
	id, ok := pathUUID(c)
	if !ok {
		return
	}
	if err := h.ledger.DeleteEntry(c.Request.Context(), actor.CompanyID, id); err != nil {
		writeError(c, err)
		return
	}
	response.OK(c, gin.H{"deleted": true})
}
