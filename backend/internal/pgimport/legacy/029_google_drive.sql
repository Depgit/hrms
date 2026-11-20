-- Every uploaded document is kept in the company's Google Drive (owner, 11 Nov 2026). An HR Admin connects a Google
-- account (OAuth) and pastes a Drive folder link in Masters › Company. An upload is staged on the server's disk, a
-- River job puts it in Drive (subfolder per person: SI-0001, EMP-1020, OFR-1001, or Company), records the Drive file
-- id here and deletes the staged copy. Opening a document streams it from Drive through the app.
--
-- Additive only, safe on a database with real data: two new tables and new columns on `document`. Documents that
-- exist before this migration stay on the server ('local') until an HR Admin clicks "Move existing files to Drive";
-- documents uploaded from now on are 'waiting' until they are safely in Drive ('drive').

-- One row: the connected account and the folder. The refresh token is encrypted with AES-GCM (key from APP_SECRET,
-- or a key file next to STORAGE_DIR); it is never returned by the API, logged or written to the audit log.
CREATE TABLE IF NOT EXISTS drive_setting (
  id int PRIMARY KEY CHECK (id = 1),
  folder_id text,                 -- the pasted folder (the root of every subfolder below)
  folder_name text,
  folder_link text,
  account_email text,             -- "Connected as"
  refresh_token bytea,            -- encrypted; NULL when not connected
  connected_by text,
  connected_at timestamptz,
  broken text,                    -- why the connection stopped working (access revoked), NULL when fine
  last_upload_at timestamptz,
  last_error text,                -- the last upload problem, in plain words
  last_error_at timestamptz,
  updated_at timestamptz NOT NULL DEFAULT now()
);
INSERT INTO drive_setting(id) VALUES (1) ON CONFLICT (id) DO NOTHING;

-- Subfolders already made under a root folder, so each is created once. A folder deleted or trashed in Drive is
-- noticed before the next upload and made again.
CREATE TABLE IF NOT EXISTS drive_folder (
  root_id text NOT NULL,
  name text NOT NULL,             -- SI-0001, EMP-1020, OFR-1001, Company
  folder_id text NOT NULL,
  created_at timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (root_id, name)
);

-- Rows already here are 'local' (moved only on request); the default for new rows is 'waiting'.
ALTER TABLE document ADD COLUMN IF NOT EXISTS drive_state text NOT NULL DEFAULT 'local';
ALTER TABLE document ALTER COLUMN drive_state SET DEFAULT 'waiting';
DO $$
BEGIN
  IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname='document_drive_state_check' AND conrelid='document'::regclass) THEN
    ALTER TABLE document ADD CONSTRAINT document_drive_state_check CHECK (drive_state IN ('local','waiting','drive'));
  END IF;
END $$;
ALTER TABLE document ADD COLUMN IF NOT EXISTS drive_file_id text;          -- set once the upload is confirmed
ALTER TABLE document ADD COLUMN IF NOT EXISTS drive_name text;             -- the file name in Drive
ALTER TABLE document ADD COLUMN IF NOT EXISTS drive_folder text;           -- the subfolder name (SI-0001 …)
ALTER TABLE document ADD COLUMN IF NOT EXISTS drive_uploaded_at timestamptz;
ALTER TABLE document ADD COLUMN IF NOT EXISTS drive_replaced_named boolean NOT NULL DEFAULT false; -- renamed "(replaced …)"
ALTER TABLE document ADD COLUMN IF NOT EXISTS drive_attempts int NOT NULL DEFAULT 0;
ALTER TABLE document ADD COLUMN IF NOT EXISTS drive_next_at timestamptz;   -- the next try after a failure
ALTER TABLE document ADD COLUMN IF NOT EXISTS drive_claim_until timestamptz; -- an upload in progress
ALTER TABLE document ADD COLUMN IF NOT EXISTS drive_error text;
CREATE INDEX IF NOT EXISTS document_drive_waiting_idx ON document(drive_next_at) WHERE drive_state = 'waiting';
CREATE INDEX IF NOT EXISTS document_drive_rename_idx ON document(id) WHERE drive_file_id IS NOT NULL AND replaced_at IS NOT NULL AND NOT drive_replaced_named;
