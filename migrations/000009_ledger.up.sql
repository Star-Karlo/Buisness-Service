-- The console's Finance pages: a chart of accounts and manual journal
-- entries per company.
--
-- Only what a person types is stored. The automatic postings — revenue when
-- an invoice is finalised, uang sangu when a trip is paid — are derived by
-- the console from orders and invoices every time the page loads, so the
-- journal can never disagree with the order it came from. Persisting them
-- would create a second copy of the truth to keep in step.
--
-- The nine system accounts every company starts with are inserted on first
-- read (see repository.LedgerRepository.EnsureSystemAccounts) rather than
-- here, because a company that does not exist yet cannot be seeded.

CREATE TABLE IF NOT EXISTS ledger_accounts (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    company_id  UUID NOT NULL,
    kode        TEXT NOT NULL,
    nama        TEXT NOT NULL,
    -- aset | kewajiban | modal | pendapatan | beban
    tipe        TEXT NOT NULL,
    is_system   BOOLEAN NOT NULL DEFAULT FALSE,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (company_id, kode)
);

CREATE TABLE IF NOT EXISTS ledger_manual_entries (
    id                 UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    company_id         UUID NOT NULL,
    tanggal            DATE NOT NULL,
    keterangan         TEXT NOT NULL DEFAULT '',
    akun_debit_kode    TEXT NOT NULL,
    akun_kredit_kode   TEXT NOT NULL,
    jumlah             NUMERIC(18,2) NOT NULL,
    created_by_user_id UUID,
    created_at         TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at         TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS idx_ledger_manual_entries_company_date
    ON ledger_manual_entries (company_id, tanggal DESC);
