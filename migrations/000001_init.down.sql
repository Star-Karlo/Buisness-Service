DROP TRIGGER IF EXISTS trg_invoices_updated_at   ON invoices;
DROP TRIGGER IF EXISTS trg_shipments_updated_at  ON shipments;
DROP TRIGGER IF EXISTS trg_orders_updated_at     ON orders;
DROP TRIGGER IF EXISTS trg_agreements_updated_at ON agreements;
DROP FUNCTION IF EXISTS set_updated_at();
DROP FUNCTION IF EXISTS next_sequence_value(VARCHAR, VARCHAR);

DROP TABLE IF EXISTS number_sequences;
DROP TABLE IF EXISTS ratings;
DROP TABLE IF EXISTS invoice_lines;
DROP TABLE IF EXISTS invoices;
DROP TABLE IF EXISTS shipment_documents;
DROP TABLE IF EXISTS shipments;
DROP TABLE IF EXISTS order_status_history;
DROP TABLE IF EXISTS orders;
DROP TABLE IF EXISTS agreement_rates;
DROP TABLE IF EXISTS agreements;
