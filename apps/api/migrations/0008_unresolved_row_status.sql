-- no-transaction
-- R04 (ADR 0003): a row whose destination call may have been applied but
-- could not be confirmed.
ALTER TYPE row_status ADD VALUE IF NOT EXISTS 'unresolved';
